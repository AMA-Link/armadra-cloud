package server_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// ---- 辅助 ----

func guestOf(accountID string) map[string]any {
	return map[string]any{"kind": "guest", "accountId": accountID}
}
func memberOf(accountID string) map[string]any {
	return map[string]any{"kind": "member", "accountId": accountID}
}
func teamOf(teamID string) map[string]any { return map[string]any{"kind": "team", "teamId": teamID} }

func (r *rig) createGrant(s session, sourceID string, subject map[string]any, role string, extra map[string]any) response {
	r.t.Helper()
	body := map[string]any{"sourceId": sourceID, "subject": subject, "role": role}
	for k, v := range extra {
		body[k] = v
	}
	return r.do("POST", "/v1/grants", body, s.bearer())
}

// mustGrant 建授予并断言成功，答 grantId。
func (r *rig) mustGrant(s session, sourceID string, subject map[string]any, role string, extra map[string]any) string {
	r.t.Helper()
	res := r.createGrant(s, sourceID, subject, role, extra)
	r.expect(res, 200, "")
	return res.json["grantId"].(string)
}

func (r *rig) assertion(s session, sourceID string) response {
	r.t.Helper()
	return r.do("POST", "/v1/sources/"+sourceID+"/assertion", map[string]any{}, s.bearer())
}

// jtisOf 是一次断言答复里的两个 jti（断言、中继令牌）。
func jtisOf(t *testing.T, res response) (assertion, relay string) {
	t.Helper()
	a, _ := payloadOf(t, res.json["assertion"].(string))["jti"].(string)
	b, _ := payloadOf(t, res.json["relayToken"].(string))["jti"].(string)
	if a == "" || b == "" {
		t.Fatalf("答复里没有 jti：%s", res.body)
	}
	return a, b
}

func (r *rig) issuedCount(accountID, sourceID string) int {
	return r.queryInt("SELECT count(*)::int FROM cloud_issued_tokens WHERE tenant_id = 'cloud' AND account_id = $1 AND source_id = $2", accountID, sourceID)
}

func (r *rig) grantRow(grantID string) (revokedAt *int64, revokedBy string) {
	r.t.Helper()
	var by *string
	if err := r.run.DB.Pool().QueryRow(ctxBackground,
		"SELECT revoked_at_ms, revoked_by FROM source_grants WHERE grant_id = $1", grantID).Scan(&revokedAt, &by); err != nil {
		r.t.Fatal(err)
	}
	if by != nil {
		revokedBy = *by
	}
	return revokedAt, revokedBy
}

func (r *rig) kicks() (out []string) {
	for _, c := range r.run.Relay.Commands() {
		if c.Type == relayctl.CommandKick {
			out = append(out, c.SourceID+"|"+c.Subject+"|"+c.Reason)
		}
	}
	return out
}

// ---- grants.* ----

// 个人空间的访客授予：校验、重复、管理权、列表与有效权限、改与撤。
func TestGrantsPersonalGuestLifecycle(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend, stranger := r.verified(), r.verified(), r.verified()
	unverified := r.registerBearer(r.email())
	h := r.addHost(owner, nil, "0.11.0")

	r.expect(r.createGrant(owner, h.id, guestOf(unverified.accountID), "viewer", nil), 403, "email_unverified")
	r.expect(r.createGrant(owner, h.id, guestOf("nobody"), "viewer", nil), 404, "not_found")
	r.expect(r.createGrant(owner, h.id, map[string]any{"kind": "group", "groupId": "g"}, "viewer", nil), 403, "plan_feature_unavailable")
	r.expect(r.createGrant(owner, h.id, teamOf("t"), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, h.id, memberOf(friend.accountID), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, h.id, guestOf(owner.accountID), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, h.id, guestOf(friend.accountID), "viewer", map[string]any{"expiresAtMs": r.clock.Ms() - 1}), 400, "bad_request")
	r.expect(r.createGrant(stranger, h.id, guestOf(friend.accountID), "viewer", nil), 404, "not_found")
	r.expect(r.createGrant(owner, strings.Repeat("0", 32), guestOf(friend.accountID), "viewer", nil), 404, "not_found")

	expires := r.clock.Ms() + 3_600_000
	res := r.createGrant(owner, h.id, guestOf(friend.accountID), "editor", map[string]any{"workspaceIds": []string{"w2", "w1", "w1"}, "expiresAtMs": expires})
	r.expect(res, 200, "")
	grantID := res.json["grantId"].(string)
	if !reflect.DeepEqual(res.json["subject"], guestOf(friend.accountID)) || res.json["role"] != "editor" || res.json["revokedAtMs"] != nil ||
		!reflect.DeepEqual(res.json["workspaceIds"], []any{"w1", "w2"}) || res.json["expiresAtMs"] != float64(expires) || res.json["grantedBy"] != owner.accountID {
		t.Fatalf("授予：%s", res.body)
	}
	r.expect(r.createGrant(owner, h.id, guestOf(friend.accountID), "viewer", nil), 409, "conflict")
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'grant.expire' AND dedupe_key = $1 AND run_at_ms = $2", grantID, expires); n != 1 {
		t.Fatal("应排一个到期作业")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'grant.create' AND target_id = $1 AND source_id = $2", grantID, h.id); n != 1 {
		t.Fatal("应记审计")
	}

	// 访客看得到主机（来路 link），管不了它的授予。
	res = r.do("GET", "/v1/sources", nil, friend.bearer())
	r.expect(res, 200, "")
	if got := ids(res); !reflect.DeepEqual(got, []string{h.id}) || res.json["sources"].([]any)[0].(map[string]any)["via"] != "link" {
		t.Fatalf("访客的目录：%s", res.body)
	}
	r.expect(r.do("GET", sourcePath(h.id), nil, friend.bearer()), 200, "")
	r.expect(r.createGrant(friend, h.id, guestOf(stranger.accountID), "viewer", nil), 403, "source_access_denied")
	r.expect(r.do("GET", "/v1/grants?sourceId="+h.id, nil, friend.bearer()), 403, "source_access_denied")
	r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"role": "driver"}, friend.bearer()), 403, "source_access_denied")
	r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, friend.bearer()), 403, "source_access_denied")
	// 外人连主机都看不到。
	r.expect(r.do("GET", "/v1/grants?sourceId="+h.id, nil, stranger.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, stranger.bearer()), 404, "not_found")

	// 列表：按主机、按账号（只列调用者管得了的主机上的）、缺过滤 400、分组 403。
	res = r.do("GET", "/v1/grants?sourceId="+h.id, nil, owner.bearer())
	r.expect(res, 200, "")
	if rows := res.json["grants"].([]any); len(rows) != 1 || rows[0].(map[string]any)["grantId"] != grantID {
		t.Fatalf("按主机列：%s", res.body)
	}
	res = r.do("GET", "/v1/grants?accountId="+friend.accountID, nil, owner.bearer())
	r.expect(res, 200, "")
	if rows := res.json["grants"].([]any); len(rows) != 1 {
		t.Fatalf("按账号列：%s", res.body)
	}
	res = r.do("GET", "/v1/grants?accountId="+friend.accountID, nil, stranger.bearer())
	r.expect(res, 200, "")
	if rows := res.json["grants"].([]any); len(rows) != 0 {
		t.Fatalf("别人管的主机上的授予不该列出来：%s", res.body)
	}
	r.expect(r.do("GET", "/v1/grants", nil, owner.bearer()), 400, "bad_request")
	r.expect(r.do("GET", "/v1/grants?groupId=g", nil, owner.bearer()), 403, "plan_feature_unavailable")

	// 有效权限：访客 editor 限 w1 / w2、到期；所有者 driver 整台；陌生人无。
	res = r.do("GET", "/v1/grants/effective?sourceId="+h.id+"&accountId="+friend.accountID, nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["role"] != "editor" || !reflect.DeepEqual(res.json["workspaceIds"], []any{"w1", "w2"}) || res.json["expiresAtMs"] != float64(expires) {
		t.Fatalf("访客的有效权限：%s", res.body)
	}
	from := res.json["from"].([]any)[0].(map[string]any)
	if from["via"] != "guest" || from["grantId"] != grantID {
		t.Fatalf("来路：%s", res.body)
	}
	res = r.do("GET", "/v1/grants/effective?sourceId="+h.id+"&accountId="+owner.accountID, nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["role"] != "driver" || len(res.json["workspaceIds"].([]any)) != 0 || res.json["from"].([]any)[0].(map[string]any)["via"] != "owner" {
		t.Fatalf("所有者：%s", res.body)
	}
	res = r.do("GET", "/v1/grants/effective?sourceId="+h.id+"&accountId="+stranger.accountID, nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["role"] != nil || len(res.json["from"].([]any)) != 0 {
		t.Fatalf("陌生人：%s", res.body)
	}

	// 改：至少给一项；到期必须在将来；null 清到期（并撤掉作业）；范围可清成整台。
	r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{}, owner.bearer()), 400, "bad_request")
	r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"expiresAtMs": r.clock.Ms() - 1}, owner.bearer()), 400, "bad_request")
	r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"role": "root"}, owner.bearer()), 400, "bad_request")
	res = r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"role": "driver", "workspaceIds": []string{}, "expiresAtMs": nil}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["role"] != "driver" || res.json["expiresAtMs"] != nil || len(res.json["workspaceIds"].([]any)) != 0 {
		t.Fatalf("改后：%s", res.body)
	}
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'grant.expire' AND dedupe_key = $1 AND done_at_ms IS NULL", grantID); n != 0 {
		t.Fatal("清掉到期应撤掉作业")
	}
	r.expect(r.do("PATCH", "/v1/grants/nope", map[string]any{"role": "viewer"}, owner.bearer()), 404, "not_found")

	// 撤：之后访客看不到主机、再撤 404、可以重新建。
	r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, owner.bearer()), 200, "")
	r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"role": "viewer"}, owner.bearer()), 404, "not_found")
	if got := ids(r.do("GET", "/v1/sources", nil, friend.bearer())); len(got) != 0 {
		t.Fatalf("撤销后访客还看得到：%v", got)
	}
	r.expect(r.do("GET", sourcePath(h.id), nil, friend.bearer()), 404, "not_found")
	if at, by := r.grantRow(grantID); at == nil || by != owner.accountID {
		t.Fatalf("撤销记录：%v %q", at, by)
	}
	r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", nil)
}

// 团队主机：谁能管、主体合法性（成员角色、guest、allowGuests、主机设置）、跨团队 404。
func TestGrantsTeamManageAndSubjects(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, other, billing, guest, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	t1 := r.seedTeam("gr-1", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", other.accountID: "member",
		billing.accountID: "billing", guest.accountID: "guest",
	})
	t2 := r.seedTeam("gr-2", map[string]string{outsider.accountID: "owner"})
	hOwn := r.addHost(owner, teamSpace(t1), "0.10.0")
	hMem := r.addHost(member, teamSpace(t1), "0.10.0")
	hOut := r.addHost(outsider, teamSpace(t2), "0.10.0")

	// admin 管团队所有主机；member 只管自己的；看得到却管不了答 team_role_insufficient；看不到 404。
	gAdmin := r.mustGrant(admin, hOwn.id, memberOf(other.accountID), "operator", nil)
	r.mustGrant(member, hMem.id, teamOf(t1), "viewer", map[string]any{"workspaceIds": []string{"w1"}})
	r.expect(r.createGrant(member, hOwn.id, memberOf(other.accountID), "viewer", nil), 404, "not_found")
	r.expect(r.createGrant(other, hOwn.id, memberOf(member.accountID), "viewer", nil), 403, "team_role_insufficient")
	r.expect(r.do("DELETE", "/v1/grants/"+gAdmin, nil, other.bearer()), 403, "team_role_insufficient")
	for _, s := range []session{billing, guest, outsider} {
		r.expect(r.createGrant(s, hOwn.id, memberOf(other.accountID), "viewer", nil), 404, "not_found")
		r.expect(r.do("GET", "/v1/grants?sourceId="+hOwn.id, nil, s.bearer()), 404, "not_found")
	}
	// 团队 owner 之外的团队的人用别的团队的 grantId 也是 404。
	r.expect(r.do("PATCH", "/v1/grants/"+gAdmin, map[string]any{"role": "viewer"}, outsider.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/grants/"+gAdmin, nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/grants/effective?sourceId="+hOwn.id+"&accountId="+other.accountID, nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/sources/"+hOwn.id+"/access", nil, outsider.bearer()), 404, "not_found")

	// 主体：team 只能是本团队；member 只能是 owner / admin / member；guest 只能是 guest 角色；非成员 404。
	r.expect(r.createGrant(owner, hOwn.id, teamOf(t2), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, hOwn.id, memberOf(billing.accountID), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, hOwn.id, memberOf(guest.accountID), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, hOwn.id, memberOf(outsider.accountID), "viewer", nil), 404, "not_found")
	r.expect(r.createGrant(owner, hOwn.id, guestOf(other.accountID), "viewer", nil), 400, "bad_request")
	r.expect(r.createGrant(owner, hOwn.id, guestOf(outsider.accountID), "viewer", nil), 404, "not_found")
	r.expect(r.createGrant(owner, hOwn.id, memberOf(owner.accountID), "viewer", nil), 400, "bad_request") // 所有者本来就有
	r.expect(r.createGrant(owner, hOwn.id, memberOf(other.accountID), "viewer", nil), 409, "conflict")
	r.expect(r.createGrant(owner, hOwn.id, guestOf(guest.accountID), "viewer", nil), 400, "bad_request") // 团队 guest 要给具体的工作空间
	r.expect(r.createGrant(owner, hOwn.id, guestOf(guest.accountID), "viewer", map[string]any{"workspaceIds": []string{"bad id!"}}), 400, "bad_request")
	r.expect(r.createGrant(owner, hOwn.id, memberOf(other.accountID), "viewer", map[string]any{"workspaceIds": []string{strings.Repeat("a", 129)}}), 400, "bad_request")
	gGuest := r.mustGrant(owner, hOwn.id, guestOf(guest.accountID), "viewer", map[string]any{"workspaceIds": []string{"w:1.a-b_c"}})
	r.expect(r.do("PATCH", "/v1/grants/"+gGuest, map[string]any{"workspaceIds": []string{}}, owner.bearer()), 400, "bad_request") // 不能改成整台
	r.expect(r.do("PATCH", "/v1/grants/"+gGuest, map[string]any{"workspaceIds": []string{"no/slash"}}, owner.bearer()), 400, "bad_request")
	// 团队关掉 allowGuests、或主机设置关掉：新增 guest 授予 403，已有的不动（Q8）。
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{allowGuests}', 'false') WHERE team_id = $1", t1)
	r.expect(r.createGrant(owner, hMem.id, guestOf(guest.accountID), "viewer", nil), 403, "guests_disabled")
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{allowGuests}', 'true') WHERE team_id = $1", t1)
	r.expect(r.do("PATCH", sourcePath(hMem.id), map[string]any{"settings": map[string]any{"allowGuests": false}}, owner.bearer()), 200, "")
	r.expect(r.createGrant(owner, hMem.id, guestOf(guest.accountID), "viewer", nil), 403, "guests_disabled")
	if at, _ := r.grantRow(gGuest); at != nil {
		t.Fatal("已有的 guest 授予不该被动")
	}
	// 团队进恢复期：团队主机的授予接口全部看不到。
	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", t1)
	r.expect(r.createGrant(owner, hOwn.id, memberOf(other.accountID), "viewer", nil), 404, "not_found")
	_ = hOut
}

// ---- sources.assertion ----

func claimsOf(t *testing.T, res response) (assertion, relay map[string]any) {
	t.Helper()
	return payloadOf(t, res.json["assertion"].(string)), payloadOf(t, res.json["relayToken"].(string))
}

// 断言合成：个人空间所有者与个人中转同形；团队主机带 team、role、email；范围进 scp；jti 前缀与签发索引、审计。
func TestAssertionClaimsAndIndex(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, other := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("as-1", map[string]string{owner.accountID: "owner", admin.accountID: "admin", other.accountID: "member"})
	hTeam := r.addHost(owner, teamSpace(team), "0.10.0")
	hPersonal := r.addHost(owner, nil, "0.9.2")

	// 个人空间所有者：不受版本门，不带 role / team / scp。
	res := r.assertion(owner, hPersonal.id)
	r.expect(res, 200, "")
	a, rt := claimsOf(t, res)
	for _, key := range []string{"role", "team", "scp", "link"} {
		if _, has := a[key]; has {
			t.Fatalf("个人空间所有者的断言不该带 %s：%v", key, a)
		}
	}
	if a["sub"] != owner.accountID || a["aud"] != hPersonal.id || a["email"] == nil || a["emailVerified"] != true || a["name"] != "测试用户" {
		t.Fatalf("个人空间所有者的断言：%v", a)
	}
	if _, has := rt["team"]; has || rt["src"] != hPersonal.id || rt["dev"] != owner.deviceID || rt["aud"] != "relay" {
		t.Fatalf("个人空间的中继令牌：%v", rt)
	}
	if res.json["relayOrigin"] != "https://"+hPersonal.id+".src.src.localhost" || res.json["relayBaseUrl"] != res.json["relayOrigin"] || res.json["online"] != false {
		t.Fatalf("地址：%s", res.body)
	}

	// 团队 admin 看得到全部主机，没有授予拿不到断言；有了 member 授予后拿到，带 team、role、scp。
	r.expect(r.assertion(admin, hTeam.id), 403, "source_access_denied")
	r.expect(r.assertion(other, hTeam.id), 403, "source_access_denied")
	r.mustGrant(owner, hTeam.id, memberOf(admin.accountID), "operator", map[string]any{"workspaceIds": []string{"w2", "w1"}})
	res = r.assertion(admin, hTeam.id)
	r.expect(res, 200, "")
	a, rt = claimsOf(t, res)
	scp := fmt.Sprint(a["scp"])
	if a["role"] != "operator" || scp != "[ws:w1 ws:w2]" || fmt.Sprint(a["team"]) != fmt.Sprint(map[string]any{"teamId": team, "role": "admin"}) {
		t.Fatalf("团队断言：%v", a)
	}
	if rt["team"] != team || rt["sub"] != admin.accountID {
		t.Fatalf("团队中继令牌：%v", rt)
	}
	ja, jr := jtisOf(t, res)
	if !strings.HasPrefix(ja, "s."+admin.deviceID+"."+hTeam.id+".") || !strings.HasPrefix(jr, "r."+admin.deviceID+"."+hTeam.id+".") {
		t.Fatalf("jti 前缀：%s %s", ja, jr)
	}
	// 团队所有者主机的所有者：driver 整台。
	res = r.assertion(owner, hTeam.id)
	r.expect(res, 200, "")
	a, _ = claimsOf(t, res)
	if a["role"] != "driver" || fmt.Sprint(a["team"]) != fmt.Sprint(map[string]any{"teamId": team, "role": "owner"}) {
		t.Fatalf("团队所有者：%v", a)
	}
	if _, has := a["scp"]; has {
		t.Fatalf("整台不带 scp：%v", a)
	}

	// 签发索引：两个 jti 都在 PostgreSQL 与内存里（先落库）；审计 assertion.issue 带断言的 jti 与团队。
	if n := r.issuedCount(admin.accountID, hTeam.id); n != 2 {
		t.Fatalf("签发索引：%d", n)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'assertion.issue' AND jti = $1 AND team_id = $2 AND source_id = $3 AND target_kind = 'host' AND actor_id = $4",
		ja, team, hTeam.id, admin.accountID); n != 1 {
		t.Fatalf("审计：%d", n)
	}
	// 个人空间的审计没有团队。
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'assertion.issue' AND source_id = $1 AND team_id IS NULL", hPersonal.id); n != 1 {
		t.Fatalf("个人空间的审计：%d", n)
	}

	// 未知主机、没有会话。
	r.expect(r.assertion(owner, strings.Repeat("a", 32)), 403, "source_access_denied")
	r.expect(r.do("POST", "/v1/sources/"+hTeam.id+"/assertion", map[string]any{}, nil), 401, "unauthenticated")
}

// 版本门：取在线隧道上报的版本，不在线才用登记时的；团队 0.10.0，个人空间访客 0.11.0，所有者本人不受限。
// 不够门槛答 403 source_core_outdated（与访问被收回的 source_access_denied 分开），details 是 { coreVersion, minVersion }。
func TestAssertionVersionGate(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend := r.verified(), r.verified()
	team := r.seedTeam("vg", map[string]string{owner.accountID: "owner"})
	hTeam := r.addHost(owner, teamSpace(team), "0.10.0")
	hGuest := r.addHost(owner, nil, "0.11.0")
	hOld := r.addHost(owner, nil, "0.10.5")
	r.mustGrant(owner, hGuest.id, guestOf(friend.accountID), "viewer", nil)
	r.mustGrant(owner, hOld.id, guestOf(friend.accountID), "viewer", nil)

	// 登记版本够门槛、不在线：放行。
	r.expect(r.assertion(owner, hTeam.id), 200, "")
	r.expect(r.assertion(friend, hGuest.id), 200, "")
	// role-only 的门槛是 0.11.0：登记 0.10.5 的访客拿不到；所有者拿得到。
	r.expectOutdated(r.assertion(friend, hOld.id), "0.10.5", gen.MinCoreVersionRoleOnlyAssertion)
	r.expect(r.assertion(owner, hOld.id), 200, "")

	// 在线时取隧道上报的版本：旧 core 连上来就不再签带 team 的断言 / 只带 role 的断言。
	r.run.Relay.SetRoute(hTeam.id, "0.9.9")
	r.run.Relay.SetRoute(hGuest.id, "0.10.9")
	r.expectOutdated(r.assertion(owner, hTeam.id), "0.9.9", gen.MinCoreVersionTeamAssertion)
	r.expectOutdated(r.assertion(friend, hGuest.id), "0.10.9", gen.MinCoreVersionRoleOnlyAssertion)
	r.expect(r.assertion(owner, hGuest.id), 200, "") // 所有者不受门限
	// 反过来：登记时低、在线时高，放行。
	r.run.Relay.SetRoute(hOld.id, "0.11.0")
	r.run.Relay.SetRoute(hTeam.id, "0.10.0")
	r.expect(r.assertion(friend, hOld.id), 200, "")
	r.expect(r.assertion(owner, hTeam.id), 200, "")
	// 解析不了的版本不放行（不签降级断言）。
	r.run.Relay.SetRoute(hTeam.id, "dev")
	r.expectOutdated(r.assertion(owner, hTeam.id), "dev", gen.MinCoreVersionTeamAssertion)
	// 下线后回到登记版本。
	r.run.Relay.SetRoute(hTeam.id, "")
	r.expect(r.assertion(owner, hTeam.id), 200, "")
}

// expectOutdated：版本门的答复是 403 source_core_outdated，details 带门槛按的版本与最低版本。
func (r *rig) expectOutdated(res response, coreVersion, minVersion string) {
	r.t.Helper()
	r.expect(res, 403, "source_core_outdated")
	details, _ := res.json["details"].(map[string]any)
	if details["coreVersion"] != coreVersion || details["minVersion"] != minVersion {
		r.t.Fatalf("版本门的 details：%s", res.body)
	}
}

// 团队停用 team_suspended；团队删除 / 外人 / 跨团队拿不到；主机撤销 410；事故开关 503；限流 429。
func TestAssertionGates(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, member, outsider := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("ag-1", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	t2 := r.seedTeam("ag-2", map[string]string{outsider.accountID: "owner"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	hOut := r.addHost(outsider, teamSpace(t2), "0.10.0")
	r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	r.expect(r.assertion(member, h.id), 200, "")

	// 跨团队：T2 的 owner 拿不到 T1 的断言，T1 的 owner 也拿不到 T2 的。
	r.expect(r.assertion(outsider, h.id), 403, "source_access_denied")
	r.expect(r.assertion(owner, hOut.id), 403, "source_access_denied")

	r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
	r.expect(r.assertion(member, h.id), 403, "team_suspended")
	r.expect(r.assertion(owner, h.id), 403, "team_suspended")
	r.expect(r.assertion(outsider, h.id), 403, "source_access_denied") // 外人不知道它停用了
	r.exec("UPDATE teams SET status = 'active' WHERE team_id = $1", team)
	r.expect(r.assertion(member, h.id), 200, "")

	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", team)
	r.expect(r.assertion(member, h.id), 403, "source_access_denied")
	r.expect(r.assertion(owner, h.id), 403, "source_access_denied")
	r.exec("UPDATE teams SET deleted_at_ms = NULL, purge_at_ms = NULL WHERE team_id = $1", team)

	// 事故开关。
	r.exec(`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms) VALUES ('cloud', 'incident', '{"assertionsPaused": true}', 1)`)
	r.expect(r.assertion(member, h.id), 503, "unavailable")
	r.exec("DELETE FROM platform_settings WHERE key = 'incident'")
	r.expect(r.assertion(member, h.id), 200, "")

	// 主机撤销：410，已有的令牌进撤销表。
	r.expect(r.do("DELETE", sourcePath(h.id), nil, owner.bearer()), 200, "")
	r.expect(r.assertion(member, h.id), 410, "source_revoked")
	r.expect(r.assertion(owner, h.id), 410, "source_revoked")
}

func TestAssertionRateLimit(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.10.0")
	for i := 0; i < 60; i++ {
		if res := r.assertion(owner, h.id); res.status != 200 {
			t.Fatalf("第 %d 次：%d %s", i+1, res.status, res.body)
		}
	}
	res := r.assertion(owner, h.id)
	r.expect(res, 429, "rate_limited")
}

// ---- 收窄即撤销 ----

// revokedBetween 报告两枚令牌是否都已进撤销表（内存与 PostgreSQL 两处）。
func (r *rig) bothRevoked(res response) bool {
	r.t.Helper()
	a, b := jtisOf(r.t, res)
	inDB := func(jti string) bool {
		return r.queryInt("SELECT count(*)::int FROM relay_revoked_tokens WHERE jti = $1", jti) == 1
	}
	return r.run.Relay.IsRevoked(a) && r.run.Relay.IsRevoked(b) && inDB(a) && inDB(b)
}

func (r *rig) neitherRevoked(res response) bool {
	r.t.Helper()
	a, b := jtisOf(r.t, res)
	return !r.run.Relay.IsRevoked(a) && !r.run.Relay.IsRevoked(b)
}

// 降级、缩范围、提前到期、撤销、按账号撤：已签断言与中继令牌进撤销表并按人踢；放宽不撤；别人的令牌不动。
func TestGrantNarrowingRevokesIssuedTokens(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend, bystander := r.verified(), r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	gFriend := r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)
	r.mustGrant(owner, h.id, guestOf(bystander.accountID), "driver", nil)

	sign := func(s session) response {
		res := r.assertion(s, h.id)
		r.expect(res, 200, "")
		return res
	}
	patch := func(body map[string]any) {
		t.Helper()
		r.expect(r.do("PATCH", "/v1/grants/"+gFriend, body, owner.bearer()), 200, "")
	}
	friendTok, bystanderTok := sign(friend), sign(bystander)

	// 放宽不撤：先放宽（升角色已是 driver，改范围 → 整台不变；加到期是收窄，所以先测不变）。
	patch(map[string]any{"role": "driver"})
	if !r.neitherRevoked(friendTok) {
		t.Fatal("没变化不该撤")
	}
	// 降级。
	patch(map[string]any{"role": "operator"})
	if !r.bothRevoked(friendTok) {
		t.Fatal("降级后令牌应进撤销表")
	}
	if !r.neitherRevoked(bystanderTok) {
		t.Fatal("别人的令牌不该动")
	}
	if got := r.kicks(); len(got) != 1 || got[0] != h.id+"|"+friend.accountID+"|grant_changed" {
		t.Fatalf("按人踢：%v", got)
	}
	if n := r.issuedCount(friend.accountID, h.id); n != 0 {
		t.Fatalf("撤掉的令牌应从索引删：%d", n)
	}

	// 升角色（放宽）不撤；缩范围（整台 → 限范围）撤；范围放大不撤；换一个范围撤。
	tok := sign(friend)
	patch(map[string]any{"role": "driver"})
	if !r.neitherRevoked(tok) {
		t.Fatal("升角色不该撤")
	}
	patch(map[string]any{"workspaceIds": []string{"w1"}})
	if !r.bothRevoked(tok) {
		t.Fatal("整台缩成一个范围应撤")
	}
	tok = sign(friend)
	patch(map[string]any{"workspaceIds": []string{"w1", "w2"}})
	if !r.neitherRevoked(tok) {
		t.Fatal("范围放大不该撤")
	}
	patch(map[string]any{"workspaceIds": []string{"w2"}})
	if !r.bothRevoked(tok) {
		t.Fatal("范围换了一个应撤")
	}
	// 加到期（不过期 → 有到期）算收窄；推后到期不撤。
	patch(map[string]any{"workspaceIds": []string{}})
	tok = sign(friend)
	patch(map[string]any{"expiresAtMs": r.clock.Ms() + 600_000})
	if !r.bothRevoked(tok) {
		t.Fatal("加到期应撤")
	}
	tok = sign(friend)
	patch(map[string]any{"expiresAtMs": r.clock.Ms() + 900_000})
	if !r.neitherRevoked(tok) {
		t.Fatal("推后到期不该撤")
	}
	// 撤销授予：令牌撤、再拿断言 403。
	r.expect(r.do("DELETE", "/v1/grants/"+gFriend, nil, owner.bearer()), 200, "")
	if !r.bothRevoked(tok) {
		t.Fatal("撤销授予应撤令牌")
	}
	r.expect(r.assertion(friend, h.id), 403, "source_access_denied")
	if !r.neitherRevoked(bystanderTok) {
		t.Fatal("别人的令牌始终不动")
	}

	// 按账号撤（accessRevoke）：撤掉他本人的授予与令牌；没有授予 404。
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+friend.accountID, nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+bystander.accountID, nil, friend.bearer()), 404, "not_found") // 他的授予已撤，主机也看不到了
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+bystander.accountID, nil, owner.bearer()), 200, "")
	if !r.bothRevoked(bystanderTok) {
		t.Fatal("accessRevoke 应撤令牌")
	}
	r.expect(r.assertion(bystander, h.id), 403, "source_access_denied")
}

// 合成不放大（#111）：driver@w1（member 授予）+ 团队 viewer 整台 → viewer 整台，断言不带 scp；撤团队授予后回到 driver {w1}；
// 再加 viewer@w2 → viewer {w1,w2}。
func TestAssertionComposeDoesNotEscalate(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, m := r.verified(), r.verified()
	team := r.seedTeam("esc", map[string]string{owner.accountID: "owner", m.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	r.mustGrant(owner, h.id, memberOf(m.accountID), "driver", map[string]any{"workspaceIds": []string{"w1"}})
	gTeam := r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	res := r.assertion(m, h.id)
	r.expect(res, 200, "")
	a, _ := claimsOf(t, res)
	if _, has := a["scp"]; a["role"] != "viewer" || has {
		t.Fatalf("driver@w1 + viewer 整台应为 viewer 整台：%v", a)
	}
	r.expect(r.do("DELETE", "/v1/grants/"+gTeam, nil, owner.bearer()), 200, "")
	if !r.bothRevoked(res) {
		t.Fatal("viewer 整台 → driver {w1} 的令牌应撤")
	}
	res = r.assertion(m, h.id)
	r.expect(res, 200, "")
	if a, _ = claimsOf(t, res); a["role"] != "driver" || fmt.Sprint(a["scp"]) != "[ws:w1]" {
		t.Fatalf("剩 driver@w1：%v", a)
	}
	r.mustGrant(owner, h.id, teamOf(team), "viewer", map[string]any{"workspaceIds": []string{"w2"}})
	if !r.bothRevoked(res) {
		t.Fatal("角色降为 viewer 的令牌应撤")
	}
	res = r.assertion(m, h.id)
	r.expect(res, 200, "")
	if a, _ = claimsOf(t, res); a["role"] != "viewer" || fmt.Sprint(a["scp"]) != "[ws:w1 ws:w2]" {
		t.Fatalf("driver@w1 + viewer@w2：%v", a)
	}
}

// 团队授予收窄：撤销团队授予撤掉每个成员的令牌；成员被降为 guest 的令牌撤销走 S4 的流程，这里确认索引也在。
func TestTeamGrantRevokeRevokesEveryMembersTokens(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, m1, m2, m3 := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("tg", map[string]string{owner.accountID: "owner", m1.accountID: "member", m2.accountID: "member", m3.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	gTeam := r.mustGrant(owner, h.id, teamOf(team), "editor", nil)
	gM3 := r.mustGrant(owner, h.id, memberOf(m3.accountID), "viewer", nil)
	sign := func(s session) response {
		res := r.assertion(s, h.id)
		r.expect(res, 200, "")
		return res
	}
	t1, t2, t3, tOwn := sign(m1), sign(m2), sign(m3), sign(owner)
	// m3 同时有 member 授予（viewer）：撤团队授予后 editor → viewer，仍是收窄，三个人的令牌都撤；所有者的不动。
	r.expect(r.do("DELETE", "/v1/grants/"+gTeam, nil, owner.bearer()), 200, "")
	for i, tok := range []response{t1, t2, t3} {
		if !r.bothRevoked(tok) {
			t.Fatalf("成员 %d 的令牌应撤", i+1)
		}
	}
	if !r.neitherRevoked(tOwn) {
		t.Fatal("所有者的令牌不该动")
	}
	r.expect(r.assertion(m1, h.id), 403, "source_access_denied")
	res := r.assertion(m3, h.id)
	r.expect(res, 200, "")
	if a, _ := claimsOf(t, res); a["role"] != "viewer" {
		t.Fatalf("m3 剩 member 授予：%v", a)
	}
	// 撤掉 m3 的 member 授予：只有 m3 的令牌撤。
	tok := sign(m3)
	r.expect(r.do("DELETE", "/v1/grants/"+gM3, nil, owner.bearer()), 200, "")
	if !r.bothRevoked(tok) {
		t.Fatal("m3 的令牌应撤")
	}
	// 改成员的角色（S4 的路径，同一套签发索引）：m1 重新拿到授予后被降成 guest → 令牌撤。
	r.mustGrant(owner, h.id, memberOf(m1.accountID), "viewer", nil)
	tok = sign(m1)
	r.expect(r.do("PATCH", teamPath(team, "members", m1.accountID), map[string]any{"role": "guest"}, owner.bearer()), 200, "")
	if !r.bothRevoked(tok) {
		t.Fatal("降成 guest 应撤令牌")
	}
	r.expect(r.assertion(m1, h.id), 403, "source_access_denied")
}

// 到期：令牌寿命不超过授予的到期，grant.expire 在到期那一刻写撤销（revoked_by = system）、对权限真正收窄的账号踢人、审计。
// 另有来路而权限没变的人不受牵连。
func TestGrantExpiryJobRevokesAndKicks(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend, narrowed, kept := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("ex", map[string]string{owner.accountID: "owner", narrowed.accountID: "member", kept.accountID: "member"})
	hP := r.addHost(owner, nil, "0.11.0")
	hT := r.addHost(owner, teamSpace(team), "0.10.0")
	exp := r.clock.Ms() + 200_000
	gP := r.mustGrant(owner, hP.id, guestOf(friend.accountID), "editor", map[string]any{"expiresAtMs": exp})
	gT := r.mustGrant(owner, hT.id, teamOf(team), "viewer", map[string]any{"expiresAtMs": exp})
	// kept 另有一条不过期的 member 授予（同角色同范围）：团队授予到期后他的权限没变。
	gKeep := r.mustGrant(owner, hT.id, memberOf(kept.accountID), "viewer", nil)
	gLater := r.mustGrant(owner, hP.id, guestOf(narrowed.accountID), "viewer", map[string]any{"expiresAtMs": exp + 3_600_000})

	friendTok := r.assertion(friend, hP.id)
	r.expect(friendTok, 200, "")
	// 令牌寿命不超过授予的到期（断言 5 分钟、中继令牌 1 小时的缺省都被截短）。
	if got := int64(friendTok.json["assertionExpiresAtMs"].(float64)); got > exp {
		t.Fatalf("断言寿命超过授予的到期：%d > %d", got, exp)
	}
	if got := int64(friendTok.json["relayTokenExpiresAtMs"].(float64)); got > exp {
		t.Fatalf("中继令牌寿命超过授予的到期：%d > %d", got, exp)
	}
	r.expect(r.assertion(narrowed, hT.id), 200, "")
	r.expect(r.assertion(kept, hT.id), 200, "")

	r.clock.Advance(201 * time.Second)
	eventuallyDB(t, "到期的授予被撤销", func() bool {
		at, _ := r.grantRow(gP)
		at2, _ := r.grantRow(gT)
		return at != nil && at2 != nil
	})
	for _, id := range []string{gP, gT} {
		if at, by := r.grantRow(id); at == nil || by != "system" {
			t.Fatalf("到期撤销记录：%v %q", at, by)
		}
	}
	for _, id := range []string{gKeep, gLater} {
		if at, _ := r.grantRow(id); at != nil {
			t.Fatal("没到期的不动")
		}
	}
	eventuallyDB(t, "按人踢", func() bool {
		kicks := r.kicks()
		return contains(kicks, hP.id+"|"+friend.accountID+"|grant_expired") && contains(kicks, hT.id+"|"+narrowed.accountID+"|grant_expired")
	})
	for _, k := range r.kicks() {
		if strings.Contains(k, kept.accountID) {
			t.Fatalf("另有来路、权限没变的人不该被踢：%s", k)
		}
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'grant.expire' AND actor_kind = 'system' AND target_id = ANY($1)", []string{gP, gT}); n != 2 {
		t.Fatalf("到期审计：%d", n)
	}
	r.expect(r.assertion(friend, hP.id), 403, "source_access_denied")
	r.expect(r.assertion(kept, hT.id), 200, "")
	if got := ids(r.do("GET", "/v1/sources", nil, narrowed.bearer())); !reflect.DeepEqual(got, []string{hP.id}) {
		t.Fatalf("到期后的目录：%v", got)
	}
}

// 授予不足 1 秒到期时签不出令牌；有到期的授予签出的令牌寿命不超过它。
func TestAssertionLifetimeCappedByGrantExpiry(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", map[string]any{"expiresAtMs": r.clock.Ms() + 30_000})
	res := r.assertion(friend, h.id)
	r.expect(res, 200, "")
	a, rt := claimsOf(t, res)
	limit := (r.clock.Ms() + 30_000) / 1000
	if int64(a["exp"].(float64)) > limit || int64(rt["exp"].(float64)) > limit {
		t.Fatalf("令牌 exp 超过授予的到期：%v %v > %d", a["exp"], rt["exp"], limit)
	}
	r.clock.Advance(29_500 * time.Millisecond) // 剩 0.5 秒
	r.expect(r.assertion(friend, h.id), 403, "source_access_denied")
	// 没有到期（所有者）仍是缺省寿命：断言 5 分钟、中继令牌 1 小时。
	res = r.assertion(owner, h.id)
	r.expect(res, 200, "")
	now := r.clock.Ms()
	if got := int64(res.json["assertionExpiresAtMs"].(float64)); got < now+290_000 || got > now+300_000 {
		t.Fatalf("缺省断言寿命：%d", got-now)
	}
	if got := int64(res.json["relayTokenExpiresAtMs"].(float64)); got < now+3_590_000 || got > now+3_600_000 {
		t.Fatalf("缺省中继令牌寿命：%d", got-now)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// 兜底：到期作业丢了，每小时的 grant.expireDue 补插，授予照样被撤。
func TestGrantExpireDueBackstop(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	gID := r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", map[string]any{"expiresAtMs": r.clock.Ms() + 600_000})
	r.exec("DELETE FROM jobs WHERE kind = 'grant.expire' AND dedupe_key = $1", gID)
	r.clock.Advance(61 * time.Minute)
	eventuallyDB(t, "兜底作业撤销到期的授予", func() bool {
		at, _ := r.grantRow(gID)
		return at != nil
	})
}

// ---- 签发与撤销交错 ----

// gate 在断言签发的某个交错点停住第一次到达的请求。
type gate struct {
	stage   string
	once    sync.Once
	reached chan struct{}
	resume  chan struct{}
}

func newGate(stage string) *gate {
	return &gate{stage: stage, reached: make(chan struct{}), resume: make(chan struct{})}
}

func (g *gate) hook(stage string) {
	if stage != g.stage {
		return
	}
	g.once.Do(func() {
		close(g.reached)
		<-g.resume
	})
}

func (r *rig) revokedJTIs() (out []string) {
	rows, err := r.run.DB.Pool().Query(ctxBackground, "SELECT jti FROM relay_revoked_tokens ORDER BY jti")
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var jti string
		if err := rows.Scan(&jti); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, jti)
	}
	return out
}

// 照 §4.5 的顺序「签令牌 → 登记签发索引 → 复核」：撤销落在签发的任何两步之间，签出的令牌都不漏撤。
// 收窄为无权限时答 403；收窄为较低角色时撤掉旧的、按新权限重签。
func TestAssertionInterleavedWithNarrowing(t *testing.T) {
	for _, stage := range []string{"signed", "indexed"} {
		for _, mode := range []string{"revoke", "downgrade"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				t.Parallel()
				g := newGate(stage)
				r := newRig(t, rigOptions{grantsHook: g.hook})
				owner, friend := r.verified(), r.verified()
				h := r.addHost(owner, nil, "0.11.0")
				grantID := r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)

				result := make(chan response, 1)
				go func() { result <- r.assertion(friend, h.id) }()
				<-g.reached
				if mode == "revoke" {
					r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, owner.bearer()), 200, "")
				} else {
					r.expect(r.do("PATCH", "/v1/grants/"+grantID, map[string]any{"role": "viewer"}, owner.bearer()), 200, "")
				}
				close(g.resume)
				res := <-result

				first := r.revokedJTIs()
				if len(first) != 2 {
					t.Fatalf("第一次签出的两枚令牌都该撤：%v", first)
				}
				for _, jti := range first {
					if !r.run.Relay.IsRevoked(jti) {
						t.Fatalf("内存撤销表漏了 %s", jti)
					}
				}
				if mode == "revoke" {
					r.expect(res, 403, "source_access_denied")
					if n := r.issuedCount(friend.accountID, h.id); n != 0 {
						t.Fatalf("索引里还有 %d 枚", n)
					}
					return
				}
				r.expect(res, 200, "")
				a, _ := claimsOf(t, res)
				if a["role"] != "viewer" {
					t.Fatalf("应按新权限重签：%v", a)
				}
				ja, jr := jtisOf(t, res)
				if r.run.Relay.IsRevoked(ja) || r.run.Relay.IsRevoked(jr) || contains(first, ja) {
					t.Fatal("重签出的令牌不该被撤")
				}
				if n := r.issuedCount(friend.accountID, h.id); n != 2 {
					t.Fatalf("索引里应只有重签的两枚：%d", n)
				}
			})
		}
	}
}

// 主机在签发中间被撤销：断言答 410，签出的令牌也撤掉。
func TestAssertionInterleavedWithSourceRevoke(t *testing.T) {
	for _, stage := range []string{"signed", "indexed"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			g := newGate(stage)
			r := newRig(t, rigOptions{grantsHook: g.hook})
			owner := r.verified()
			h := r.addHost(owner, nil, "0.11.0")
			result := make(chan response, 1)
			go func() { result <- r.assertion(owner, h.id) }()
			<-g.reached
			r.expect(r.do("DELETE", sourcePath(h.id), nil, owner.bearer()), 200, "")
			close(g.resume)
			r.expect(<-result, 410, "source_revoked")
			for _, jti := range r.revokedJTIs() {
				if !r.run.Relay.IsRevoked(jti) {
					t.Fatalf("内存撤销表漏了 %s", jti)
				}
			}
			if n := r.issuedCount(owner.accountID, h.id); n != 0 {
				t.Fatalf("索引里还有 %d 枚", n)
			}
		})
	}
}

// 并发压力：很多断言请求与授予的建 / 撤反复交错，最后一次撤销之后，任何一枚答过 200 的令牌都已被撤。
func TestAssertionStormVersusGrantChurn(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	grantID := r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)

	var (
		mu       sync.Mutex
		tokens   []string
		wg       sync.WaitGroup
		denied   atomic.Int64
		busy     atomic.Int64
		budget   atomic.Int64          // 循环期间还能发的请求数
		churning = make(chan struct{}) // 撤销重建循环结束（授予稳定）后关闭
	)
	budget.Store(30)
	// attempt 签一次；答 200 记下令牌并返回 true。
	attempt := func() bool {
		res := r.assertion(friend, h.id)
		switch res.status {
		case 200:
			a, b := jtisOf(t, res)
			mu.Lock()
			tokens = append(tokens, a, b)
			mu.Unlock()
			return true
		case 403:
			denied.Add(1)
		case 503:
			busy.Add(1)
		default:
			t.Errorf("意外的答复 %d：%s", res.status, res.body)
		}
		return false
	}
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 签到循环结束：压力覆盖整个撤销重建过程，不依赖调度快慢。断言每账号每分钟 60 次（429），
			// 所以循环期间共用一份预算，留出余量给循环结束后的必签。
			for stop := false; !stop; {
				select {
				case <-churning:
					stop = true
				default:
					if budget.Add(-1) >= 0 {
						attempt()
					}
					time.Sleep(3 * time.Millisecond)
				}
			}
			// 授予已稳定，此后必然能签出（503 只是瞬时繁忙，重试几次）。
			for i := 0; i < 4; i++ {
				if attempt() {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Errorf("授予稳定之后 4 次都没签出来")
		}()
	}
	for round := 0; round < 4; round++ {
		r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, owner.bearer()), 200, "")
		grantID = r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)
	}
	close(churning)
	wg.Wait()
	r.expect(r.do("DELETE", "/v1/grants/"+grantID, nil, owner.bearer()), 200, "")
	t.Logf("签出 %d 枚令牌；403 %d 次，503 %d 次", len(tokens), denied.Load(), busy.Load())
	if len(tokens) < 12 {
		t.Fatalf("每个协程在授予稳定后至少签出一次，应不少于 12 枚：%d", len(tokens))
	}
	for _, jti := range tokens {
		if !r.run.Relay.IsRevoked(jti) {
			t.Errorf("最后一次撤销之后 %s 仍有效", jti)
		}
		if n := r.queryInt("SELECT count(*)::int FROM relay_revoked_tokens WHERE jti = $1", jti); n != 1 {
			t.Errorf("PostgreSQL 撤销表漏了 %s", jti)
		}
	}
	if n := r.issuedCount(friend.accountID, h.id); n != 0 {
		t.Fatalf("索引里还有 %d 枚", n)
	}
}

// ---- 可见性与有效权限一致 ----

// 目录里看得到 ⟺ 有有效权限，或是团队 owner / admin（管得了）：把目录 SQL 与 Compose 逐个主体对照。
func TestDirectoryVisibilityOverHTTPMatchesCompose(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	roles := []string{"owner", "admin", "member", "billing", "guest"}
	who := map[string]session{}
	members := map[string]string{}
	for _, role := range roles {
		who[role] = r.verified()
		members[who[role].accountID] = role
	}
	who["member2"] = r.verified()
	members[who["member2"].accountID] = "member"
	who["stranger"] = r.verified()
	team := r.seedTeam("par", members)
	hosts := map[string]testHost{
		"owner-registered":  r.addHost(who["owner"], teamSpace(team), "0.10.0"),
		"member-registered": r.addHost(who["member"], teamSpace(team), "0.10.0"),
		"billing-owned":     r.addHost(who["owner"], teamSpace(team), "0.10.0"),
		"personal":          r.addHost(who["owner"], nil, "0.11.0"),
	}
	r.exec("UPDATE source_spaces SET owner_account_id = $1 WHERE source_id = $2", who["billing"].accountID, hosts["billing-owned"].id)
	past, future := r.clock.Ms()-1, r.clock.Ms()+3_600_000
	insert := func(host, kind, subject string, expires *int64, revoked bool) {
		var revokedAt *int64
		if revoked {
			one := int64(1)
			revokedAt = &one
		}
		teamID := any(team)
		if host == "personal" {
			teamID = nil
		}
		r.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms, expires_at_ms, revoked_at_ms)
		  VALUES ('cloud', $1, $2, $3, $4, $5, 'viewer', 'x', 1, 1, $6, $7)`, fmt.Sprintf("g-%d", rand.Uint32()), hosts[host].id, teamID, kind, subject, expires, revokedAt)
	}
	// 团队授予：给 owner-registered 一条有效的，给 member-registered 一条已到期的。
	insert("owner-registered", "team", team, &future, false)
	insert("member-registered", "team", team, &past, false)
	// member / guest 授予各种状态。
	insert("billing-owned", "member", who["member2"].accountID, nil, false)
	insert("billing-owned", "member", who["billing"].accountID, nil, false) // billing 拿到 member 授予也没用
	insert("billing-owned", "guest", who["guest"].accountID, nil, false)
	insert("member-registered", "guest", who["guest"].accountID, nil, true) // 已撤销
	insert("personal", "guest", who["stranger"].accountID, &future, false)
	insert("personal", "guest", who["admin"].accountID, &past, false)

	reader := r.run.DB.SaaS().Read()
	for name, s := range who {
		listed := map[string]bool{}
		for _, id := range ids(r.do("GET", "/v1/sources", nil, s.bearer())) {
			listed[id] = true
		}
		for hostName, host := range hosts {
			basis, err := reader.AccessBasis(ctxBackground, "cloud", s.accountID, host.id)
			if err != nil {
				t.Fatal(err)
			}
			eff := grants.Compose(basis, s.accountID, r.clock.Ms())
			want := !eff.None() || (basis.TeamID != "" && !basis.TeamDeleted && (basis.TeamRole == "owner" || basis.TeamRole == "admin"))
			if listed[host.id] != want {
				t.Errorf("%s 对 %s：目录里看得到 = %v，有效权限 = %q，应为 %v", name, hostName, listed[host.id], eff.Role, want)
			}
			// 目录里看不到的主机，断言一律拿不到。
			if !listed[host.id] {
				r.expect(r.assertion(s, host.id), 403, "source_access_denied")
			} else if eff.None() {
				r.expect(r.assertion(s, host.id), 403, "source_access_denied")
			}
		}
	}
}

// ---- 访问清单 ----

func TestAccessListAndRevokeByAccount(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, guest := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("al", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", guest.accountID: "guest"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	gTeam := r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	r.mustGrant(owner, h.id, memberOf(member.accountID), "operator", map[string]any{"workspaceIds": []string{"w1"}})
	r.mustGrant(owner, h.id, guestOf(guest.accountID), "editor", map[string]any{"workspaceIds": []string{"w1"}})

	res := r.do("GET", "/v1/sources/"+h.id+"/access", nil, owner.bearer())
	r.expect(res, 200, "")
	rows := map[string]map[string]any{}
	for _, row := range res.json["access"].([]any) {
		m := row.(map[string]any)
		rows[m["accountId"].(string)] = m
	}
	if len(rows) != 4 {
		t.Fatalf("应有 4 个账号：%s", res.body)
	}
	if rows[owner.accountID]["via"] != "owner" || rows[owner.accountID]["role"] != "driver" || rows[owner.accountID]["grantId"] != nil {
		t.Fatalf("所有者：%v", rows[owner.accountID])
	}
	// 团队授予展开到 owner / admin / member；member 另有更高的 member 授予（operator，限 w1）——不放大：团队 viewer 整台 = viewer 整台。
	if rows[admin.accountID]["role"] != "viewer" || rows[admin.accountID]["grantId"] != gTeam || rows[admin.accountID]["via"] != "team" {
		t.Fatalf("admin：%v", rows[admin.accountID])
	}
	if rows[member.accountID]["role"] != "viewer" || rows[member.accountID]["grantId"] != gTeam || len(rows[member.accountID]["workspaceIds"].([]any)) != 0 {
		t.Fatalf("member：%v", rows[member.accountID])
	}
	if rows[guest.accountID]["role"] != "editor" || rows[guest.accountID]["email"] == nil {
		t.Fatalf("guest：%v", rows[guest.accountID])
	}
	// 管理权：admin 可看，member / guest / 外人不行。
	r.expect(r.do("GET", "/v1/sources/"+h.id+"/access", nil, admin.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/sources/"+h.id+"/access", nil, member.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("GET", "/v1/sources/"+h.id+"/access", nil, r.verified().bearer()), 404, "not_found")

	// accessRevoke 只撤本人的授予，团队授予撤不掉单个人：member 撤后还有团队授予的 viewer；合成结果本来就是 viewer 整台
	//（operator@w1 被舍弃），所以没有收窄，令牌不撤。
	tok := r.assertion(member, h.id)
	r.expect(tok, 200, "")
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+member.accountID, nil, admin.bearer()), 200, "")
	if !r.neitherRevoked(tok) {
		t.Fatal("合成结果没变，不该撤令牌")
	}
	res = r.assertion(member, h.id)
	r.expect(res, 200, "")
	if a, _ := claimsOf(t, res); a["role"] != "viewer" {
		t.Fatalf("剩团队授予：%v", a)
	}
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+member.accountID, nil, admin.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+guest.accountID, nil, member.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+guest.accountID, nil, owner.bearer()), 200, "")
	if got := ids(r.do("GET", "/v1/sources", nil, guest.bearer())); len(got) != 0 {
		t.Fatalf("guest 被撤后：%v", got)
	}
}

// 团队里的 guest 只拿得到被授予的主机，并且只在被授予的范围里。
func TestTeamGuestOnlyGetsGrantedHostAndScope(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, guest, other := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("tgu", map[string]string{owner.accountID: "owner", guest.accountID: "guest", other.accountID: "guest"})
	granted := r.addHost(owner, teamSpace(team), "0.10.0")
	hidden := r.addHost(owner, teamSpace(team), "0.10.0")
	r.mustGrant(owner, granted.id, guestOf(guest.accountID), "editor", map[string]any{"workspaceIds": []string{"w9"}})

	if got := ids(r.do("GET", "/v1/sources", nil, guest.bearer())); !reflect.DeepEqual(got, []string{granted.id}) {
		t.Fatalf("guest 的目录：%v", got)
	}
	r.expect(r.do("GET", sourcePath(hidden.id), nil, guest.bearer()), 404, "not_found")
	r.expect(r.assertion(guest, hidden.id), 403, "source_access_denied")
	res := r.assertion(guest, granted.id)
	r.expect(res, 200, "")
	a, rt := claimsOf(t, res)
	if a["role"] != "editor" || fmt.Sprint(a["scp"]) != "[ws:w9]" || fmt.Sprint(a["team"]) != fmt.Sprint(map[string]any{"teamId": team, "role": "guest"}) || rt["team"] != team {
		t.Fatalf("guest 的断言：%v %v", a, rt)
	}
	// 另一个 guest 没被授予，什么都没有。
	if got := ids(r.do("GET", "/v1/sources", nil, other.bearer())); len(got) != 0 {
		t.Fatalf("没被授予的 guest：%v", got)
	}
	r.expect(r.assertion(other, granted.id), 403, "source_access_denied")
	// guest 角色的人不能管授予。
	r.expect(r.createGrant(guest, granted.id, guestOf(other.accountID), "viewer", nil), 403, "team_role_insufficient")
}

// issued.gc：每小时删已到期的签发索引与撤销行，没到期的不动。
func TestIssuedGCJobDeletesExpiredRows(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	now := r.clock.Ms()
	r.exec(`INSERT INTO cloud_issued_tokens (tenant_id, jti, account_id, source_id, device_id, exp_ms) VALUES
	  ('cloud', 'old', 'a', 's', 'd', $1), ('cloud', 'live', 'a', 's', 'd', $2), ('other', 'old-other', 'a', 's', 'd', $1)`, now+1000, now+7_200_000)
	r.exec("INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('old', $1), ('live', $2)", now+1000, now+7_200_000)
	r.clock.Advance(61 * time.Minute)
	eventuallyDB(t, "过期的行被清掉", func() bool {
		return r.queryInt("SELECT count(*)::int FROM cloud_issued_tokens WHERE jti = 'old'") == 0 &&
			r.queryInt("SELECT count(*)::int FROM relay_revoked_tokens WHERE jti = 'old'") == 0
	})
	if r.queryInt("SELECT count(*)::int FROM cloud_issued_tokens WHERE jti = 'live'") != 1 || r.queryInt("SELECT count(*)::int FROM relay_revoked_tokens WHERE jti = 'live'") != 1 {
		t.Fatal("没到期的不该删")
	}
	if r.queryInt("SELECT count(*)::int FROM cloud_issued_tokens WHERE tenant_id = 'other'") != 1 {
		t.Fatal("别的租户的行不该删")
	}
}

// 签发中途登出 / 撤设备：复核发现设备不再有效，撤掉自己签的令牌并答 401。
func TestAssertionInterleavedWithLogout(t *testing.T) {
	for _, stage := range []string{"signed", "indexed"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			g := newGate(stage)
			r := newRig(t, rigOptions{grantsHook: g.hook})
			owner := r.verified()
			h := r.addHost(owner, nil, "0.11.0")
			result := make(chan response, 1)
			go func() { result <- r.assertion(owner, h.id) }()
			<-g.reached
			r.expect(r.do("POST", "/v1/auth/logout", nil, owner.bearer()), 200, "")
			close(g.resume)
			r.expect(<-result, 401, "unauthenticated")
			jtis := r.revokedJTIs()
			if len(jtis) != 2 {
				t.Fatalf("签出的两枚令牌都该撤：%v", jtis)
			}
			for _, jti := range jtis {
				if !r.run.Relay.IsRevoked(jti) {
					t.Fatalf("内存撤销表漏了 %s", jti)
				}
			}
			if n := r.issuedCount(owner.accountID, h.id); n != 0 {
				t.Fatalf("索引里还有 %d 枚", n)
			}
		})
	}
}

// 签发中途团队被停用：撤掉自己签的令牌并答 team_suspended。
func TestAssertionInterleavedWithTeamSuspension(t *testing.T) {
	t.Parallel()
	g := newGate("signed")
	r := newRig(t, rigOptions{grantsHook: g.hook})
	owner := r.verified()
	team := r.seedTeam("susp", map[string]string{owner.accountID: "owner"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	result := make(chan response, 1)
	go func() { result <- r.assertion(owner, h.id) }()
	<-g.reached
	r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
	close(g.resume)
	r.expect(<-result, 403, "team_suspended")
	jtis := r.revokedJTIs()
	if len(jtis) != 2 {
		t.Fatalf("签出的两枚令牌都该撤：%v", jtis)
	}
	for _, jti := range jtis {
		if !r.run.Relay.IsRevoked(jti) {
			t.Fatalf("内存撤销表漏了 %s", jti)
		}
	}
}
