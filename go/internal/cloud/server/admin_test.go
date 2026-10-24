package server_test

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// ops 造一个运营 Bearer 会话（直接建运营设备；MFA 登录的完整流程在 operator_test.go）。
func (r *rig) ops() (session, string) {
	r.t.Helper()
	return r.operator(core.TransportBearer)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func adminIDs(res response, list, key string) string {
	var out []string
	for _, row := range res.json[list].([]any) {
		out = append(out, fmt.Sprint(row.(map[string]any)[key]))
	}
	return strings.Join(out, ",")
}

// 运营后台只读：搜索（账号与团队混排、ILIKE、字面量通配符）、列表筛选与游标、详情（席位、本月流量、最近运营动作），
// 字段只有元数据，不含任何内容与令牌。
func TestAdminReadsAreMetadataOnly(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	alice, bob := r.verified(), r.verified()
	r.exec("UPDATE accounts SET display_name = '爱丽丝_甲', created_at_ms = 1000 WHERE account_id = $1", alice.accountID)
	r.exec("UPDATE accounts SET display_name = '鲍勃', created_at_ms = 2000 WHERE account_id = $1", bob.accountID)
	team := r.seedTeam("zeta", map[string]string{alice.accountID: "owner", bob.accountID: "member"})
	r.exec("UPDATE teams SET created_at_ms = 3000, name = '泽塔队' WHERE team_id = $1", team)
	h := r.addHost(alice, teamSpace(team), "0.10.0")
	r.traffic(r.clock.Now().UnixMilli(), h.id, team, "e2e", 21)
	token := alice.access

	// 搜索：邮箱 / 显示名 / slug 包含（不分大小写）；通配符是字面量；账号与团队混排、按创建时间倒序。
	res := r.do("GET", "/v1/admin/tenants?q="+url.QueryEscape("zeta"), nil, op.bearer())
	r.expect(res, 200, "")
	if got := adminIDs(res, "tenants", "id"); got != team {
		t.Fatalf("按 slug：%s", res.body)
	}
	row := res.json["tenants"].([]any)[0].(map[string]any)
	if row["kind"] != "team" || row["status"] != "active" || row["hostCount"] != float64(1) || row["plan"] != "free" || row["name"] != "泽塔队" {
		t.Fatalf("团队行：%v", row)
	}
	res = r.do("GET", "/v1/admin/tenants?q="+url.QueryEscape(strings.ToUpper(strings.Split(r.queryString("SELECT email FROM accounts WHERE account_id = $1", bob.accountID), "@")[0])), nil, op.bearer())
	r.expect(res, 200, "")
	if got := adminIDs(res, "tenants", "id"); got != bob.accountID {
		t.Fatalf("按邮箱包含：%s", res.body)
	}
	for _, q := range []string{"%25", "u_"} {
		res = r.do("GET", "/v1/admin/tenants?q="+q, nil, op.bearer())
		r.expect(res, 200, "")
		if len(res.json["tenants"].([]any)) != 0 {
			t.Fatalf("通配符 %q 按字面量：%s", q, res.body)
		}
	}
	res = r.do("GET", "/v1/admin/tenants?q="+url.QueryEscape("爱丽丝_"), nil, op.bearer())
	r.expect(res, 200, "")
	if adminIDs(res, "tenants", "id") != alice.accountID {
		t.Fatalf("显示名里的下划线是字面量：%s", res.body)
	}

	// 游标分页：每页 1 条，四页取完（运营账号最新，团队 3000 > bob 2000 > alice 1000）。
	seen := []string{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/v1/admin/tenants?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		res = r.do("GET", path, nil, op.bearer())
		r.expect(res, 200, "")
		seen = append(seen, adminIDs(res, "tenants", "id"))
		next, _ := res.json["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 4 || seen[0] != op.accountID || seen[1] != team || seen[2] != bob.accountID || seen[3] != alice.accountID {
		t.Fatalf("翻页顺序（运营账号最新，然后团队、bob、alice）：%v", seen)
	}
	r.expect(r.do("GET", "/v1/admin/tenants?cursor=bogus", nil, op.bearer()), 400, "bad_request")

	// 账号列表筛选与详情。
	r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", bob.accountID)
	res = r.do("GET", "/v1/admin/accounts?status=disabled", nil, op.bearer())
	r.expect(res, 200, "")
	if adminIDs(res, "accounts", "accountId") != bob.accountID {
		t.Fatalf("按状态：%s", res.body)
	}
	res = r.do("GET", "/v1/admin/accounts/"+alice.accountID, nil, op.bearer())
	r.expect(res, 200, "")
	if res.json["hostCount"] != float64(1) || res.json["teamCount"] != float64(1) || res.json["seats"] != float64(1) || res.json["emailVerified"] != true ||
		res.json["plan"] != "free" || res.json["relayBytesMonth"] != float64(0) || res.json["lastSeenAtMs"] == nil || res.json["recentActions"] == nil {
		t.Fatalf("账号详情：%s", res.body)
	}
	allowed := "accountId,createdAtMs,displayName,email,emailVerified,hostCount,lastSeenAtMs,plan,recentActions,relayBytesMonth,seats,status,teamCount"
	if got := strings.Join(keysOf(res.json), ","); got != allowed {
		t.Fatalf("账号详情只有元数据字段：%s", got)
	}
	r.expect(r.do("GET", "/v1/admin/accounts/nobody", nil, op.bearer()), 404, "not_found")

	// 团队列表与详情。
	res = r.do("GET", "/v1/admin/teams", nil, op.bearer())
	r.expect(res, 200, "")
	if adminIDs(res, "teams", "teamId") != team {
		t.Fatalf("团队列表：%s", res.body)
	}
	res = r.do("GET", "/v1/admin/teams/"+team, nil, op.bearer())
	r.expect(res, 200, "")
	if res.json["memberCount"] != float64(2) || res.json["hostCount"] != float64(1) || res.json["seats"] != float64(2) || res.json["relayBytesMonth"] != float64(42) ||
		res.json["status"] != "active" || res.json["deletedAtMs"] != nil {
		t.Fatalf("团队详情：%s", res.body)
	}
	allowed = "createdAtMs,deletedAtMs,hostCount,memberCount,name,plan,recentActions,relayBytesMonth,seats,slug,status,teamId"
	if got := strings.Join(keysOf(res.json), ","); got != allowed {
		t.Fatalf("团队详情只有元数据字段：%s", got)
	}
	r.expect(r.do("GET", "/v1/admin/teams?status=suspended", nil, op.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/admin/teams/nobody", nil, op.bearer()), 404, "not_found")
	// 什么答复里都没有访问令牌与口令。
	for _, body := range []string{string(res.body)} {
		if strings.Contains(body, token) || strings.Contains(body, password) {
			t.Fatal("运营答复里不该有令牌与口令")
		}
	}
}

// 停用账号：拒绝登录、撤销全部设备、已签令牌进撤销表并逐主机按人踢；写审计（operator、reason、无团队）；恢复只改状态。
func TestAdminAccountStatusKicksAndAudits(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("kick", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	issued := r.assertion(member, h.id)
	r.expect(issued, 200, "")
	email := r.queryString("SELECT email FROM accounts WHERE account_id = $1", member.accountID)

	// 未带理由、状态不合法：400；账号不存在：404；不许停用自己。
	r.expect(r.do("PUT", "/v1/admin/accounts/"+member.accountID+"/status", map[string]any{"status": "disabled"}, op.bearer()), 400, "bad_request")
	r.expect(r.do("PUT", "/v1/admin/accounts/"+member.accountID+"/status", map[string]any{"status": "gone", "reason": "x"}, op.bearer()), 400, "bad_request")
	r.expect(r.do("PUT", "/v1/admin/accounts/nobody/status", map[string]any{"status": "disabled", "reason": "x"}, op.bearer()), 404, "not_found")
	r.expect(r.do("PUT", "/v1/admin/accounts/"+op.accountID+"/status", map[string]any{"status": "disabled", "reason": "x"}, op.bearer()), 403, "forbidden")

	res := r.do("PUT", "/v1/admin/accounts/"+member.accountID+"/status", map[string]any{"status": "disabled", "reason": "滥用中继"}, op.bearer())
	r.expect(res, 200, "")
	if res.json["status"] != "disabled" || res.json["accountId"] != member.accountID {
		t.Fatalf("答复是更新后的账号：%s", res.body)
	}
	// 登录与已有会话都不行；已签断言与中继令牌进撤销表，在线连接被踢。
	r.expect(r.login(email, password, nil), 403, "account_disabled")
	r.expect(r.do("GET", "/v1/me", nil, member.bearer()), 401, "")
	if !r.bothRevoked(issued) {
		t.Fatal("停用应撤销该账号已签的令牌")
	}
	if kicks := r.kicks(); !contains(kicks, h.id+"|"+member.accountID+"|devices_revoked") {
		t.Fatalf("应按人踢掉该账号在主机上的连接：%v", kicks)
	}
	if r.queryInt("SELECT count(*)::int FROM devices WHERE subject = $1 AND revoked_at_ms IS NULL", member.accountID) != 0 {
		t.Fatal("全部设备撤销")
	}
	// 审计：actor.kind = operator、reason 列、没有团队。
	var kind, actor, reason, target string
	var teamID *string
	if err := r.run.DB.Pool().QueryRow(ctxBackground,
		"SELECT actor_kind, actor_id, reason, target_id, team_id FROM audit_log WHERE action = 'account.disable'").Scan(&kind, &actor, &reason, &target, &teamID); err != nil {
		t.Fatal(err)
	}
	if kind != "operator" || actor != op.accountID || reason != "滥用中继" || target != member.accountID || teamID != nil {
		t.Fatalf("审计：%s %s %s %s %v", kind, actor, reason, target, teamID)
	}
	// 详情里看得到最近的运营动作，但看不到理由之外的内容；团队审计里没有这条（它不属于团队）。
	res = r.do("GET", "/v1/admin/accounts/"+member.accountID, nil, op.bearer())
	r.expect(res, 200, "")
	actions := res.json["recentActions"].([]any)
	if len(actions) != 1 || actions[0].(map[string]any)["action"] != "account.disable" || actions[0].(map[string]any)["actor"].(map[string]any)["kind"] != "operator" {
		t.Fatalf("最近的运营动作：%s", res.body)
	}
	if strings.Contains(string(res.body), "滥用中继") {
		t.Fatal("理由只在审计表里，不进事件形状")
	}

	// 恢复：只改状态，旧设备不复活，重新登录可以。
	r.expect(r.do("PUT", "/v1/admin/accounts/"+member.accountID+"/status", map[string]any{"status": "active", "reason": "申诉通过"}, op.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, member.bearer()), 401, "")
	r.expect(r.login(email, password, nil), 200, "")
	if r.auditCount("action = 'account.enable' AND actor_kind = 'operator' AND reason = '申诉通过'") != 1 {
		t.Fatal("恢复也写审计")
	}
}

// 停用团队：拒绝断言（team_suspended）、撤销每台主机已签的令牌并踢掉全部连接、隧道保留；写审计带对象团队；恢复后断言可用。
func TestAdminTeamStatusKicksHostsKeepsTunnels(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("susp", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	h1 := r.addHost(owner, teamSpace(team), "0.10.0")
	h2 := r.addHost(owner, teamSpace(team), "0.10.0")
	personal := r.addHost(owner, nil, "0.10.0")
	r.mustGrant(owner, h1.id, teamOf(team), "viewer", nil)
	r.mustGrant(owner, h2.id, teamOf(team), "viewer", nil)
	a1, a2 := r.assertion(member, h1.id), r.assertion(member, h2.id)
	r.expect(a1, 200, "")
	r.expect(a2, 200, "")
	before := len(r.run.Relay.Commands())

	r.expect(r.do("PUT", "/v1/admin/teams/"+team+"/status", map[string]any{"status": "suspended"}, op.bearer()), 400, "bad_request")
	r.expect(r.do("PUT", "/v1/admin/teams/nobody/status", map[string]any{"status": "suspended", "reason": "x"}, op.bearer()), 404, "not_found")
	res := r.do("PUT", "/v1/admin/teams/"+team+"/status", map[string]any{"status": "suspended", "reason": "付款纠纷"}, op.bearer())
	r.expect(res, 200, "")
	if res.json["status"] != "suspended" {
		t.Fatalf("答复：%s", res.body)
	}
	if !r.bothRevoked(a1) || !r.bothRevoked(a2) {
		t.Fatal("团队每台主机的已签令牌都进撤销表")
	}
	kicked, closed := map[string]bool{}, false
	for _, c := range r.run.Relay.Commands()[before:] {
		switch c.Type {
		case relayctl.CommandKick:
			if c.Subject == "" {
				kicked[c.SourceID] = true // 按源踢掉全部连接
			}
		case relayctl.CommandCloseTunnel:
			closed = true
		}
	}
	if !kicked[h1.id] || !kicked[h2.id] || kicked[personal.id] || closed {
		t.Fatalf("踢团队两台主机的全部连接，不碰个人主机，隧道保留：%v closed=%v", kicked, closed)
	}
	r.expect(r.assertion(member, h1.id), 403, "team_suspended")
	r.expect(r.assertion(owner, h2.id), 403, "team_suspended")
	r.expect(r.hostToken(owner, teamSpace(team)), 403, "team_suspended")
	r.expect(r.assertion(owner, personal.id), 200, "") // 个人主机不受影响

	var kind, tid, reason string
	if err := r.run.DB.Pool().QueryRow(ctxBackground,
		"SELECT actor_kind, team_id, reason FROM audit_log WHERE action = 'team.suspend'").Scan(&kind, &tid, &reason); err != nil {
		t.Fatal(err)
	}
	if kind != "operator" || tid != team || reason != "付款纠纷" {
		t.Fatalf("审计带对象团队：%s %s %s", kind, tid, reason)
	}
	// 团队审计里能看到这条（owner 能读 audit.list），但看不到理由。
	list := r.do("GET", teamPath(team, "audit")+"?action=team.suspend", nil, owner.bearer())
	r.expect(list, 200, "")
	if len(list.json["events"].([]any)) != 1 || strings.Contains(string(list.body), "付款纠纷") {
		t.Fatalf("团队审计：%s", list.body)
	}
	res = r.do("GET", "/v1/admin/teams?status=suspended", nil, op.bearer())
	r.expect(res, 200, "")
	if adminIDs(res, "teams", "teamId") != team {
		t.Fatalf("按状态筛选：%s", res.body)
	}

	r.expect(r.do("PUT", "/v1/admin/teams/"+team+"/status", map[string]any{"status": "active", "reason": "已结清"}, op.bearer()), 200, "")
	r.expect(r.assertion(member, h1.id), 200, "")
	if r.auditCount("action = 'team.resume' AND team_id = $1", team) != 1 {
		t.Fatal("恢复也写审计")
	}
}

// 中继节点：列出（含排空中）、排空发 drain 命令并写审计；没有的节点 404；取消排空是空操作（节点只能重启）。
func TestAdminRelayNodes(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	r.run.Relay.SetNode(relayctl.Node{ID: "b", URL: "wss://b.edge.test/t/v1", Version: "0.10.0", Connections: 5, Tunnels: 2, Draining: true})
	r.run.Relay.SetNode(relayctl.Node{ID: "a", URL: "wss://a.edge.test/t/v1", Version: "0.10.0", Connections: 9, Tunnels: 4})

	res := r.do("GET", "/v1/admin/relay-nodes", nil, op.bearer())
	r.expect(res, 200, "")
	nodes := res.json["nodes"].([]any)
	if len(nodes) != 2 || nodes[0].(map[string]any)["node"] != "a" || nodes[1].(map[string]any)["draining"] != true {
		t.Fatalf("含排空中的，按 id 排序：%s", res.body)
	}
	if n := nodes[0].(map[string]any); n["online"] != true || n["connections"] != float64(9) || n["tunnels"] != float64(4) || n["version"] != "0.10.0" || n["lastSeenAtMs"] == nil {
		t.Fatalf("节点行：%v", n)
	}
	res = r.do("PUT", "/v1/admin/relay-nodes/a/drain", map[string]any{"drain": true, "reason": "升级前排空"}, op.bearer())
	r.expect(res, 200, "")
	if res.json["node"] != "a" || res.json["draining"] != true {
		t.Fatalf("答复：%s", res.body)
	}
	drains := 0
	for _, c := range r.run.Relay.Commands() {
		if c.Type == relayctl.CommandDrain && c.Node == "a" {
			drains++
		}
	}
	if drains != 1 || r.auditCount("action = 'relay.drain' AND actor_kind = 'operator' AND target_id = 'a'") != 1 {
		t.Fatalf("发一条 drain 命令并写审计：%d", drains)
	}
	if n := r.auditCount("action = 'relay.drain' AND target_id = 'a' AND reason = '升级前排空'"); n != 1 {
		t.Fatalf("理由应进审计：%d", n)
	}
	r.expect(r.do("PUT", "/v1/admin/relay-nodes/a/drain", map[string]any{"drain": true, "reason": ""}, op.bearer()), 400, "bad_request")
	r.expect(r.do("PUT", "/v1/admin/relay-nodes/a/drain", map[string]any{"drain": true, "reason": strings.Repeat("x", 501)}, op.bearer()), 400, "bad_request")
	r.expect(r.do("PUT", "/v1/admin/relay-nodes/zzz/drain", map[string]any{"drain": true}, op.bearer()), 404, "not_found")
	// 取消排空没有对应的命令（只能重启节点）：空操作，答复如实报节点此刻在排空。
	res = r.do("PUT", "/v1/admin/relay-nodes/b/drain", map[string]any{"drain": false}, op.bearer())
	r.expect(res, 200, "")
	if res.json["draining"] != true {
		t.Fatalf("drain=false 不改状态：%s", res.body)
	}
	// 已在排空时再排空：不重复发命令。
	r.expect(r.do("PUT", "/v1/admin/relay-nodes/b/drain", map[string]any{"drain": true}, op.bearer()), 200, "")
	for _, c := range r.run.Relay.Commands() {
		if c.Type == relayctl.CommandDrain && c.Node == "b" {
			t.Fatal("已经在排空的节点不重复发命令")
		}
	}
}

// 事故开关三项：停签断言、停注册、中继拒绝新连接——各自独立生效，只改给出的项，写审计带理由。
func TestAdminIncidentSwitches(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("inc", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	set := func(body map[string]any) response {
		body["reason"] = "演练"
		res := r.do("PUT", "/v1/admin/incident", body, op.bearer())
		r.expect(res, 200, "")
		return res
	}
	registers := func() int { return r.register(r.email(), nil).status }

	res := r.do("GET", "/v1/admin/incident", nil, op.bearer())
	r.expect(res, 200, "")
	if res.json["pauseAssertions"] != false || res.json["pauseRegistrations"] != false || res.json["rejectNewConnections"] != false || res.json["updatedAtMs"] != nil {
		t.Fatalf("没写过：全关：%s", res.body)
	}
	r.expect(r.do("PUT", "/v1/admin/incident", map[string]any{"pauseAssertions": true}, op.bearer()), 400, "bad_request") // 缺理由

	// 1) 停签断言：sources.assertion 503，注册与中继不受影响。
	res = set(map[string]any{"pauseAssertions": true})
	if res.json["pauseAssertions"] != true || res.json["pauseRegistrations"] != false || res.json["rejectNewConnections"] != false || res.json["updatedAtMs"] != float64(r.clock.Ms()) {
		t.Fatalf("答复：%s", res.body)
	}
	r.expect(r.assertion(member, h.id), 503, "unavailable")
	r.expect(r.assertion(owner, h.id), 503, "unavailable")
	if registers() != 200 || r.run.Relay.CurrentIncident().RejectNewConnections {
		t.Fatal("只停断言")
	}

	// 2) 停注册：auth.register 503 registrations_paused，platform.info 报 registrationsOpen = false；断言仍停（只改给出的项）。
	set(map[string]any{"pauseRegistrations": true})
	res = r.register(r.email(), nil)
	r.expect(res, 503, "registrations_paused")
	info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(info, 200, "")
	if info.json["registrationsOpen"] != false {
		t.Fatalf("platform.info：%s", info.body)
	}
	r.expect(r.assertion(member, h.id), 503, "unavailable")
	r.expect(r.login(r.queryString("SELECT email FROM accounts WHERE account_id = $1", member.accountID), password, nil), 200, "") // 登录不受影响

	// 3) 中继拒绝新连接：写 relay:incident（在线连接保留是节点的事）。
	set(map[string]any{"rejectNewConnections": true})
	if got := r.run.Relay.CurrentIncident(); !got.RejectNewConnections || got.AtMs != r.clock.Ms() {
		t.Fatalf("中继事故开关：%+v", got)
	}

	// 全部关掉：恢复。
	set(map[string]any{"pauseAssertions": false, "pauseRegistrations": false, "rejectNewConnections": false})
	r.expect(r.assertion(member, h.id), 200, "")
	if registers() != 200 || r.run.Relay.CurrentIncident().RejectNewConnections {
		t.Fatal("全部恢复")
	}
	res = r.do("GET", "/v1/admin/incident", nil, op.bearer())
	r.expect(res, 200, "")
	if res.json["pauseAssertions"] != false || res.json["updatedAtMs"] == nil {
		t.Fatalf("读回：%s", res.body)
	}
	// 每次改动都有审计，理由在 reason 列，对象是 incident。
	if n := r.auditCount("action = 'incident.set' AND actor_kind = 'operator' AND reason = '演练' AND actor_id = $1", op.accountID); n != 4 {
		t.Fatalf("审计：%d", n)
	}
}

// 别的实例改了库：各实例每隔一段时间按库把事故开关同步给节点，不靠 Redis 保活；Redis 里的键丢了也会重写。
func TestIncidentIsSyncedFromTheDatabase(t *testing.T) {
	r := newRig(t, rigOptions{incidentEvery: 30 * time.Millisecond})
	// 启动时就写过一遍（全关）。
	eventuallyDB(t, "启动同步", func() bool { return r.run.Relay.CurrentIncident().AtMs == 0 })
	r.exec(`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms, updated_by)
	  VALUES ('cloud', 'incident', '{"assertionsPaused":false,"registrationsPaused":false,"rejectNewConnections":true}', 4242, 'other-instance')`)
	eventuallyDB(t, "按库同步到节点", func() bool {
		got := r.run.Relay.CurrentIncident()
		return got.RejectNewConnections && got.AtMs == 4242
	})
	r.exec(`UPDATE platform_settings SET value = '{"assertionsPaused":false,"registrationsPaused":false,"rejectNewConnections":false}', updated_at_ms = 4343 WHERE key = 'incident'`)
	eventuallyDB(t, "关闭也同步", func() bool { return !r.run.Relay.CurrentIncident().RejectNewConnections })
}

// 运营会话对应的账号被摘掉运营范围或停用：会话没到期也立刻不能用；非运营账号的普通会话永远调不了。
func TestAdminRequiresALiveOperatorAccount(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.ops()
	r.expect(r.do("GET", "/v1/admin/incident", nil, op.bearer()), 200, "")
	r.exec("UPDATE accounts SET scopes = '{}' WHERE account_id = $1", op.accountID)
	r.expect(r.do("GET", "/v1/admin/incident", nil, op.bearer()), 403, "forbidden")
	r.exec("UPDATE accounts SET scopes = '{operator}', status = 'disabled' WHERE account_id = $1", op.accountID)
	r.expect(r.do("GET", "/v1/admin/incident", nil, op.bearer()), 401, "unauthenticated")
	normal := r.verified()
	for _, path := range []string{"/v1/admin/tenants", "/v1/admin/accounts", "/v1/admin/teams", "/v1/admin/relay-nodes", "/v1/admin/incident"} {
		r.expect(r.do("GET", path, nil, normal.bearer()), 401, "unauthenticated")
	}
	r.expect(r.do("PUT", "/v1/admin/incident", map[string]any{"pauseAssertions": true, "reason": "x"}, normal.bearer()), 401, "unauthenticated")
	if r.auditCount("action = 'incident.set'") != 0 {
		t.Fatal("普通会话改不了事故开关")
	}
}
