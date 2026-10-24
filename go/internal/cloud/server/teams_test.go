package server_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
)

var (
	ctxBackground = context.Background()
	seedCounter   atomic.Int64
)

const defaultSettingsJSON = `{"membersCanRegisterHosts":true,"defaultHostRole":"viewer","allowGuests":true,"allowAnonymousLinks":false,` +
	`"deviceEndorsers":"owner_admin","memberSelfEndorse":true,"requireMfa":false,"sessionMaxAgeMs":null,"allowedEmailDomains":[]}`

// verified 注册一个邮箱已验证的 Bearer 账号。
func (r *rig) verified() session {
	r.t.Helper()
	s := r.registerBearer(r.email())
	r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", s.accountID)
	return s
}

// seedTeam 直接写库建一个团队：members 是 账号 id → 角色（至少一个 owner）。
func (r *rig) seedTeam(slug string, members map[string]string) string {
	r.t.Helper()
	id := "team-" + slug
	ids := make([]string, 0, len(members))
	for accountID := range members {
		ids = append(ids, accountID)
	}
	sort.Strings(ids)
	creator := ""
	statements := [][2]any{}
	for _, accountID := range ids {
		if members[accountID] == "owner" && creator == "" {
			creator = accountID
		}
	}
	statements = append(statements, [2]any{`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
	  VALUES ('cloud', $1, $2, $3, $4, $5, 1, 1)`, []any{id, slug, "团队 " + slug, defaultSettingsJSON, creator}})
	for i, accountID := range ids {
		statements = append(statements, [2]any{`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms)
		  VALUES ('cloud', $1, $2, $3, $4)`, []any{id, accountID, members[accountID], int64(i + 1)}})
	}
	r.execTx(statements...)
	return id
}

// seedHost 写一台团队主机（sources 加 source_spaces）。
func (r *rig) seedHost(sourceID, teamID, ownerID string) {
	r.t.Helper()
	r.execTx(
		[2]any{`INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version, capabilities_json, registered_at_ms, trusted_origins_json)
		  VALUES ('cloud', $1, $1, 'desktop', '{}', '0.3.0', '[]', 1, '[]')`, []any{sourceID}},
		[2]any{`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms)
		  VALUES ('cloud', $1, $2, $3, 1, 1)`, []any{sourceID, teamID, ownerID}},
	)
}

func (r *rig) queryInt(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.run.DB.Pool().QueryRow(ctxBackground, sql, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (r *rig) queryString(sql string, args ...any) string {
	r.t.Helper()
	var s string
	if err := r.run.DB.Pool().QueryRow(ctxBackground, sql, args...).Scan(&s); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func teamPath(id string, rest ...string) string {
	path := "/v1/teams/" + id
	for _, part := range rest {
		path += "/" + part
	}
	return path
}

func TestTeamsCreateQuotaAndViews(t *testing.T) {
	r := newRig(t, rigOptions{})
	// 未验证邮箱不能建团队。
	raw := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "alpha", "name": "甲"}, raw.bearer()), 403, "email_unverified")

	owner := r.verified()
	res := r.do("POST", "/v1/teams", map[string]any{"slug": "alpha", "name": "  甲队  "}, owner.bearer())
	r.expect(res, 200, "")
	teamID := res.json["teamId"].(string)
	if res.json["role"] != "owner" || res.json["plan"] != "free" || res.json["name"] != "甲队" || res.json["slug"] != "alpha" {
		t.Fatalf("创建的团队：%s", res.body)
	}
	// 团队并发额度经 relayctl 写给节点：Free 的 10（与节点缺键时的最严缺省一致）。
	if quota, ok := r.run.Relay.Quota(teamID); !ok || quota != (relayctl.Quota{Concurrent: 10}) {
		t.Fatalf("团队并发额度：%+v %v", quota, ok)
	}
	// 创建者是 owner、有 Free 计划的 entitlements、审计一行。
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND account_id = $2 AND role = 'owner'", teamID, owner.accountID); n != 1 {
		t.Fatal("创建者应是 owner")
	}
	if n := r.queryInt("SELECT count(*)::int FROM entitlements WHERE space_kind = 'team' AND space_id = $1 AND plan = 'free'", teamID); n != 1 {
		t.Fatal("应有 Free 计划的 entitlements")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE team_id = $1 AND action = 'team.create' AND actor_id = $2", teamID, owner.accountID); n != 1 {
		t.Fatal("应有 team.create 审计")
	}

	// slug 被占用；再建一个撞个人账号的团队额度（Free 一个）。
	other := r.verified()
	r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "alpha", "name": "乙"}, other.bearer()), 409, "team_slug_taken")
	res = r.do("POST", "/v1/teams", map[string]any{"slug": "beta", "name": "乙"}, owner.bearer())
	r.expect(res, 429, "limit_reached")
	details := res.json["details"].(map[string]any)
	if details["quota"] != "teams" || details["used"] != float64(1) || details["limit"] != float64(1) {
		t.Fatalf("limit_reached 的 details：%s", res.body)
	}
	// 非法 slug / 空名字。
	r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "Bad_Slug", "name": "x"}, other.bearer()), 400, "bad_request")
	r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "gamma", "name": "   "}, other.bearer()), 400, "bad_request")

	// list / get / me.get / me.spaces。
	res = r.do("GET", "/v1/teams", nil, owner.bearer())
	r.expect(res, 200, "")
	if list := res.json["teams"].([]any); len(list) != 1 || list[0].(map[string]any)["teamId"] != teamID {
		t.Fatalf("teams.list：%s", res.body)
	}
	r.expect(r.do("GET", teamPath(teamID), nil, owner.bearer()), 200, "")
	res = r.do("GET", "/v1/me", nil, owner.bearer())
	if teams := res.json["teams"].([]any); len(teams) != 1 || teams[0].(map[string]any)["teamId"] != teamID {
		t.Fatalf("me.get 的 teams：%s", res.body)
	}
	res = r.do("GET", "/v1/me/spaces", nil, owner.bearer())
	r.expect(res, 200, "")
	want := []any{
		map[string]any{"kind": "personal", "plan": "free"},
		map[string]any{"kind": "team", "teamId": teamID, "slug": "alpha", "name": "甲队", "role": "owner", "plan": "free"},
	}
	if !reflect.DeepEqual(res.json["spaces"], want) {
		t.Fatalf("me.spaces：%s", res.body)
	}
	res = r.do("GET", "/v1/me/spaces", nil, other.bearer())
	if !reflect.DeepEqual(res.json["spaces"], []any{map[string]any{"kind": "personal", "plan": "free"}}) {
		t.Fatalf("没有团队的账号：%s", res.body)
	}

	// update：改名、设头像、清头像；头像必须是 http(s)。
	res = r.do("PATCH", teamPath(teamID), map[string]any{"name": "甲队改", "avatarUrl": "https://img.example.com/a.png"}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["name"] != "甲队改" || res.json["avatarUrl"] != "https://img.example.com/a.png" {
		t.Fatalf("teams.update：%s", res.body)
	}
	res = r.do("PATCH", teamPath(teamID), map[string]any{"avatarUrl": nil}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["avatarUrl"] != nil || res.json["name"] != "甲队改" {
		t.Fatalf("清头像不动名字：%s", res.body)
	}
	r.expect(r.do("PATCH", teamPath(teamID), map[string]any{"avatarUrl": "javascript:alert(1)"}, owner.bearer()), 400, "bad_request")
	r.expect(r.do("PATCH", teamPath(teamID), map[string]any{"name": " "}, owner.bearer()), 400, "bad_request")
}

// 并发建团队不越过个人账号的 Free 额度（锁账号行后计数）。
func TestTeamsCreateConcurrentRespectsQuota(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = r.do("POST", "/v1/teams", map[string]any{"slug": fmt.Sprintf("race-%d", i), "name": "竞争"}, owner.bearer()).status
		}()
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, status := range statuses {
		switch status {
		case 200:
			ok++
		case 429:
			limited++
		}
	}
	if ok != 1 || limited != 3 {
		t.Fatalf("应恰好一个成功：%v", statuses)
	}
}

type matrixCase struct {
	name   string
	method string
	path   func(team string) string
	body   func() any
	// allowed 是通过角色门槛的角色；其余答 403 team_role_insufficient。
	allowed []string
	// outcome 是通过门槛的角色得到的 (状态, 错误码)；缺省 (200, "")。
	outcome func(role string) (int, string)
}

var (
	roles      = []string{"owner", "admin", "member", "billing", "guest"}
	anyRole    = roles
	directory  = []string{"owner", "admin", "member", "billing"}
	staff      = []string{"owner", "admin", "member"}
	adminUp    = []string{"owner", "admin"}
	ownerOnly  = []string{"owner"}
	matrixNoop = func(string) (int, string) { return 200, "" }
)

// 角色矩阵：5 个角色 × 每个带 teamId 的过程。每个格子一个全新的团队，互不影响。
// 团队里除了五个被测角色，还有一个可被改动的 member（x）和一个可接任 owner 的 admin（y）。
func TestTeamsRoleMatrix(t *testing.T) {
	r := newRig(t, rigOptions{})
	cases := []matrixCase{
		{name: "teams.get", method: "GET", path: func(id string) string { return teamPath(id) }, allowed: anyRole},
		{name: "teams.update", method: "PATCH", path: func(id string) string { return teamPath(id) },
			body: func() any { return map[string]any{"name": "新名字"} }, allowed: adminUp},
		{name: "teams.delete", method: "DELETE", path: func(id string) string { return teamPath(id) }, allowed: ownerOnly},
		{name: "teams.restore", method: "POST", path: func(id string) string { return teamPath(id, "restore") }, allowed: ownerOnly},
		{name: "teams.transferOwnership", method: "POST", path: func(id string) string { return teamPath(id, "transfer-ownership") },
			body: func() any { return map[string]any{"accountId": "y"} }, allowed: ownerOnly},
		{name: "teams.members", method: "GET", path: func(id string) string { return teamPath(id, "members") }, allowed: directory},
		{name: "teams.memberRole", method: "PATCH", path: func(id string) string { return teamPath(id, "members", "x") },
			body: func() any { return map[string]any{"role": "billing"} }, allowed: adminUp},
		{name: "teams.memberRemove", method: "DELETE", path: func(id string) string { return teamPath(id, "members", "x") }, allowed: adminUp},
		{name: "teams.leave", method: "POST", path: func(id string) string { return teamPath(id, "leave") }, allowed: anyRole,
			// 唯一的 owner 不能走。
			outcome: func(role string) (int, string) {
				if role == "owner" {
					return 409, "team_last_owner"
				}
				return 200, ""
			}},
		{name: "teams.settings", method: "GET", path: func(id string) string { return teamPath(id, "settings") }, allowed: staff},
		{name: "teams.settingsUpdate", method: "PATCH", path: func(id string) string { return teamPath(id, "settings") },
			body: func() any { return map[string]any{"allowGuests": false} }, allowed: adminUp},
		{name: "teams.usage", method: "GET", path: func(id string) string { return teamPath(id, "usage") }, allowed: directory},
	}
	people := map[string]session{}
	for _, role := range append(append([]string{}, roles...), "x", "y") {
		people[role] = r.verified()
	}
	for _, c := range cases {
		for _, role := range roles {
			t.Run(c.name+"/"+role, func(t *testing.T) {
				members := map[string]string{}
				for _, name := range append(append([]string{}, roles...), "x", "y") {
					kind := name
					switch name {
					case "x":
						kind = "member"
					case "y":
						kind = "admin"
					}
					members[people[name].accountID] = kind
				}
				team := r.seedTeam(fmt.Sprintf("m-%d-%s", seedCounter.Add(1), role), members)
				path := c.path(team)
				// 路径里的占位成员 x / y 换成真实账号 id。
				path = replaceAccount(path, "/members/x", "/members/"+people["x"].accountID)
				var body any
				if c.body != nil {
					body = fixAccounts(c.body(), people)
				}
				res := r.do(c.method, path, body, people[role].bearer())
				allowed := false
				for _, a := range c.allowed {
					allowed = allowed || a == role
				}
				if !allowed {
					r.expect(res, 403, "team_role_insufficient")
					return
				}
				outcome := c.outcome
				if outcome == nil {
					outcome = matrixNoop
				}
				status, code := outcome(role)
				r.expect(res, status, code)
			})
		}
	}
}

func replaceAccount(path, from, to string) string {
	if len(path) >= len(from) && path[len(path)-len(from):] == from {
		return path[:len(path)-len(from)] + to
	}
	return path
}

func fixAccounts(body any, people map[string]session) any {
	m, ok := body.(map[string]any)
	if !ok {
		return body
	}
	if id, ok := m["accountId"].(string); ok {
		if p, found := people[id]; found {
			m["accountId"] = p.accountID
		}
	}
	return m
}

// 跨团队读不到：T2 的 owner 调 T1 的每个带 teamId 的过程，都答 404 not_found，不泄露存在。
func TestTeamsCrossTeamIsNotFound(t *testing.T) {
	r := newRig(t, rigOptions{})
	a, b := r.verified(), r.verified()
	t1 := r.seedTeam("cross-1", map[string]string{a.accountID: "owner"})
	r.seedTeam("cross-2", map[string]string{b.accountID: "owner"})
	calls := []struct {
		method, path string
		body         any
	}{
		{"GET", teamPath(t1), nil},
		{"PATCH", teamPath(t1), map[string]any{"name": "x"}},
		{"DELETE", teamPath(t1), nil},
		{"POST", teamPath(t1, "restore"), nil},
		{"POST", teamPath(t1, "transfer-ownership"), map[string]any{"accountId": a.accountID}},
		{"GET", teamPath(t1, "members"), nil},
		{"PATCH", teamPath(t1, "members", a.accountID), map[string]any{"role": "member"}},
		{"DELETE", teamPath(t1, "members", a.accountID), nil},
		{"POST", teamPath(t1, "leave"), nil},
		{"GET", teamPath(t1, "settings"), nil},
		{"PATCH", teamPath(t1, "settings"), map[string]any{"allowGuests": false}},
		{"GET", teamPath(t1, "usage"), nil},
		{"GET", teamPath("no-such-team", "members"), nil},
	}
	for _, c := range calls {
		r.expect(r.do(c.method, c.path, c.body, b.bearer()), 404, "not_found")
	}
	// T1 没被动过，列表里也没有对方的团队。
	res := r.do("GET", "/v1/teams", nil, b.bearer())
	if list := res.json["teams"].([]any); len(list) != 1 || list[0].(map[string]any)["slug"] != "cross-2" {
		t.Fatalf("teams.list 只列自己的：%s", res.body)
	}
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1", t1); n != 1 {
		t.Fatal("T1 的成员不该被动")
	}
}

// 最后一个 owner：两个 owner 同时各自离开（或各自把自己降级），恰好一个成功，团队留一个 owner。
func TestTeamsLastOwnerConcurrent(t *testing.T) {
	r := newRig(t, rigOptions{})
	for round, mode := range []string{"leave", "demote", "leave", "demote", "leave"} {
		o1, o2 := r.verified(), r.verified()
		team := r.seedTeam(fmt.Sprintf("last-%d", round), map[string]string{o1.accountID: "owner", o2.accountID: "owner"})
		type outcome struct {
			status int
			code   any
		}
		results := make([]outcome, 2)
		var wg sync.WaitGroup
		for i, s := range []session{o1, o2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var res response
				if mode == "leave" {
					res = r.do("POST", teamPath(team, "leave"), nil, s.bearer())
				} else {
					res = r.do("PATCH", teamPath(team, "members", s.accountID), map[string]any{"role": "admin"}, s.bearer())
				}
				results[i] = outcome{res.status, res.json["code"]}
			}()
		}
		wg.Wait()
		ok, failed := 0, 0
		for _, o := range results {
			switch {
			case o.status == 200:
				ok++
			case o.status == 409 && o.code == "team_last_owner":
				failed++
			}
		}
		if ok != 1 || failed != 1 {
			t.Fatalf("第 %d 轮（%s）应恰好一个成功一个 team_last_owner：%+v", round, mode, results)
		}
		if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND role = 'owner'", team); n != 1 {
			t.Fatalf("第 %d 轮：应剩一个 owner：%d", round, n)
		}
	}
}

func TestTeamsMemberRules(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, admin, member, guest, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("rules", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", guest.accountID: "guest",
	})

	// 目录：成员列表带 email / 角色 / groups（M1 恒空数组）；guest 才有 reachableHosts。
	res := r.do("GET", teamPath(team, "members"), nil, admin.bearer())
	r.expect(res, 200, "")
	members := res.json["members"].([]any)
	if len(members) != 4 {
		t.Fatalf("成员：%s", res.body)
	}
	for _, m := range members {
		row := m.(map[string]any)
		if !reflect.DeepEqual(row["groups"], []any{}) {
			t.Fatalf("groups 应为空数组：%s", res.body)
		}
		if _, has := row["reachableHosts"]; has != (row["role"] == "guest") {
			t.Fatalf("reachableHosts 只给 guest：%s", res.body)
		}
	}

	// admin 不能动 owner，不能移除 owner；owner 才能降 owner。
	r.expect(r.do("PATCH", teamPath(team, "members", owner.accountID), map[string]any{"role": "member"}, admin.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("DELETE", teamPath(team, "members", owner.accountID), nil, admin.bearer()), 403, "team_role_insufficient")
	// 不能经 memberRole 把人升成 owner（协议排除了 owner）。
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "owner"}, admin.bearer()), 400, "bad_request")
	// 目标不存在或不是成员。
	r.expect(r.do("PATCH", teamPath(team, "members", outsider.accountID), map[string]any{"role": "member"}, admin.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", teamPath(team, "members", outsider.accountID), nil, admin.bearer()), 404, "not_found")
	// 唯一的 owner 不能把自己降级，也不能被移除。
	r.expect(r.do("PATCH", teamPath(team, "members", owner.accountID), map[string]any{"role": "admin"}, owner.bearer()), 409, "team_last_owner")
	r.expect(r.do("DELETE", teamPath(team, "members", owner.accountID), nil, owner.bearer()), 409, "team_last_owner")

	// 同角色是空操作；member -> guest 受 allowGuests 约束；guest -> member 受 members 额度约束（Free 3 个）。
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "member"}, admin.bearer()), 200, "")
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, admin.bearer()), 200, "")
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "guest"}, admin.bearer()), 403, "guests_disabled")
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": true}, admin.bearer()), 200, "")
	// 现有 3 个非 guest（owner、admin、member）已到 Free 上限，guest 转正式成员越限。
	res = r.do("PATCH", teamPath(team, "members", guest.accountID), map[string]any{"role": "member"}, admin.bearer())
	r.expect(res, 429, "limit_reached")
	if d := res.json["details"].(map[string]any); d["quota"] != "members" || d["used"] != float64(3) || d["limit"] != float64(3) {
		t.Fatalf("details：%s", res.body)
	}
	// member -> guest 成功（guest 额度 3，现 1 个）。
	res = r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "guest"}, admin.bearer())
	r.expect(res, 200, "")
	if res.json["role"] != "guest" || res.json["accountId"] != member.accountID {
		t.Fatalf("改角色的答复：%s", res.body)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE team_id = $1 AND action = 'member.role'", team); n != 1 {
		t.Fatalf("改角色应写一条审计（空操作不写）：%d", n)
	}

	// 转移所有权：目标必须是 admin；成功后原 owner 降 admin。
	r.expect(r.do("POST", teamPath(team, "transfer-ownership"), map[string]any{"accountId": guest.accountID}, owner.bearer()), 400, "bad_request")
	r.expect(r.do("POST", teamPath(team, "transfer-ownership"), map[string]any{"accountId": outsider.accountID}, owner.bearer()), 404, "not_found")
	r.seedHost("rules-host", team, owner.accountID)
	r.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ('cloud', 'g-owner', 'rules-host', $1, 'member', $2, 'viewer', $2, 1, 1)`, team, owner.accountID)
	r.expect(r.do("POST", teamPath(team, "transfer-ownership"), map[string]any{"accountId": admin.accountID}, owner.bearer()), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM source_grants WHERE grant_id = 'g-owner' AND revoked_at_ms IS NULL"); n != 1 {
		t.Fatal("原 owner 降 admin 只撤令牌，保留显式授予")
	}
	for who, want := range map[string]string{owner.accountID: "admin", admin.accountID: "owner"} {
		if got := r.queryString("SELECT role FROM team_members WHERE team_id = $1 AND account_id = $2", team, who); got != want {
			t.Fatalf("转移后 %s 的角色 %s，应为 %s", who, got, want)
		}
	}
	// 旧 owner 现在是 admin，不能再转移。
	r.expect(r.do("POST", teamPath(team, "transfer-ownership"), map[string]any{"accountId": admin.accountID}, owner.bearer()), 403, "team_role_insufficient")
}

// 移除 / 离开 / 收窄走撤销流程：授予撤销、此人登记的主机改归团队 owner、已签令牌撤销并踢人。
func TestTeamsRemovalRevokesAccess(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, admin, member := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("revoke", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member"})
	r.seedHost("host-a", team, member.accountID)
	r.seedHost("host-b", team, owner.accountID)
	r.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ('cloud', 'g1', 'host-b', $1, 'member', $2, 'viewer', $3, 1, 1)`, team, member.accountID, owner.accountID)
	issue := func(jti, subject, source string) {
		if err := r.run.Relay.IndexIssued(ctxBackground, relayctl.Issued{Subject: subject, SourceID: source, DeviceID: "d1", JTI: jti, ExpMs: r.clock.Ms() + 600_000}); err != nil {
			t.Fatal(err)
		}
	}
	issue("t-member-a", member.accountID, "host-a")
	issue("t-member-b", member.accountID, "host-b")
	issue("t-admin-b", admin.accountID, "host-b")

	// 降为 billing 是收窄：撤销此人在两台主机上的令牌并踢人，成员授予撤销，别人的令牌不动。
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "billing"}, admin.bearer()), 200, "")
	if !r.run.Relay.IsRevoked("t-member-a") || !r.run.Relay.IsRevoked("t-member-b") || r.run.Relay.IsRevoked("t-admin-b") {
		t.Fatalf("撤销的令牌：%v", r.run.Relay.RevokedTokens())
	}
	kicked := map[string]bool{}
	for _, c := range r.run.Relay.Commands() {
		if c.Type == relayctl.CommandKick && c.Subject == member.accountID {
			kicked[c.SourceID] = true
		}
	}
	if !kicked["host-a"] || !kicked["host-b"] {
		t.Fatalf("应在每台团队主机上踢人：%+v", r.run.Relay.Commands())
	}
	if n := r.queryInt("SELECT count(*)::int FROM source_grants WHERE grant_id = 'g1' AND revoked_at_ms IS NOT NULL"); n != 1 {
		t.Fatal("降为 billing 应撤销成员的授予")
	}
	// 只收窄不改成员身份（admin 降 member、原 owner 降 admin）：撤令牌踢人，但保留显式授予。
	r.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ('cloud', 'g-admin', 'host-b', $1, 'member', $2, 'viewer', $3, 1, 1)`, team, admin.accountID, owner.accountID)
	r.expect(r.do("PATCH", teamPath(team, "members", admin.accountID), map[string]any{"role": "member"}, owner.bearer()), 200, "")
	if !r.run.Relay.IsRevoked("t-admin-b") {
		t.Fatal("admin 降 member 应撤令牌")
	}
	if n := r.queryInt("SELECT count(*)::int FROM source_grants WHERE grant_id = 'g-admin' AND revoked_at_ms IS NULL"); n != 1 {
		t.Fatal("admin 降 member 应保留显式授予")
	}
	r.expect(r.do("PATCH", teamPath(team, "members", admin.accountID), map[string]any{"role": "admin"}, owner.bearer()), 200, "")
	issue("t-admin-c", admin.accountID, "host-b")
	// 升回 member（放宽）不撤销：新签的令牌不受影响。
	issue("t-member-c", member.accountID, "host-a")
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "member"}, admin.bearer()), 200, "")
	if r.run.Relay.IsRevoked("t-member-c") {
		t.Fatal("放宽角色不该撤销令牌")
	}

	// 离开：此人登记的主机改成团队 owner，成员行没了，令牌撤销。
	r.expect(r.do("POST", teamPath(team, "leave"), nil, member.bearer()), 200, "")
	if got := r.queryString("SELECT owner_account_id FROM source_spaces WHERE source_id = 'host-a'"); got != owner.accountID {
		t.Fatalf("主机应改归团队 owner：%s", got)
	}
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND account_id = $2", team, member.accountID); n != 0 {
		t.Fatal("成员应离开")
	}
	if !r.run.Relay.IsRevoked("t-member-c") {
		t.Fatal("离开后令牌应撤销")
	}
	// 离开后看不到团队了。
	r.expect(r.do("GET", teamPath(team), nil, member.bearer()), 404, "not_found")
	// 移除 admin：owner 移除，令牌同样撤销。
	r.expect(r.do("DELETE", teamPath(team, "members", admin.accountID), nil, owner.bearer()), 200, "")
	if !r.run.Relay.IsRevoked("t-admin-c") {
		t.Fatal("被移除成员的令牌应撤销")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE team_id = $1 AND action IN ('member.leave', 'member.remove')", team); n != 2 {
		t.Fatalf("离开与移除各一条审计：%d", n)
	}
}

func TestTeamsSettings(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("settings", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	res := r.do("GET", teamPath(team, "settings"), nil, member.bearer())
	r.expect(res, 200, "")
	if res.json["allowAnonymousLinks"] != false || res.json["defaultHostRole"] != "viewer" || res.json["deviceEndorsers"] != "owner_admin" ||
		res.json["membersCanRegisterHosts"] != true || res.json["sessionMaxAgeMs"] != nil {
		t.Fatalf("缺省设置：%s", res.body)
	}
	res = r.do("PATCH", teamPath(team, "settings"), map[string]any{"defaultHostRole": "editor", "allowedEmailDomains": []string{"example.com"}, "sessionMaxAgeMs": 3600000}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["defaultHostRole"] != "editor" || res.json["sessionMaxAgeMs"] != float64(3600000) || res.json["allowGuests"] != true ||
		!reflect.DeepEqual(res.json["allowedEmailDomains"], []any{"example.com"}) {
		t.Fatalf("部分更新：%s", res.body)
	}
	res = r.do("PATCH", teamPath(team, "settings"), map[string]any{"sessionMaxAgeMs": nil}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["sessionMaxAgeMs"] != nil || res.json["defaultHostRole"] != "editor" {
		t.Fatalf("置空：%s", res.body)
	}
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"defaultHostRole": "root"}, owner.bearer()), 400, "bad_request")
	// 关 allowGuests 不自动移除已有 guest。
	guest := r.verified()
	r.exec("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'guest', 9)", team, guest.accountID)
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND role = 'guest'", team); n != 1 {
		t.Fatal("已有 guest 不该被移除")
	}
}

func TestTeamsUsage(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, member, guest := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("usage", map[string]string{owner.accountID: "owner", member.accountID: "member", guest.accountID: "guest"})
	r.seedHost("u-host", team, owner.accountID)
	month := time.UnixMilli(r.clock.Ms()).UTC()
	bucket := time.Date(month.Year(), month.Month(), 1, 3, 0, 0, 0, time.UTC).UnixMilli()
	r.exec(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind, bytes_to_source, bytes_to_client, team_id)
	  VALUES ('cloud', $1, 'n1', 'u-host', 'x', '', 'e2e', 1000, 500, $2), ('cloud', $1, 'n1', 'u-host', 'x', '', 'http', 7777, 7777, $2)`, bucket, team)
	res := r.do("GET", teamPath(team, "usage"), nil, member.bearer())
	r.expect(res, 200, "")
	if res.json["teamId"] != team || res.json["period"] != month.Format("2006-01") {
		t.Fatalf("usage 头：%s", res.body)
	}
	got := map[string][2]any{}
	for _, entry := range res.json["usage"].([]any) {
		e := entry.(map[string]any)
		got[e["metric"].(string)] = [2]any{e["value"], e["limit"]}
	}
	want := map[string][2]any{
		"hosts": {float64(1), float64(3)}, "members": {float64(2), float64(3)}, "guests": {float64(1), float64(3)},
		"concurrent": {float64(0), float64(10)}, "traffic": {float64(1500), float64(5 << 30)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage：%v\n%s", got, res.body)
	}
	// 别的月份没有流量；非法 period 被入参校验拒绝。
	res = r.do("GET", teamPath(team, "usage")+"?period=1999-01", nil, owner.bearer())
	r.expect(res, 200, "")
	for _, entry := range res.json["usage"].([]any) {
		if e := entry.(map[string]any); e["metric"] == "traffic" && e["value"] != float64(0) {
			t.Fatalf("1999-01 不该有流量：%s", res.body)
		}
	}
	r.expect(r.do("GET", teamPath(team, "usage")+"?period=2026-13", nil, owner.bearer()), 400, "bad_request")
}

// 删除 / 恢复 / 清除：在线主机挡删除；恢复期内其余调用 410；恢复撤掉清除作业；到期后清除作业清走一切。
func TestTeamsDeleteRestorePurge(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, admin := r.verified(), r.verified()
	team := r.seedTeam("purge", map[string]string{owner.accountID: "owner", admin.accountID: "admin"})
	r.seedHost("p-host", team, owner.accountID)
	r.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ('cloud', 'pg1', 'p-host', $1, 'team', $1, 'viewer', $2, 1, 1)`, team, owner.accountID)
	r.exec(`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, updated_at_ms) VALUES ('cloud', 'team', $1, 'free', 1)`, team)
	if err := r.run.Relay.IndexIssued(ctxBackground, relayctl.Issued{Subject: admin.accountID, SourceID: "p-host", DeviceID: "d", JTI: "p-tok", ExpMs: r.clock.Ms() + 600_000}); err != nil {
		t.Fatal(err)
	}
	if err := r.run.Relay.SetTeamQuota(ctxBackground, team, relayctl.Quota{Concurrent: 10}); err != nil {
		t.Fatal(err)
	}

	// 有在线主机：409 team_hosts_online，details.onlineHosts 是在线数（只数在线的），什么都不变。
	r.seedHost("p-host-2", team, owner.accountID)
	r.run.Relay.SetRoute("p-host", "0.3.0")
	res0 := r.do("DELETE", teamPath(team), nil, owner.bearer())
	r.expect(res0, 409, "team_hosts_online")
	if d, _ := res0.json["details"].(map[string]any); d["onlineHosts"] != float64(1) {
		t.Fatalf("onlineHosts：%s", res0.body)
	}
	r.run.Relay.SetRoute("p-host-2", "0.3.0")
	if d, _ := r.do("DELETE", teamPath(team), nil, owner.bearer()).json["details"].(map[string]any); d["onlineHosts"] != float64(2) {
		t.Fatal("两台在线应报 2")
	}
	r.run.Relay.SetRoute("p-host-2", "")
	r.expect(r.do("GET", teamPath(team), nil, owner.bearer()), 200, "")
	r.run.Relay.SetRoute("p-host", "")

	deletedAt := r.clock.Ms()
	res := r.do("DELETE", teamPath(team), nil, owner.bearer())
	r.expect(res, 200, "")
	purgeAt := int64(res.json["purgeAtMs"].(float64))
	if purgeAt != deletedAt+30*24*3600*1000 {
		t.Fatalf("purgeAtMs 应是 30 天后：%d", purgeAt-deletedAt)
	}
	if !r.run.Relay.IsRevoked("p-tok") {
		t.Fatal("删除应撤销团队主机上的已签令牌")
	}
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'team.purge' AND dedupe_key = $1 AND run_at_ms = $2", team, purgeAt); n != 1 {
		t.Fatal("应插入 team.purge 作业")
	}
	// 恢复期内：其余调用 410；owner 在 teams.list 里能看到（带时间），admin 看不到；me.get / me.spaces 不列。
	r.expect(r.do("GET", teamPath(team), nil, owner.bearer()), 410, "team_deleted")
	r.expect(r.do("GET", teamPath(team, "members"), nil, admin.bearer()), 410, "team_deleted")
	r.expect(r.do("POST", teamPath(team, "restore"), nil, admin.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("DELETE", teamPath(team), nil, owner.bearer()), 410, "team_deleted")
	res = r.do("GET", "/v1/teams", nil, owner.bearer())
	if list := res.json["teams"].([]any); len(list) != 1 || list[0].(map[string]any)["purgeAtMs"] != float64(purgeAt) {
		t.Fatalf("owner 的 teams.list：%s", res.body)
	}
	if list := r.do("GET", "/v1/teams", nil, admin.bearer()).json["teams"].([]any); len(list) != 0 {
		t.Fatal("非 owner 看不到恢复期内的团队")
	}
	if teams := r.do("GET", "/v1/me", nil, owner.bearer()).json["teams"].([]any); len(teams) != 0 {
		t.Fatal("me.get 不列已删除的团队")
	}
	// 删除 → 新建 → 恢复：恢复也受个人账号的团队额度约束（Free 1 个），不能借此绕过。
	r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "replacement", "name": "替身"}, owner.bearer()), 200, "")
	res = r.do("POST", teamPath(team, "restore"), nil, owner.bearer())
	r.expect(res, 429, "limit_reached")
	if d := res.json["details"].(map[string]any); d["quota"] != "teams" || d["limit"] != float64(1) {
		t.Fatalf("details：%s", res.body)
	}
	r.exec("DELETE FROM teams WHERE slug = 'replacement'") // 成员与 entitlements 随团队级联
	// 恢复：清两个时间、撤掉作业；再恢复是幂等的。
	res = r.do("POST", teamPath(team, "restore"), nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["deletedAtMs"] != nil || res.json["purgeAtMs"] != nil {
		t.Fatalf("恢复后：%s", res.body)
	}
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'team.purge' AND dedupe_key = $1", team); n != 0 {
		t.Fatal("恢复应撤掉 team.purge 作业")
	}
	r.expect(r.do("POST", teamPath(team, "restore"), nil, owner.bearer()), 200, "")
	r.expect(r.do("GET", teamPath(team), nil, admin.bearer()), 200, "")

	// 再删一次，时间越过恢复期：清除作业清走一切。
	res = r.do("DELETE", teamPath(team), nil, owner.bearer())
	r.expect(res, 200, "")
	r.clock.Advance(31 * 24 * time.Hour)
	deadline := time.Now().Add(20 * time.Second)
	for r.queryInt("SELECT count(*)::int FROM teams WHERE team_id = $1", team) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("清除作业没有在期限内清走团队")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for table, where := range map[string]string{
		"team_members": "team_id", "source_spaces": "team_id", "source_grants": "team_id", "entitlements": "space_id",
	} {
		if n := r.queryInt(fmt.Sprintf("SELECT count(*)::int FROM %s WHERE %s = $1", table, where), team); n != 0 {
			t.Fatalf("%s 里还有团队的行：%d", table, n)
		}
	}
	if n := r.queryInt("SELECT count(*)::int FROM sources WHERE source_id = 'p-host' AND revoked_at_ms IS NOT NULL"); n != 1 {
		t.Fatal("清除应撤销团队主机")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE team_id = $1 AND action IN ('team.create', 'team.delete', 'team.restore', 'team.purge')", team); n < 3 {
		t.Fatalf("审计行应保留：%d", n)
	}
	if _, ok := r.run.Relay.Quota(team); ok {
		t.Fatal("清除应删团队并发额度")
	}
	closed := false
	for _, c := range r.run.Relay.Commands() {
		closed = closed || (c.Type == relayctl.CommandCloseTunnel && c.SourceID == "p-host")
	}
	if !closed {
		t.Fatal("清除应关团队主机的隧道")
	}
}
