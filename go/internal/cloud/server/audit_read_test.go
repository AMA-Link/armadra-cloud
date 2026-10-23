package server_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// putAudit 直接写一行审计（不经业务过程）：团队审计带 team，安全日志带 account。
func (r *rig) putAudit(atMs int64, id, team, account, actorKind, actorID, action string) {
	r.t.Helper()
	r.exec(`INSERT INTO audit_log (at_ms, event_id, tenant_id, team_id, account_id, actor_kind, actor_id, action)
	  VALUES ($1, $2, 'cloud', NULLIF($3, ''), NULLIF($4, ''), $5, NULLIF($6, ''), $7)`,
		atMs, id, team, account, actorKind, actorID, action)
}

func eventIDs(t *testing.T, res response) []string {
	t.Helper()
	events, _ := res.json["events"].([]any)
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.(map[string]any)["eventId"].(string))
	}
	return out
}

// audit.list 只给团队 owner / admin；跨团队、非成员一律 404；筛选、分页与形状。
func TestAuditListRolesScopeAndFilters(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, admin, member, billing, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	t1 := r.seedTeam("t1", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", billing.accountID: "billing"})
	t2 := r.seedTeam("t2", map[string]string{outsider.accountID: "owner"})
	base := r.clock.Ms() - 10_000
	src := "0123456789abcdef0123456789abcdef"
	r.putAudit(base+1, "a01", t1, "", "account", owner.accountID, "member.add")
	r.putAudit(base+2, "a02", t1, "", "account", admin.accountID, "grant.create")
	r.putAudit(base+3, "a03", t1, "", "system", "", "grant.expire")
	r.putAudit(base+4, "a04", t2, "", "account", outsider.accountID, "member.add")
	r.putAudit(base+5, "a05", "", owner.accountID, "account", owner.accountID, "security.login")
	r.exec(`UPDATE audit_log SET source_id = '` + src + `', jti = 'j1', ip_prefix = '10.0.0.0/24', target_kind = 'grant', target_id = 'g1' WHERE event_id = 'a02'`)

	path := teamPath(t1, "audit")
	for _, allowed := range []session{owner, admin} {
		res := r.do("GET", path, nil, allowed.bearer())
		r.expect(res, 200, "")
		if got := strings.Join(eventIDs(t, res), ","); got != "a03,a02,a01" {
			t.Fatalf("T1 的审计（没有 T2、安全日志）：%s", got)
		}
		if res.json["nextCursor"] != nil {
			t.Fatalf("一页取完没有下一页：%s", res.body)
		}
	}
	r.expect(r.do("GET", path, nil, member.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("GET", path, nil, billing.bearer()), 403, "team_role_insufficient")
	// 非成员与别的团队的 owner：404，不泄露团队存在。
	r.expect(r.do("GET", path, nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("GET", teamPath(t2, "audit"), nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("GET", teamPath("no-such", "audit"), nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("GET", path, nil, nil), 401, "unauthenticated")

	// 形状：主体、对象、jti、IP 前缀，没有别的。
	res := r.do("GET", path+"?action=grant.create", nil, owner.bearer())
	r.expect(res, 200, "")
	event := res.json["events"].([]any)[0].(map[string]any)
	actor := event["actor"].(map[string]any)
	target := event["target"].(map[string]any)
	if event["teamId"] != t1 || actor["kind"] != "account" || actor["id"] != admin.accountID || event["action"] != "grant.create" ||
		target["kind"] != "grant" || target["id"] != "g1" || event["jti"] != "j1" || event["ipPrefix"] != "10.0.0.0/24" {
		t.Fatalf("事件形状：%s", res.body)
	}
	if len(event) != 8 {
		t.Fatalf("事件只有 8 个字段：%v", event)
	}

	// 筛选：动作前缀、主体、主机、时间范围（含起不含止）。
	for query, want := range map[string]string{
		"action=grant.": "a03,a02",
		"actor=" + url.QueryEscape(owner.accountID): "a01",
		"sourceId=" + src: "a02",
		fmt.Sprintf("fromMs=%d&toMs=%d", base+2, base+3): "a02",
		"action=%25": "",
	} {
		res := r.do("GET", path+"?"+query, nil, owner.bearer())
		r.expect(res, 200, "")
		if got := strings.Join(eventIDs(t, res), ","); got != want {
			t.Fatalf("筛选 %s：%q，应为 %q", query, got, want)
		}
	}

	// 分页：每页 2 条，翻页不重不漏；坏游标 400。
	res = r.do("GET", path+"?limit=2", nil, owner.bearer())
	r.expect(res, 200, "")
	cursor, _ := res.json["nextCursor"].(string)
	if cursor == "" || strings.Join(eventIDs(t, res), ",") != "a03,a02" {
		t.Fatalf("第一页：%s", res.body)
	}
	res = r.do("GET", path+"?limit=2&cursor="+url.QueryEscape(cursor), nil, owner.bearer())
	r.expect(res, 200, "")
	if strings.Join(eventIDs(t, res), ",") != "a01" || res.json["nextCursor"] != nil {
		t.Fatalf("第二页：%s", res.body)
	}
	r.expect(r.do("GET", path+"?cursor=bogus", nil, owner.bearer()), 400, "bad_request")
	r.expect(r.do("GET", path+"?limit=501", nil, owner.bearer()), 400, "bad_request")
}

// me.securityLog 只给本人的 security.*；设备名是当时的快照；分页与时间范围。
func TestSecurityLogIsOwnerOnly(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.registerBearer(r.email())
	b := r.registerBearer(r.email())
	team := r.seedTeam("tt", map[string]string{a.accountID: "owner"})
	r.putAudit(r.clock.Ms(), "x1", team, "", "account", a.accountID, "member.add")

	res := r.do("GET", "/v1/me/security-log", nil, a.bearer())
	r.expect(res, 200, "")
	events := res.json["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("a 只有注册那一条：%s", res.body)
	}
	event := events[0].(map[string]any)
	if event["action"] != "security.register" || event["deviceName"] != "测试浏览器" || !strings.HasPrefix(event["ipPrefix"].(string), "10.") || !strings.HasSuffix(event["ipPrefix"].(string), "/24") {
		t.Fatalf("注册的安全日志：%s", res.body)
	}
	if len(event) != 5 {
		t.Fatalf("安全日志一条只有 5 个字段：%v", event)
	}
	// 改设备名不改历史快照；再登录一次多一条，倒序；b 看不到 a 的。
	r.exec("UPDATE devices SET name = '改过的名字' WHERE subject = $1", a.accountID)
	r.clock.Advance(time.Second)
	r.expect(r.login(r.queryString("SELECT email FROM accounts WHERE account_id = $1", a.accountID), password, nil), 200, "")
	res = r.do("GET", "/v1/me/security-log", nil, a.bearer())
	r.expect(res, 200, "")
	events = res.json["events"].([]any)
	if len(events) < 2 || events[0].(map[string]any)["action"] == "security.register" || events[len(events)-1].(map[string]any)["action"] != "security.register" {
		t.Fatalf("倒序：%s", res.body)
	}
	if events[len(events)-1].(map[string]any)["deviceName"] != "测试浏览器" {
		t.Fatal("设备名是当时的快照")
	}
	resB := r.do("GET", "/v1/me/security-log", nil, b.bearer())
	r.expect(resB, 200, "")
	if len(resB.json["events"].([]any)) != 1 {
		t.Fatalf("b 只看到自己的：%s", resB.body)
	}
	// 分页与时间范围。
	res = r.do("GET", "/v1/me/security-log?limit=1", nil, a.bearer())
	r.expect(res, 200, "")
	cursor, _ := res.json["nextCursor"].(string)
	if cursor == "" || len(res.json["events"].([]any)) != 1 {
		t.Fatalf("limit=1：%s", res.body)
	}
	res = r.do("GET", fmt.Sprintf("/v1/me/security-log?toMs=%d", r.clock.Ms()-500), nil, a.bearer())
	r.expect(res, 200, "")
	if len(res.json["events"].([]any)) != 1 {
		t.Fatalf("toMs 不含：%s", res.body)
	}
	r.expect(r.do("GET", "/v1/me/security-log?cursor=bogus", nil, a.bearer()), 400, "bad_request")
	r.expect(r.do("GET", "/v1/me/security-log", nil, nil), 401, "unauthenticated")
}

// 审计与安全日志里没有敏感数据：库里只有白名单的列，一轮真实流程之后行里没有口令、令牌、邮箱与完整 IP。
func TestAuditHoldsNoSensitiveData(t *testing.T) {
	r := newRig(t, rigOptions{})
	cols := r.queryString(`SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns
	  WHERE table_schema = current_schema() AND table_name = 'audit_log'`)
	if cols != "account_id,action,actor_id,actor_kind,at_ms,device_name,event_id,ip_prefix,jti,reason,source_id,target_id,target_kind,team_id,tenant_id" {
		t.Fatalf("审计表的列：%s", cols)
	}
	email := r.email()
	s := r.registerBearer(email)
	r.expect(r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": password + "-new"}, s.bearer()), 200, "")
	r.expect(r.login(email, password+"-new", nil), 200, "")
	owner := r.verified()
	team := r.seedTeam("clean", map[string]string{owner.accountID: "owner"})
	r.expect(r.do("PATCH", teamPath(team), map[string]any{"name": "新名字"}, owner.bearer()), 200, "")
	text := r.queryString(`SELECT COALESCE(string_agg(a::text, E'\n'), '') FROM audit_log a`)
	if text == "" {
		t.Fatal("应有审计行")
	}
	for name, secret := range map[string]string{
		"口令": password, "新口令": password + "-new", "访问令牌": s.access, "刷新令牌": s.refresh, "邮箱": email, "完整 IP": r.ip,
		"owner 的访问令牌": owner.access,
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("审计行里出现了%s", name)
		}
	}
	if !strings.Contains(text, r.ip[:strings.LastIndex(r.ip, ".")]+".0/24") {
		t.Fatalf("应只记 IP 前缀：%s", text)
	}
}

// ---- 保留期 ----

const dayDur = 24 * time.Hour

func (r *rig) auditCount(where string, args ...any) int {
	return r.queryInt("SELECT count(*)::int FROM audit_log WHERE "+where, args...)
}

// 每天的保留期作业：按团队计划删团队审计（只动本团队）、安全日志 90 天、过 13 个月的整月分区 DROP、流量桶按配置的天数。
func TestRetentionJobsDeleteOnlyTheirScope(t *testing.T) {
	r := newRig(t, rigOptions{env: map[string]string{"TRAFFIC_RETENTION_DAYS": "30"}})
	owner, other := r.verified(), r.verified()
	free := r.seedTeam("free", map[string]string{owner.accountID: "owner"})
	pro := r.seedTeam("pro", map[string]string{owner.accountID: "owner"})
	rival := r.seedTeam("rival", map[string]string{other.accountID: "owner"})
	r.exec(`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, status, updated_at_ms) VALUES ('cloud', 'team', $1, 'pro', 'active', 1)`, pro)
	// 种子都相对「明天」：先把时钟拨到下一个时间槽，再播种，再让作业跑。
	now := r.clock.Now().Add(dayDur + time.Minute)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	r.putAudit(ago(10*dayDur), "f-old", free, "", "system", "", "x.old")
	r.putAudit(ago(3*dayDur), "f-new", free, "", "system", "", "x.new")
	r.putAudit(ago(10*dayDur), "p-10d", pro, "", "system", "", "x.mid")
	r.putAudit(ago(40*dayDur), "p-40d", pro, "", "system", "", "x.old")
	r.putAudit(ago(10*dayDur), "r-old", rival, "", "system", "", "x.old")
	r.putAudit(ago(95*dayDur), "s-old", "", owner.accountID, "account", owner.accountID, "security.login")
	r.putAudit(ago(80*dayDur), "s-new", "", owner.accountID, "account", owner.accountID, "security.login")
	r.putAudit(ago(200*dayDur), "op-old", "", "", "operator", "op", "account.disable")
	// 十四个月前的整月分区连同里面的行一起 DROP。
	ancient := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -14, 0)
	name := fmt.Sprintf("audit_log_%04d_%02d", ancient.Year(), int(ancient.Month()))
	r.exec("SELECT audit_ensure_partition($1, $2, $3)", ancient.UnixMilli(), ancient.AddDate(0, 1, 0).UnixMilli(), name)
	r.putAudit(ancient.UnixMilli()+1, "ancient", "", "", "system", "", "x.ancient")
	// 流量桶：400 天以外的缺省被配置的 30 天取代。
	for i, at := range []int64{ago(40 * dayDur), ago(10 * dayDur)} {
		r.exec(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind, team_id, bytes_to_source)
		  VALUES ('cloud', $1, 'n', $2, 'a', 'e2e', '', 1)`, at/3600000*3600000, fmt.Sprintf("src%d", i))
	}

	r.clock.Advance(dayDur + time.Minute)
	eventuallyDB(t, "保留期作业清完", func() bool {
		return r.auditCount("event_id = 'ancient'") == 0 && r.queryInt("SELECT count(*)::int FROM relay_traffic_buckets") == 1 &&
			r.auditCount("event_id = 's-old'") == 0
	})
	eventuallyDB(t, "团队审计清完", func() bool { return r.auditCount("event_id IN ('f-old', 'p-40d', 'r-old')") == 0 })
	if got := r.auditCount("event_id IN ('f-new', 'p-10d', 's-new', 'op-old')"); got != 4 {
		t.Fatalf("没到期的、运营动作都留着：%d %s", got, r.queryString("SELECT string_agg(event_id, ',') FROM audit_log"))
	}
	if n := r.queryInt("SELECT count(*)::int FROM pg_class WHERE relname = $1 AND relnamespace = current_schema()::regnamespace", name); n != 0 {
		t.Fatalf("过期的整月分区应被 DROP：%s", name)
	}
	if n := r.queryInt("SELECT count(*)::int FROM relay_traffic_buckets WHERE source_id = 'src1'"); n != 1 {
		t.Fatal("30 天内的流量桶留着")
	}
	// 同一天的时间槽只跑一次：三个作业各一行。
	for _, kind := range []string{"audit.partitions", "audit.retention", "traffic.retention"} {
		if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = $1 AND done_at_ms IS NOT NULL", kind); n < 1 {
			t.Fatalf("%s 没跑过", kind)
		}
	}
}

// 团队清除后 entitlements 随之删掉，但审计行按设计保留：不能按 free 的 7 天删，只随 13 个月的整月分区 DROP。
func TestRetentionKeepsAuditOfPurgedTeams(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	gone := r.seedTeam("gone", map[string]string{owner.accountID: "owner"})
	live := r.seedTeam("live", map[string]string{owner.accountID: "owner"})
	r.exec(`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, status, updated_at_ms) VALUES ('cloud', 'team', $1, 'enterprise', 'active', 1)`, gone)
	now := r.clock.Now().Add(dayDur + time.Minute)
	eighth := now.Add(-8 * dayDur).UnixMilli()
	r.putAudit(eighth, "gone-8d", gone, "", "system", "", "team.purge")
	r.putAudit(eighth, "live-8d", live, "", "system", "", "x.old") // 对照：还在的 free 团队，8 天前的行要被删
	// 清除：团队行（成员级联）与 entitlements 都没了，审计行还在。
	r.exec("DELETE FROM entitlements WHERE space_id = $1", gone)
	r.exec("DELETE FROM teams WHERE team_id = $1", gone)

	r.clock.Advance(dayDur + time.Minute)
	eventuallyDB(t, "保留期作业跑过", func() bool { return r.auditCount("event_id = 'live-8d'") == 0 })
	if r.auditCount("event_id = 'gone-8d'") != 1 {
		t.Fatal("已清除团队的审计行不按 free 的 7 天删")
	}
}
