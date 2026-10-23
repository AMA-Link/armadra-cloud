package events_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/events"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

var ctx = context.Background()

func redisOptions(t *testing.T) *goredis.Options {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL，跳过")
	}
	options, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	return options
}

func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// 每种事件编码再解码得到原样；外壳是 {origin, event, audience}；不认识的 type、坏外壳被忽略。
func TestCodecRoundTripsEveryEvent(t *testing.T) {
	aud := &core.Audience{AccountIDs: []string{"a1", "a2"}}
	link := &core.Audience{LinkID: "l1"}
	cases := []struct {
		event    core.Event
		audience *core.Audience
	}{
		{events.NewMemberChanged("t1", "a1", events.MemberAdded, 5), aud},
		{events.NewMemberChanged("t1", "", events.MemberRemoved, 6), aud},
		{events.NewInvitationChanged("i1", "t1", "", "pending", 7), aud},
		{events.NewInvitationChanged("i2", "", "s1", "accepted", 8), nil},
		{events.NewGrantChanged("s1", "g1", "t1", 9), aud},
		{core.NewSourceRevoked("s1"), nil},
		{core.NewAccessRevoked("s1"), link},
		{core.NewSourceOnline("s1", 1, "https://s1.src.example"), nil},
		{core.NewSourceOffline("s1", 2), nil},
		{core.LinkUsed{Type: "linkUsed", LinkID: "l1", SourceID: "s1", By: core.LinkUsedBy{Guest: ptr(true)}, AtMs: 3}, link},
	}
	for _, c := range cases {
		data, err := events.Encode("cloud/x", c.event, c.audience)
		if err != nil {
			t.Fatal(err)
		}
		origin, event, audience, err := events.Decode(data)
		if err != nil || origin != "cloud/x" || !reflect.DeepEqual(event, c.event) || !reflect.DeepEqual(audience, c.audience) {
			t.Fatalf("%T 往返：%v %q %+v %+v（%s）", c.event, err, origin, event, audience, data)
		}
	}
	data, _ := events.Encode("cloud/x", events.NewGrantChanged("s1", "g1", "", 9), aud)
	for _, key := range []string{`"origin":"cloud/x"`, `"event":{"type":"grantChanged"`, `"audience":{"accountIds":["a1","a2"]}`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("外壳缺 %s：%s", key, data)
		}
	}
	for _, bad := range []string{``, `{`, `{"origin":"","event":{"type":"grantChanged"}}`, `{"origin":"o"}`,
		`{"origin":"o","event":{"type":"somethingNew"}}`, `{"origin":"o","event":{"type":"grantChanged","atMs":"x"}}`} {
		if _, _, _, err := events.Decode([]byte(bad)); err == nil {
			t.Fatalf("应忽略：%q", bad)
		}
	}
	// 新的事件种类登记之后能解（S6 / S7 的信任请求、计划变更等走这条路）。
	events.Register[probeEvent]("probeEvent")
	data, _ = events.Encode("cloud/x", probeEvent{Type: "probeEvent", Note: "n"}, nil)
	if _, event, _, err := events.Decode(data); err != nil || event != core.Event(probeEvent{Type: "probeEvent", Note: "n"}) {
		t.Fatalf("登记之后应能解：%v %v", event, err)
	}
}

type probeEvent struct {
	Type string `json:"type"`
	Note string `json:"note"`
}

func (probeEvent) EventSource() string { return "" }

func ptr[T any](v T) *T { return &v }

type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.got = append(r.got, s)
	r.mu.Unlock()
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newHub(t *testing.T, channel, origin string, rec *recorder, options *goredis.Options, mod func(*events.HubOptions)) (*events.Hub, *core.EventBus) {
	t.Helper()
	client := goredis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	bus := core.NewEventBus()
	bus.Subscribe(func(e core.Event, a *core.Audience) {
		name := reflect.ValueOf(e).FieldByName("Type").String()
		if g, ok := e.(events.GrantChanged); ok {
			name += ":" + g.GrantID
		}
		if a != nil {
			name += "@" + strings.Join(a.AccountIDs, ",")
		}
		rec.add(name)
	})
	o := events.HubOptions{Client: client, Channel: channel, Origin: origin, Bus: bus}
	if mod != nil {
		mod(&o)
	}
	hub := events.NewHub(o)
	if err := hub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Close)
	bus.SetPublisher(hub)
	return hub, bus
}

// 两个实例：一个实例 Emit，本机与对端各分发恰好一次（自己 origin 的消息不再投一次），顺序保持；Prepare 先于分发。
func TestHubDeliversOnceLocallyAndOnPeersInOrder(t *testing.T) {
	options := redisOptions(t)
	channel := "cloud-test-events:" + randomID()
	var prepared atomic.Int64
	a, b := &recorder{}, &recorder{}
	_, busA := newHub(t, channel, "cloud/a", a, options, func(o *events.HubOptions) {
		o.Prepare = func(context.Context, core.Event, *core.Audience) { prepared.Add(1) }
	})
	newHub(t, channel, "cloud/b", b, options, nil)

	aud := &core.Audience{AccountIDs: []string{"a1"}}
	for _, id := range []string{"g1", "g2", "g3"} {
		busA.Emit(events.NewGrantChanged("s1", id, "", 1), aud)
	}
	busA.Emit(core.NewSourceRevoked("s1"), nil)
	want := []string{"grantChanged:g1@a1", "grantChanged:g2@a1", "grantChanged:g3@a1", "sourceRevoked"}
	eventually(t, "对端收齐", func() bool { return len(b.list()) >= 4 })
	eventually(t, "本机收齐", func() bool { return len(a.list()) >= 4 })
	time.Sleep(200 * time.Millisecond) // 多余的投递（重复）会在这段时间出现
	if !reflect.DeepEqual(a.list(), want) || !reflect.DeepEqual(b.list(), want) {
		t.Fatalf("本机 %v，对端 %v，应都是 %v", a.list(), b.list(), want)
	}
	if prepared.Load() != 4 {
		t.Fatalf("发出方本机的分发也走 Prepare：%d", prepared.Load())
	}
}

// 订阅连接断开重连：OnResubscribed 回调、之后的事件照常到达。只杀本测试自己（按连接名）的订阅连接。
func TestHubResubscribesAfterConnectionLoss(t *testing.T) {
	options := redisOptions(t)
	channel := "cloud-test-events:" + randomID()
	name := "s8-hub-" + randomID()
	mine := *options
	mine.ClientName = name
	a, b := &recorder{}, &recorder{}
	var resubscribed atomic.Int64
	newHub(t, channel, "cloud/a", a, &mine, func(o *events.HubOptions) {
		o.OnResubscribed = func() { resubscribed.Add(1) }
		o.ReconnectMin = 20 * time.Millisecond
	})
	_, busB := newHub(t, channel, "cloud/b", b, options, nil)

	admin := goredis.NewClient(options)
	defer func() { _ = admin.Close() }()
	list, err := admin.ClientList(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	killed := 0
	for _, line := range strings.Split(list, "\n") {
		if !strings.Contains(line, "name="+name+" ") || !strings.Contains(line, "sub=1") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if id, ok := strings.CutPrefix(field, "id="); ok {
				if err := admin.Do(ctx, "CLIENT", "KILL", "ID", id).Err(); err != nil {
					t.Fatal(err)
				}
				killed++
			}
		}
	}
	if killed != 1 {
		t.Fatalf("应杀掉恰好一条订阅连接：%d", killed)
	}
	eventually(t, "重新订阅并回调", func() bool { return resubscribed.Load() == 1 })
	busB.Emit(events.NewGrantChanged("s1", "after", "", 1), &core.Audience{AccountIDs: []string{"a1"}})
	eventually(t, "重连之后的事件到达", func() bool { return len(a.list()) == 1 })
}
