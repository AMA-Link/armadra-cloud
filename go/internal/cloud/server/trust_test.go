package server_test

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// ---- 辅助 ----

type trustRig struct {
	r                                  *rig
	owner, admin, member, other, guest session
	billing, outsider                  session
	team, otherTeam                    string
	host                               testHost
}

// newTrustRig 建一个团队：owner 登记了团队主机，admin、member、guest 各有访问权（member 整台、guest 限工作空间），
// other 是没有授予的 member，billing 不访问主机；另有别的团队的 outsider。
func newTrustRig(t *testing.T) *trustRig {
	t.Helper()
	r := newRig(t, rigOptions{})
	x := &trustRig{r: r}
	x.owner, x.admin, x.member, x.other, x.guest = r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	x.billing, x.outsider = r.verified(), r.verified()
	x.team = r.seedTeam("tr-1", map[string]string{
		x.owner.accountID: "owner", x.admin.accountID: "admin", x.member.accountID: "member", x.other.accountID: "member",
		x.guest.accountID: "guest", x.billing.accountID: "billing",
	})
	x.otherTeam = r.seedTeam("tr-2", map[string]string{x.outsider.accountID: "owner"})
	x.host = r.addHost(x.owner, teamSpace(x.team), "0.11.0")
	r.mustGrant(x.owner, x.host.id, memberOf(x.member.accountID), "viewer", nil)
	r.mustGrant(x.owner, x.host.id, memberOf(x.admin.accountID), "viewer", nil)
	r.mustGrant(x.owner, x.host.id, guestOf(x.guest.accountID), "viewer", map[string]any{"workspaceIds": []string{"w1"}})
	return x
}

func (x *trustRig) report(h testHost, s session, pendingID string, ttl time.Duration) response {
	x.r.t.Helper()
	return x.r.do("POST", "/v1/sources/me/trust-requests", map[string]any{
		"pendingId": pendingID, "accountId": s.accountID, "deviceId": s.deviceID, "expiresAtMs": x.r.clock.Ms() + ttl.Milliseconds(),
	}, x.r.sourceAuth(h))
}

func (x *trustRig) resolve(h testHost, pendingID, result string) response {
	x.r.t.Helper()
	return x.r.do("POST", "/v1/sources/me/trust-requests/"+pendingID+"/resolve", map[string]any{"pendingId": pendingID, "result": result}, x.r.sourceAuth(h))
}

func (x *trustRig) list(s session, query string) []map[string]any {
	x.r.t.Helper()
	res := x.r.do("GET", "/v1/trust-requests"+query, nil, s.bearer())
	x.r.expect(res, 200, "")
	var out []map[string]any
	for _, item := range res.json["requests"].([]any) {
		out = append(out, item.(map[string]any))
	}
	return out
}

func (x *trustRig) reject(s session, requestID string) response {
	x.r.t.Helper()
	return x.r.do("POST", "/v1/trust-requests/"+requestID+"/reject", nil, s.bearer())
}

func (x *trustRig) must(h testHost, s session, pendingID string) string {
	x.r.t.Helper()
	res := x.report(h, s, pendingID, time.Hour)
	x.r.expect(res, 200, "")
	if res.json["status"] != "pending" {
		x.r.t.Fatalf("应登记为 pending：%s", res.body)
	}
	return res.json["requestId"].(string)
}

func requestIDs(list []map[string]any) []string {
	out := []string{}
	for _, item := range list {
		out = append(out, item["requestId"].(string))
	}
	return out
}

// ---- sources.trustRequest / trustResolve ----

// 上报的校验：Source 认证、24 小时上限、源在团队里、账号是成员且对主机有访问权、设备是该账号未撤销的；
// 设备名与平台取自云端的设备表。
func TestTrustRequestValidation(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r

	// 没有 Source 认证 401。
	r.expect(r.do("POST", "/v1/sources/me/trust-requests", map[string]any{
		"pendingId": "p", "accountId": x.member.accountID, "deviceId": x.member.deviceID, "expiresAtMs": r.clock.Ms() + 1000,
	}, nil), 401, "unauthenticated")
	// 24 小时上限与过去的时刻。
	r.expect(x.report(x.host, x.member, "p1", 24*time.Hour+time.Second), 400, "bad_request")
	r.expect(x.report(x.host, x.member, "p1", -time.Second), 400, "bad_request")
	// 账号不是该团队成员、成员但没有访问权、guest 的访问权随授予、billing 无。
	r.expect(x.report(x.host, x.outsider, "p2", time.Hour), 403, "source_access_denied")
	r.expect(x.report(x.host, x.other, "p3", time.Hour), 403, "source_access_denied")
	r.expect(x.report(x.host, x.billing, "p4", time.Hour), 403, "source_access_denied")
	r.expect(x.report(x.host, x.guest, "p5", time.Hour), 200, "")
	// 个人空间的主机没有团队背书。
	personal := r.addHost(x.owner, nil, "0.11.0")
	r.expect(x.report(personal, x.owner, "p6", time.Hour), 403, "source_access_denied")
	// 设备：不存在、别人的、已撤销的 404。
	bad := x.member
	bad.deviceID = "nope"
	r.expect(x.report(x.host, bad, "p7", time.Hour), 404, "not_found")
	bad.deviceID = x.admin.deviceID
	r.expect(x.report(x.host, bad, "p8", time.Hour), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/me/devices/"+x.admin.deviceID, nil, x.admin.bearer()), 200, "")
	revoked := x.admin
	r.expect(x.report(x.host, revoked, "p9", time.Hour), 404, "not_found")
	// 设备名与平台取自设备表，不含校验码。
	id := x.must(x.host, x.member, "p10")
	var name, platform string
	if err := r.run.DB.Pool().QueryRow(ctxBackground, "SELECT device_name, platform FROM trust_requests WHERE request_id = $1", id).Scan(&name, &platform); err != nil {
		t.Fatal(err)
	}
	if name != "测试浏览器" || platform != "browser" {
		t.Fatalf("设备名与平台应取自设备表：%q %q", name, platform)
	}
}

// 幂等、冷却、取代：同 (源, pendingId) 重报答同一个 requestId；复用条目 id 给别的设备 409；
// 同设备换了条目 id 时旧的作废；被拒绝后的冷却期内答 rejected 且不建新请求，冷却结束后可以重新登记。
func TestTrustRequestIdempotentCooldownAndSupersede(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r

	id := x.must(x.host, x.member, "p1")
	again := x.report(x.host, x.member, "p1", time.Hour)
	r.expect(again, 200, "")
	if again.json["requestId"] != id || again.json["status"] != "pending" {
		t.Fatalf("重报应答同一个请求：%s", again.body)
	}
	// 同一个条目 id 给了别的设备：409。
	r.expect(x.report(x.host, x.guest, "p1", time.Hour), 409, "conflict")
	// 并发重报：同一个 requestId，只有一行。
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := x.report(x.host, x.member, "p1", time.Hour)
			got[i], _ = res.json["requestId"].(string)
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g != id {
			t.Fatalf("并发重报：%v", got)
		}
	}
	// 主机给同一设备换了新条目 id：旧的作废，只留一个 pending。
	id2 := x.must(x.host, x.member, "p2")
	if id2 == id || r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id) != "expired" ||
		r.queryString("SELECT resolved_by FROM trust_requests WHERE request_id = $1", id) != "superseded" {
		t.Fatal("同设备换条目 id 应作废旧请求")
	}
	if n := r.queryInt("SELECT count(*)::int FROM trust_requests WHERE device_id = $1 AND state = 'pending'", x.member.deviceID); n != 1 {
		t.Fatalf("同一设备只能有一个 pending：%d", n)
	}
	// 作废了的条目再报：409。
	r.expect(x.report(x.host, x.member, "p1", time.Hour), 409, "conflict")

	// 拒绝 → 冷却：同条目 id 与新条目 id 都答 rejected（同一个 requestId），不建新请求，列表里没有。
	r.expect(x.reject(x.owner, id2), 200, "")
	for _, pending := range []string{"p2", "p3"} {
		res := x.report(x.host, x.member, pending, time.Hour)
		r.expect(res, 200, "")
		if res.json["status"] != "rejected" || res.json["requestId"] != id2 {
			t.Fatalf("冷却期内应答 rejected：%s", res.body)
		}
	}
	if n := r.queryInt("SELECT count(*)::int FROM trust_requests WHERE device_id = $1", x.member.deviceID); n != 2 {
		t.Fatalf("冷却期内不该建新请求：%d", n)
	}
	if len(x.list(x.owner, "")) != 0 {
		t.Fatal("被拒绝的请求不再出现在列表里")
	}
	// 别的设备不受这台设备的冷却影响。
	x.must(x.host, x.guest, "g1")
	// 冷却结束（1 小时）：新条目 id 可以重新登记；旧条目 id 已是解决了的，409。
	r.clock.Advance(Cooldown + time.Second)
	res := x.report(x.host, x.member, "p3", time.Hour)
	r.expect(res, 200, "")
	if res.json["status"] != "pending" || res.json["requestId"] == id2 {
		t.Fatalf("冷却结束后应建新请求：%s", res.body)
	}
	// 旧条目 id 冷却结束后可以重新开：同一个 p2 变回 pending（新的 requestId），p3 那条作废，库里只剩一个 pending。
	res = x.report(x.host, x.member, "p2", time.Hour)
	r.expect(res, 200, "")
	reopened := res.json["requestId"].(string)
	if res.json["status"] != "pending" || reopened == id2 {
		t.Fatalf("冷却结束后同一条目 id 应重开成 pending（新 requestId）：%s", res.body)
	}
	if r.queryInt("SELECT count(*)::int FROM trust_requests WHERE request_id = $1", id2) != 0 ||
		r.queryString("SELECT pending_id FROM trust_requests WHERE request_id = $1", reopened) != "p2" ||
		r.queryInt("SELECT count(*)::int FROM trust_requests WHERE device_id = $1 AND state = 'pending'", x.member.deviceID) != 1 {
		t.Fatal("重开后应只有 p2 一个 pending")
	}
	again2 := x.report(x.host, x.member, "p2", time.Hour)
	if again2.json["requestId"] != reopened {
		t.Fatalf("重开后重报仍是同一个 requestId：%s", again2.body)
	}
}

// Cooldown 与服务端同值（1 小时）。
const Cooldown = time.Hour

// 回报：trusted / expired 改状态；没有的 404；已解决的 409；回报后请求不再列出，也不能再拒绝。
func TestTrustResolve(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r

	id := x.must(x.host, x.member, "p1")
	r.expect(x.resolve(x.host, "nope", "trusted"), 404, "not_found")
	r.expect(r.do("POST", "/v1/sources/me/trust-requests/p1/resolve", map[string]any{"pendingId": "p1", "result": "trusted"}, nil), 401, "unauthenticated")
	// 别的主机上的同名条目找不到（按 (源, pendingId) 找）。
	other := r.addHost(x.owner, teamSpace(x.team), "0.11.0")
	r.expect(x.resolve(other, "p1", "trusted"), 404, "not_found")
	r.expect(x.resolve(x.host, "p1", "trusted"), 200, "")
	if r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id) != "trusted" || r.queryString("SELECT resolved_by FROM trust_requests WHERE request_id = $1", id) != "source" {
		t.Fatal("应记 trusted / source")
	}
	r.expect(x.resolve(x.host, "p1", "trusted"), 409, "conflict")
	r.expect(x.resolve(x.host, "p1", "expired"), 409, "conflict")
	r.expect(x.reject(x.owner, id), 409, "conflict")
	if len(x.list(x.owner, "")) != 0 {
		t.Fatal("已批准的不再列出")
	}
	// expired 同理；回报后同条目 id 再登记 409。
	x.must(x.host, x.guest, "g1")
	r.expect(x.resolve(x.host, "g1", "expired"), 200, "")
	r.expect(x.report(x.host, x.guest, "g1", time.Hour), 409, "conflict")
	// 回报的限流与上报共用「每源每分钟 30 次」。
	for i := 0; i < 30; i++ {
		_ = x.resolve(x.host, fmt.Sprintf("none-%d", i), "trusted")
	}
	r.expect(x.resolve(x.host, "none-x", "trusted"), 429, "rate_limited")
	r.expect(x.report(x.host, x.member, "p9", time.Hour), 429, "rate_limited")
}

// ---- trust.list / trust.reject ----

// 按背书人收窄：主机所有者与团队 owner / admin（deviceEndorsers = owner_admin）、请求者本人（memberSelfEndorse）；
// 设置收窄之后相应的人看不到；别的成员、billing、别的团队看不到；列表里没有校验码。
func TestTrustListNarrowsByEndorser(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	idMember := x.must(x.host, x.member, "m1")
	idGuest := x.must(x.host, x.guest, "g1")

	list := x.list(x.owner, "")
	if !reflect.DeepEqual(requestIDs(list), []string{idGuest, idMember}) && !reflect.DeepEqual(requestIDs(list), []string{idMember, idGuest}) {
		t.Fatalf("owner 应看到两条：%v", list)
	}
	for _, item := range list {
		if _, bad := item["sas"]; bad {
			t.Fatal("不该有校验码")
		}
		if _, bad := item["code"]; bad {
			t.Fatal("不该有校验码")
		}
	}
	var memberItem map[string]any
	for _, item := range list {
		if item["requestId"] == idMember {
			memberItem = item
		}
	}
	memberEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", x.member.accountID)
	want := map[string]any{
		"requestId": idMember, "sourceId": x.host.id, "sourceName": "主机 " + x.host.id[:6], "teamId": x.team,
		"member":        map[string]any{"accountId": x.member.accountID, "email": memberEmail, "displayName": "测试用户"},
		"device":        map[string]any{"name": "测试浏览器", "platform": "browser"},
		"requestedAtMs": float64(r.clock.Ms()), "expiresAtMs": float64(r.clock.Ms() + 3600*1000),
	}
	if !reflect.DeepEqual(memberItem, want) {
		t.Fatalf("请求的形状：%v", memberItem)
	}
	// admin（owner_admin）看到全部；请求者本人只看到自己的；没有关系的成员、billing、别的团队看不到。
	if got := requestIDs(x.list(x.admin, "")); !reflect.DeepEqual(got, []string{idMember}) {
		t.Fatalf("admin 看不到 guest 的请求（guest 的请求只给主机所有者）：%v", got)
	}
	if got := requestIDs(x.list(x.member, "")); !reflect.DeepEqual(got, []string{idMember}) {
		t.Fatalf("member 本人：%v", got)
	}
	if got := x.list(x.guest, ""); len(got) != 0 {
		t.Fatalf("guest 本人也不是背书人：%v", got)
	}
	r.expect(x.reject(x.guest, idGuest), 403, "team_role_insufficient")
	r.expect(x.reject(x.admin, idGuest), 403, "team_role_insufficient")
	for _, s := range []session{x.other, x.billing, x.outsider} {
		if got := x.list(s, ""); len(got) != 0 {
			t.Fatalf("不相干的人看不到：%v", got)
		}
	}
	// space 过滤：团队空间、个人空间（空）、不是成员的团队 404。
	if got := len(x.list(x.owner, "?space[kind]=team&space[teamId]="+x.team)); got != 2 {
		t.Fatalf("按团队：%d", got)
	}
	if got := x.list(x.owner, "?space[kind]=personal"); len(got) != 0 {
		t.Fatalf("个人空间：%v", got)
	}
	r.expect(r.do("GET", "/v1/trust-requests?space[kind]=team&space[teamId]="+x.otherTeam, nil, x.owner.bearer()), 404, "not_found")

	// 新设备背书人收窄为只有主机所有者：admin 看不到了，owner 仍看得到；
	r.expect(r.do("PATCH", teamPath(x.team, "settings"), map[string]any{"deviceEndorsers": "owner"}, x.owner.bearer()), 200, "")
	if got := len(x.list(x.admin, "")); got != 0 {
		t.Fatalf("deviceEndorsers = owner 时 admin 不是背书人：%d", got)
	}
	if got := len(x.list(x.owner, "")); got != 2 {
		t.Fatalf("主机所有者仍是背书人：%d", got)
	}
	r.expect(x.reject(x.admin, idMember), 403, "team_role_insufficient")
	// 关掉成员本人背书：member 看不到、拒绝不了自己的请求。
	r.expect(r.do("PATCH", teamPath(x.team, "settings"), map[string]any{"memberSelfEndorse": false}, x.owner.bearer()), 200, "")
	if got := len(x.list(x.member, "")); got != 0 {
		t.Fatalf("memberSelfEndorse 关掉后本人不是背书人：%d", got)
	}
	r.expect(x.reject(x.member, idMember), 403, "team_role_insufficient")
	r.expect(r.do("PATCH", teamPath(x.team, "settings"), map[string]any{"memberSelfEndorse": true, "deviceEndorsers": "owner_admin"}, x.owner.bearer()), 200, "")

	// 请求者离开团队后，请求不再列出。
	r.exec("DELETE FROM team_members WHERE team_id = $1 AND account_id = $2", x.team, x.guest.accountID)
	if got := requestIDs(x.list(x.owner, "")); !reflect.DeepEqual(got, []string{idMember}) {
		t.Fatalf("成员离开后：%v", got)
	}
}

// 拒绝：背书人才行；别的团队 404（跨团队不可见）；已拒绝再拒绝是空操作；写审计；到期的请求 409 且不再列出；
// 到期作业把过期的 pending 落库。
func TestTrustRejectPermissionsAndExpiry(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	id := x.must(x.host, x.member, "m1")

	r.expect(x.reject(x.other, id), 403, "team_role_insufficient")
	r.expect(x.reject(x.billing, id), 403, "team_role_insufficient")
	r.expect(x.reject(x.outsider, id), 404, "not_found") // 别的团队的人连请求存在与否都看不到
	r.expect(x.reject(x.owner, "nope"), 404, "not_found")
	r.expect(x.reject(x.admin, id), 200, "")
	r.expect(x.reject(x.admin, id), 200, "") // 再拒绝是空操作
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'trust.reject' AND target_id = $1 AND actor_id = $2 AND team_id = $3", id, x.admin.accountID, x.team); n != 1 {
		t.Fatalf("应记一条审计：%d", n)
	}
	if got := r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id); got != "rejected" {
		t.Fatalf("状态：%s", got)
	}

	// 请求者本人（memberSelfEndorse 开着）可以拒绝自己的。
	id2 := x.must(x.host, x.guest, "g1")
	r.expect(x.reject(x.owner, id2), 200, "") // guest 的请求只有主机所有者能拒绝

	// 到期：时间一过就不再列出、拒绝 409；作业把它落库成 expired；同设备可以重新登记（过期的不挡）。
	idExpiring := func() string {
		res := x.report(x.host, x.member, "m2", 30*time.Minute)
		r.expect(res, 200, "")
		return res.json["requestId"].(string)
	}
	r.clock.Advance(Cooldown + time.Second) // 先让上一次拒绝的冷却结束
	owner := r.relogin(x.owner, r.queryString("SELECT email FROM accounts WHERE account_id = $1", x.owner.accountID))
	id3 := idExpiring()
	if len(x.list(owner, "")) != 1 {
		t.Fatal("新请求应列出")
	}
	// trust.expire 是每小时一个时间槽的定时作业：要越过下一个整点，作业才会再插一次。
	target := r.clock.Now().Add(31 * time.Minute)
	if target.Truncate(time.Hour).Equal(r.clock.Now().Truncate(time.Hour)) {
		target = r.clock.Now().Truncate(time.Hour).Add(time.Hour + time.Minute)
	}
	r.clock.Advance(target.Sub(r.clock.Now()))
	owner = r.relogin(owner, r.queryString("SELECT email FROM accounts WHERE account_id = $1", owner.accountID))
	if got := x.list(owner, ""); len(got) != 0 {
		t.Fatalf("到期的请求不再列出：%v", got)
	}
	r.expect(x.reject(owner, id3), 409, "conflict")
	eventuallyDB(t, "trust.expire 落库", func() bool {
		return r.queryString("SELECT state FROM trust_requests WHERE request_id = $1", id3) == "expired"
	})
	r.expect(x.report(x.host, x.member, "m2", time.Hour), 409, "conflict") // 已过期的条目 id 不能再登记
	x.must(x.host, x.member, "m3")
}

// 背书人条件写进 SQL 再 LIMIT：别人（背书人看不到的）有 500 多条更新的请求时，自己的请求仍然列得出来。
func TestTrustListFiltersBeforeLimit(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	mine := x.must(x.host, x.member, "m1")
	for i := 0; i < 510; i++ {
		r.exec(`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, team_id, account_id, device_id, device_name, platform,
		  state, requested_at_ms, expires_at_ms) VALUES ('cloud', $1, $2, $3, $4, $5, $6, 'x', 'desktop', 'pending', $7, $8)`,
			fmt.Sprintf("bulk-%d", i), x.host.id, fmt.Sprintf("bp-%d", i), x.team, x.other.accountID, fmt.Sprintf("bd-%d", i),
			r.clock.Ms()+int64(i)+1000, r.clock.Ms()+3600*1000)
	}
	if got := requestIDs(x.list(x.member, "")); !reflect.DeepEqual(got, []string{mine}) {
		t.Fatalf("member 应看到自己的那一条：%v", got)
	}
	if got := len(x.list(x.owner, "")); got != 500 {
		t.Fatalf("主机所有者看到的是截断到 500 的全部：%d", got)
	}
}

// 冷却检查在最前：p1 的冷却早已结束、p2 刚被拒时，重报旧的 p1 仍答 rejected 和最近被拒（p2）的 requestId；
// 冷却结束后重开 p1 时设备名、平台与团队按此刻的值刷新。
func TestTrustCooldownAppliesToOldPendingIDs(t *testing.T) {
	t.Parallel()
	x := newTrustRig(t)
	r := x.r
	ownerEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", x.owner.accountID)
	id1 := x.must(x.host, x.member, "p1")
	r.expect(x.reject(x.owner, id1), 200, "")
	r.clock.Advance(Cooldown + time.Second)
	owner := r.relogin(x.owner, ownerEmail)
	id2 := x.must(x.host, x.member, "p2")
	r.expect(x.reject(owner, id2), 200, "")

	res := x.report(x.host, x.member, "p1", time.Hour)
	r.expect(res, 200, "")
	if res.json["status"] != "rejected" || res.json["requestId"] != id2 {
		t.Fatalf("旧条目 id 也受冷却约束，应答最近被拒的 %s：%s", id2, res.body)
	}
	// 冷却结束：p1 重开成 pending，设备名刷新。
	r.clock.Advance(Cooldown + time.Second)
	r.exec("UPDATE devices SET name = '新设备名' WHERE device_id = $1", x.member.deviceID)
	res = x.report(x.host, x.member, "p1", time.Hour)
	r.expect(res, 200, "")
	if res.json["status"] != "pending" || res.json["requestId"] == id1 || res.json["requestId"] == id2 {
		t.Fatalf("冷却结束后 p1 重开成 pending：%s", res.body)
	}
	if got := r.queryString("SELECT device_name FROM trust_requests WHERE request_id = $1", res.json["requestId"]); got != "新设备名" {
		t.Fatalf("重开时设备名应刷新：%s", got)
	}
}
