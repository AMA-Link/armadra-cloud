package server_test

import (
	"testing"
	"time"
)

// ---- me-stream 的 deviceTrustRequested / deviceTrustResolved（契约 §31、§32）----

// trustWatch 是信任事件测试里每个账号的一条 me-stream 连接（一部分在另一个实例上）。
type trustWatch struct {
	x       *trustRig
	streams map[string]*streamConn
	names   map[string]string
}

func watchTrust(x *trustRig) *trustWatch {
	peer := x.r.at(x.r.startPeer().URL)
	w := &trustWatch{x: x, streams: map[string]*streamConn{}, names: map[string]string{}}
	for name, s := range map[string]session{
		"owner": x.owner, "admin": x.admin, "member": x.member, "other": x.other,
		"guest": x.guest, "billing": x.billing, "outsider": x.outsider,
	} {
		w.names[name] = s.accountID
		if name == "admin" || name == "member" || name == "outsider" {
			w.streams[name] = peer.stream(s) // 跨实例：事件经 Redis 到另一个实例的连接
		} else {
			w.streams[name] = x.r.stream(s)
		}
	}
	return w
}

func trustEvent(typ, requestID string) func(map[string]any) bool {
	return func(e map[string]any) bool { return e["type"] == typ && e["requestId"] == requestID }
}

// expect：to 里的账号都收到这条事件（答其中一条），其余账号在一小段时间内收不到。
func (w *trustWatch) expect(what string, match func(map[string]any) bool, to ...string) map[string]any {
	w.x.r.t.Helper()
	want := map[string]bool{}
	var got map[string]any
	for _, name := range to {
		want[name] = true
		got = w.streams[name].waitFor(what+"："+name, match)
	}
	first := true
	for name, stream := range w.streams {
		if want[name] {
			continue
		}
		wait := time.Duration(0)
		if first {
			wait = 300 * time.Millisecond // 给晚到的（另一个实例上的）事件留时间
			first = false
		}
		stream.quiet(wait, match, what+" 不该发给 "+name)
	}
	return got
}

// 登记、批准、拒绝、作废与到期都在提交之后发事件，只发给这条请求的背书人（与 trust.list 同一个判断）；
// 非背书人（没有授予的成员、guest、billing、别的团队）收不到。
func TestTrustEventsReachOnlyEndorsers(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	w := watchTrust(x)

	// 登记：成员的请求 → 主机所有者、团队 admin、本人（memberSelfEndorse 缺省开）。形状按契约 §32。
	id1 := x.must(x.host, x.member, "p1")
	ev := w.expect("登记", trustEvent("deviceTrustRequested", id1), "owner", "admin", "member")
	if ev["sourceId"] != x.host.id || ev["teamId"] != x.team || ev["atMs"] == nil || len(ev) != 5 {
		t.Fatalf("deviceTrustRequested 的形状：%v", ev)
	}
	// 重报同一个条目 id：同一个请求，不再发。
	r.expect(x.report(x.host, x.member, "p1", time.Hour), 200, "")
	time.Sleep(200 * time.Millisecond)
	if n := countEvents(w.streams["owner"], trustEvent("deviceTrustRequested", id1)); n != 1 {
		t.Fatalf("重报不该再发：%d", n)
	}

	// 批准（core 回报 trusted）：deviceTrustResolved outcome = trusted。
	r.expect(x.resolve(x.host, "p1", "trusted"), 200, "")
	ev = w.expect("批准", trustEvent("deviceTrustResolved", id1), "owner", "admin", "member")
	if ev["outcome"] != "trusted" || ev["atMs"] == nil || len(ev) != 4 {
		t.Fatalf("deviceTrustResolved 的形状：%v", ev)
	}

	// 拒绝：outcome = rejected；再拒绝是空操作，不再发。
	id2 := x.must(x.host, x.member, "p2")
	w.expect("登记 p2", trustEvent("deviceTrustRequested", id2), "owner", "admin", "member")
	r.expect(x.reject(x.admin, id2), 200, "")
	ev = w.expect("拒绝", trustEvent("deviceTrustResolved", id2), "owner", "admin", "member")
	if ev["outcome"] != "rejected" {
		t.Fatalf("拒绝的 outcome：%v", ev)
	}

	// guest 的请求只给主机所有者：团队 admin 与本人都不是背书人。
	idGuest := x.must(x.host, x.guest, "g1")
	w.expect("guest 登记", trustEvent("deviceTrustRequested", idGuest), "owner")
	// 同一设备换了条目 id：旧的作废（outcome = expired），新的登记。
	idGuest2 := x.must(x.host, x.guest, "g2")
	if ev := w.expect("作废", trustEvent("deviceTrustResolved", idGuest), "owner"); ev["outcome"] != "expired" {
		t.Fatalf("作废的 outcome：%v", ev)
	}
	w.expect("guest 再登记", trustEvent("deviceTrustRequested", idGuest2), "owner")
	// core 回报过期：outcome = expired。
	r.expect(x.resolve(x.host, "g2", "expired"), 200, "")
	if ev := w.expect("回报过期", trustEvent("deviceTrustResolved", idGuest2), "owner"); ev["outcome"] != "expired" {
		t.Fatalf("回报过期的 outcome：%v", ev)
	}

	// 团队设置收窄：deviceEndorsers = owner、memberSelfEndorse 关 → 只剩主机所有者。先让 p2 被拒的冷却结束。
	r.clock.Advance(Cooldown + time.Second)
	owner := r.relogin(x.owner, r.queryString("SELECT email FROM accounts WHERE account_id = $1", x.owner.accountID))
	r.expect(r.do("PATCH", teamPath(x.team, "settings"), map[string]any{"deviceEndorsers": "owner", "memberSelfEndorse": false}, owner.bearer()), 200, "")
	id3 := x.must(x.host, x.member, "p3")
	w.expect("收窄后登记", trustEvent("deviceTrustRequested", id3), "owner")
	r.expect(x.resolve(x.host, "p3", "trusted"), 200, "")
	w.expect("收窄后批准", trustEvent("deviceTrustResolved", id3), "owner")
	r.expect(r.do("PATCH", teamPath(x.team, "settings"), map[string]any{"deviceEndorsers": "owner_admin", "memberSelfEndorse": true}, owner.bearer()), 200, "")

	// 到期作业：trust.expire 落库成 expired 后发 outcome = expired。作业是每小时一个时间槽，要越过下一个整点。
	id4 := func() string {
		res := x.report(x.host, x.member, "p4", 30*time.Minute)
		r.expect(res, 200, "")
		return res.json["requestId"].(string)
	}()
	w.expect("登记 p4", trustEvent("deviceTrustRequested", id4), "owner", "admin", "member")
	target := r.clock.Now().Add(31 * time.Minute)
	if target.Truncate(time.Hour).Equal(r.clock.Now().Truncate(time.Hour)) {
		target = r.clock.Now().Truncate(time.Hour).Add(time.Hour + time.Minute)
	}
	r.clock.Advance(target.Sub(r.clock.Now()))
	eventuallyDB(t, "trust.expire 落库", func() bool {
		return r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id4) == "expired"
	})
	if ev := w.expect("到期", trustEvent("deviceTrustResolved", id4), "owner", "admin", "member"); ev["outcome"] != "expired" {
		t.Fatalf("到期的 outcome：%v", ev)
	}
}

// 事务回滚不发：拒绝在写审计时失败（整笔回滚）、上报在插新请求时失败（作废旧请求一并回滚），都不发事件；
// 请求的状态不变。去掉故障之后照常发。
func TestTrustEventsNotSentOnRollback(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	w := watchTrust(x)

	id := x.must(x.host, x.member, "p1")
	w.expect("登记", trustEvent("deviceTrustRequested", id), "owner", "admin", "member")

	// 审计写不进去：trust.reject 的事务回滚。
	r.exec(`CREATE FUNCTION te_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'te: 注入的失败'; END $$`)
	r.exec(`CREATE TRIGGER te_audit BEFORE INSERT ON audit_log FOR EACH ROW WHEN (NEW.action = 'trust.reject') EXECUTE FUNCTION te_fail()`)
	if res := x.reject(x.owner, id); res.status < 500 {
		t.Fatalf("注入失败后拒绝应答 5xx：%d %s", res.status, res.body)
	}
	if got := r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id); got != "pending" {
		t.Fatalf("回滚后应仍是 pending：%s", got)
	}

	// 插新请求失败：同设备换了条目 id 时旧请求的作废与新请求的登记一起回滚。
	r.exec(`CREATE TRIGGER te_insert BEFORE INSERT ON trust_requests FOR EACH ROW EXECUTE FUNCTION te_fail()`)
	if res := x.report(x.host, x.member, "p2", time.Hour); res.status < 500 {
		t.Fatalf("注入失败后上报应答 5xx：%d %s", res.status, res.body)
	}
	if got := r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id); got != "pending" {
		t.Fatalf("回滚后旧请求应仍是 pending：%s", got)
	}
	for name, stream := range w.streams {
		wait := time.Duration(0)
		if name == "owner" {
			wait = 500 * time.Millisecond
		}
		stream.quiet(wait, isType("deviceTrustResolved"), "回滚的事务不该发 deviceTrustResolved（"+name+"）")
		if n := countEvents(stream, isType("deviceTrustRequested")); name == "owner" && n != 1 {
			t.Fatalf("回滚的上报不该发 deviceTrustRequested：%v", stream.types())
		}
	}

	// 去掉故障：照常发。
	r.exec(`DROP TRIGGER te_insert ON trust_requests`)
	r.exec(`DROP TRIGGER te_audit ON audit_log`)
	r.expect(x.reject(x.owner, id), 200, "")
	w.expect("拒绝", trustEvent("deviceTrustResolved", id), "owner", "admin", "member")
}

func countEvents(c *streamConn, match func(map[string]any) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if match(e) {
			n++
		}
	}
	return n
}
