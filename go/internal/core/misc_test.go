package core_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func TestTrafficSummary(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	traffic := core.NewTraffic(e.h.Context)
	row := func(subject string, bucket int64) store.TrafficRow {
		return store.TrafficRow{BucketStartMs: bucket * store.HourMs, Node: "n", SourceID: sourceID, Subject: subject, Kind: "ws", Connections: 1}
	}
	e.must(e.h.Context.Ports.Traffic.Upsert(e.ctx, "t1", []store.TrafficRow{row("acct:alice", 1), row("guest:1111111111111111:ab", 2)}))

	input := core.TrafficSummaryInput{FromMs: 0, ToMs: 10 * store.HourMs}
	rows, err := traffic.Summary(e.ctx, owner, input)
	e.must(err)
	eq(t, len(rows), 2, "账号本人看全部")
	guest := core.Caller{Subject: "guest:1111111111111111:ab", DeviceID: "d", Role: core.RoleGuest, LinkID: "1111111111111111"}
	rows, err = traffic.Summary(e.ctx, guest, input)
	e.must(err)
	eq(t, len(rows), 1, "访客只看自己的")
	rows, err = traffic.Summary(e.ctx, owner, core.TrafficSummaryInput{FromMs: 10 * store.HourMs, ToMs: 20 * store.HourMs})
	e.must(err)
	if rows == nil || len(rows) != 0 {
		t.Fatalf("没有行时答空数组而不是 null：%v", rows)
	}
	_, err = traffic.Summary(e.ctx, owner, core.TrafficSummaryInput{FromMs: 5, ToMs: 5})
	eq(t, code(err), "bad_request", "from ≥ to")
}

func TestTrafficSummaryRefusesMoreThanTheCap(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	var rows []store.TrafficRow
	for i := 0; i < core.TrafficMaxRows+1; i++ {
		rows = append(rows, store.TrafficRow{BucketStartMs: store.HourMs, Node: fmt.Sprintf("n%05d", i), SourceID: sourceID, Subject: "acct:alice", Kind: "ws"})
	}
	e.must(e.h.Context.Ports.Traffic.Upsert(e.ctx, "t1", rows))
	_, err := core.NewTraffic(e.h.Context).Summary(e.ctx, owner, core.TrafficSummaryInput{FromMs: 0, ToMs: 2 * store.HourMs})
	eq(t, code(err), "bad_request", "超过 10,000 行")
	got, err := core.NewTraffic(e.h.Context).Summary(e.ctx, owner, core.TrafficSummaryInput{FromMs: 0, ToMs: 2 * store.HourMs, SourceID: "ffffffffffffffffffffffffffffffff"})
	e.must(err)
	eq(t, len(got), 0, "按别的源过滤")
}

func TestRevocationByJTIShape(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	now := e.h.Clock.Ms()
	revoked := func(jti string) bool {
		r, err := core.IsRevokedJTI(e.ctx, e.h.Context, jti, now)
		e.must(err)
		return r
	}
	grant, err := sources.Assertion(e.ctx, owner, sourceID, nil)
	e.must(err)
	relayJTI := claimsOf(t, grant.RelayToken)["jti"].(string)
	if revoked(relayJTI) {
		t.Fatal("有效的中继令牌")
	}
	if !revoked("t.ffffffffffffffffffffffffffffffff.abc") || revoked("t."+sourceID+".abc") {
		t.Fatal("隧道令牌按源判断")
	}
	for _, jti := range []string{"", "x", "t", "t.", "r.", "a.", "r.d", "zz.a.b", "unknown.shape"} {
		want := jti == "t" || jti == "t." || jti == "r." || jti == "a." || jti == "r.d"
		if got := revoked(jti); got != want {
			t.Errorf("jti %q 撤销 = %v，应为 %v", jti, got, want)
		}
	}
	// 设备过期 → 中继令牌失效。
	expired, err := core.IsRevokedJTI(e.ctx, e.h.Context, relayJTI, now+31*24*3600*1000)
	e.must(err)
	if !expired {
		t.Fatal("设备过期后中继令牌应失效")
	}
	// 源撤销 → 中继令牌失效。
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	if !revoked(relayJTI) {
		t.Fatal("源撤销后中继令牌应失效")
	}
}

func TestReplayCache(t *testing.T) {
	cache := core.NewReplayCache(1000, 3)
	if cache.Seen("a", 0, 100) || !cache.Seen("a", 0, 200) {
		t.Fatal("第一次记下，第二次是重放")
	}
	if cache.Seen("a", 0, 1200) {
		t.Fatal("过了保留时间不再算重放")
	}
	// 到上限时先清过期的，再淘汰最早记下的。
	cache = core.NewReplayCache(1000, 3)
	for i, id := range []string{"a", "b", "c"} {
		if cache.Seen(id, 0, int64(100+i)) {
			t.Fatal("新键")
		}
	}
	if cache.Seen("d", 0, 110) {
		t.Fatal("新键")
	}
	if cache.Seen("a", 0, 111) { // a 是最早的，被淘汰了
		t.Fatal("最早的应被淘汰")
	}
	if !cache.Seen("d", 0, 112) {
		t.Fatal("d 还在")
	}
}

func TestMeStreamTickets(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	stream := core.NewMeStream(e.h.Context)

	ticket := stream.Ticket(owner)
	eq(t, ticket.ExpiresAtMs, e.h.Clock.Ms()+30_000, "票 30 秒")
	e.h.Clock.Set(e.h.Clock.Ms() + 30_000)
	_, ok, err := stream.Redeem(e.ctx, ticket.Ticket)
	e.must(err)
	if ok {
		t.Fatal("过期的票不能兑换")
	}
	e.h.Clock.Set(coretest.DefaultNow)
	// 设备被撤销之后票作废。
	ticket = stream.Ticket(owner)
	_, err = core.RevokeDevice(e.ctx, e.h.Context, owner.DeviceID)
	e.must(err)
	if _, ok, _ := stream.Redeem(e.ctx, ticket.Ticket); ok {
		t.Fatal("设备失效后票不能兑换")
	}
	valid, err := stream.StillValid(e.ctx, owner)
	e.must(err)
	if valid {
		t.Fatal("设备已撤销")
	}
	// 票存哈希：明文不在内存表里是实现细节，这里只确认随机且不重复。
	a, b := stream.Ticket(owner), stream.Ticket(owner)
	if a.Ticket == b.Ticket || len(a.Ticket) != 43 {
		t.Fatalf("票应随机：%q %q", a.Ticket, b.Ticket)
	}
	stream.Clear()
	if _, ok, _ := stream.Redeem(e.ctx, a.Ticket); ok {
		t.Fatal("Clear 之后票作废")
	}
}

func TestMeStreamDeliversAccessRevokedOnlyToThatLink(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	stream := core.NewMeStream(e.h.Context)
	mk := func() (core.Caller, core.LinkAccepted, string) {
		created, err := links.Create(e.ctx, owner, linkInput(e))
		e.must(err)
		accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
		e.must(err)
		guest, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+accepted.GuestSession.AccessToken)
		e.must(err)
		return guest, accepted, created.LinkID
	}
	guestA, _, linkA := mk()
	guestB, _, _ := mk()
	var mu sync.Mutex
	var gotA, gotB, gotOwner []string
	collect := func(into *[]string) func(core.Event) {
		return func(ev core.Event) {
			mu.Lock()
			*into = append(*into, fmt.Sprintf("%T", ev))
			mu.Unlock()
		}
	}
	read := func(from *[]string) string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprint(*from)
	}
	_, _ = stream.Attach(guestA, collect(&gotA), nil)
	_, _ = stream.Attach(guestB, collect(&gotB), nil)
	detach, _ := stream.Attach(owner, collect(&gotOwner), nil)
	e.must(links.Revoke(e.ctx, owner, linkA))
	// 送达在连接自己的协程里：先等被撤销的访客收到，别的连接这时也该处理完了。
	waitUntil(t, func() bool { return read(&gotA) == "[core.AccessRevoked]" })
	time.Sleep(30 * time.Millisecond)
	eq(t, read(&gotA), "[core.AccessRevoked]", "被撤销的那条链接的访客")
	eq(t, read(&gotB), "[]", "别的链接的访客")
	eq(t, read(&gotOwner), "[]", "accessRevoked 带 audience，不发给 owner")
	detach()
	e.h.Context.Emit(core.NewSourceRevoked(sourceID), nil)
	time.Sleep(30 * time.Millisecond)
	eq(t, read(&gotOwner), "[]", "退订之后不再收到")
}

func TestEventBusIsolatesPanickingListeners(t *testing.T) {
	bus := core.NewEventBus()
	var got []string
	bus.Subscribe(func(core.Event, *core.Audience) { panic("订阅者出错") })
	off := bus.Subscribe(func(ev core.Event, _ *core.Audience) { got = append(got, ev.EventSource()) })
	bus.Emit(core.NewSourceRevoked("a"), nil)
	off()
	bus.Emit(core.NewSourceRevoked("b"), nil)
	eq(t, fmt.Sprint(got), "[a]", "出错的订阅者不影响别的，退订后不再收")
}

func TestIdentityFingerprintIs52LowercaseBase32(t *testing.T) {
	fp := core.IdentityFingerprint(make([]byte, 32))
	eq(t, len(fp), 52, "长度")
	for _, r := range fp {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			t.Fatalf("字符 %q 不在小写 base32 字母表里", r)
		}
	}
}

func TestErrorMessagesAreFixedEnglishPhrases(t *testing.T) {
	eq(t, core.MessageOf("account_locked"), "Too many failed sign-in attempts; try again later", "account_locked")
	eq(t, core.MessageOf("not_implemented"), "not_implemented", "没有专门文案的码答码本身")
	eq(t, core.MessageOf("bad_request"), "Bad request", "bad_request")
	eq(t, core.Fail("not_found").Error(), "not_found", "Error()")
	wait := core.FailAfter("rate_limited", 1500_001*1000) // 1500.001 ms
	eq(t, wait.RetryAfter.Milliseconds(), int64(1501), "等待向上取整到毫秒")
	eq(t, core.FailAfter("rate_limited", -5).RetryAfter.Milliseconds(), int64(0), "负数当 0")
}

func TestFailWithCopiesDetails(t *testing.T) {
	details := map[string]any{"setting": "memberHosts"}
	err := core.FailWith("forbidden", details)
	details["setting"] = "changed"
	eq(t, err.Details["setting"], any("memberHosts"), "详情是复制的")
	eq(t, string(err.Code), "forbidden", "码")
	eq(t, core.Fail("forbidden").Details == nil, true, "Fail 不带详情")
	eq(t, core.FailWith("forbidden", nil).Details == nil, true, "空详情不分配")
}

// 退订的订阅者不再留在集合里：反复订阅 / 退订之后扇出仍只调当前的订阅者。
func TestEventBusUnsubscribeRemovesSubscribers(t *testing.T) {
	bus := core.NewEventBus()
	var calls atomic.Int64
	for i := 0; i < 1000; i++ {
		off := bus.Subscribe(func(core.Event, *core.Audience) { calls.Add(1) })
		off()
		off() // 重复退订无害
	}
	keep := bus.Subscribe(func(core.Event, *core.Audience) { calls.Add(1) })
	defer keep()
	bus.Emit(core.NewSourceRevoked("a"), nil)
	eq(t, calls.Load(), int64(1), "只有仍在订阅的收到")
}

func TestEventBusSubscribeDuringEmit(t *testing.T) {
	bus := core.NewEventBus()
	var inner atomic.Int64
	var off func()
	off = bus.Subscribe(func(core.Event, *core.Audience) {
		// 订阅者里订阅 / 退订不会死锁。
		bus.Subscribe(func(core.Event, *core.Audience) { inner.Add(1) })
		off()
	})
	bus.Emit(core.NewSourceRevoked("a"), nil)
	bus.Emit(core.NewSourceRevoked("b"), nil)
	eq(t, inner.Load(), int64(1), "第一次发出时新订阅的在第二次才收到")
}

// 缓冲订阅：慢的订阅者不拖住 Emit，满了丢事件并通知，顺序保持。
func TestEventBusBufferedSubscriberNeverBlocksEmit(t *testing.T) {
	bus := core.NewEventBus()
	gate := make(chan struct{})
	var mu sync.Mutex
	var got []string
	var overflow atomic.Int64
	off := bus.SubscribeBuffered(2, func(ev core.Event, _ *core.Audience) {
		<-gate
		mu.Lock()
		got = append(got, ev.EventSource())
		mu.Unlock()
	}, func() { overflow.Add(1) })
	defer off()
	done := make(chan struct{})
	go func() {
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			bus.Emit(core.NewSourceRevoked(id), nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("慢订阅者拖住了 Emit")
	}
	if overflow.Load() == 0 {
		t.Fatal("队列满了应通知溢出")
	}
	close(gate)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 || got[0] != "a" {
		t.Fatalf("顺序：%v", got)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等不到条件成立")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 数查询次数的端口：Sources.Get 与 Links.Get。
type countingSources struct {
	store.SourceStore
	gets atomic.Int64
}

func (c *countingSources) Get(ctx context.Context, tenantID, sourceID string) (*store.SourceRecord, error) {
	c.gets.Add(1)
	return c.SourceStore.Get(ctx, tenantID, sourceID)
}

type countingLinks struct {
	store.LinkStore
	gets atomic.Int64
}

func (c *countingLinks) Get(ctx context.Context, tenantID, linkID string) (*store.LinkRecord, error) {
	c.gets.Add(1)
	return c.LinkStore.Get(ctx, tenantID, linkID)
}

// 一个源上线事件不管有多少连接，只查一次源；访客的链接只在 Attach 时查一次，之后每个事件都不再查库。
func TestMeStreamPresenceLooksUpOncePerEvent(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	e.must(err)
	guest, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+accepted.GuestSession.AccessToken)
	e.must(err)

	counted := *e.h.Context
	ports := *counted.Ports
	cs, cl := &countingSources{SourceStore: ports.Sources}, &countingLinks{LinkStore: ports.Links}
	ports.Sources, ports.Links = cs, cl
	counted.Ports = &ports
	stream := core.NewMeStream(&counted)

	var received atomic.Int64
	for i := 0; i < 3; i++ {
		attach(t, stream, owner, func(core.Event) { received.Add(1) })
	}
	for i := 0; i < 2; i++ {
		attach(t, stream, guest, func(core.Event) { received.Add(1) })
	}
	if got := cl.gets.Load(); got != 2 {
		t.Fatalf("访客各查一次链接，共 2 次，得 %d", got)
	}
	e.h.Publish(core.PresenceEvent{Online: true, SourceID: sourceID, CoreVersion: "1", AtMs: 1})
	waitUntil(t, func() bool { return received.Load() == 5 })
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("一个事件只查一次源，得 %d", got)
	}
	if got := cl.gets.Load(); got != 2 {
		t.Fatalf("事件分发不再查链接，得 %d", got)
	}
	// 同一事件循环里的 bus 事件也不查库。
	e.h.Context.Emit(core.NewSourceRevoked(sourceID), nil)
	waitUntil(t, func() bool { return received.Load() == 10 })
	if cs.gets.Load() != 1 || cl.gets.Load() != 2 {
		t.Fatalf("bus 事件不该查库：源 %d 链接 %d", cs.gets.Load(), cl.gets.Load())
	}
}

// 队列满了丢事件并通知装配方，发事件的一方不被拖住。
func TestMeStreamOverflowNotifies(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	stream := core.NewMeStream(e.h.Context)
	release := make(chan struct{})
	var overflowed atomic.Int64
	detach, _ := stream.Attach(owner, func(core.Event) { <-release }, func() { overflowed.Add(1) })
	defer detach()
	defer close(release)
	started := time.Now()
	for i := 0; i < 1000; i++ {
		e.h.Context.Emit(core.NewSourceRevoked(sourceID), nil)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("发事件被慢连接拖住了")
	}
	if overflowed.Load() == 0 {
		t.Fatal("队列满了应通知 onOverflow")
	}
}

func attach(t *testing.T, stream *core.MeStream, caller core.Caller, send func(core.Event)) {
	t.Helper()
	detach, err := stream.Attach(caller, send, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(detach)
}

type failingLinks struct{ store.LinkStore }

func (failingLinks) Get(context.Context, string, string) (*store.LinkRecord, error) {
	return nil, errors.New("库暂时不可用")
}

// 访客的链接查不了（库出错）：Attach 返回错误，由装配方关连接让客户端重连，而不是留一条永远收不到事件的连接。
func TestMeStreamAttachFailsWhenGuestLinkLookupFails(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	e.must(err)
	guest, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+accepted.GuestSession.AccessToken)
	e.must(err)
	broken := *e.h.Context
	ports := *broken.Ports
	ports.Links = failingLinks{ports.Links}
	broken.Ports = &ports
	if _, err := core.NewMeStream(&broken).Attach(guest, func(core.Event) {}, nil); err == nil {
		t.Fatal("链接查不了应返回错误")
	}
	if _, err := core.NewMeStream(&broken).Attach(owner, func(core.Event) {}, nil); err != nil {
		t.Fatalf("owner 不查链接：%v", err)
	}
}
