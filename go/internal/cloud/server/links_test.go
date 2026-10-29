package server_test

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
)

// ---- 辅助 ----

func (r *rig) createLink(s session, sourceID string, extra map[string]any) response {
	r.t.Helper()
	body := map[string]any{
		"kind": "source_invite", "sourceId": sourceID, "invitationId": "inv-1", "label": "给客户看", "role": "viewer",
		"expiresAtMs": r.clock.Ms() + 24*3600*1000,
	}
	for k, v := range extra {
		body[k] = v
	}
	return r.do("POST", "/v1/links", body, s.bearer())
}

type linkMade struct{ id, secret string }

func (r *rig) mustLink(s session, sourceID string, extra map[string]any) linkMade {
	r.t.Helper()
	res := r.createLink(s, sourceID, extra)
	r.expect(res, 200, "")
	return linkMade{id: res.json["linkId"].(string), secret: res.json["secret"].(string)}
}

func linkPathOf(id string, rest ...string) string {
	path := "/v1/links/" + id
	for _, part := range rest {
		path += "/" + part
	}
	return path
}

func (r *rig) acceptLink(l linkMade, s *session) response {
	r.t.Helper()
	var h headers
	if s != nil {
		h = s.bearer()
	}
	return r.do("POST", linkPathOf(l.id, "accept"), map[string]any{"secret": l.secret, "device": map[string]any{"platform": "browser", "name": "访客浏览器"}}, h)
}

// guestOfAccept 是接受答复里的访客会话（只有访问令牌）。
func (r *rig) guestOfAccept(res response) session {
	r.t.Helper()
	body, ok := res.json["guestSession"].(map[string]any)
	if !ok {
		r.t.Fatalf("答复里没有访客会话：%s", res.body)
	}
	dev := body["device"].(map[string]any)
	return session{access: body["accessToken"].(string), deviceID: dev["deviceId"].(string)}
}

func (r *rig) linkRow(linkID string) (uses int, revoked bool) {
	r.t.Helper()
	var at *int64
	if err := r.run.DB.Pool().QueryRow(ctxBackground, "SELECT uses, revoked_at_ms FROM links WHERE link_id = $1", linkID).Scan(&uses, &at); err != nil {
		r.t.Fatal(err)
	}
	return uses, at != nil
}

func (r *rig) guestIssued(linkID string) int {
	return r.queryInt("SELECT count(*)::int FROM cloud_issued_tokens WHERE tenant_id = 'cloud' AND account_id LIKE $1", "guest:"+linkID+":%")
}

// ---- 个人空间 ----

// 链接全流程：创建（管理权、校验）→ 落地页 → 匿名接受（断言带 link 声明，jti 进签发索引）→ 访客再要断言
// （签→索引→复核）→ 列表 / 改备注 → 撤销（访客的令牌进撤销表、设备作废、按前缀踢人）。
func TestLinksPersonalLifecycle(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, stranger := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")

	r.expect(r.createLink(stranger, h.id, nil), 404, "not_found")
	r.expect(r.createLink(owner, strings.Repeat("0", 32), nil), 404, "not_found")
	r.expect(r.createLink(owner, h.id, map[string]any{"role": "root"}), 400, "bad_request")
	r.expect(r.createLink(owner, h.id, map[string]any{"expiresAtMs": r.clock.Ms() - 1}), 400, "bad_request")
	r.expect(r.createLink(owner, h.id, map[string]any{"expiresAtMs": r.clock.Ms() + 31*24*3600*1000}), 400, "bad_request")
	r.expect(r.do("POST", "/v1/links", map[string]any{"kind": "team_join", "teamId": "t", "label": "x", "expiresAtMs": r.clock.Ms() + 1000}, owner.bearer()), 400, "bad_request")
	r.expect(r.do("POST", "/v1/links", map[string]any{
		"kind": "source_invite", "sourceId": h.id, "invitationId": "i", "label": "x", "role": "viewer", "expiresAtMs": r.clock.Ms() + 1000,
	}, nil), 401, "unauthenticated")

	res := r.createLink(owner, h.id, map[string]any{"maxUses": 3, "scope": map[string]any{"workspaceId": "ws1", "readOnly": true}})
	r.expect(res, 200, "")
	link := linkMade{id: res.json["linkId"].(string), secret: res.json["secret"].(string)}
	if res.json["url"] != appOrigin+"/j/"+link.id {
		t.Fatalf("链接地址：%s", res.body)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'link.create' AND target_id = $1 AND source_id = $2 AND actor_id = $3", link.id, h.id, owner.accountID); n != 1 {
		t.Fatal("应记审计")
	}

	// 落地页（匿名）：个人空间不要求登录。
	res = r.do("GET", linkPathOf(link.id), nil, nil)
	r.expect(res, 200, "")
	if res.json["kind"] != "source_invite" || res.json["label"] != "给客户看" || res.json["requiresAccount"] != false ||
		res.json["exhausted"] != false || res.json["sourceName"] != "主机 "+h.id[:6] {
		t.Fatalf("落地页：%s", res.body)
	}
	r.expect(r.do("GET", linkPathOf("nope"), nil, nil), 404, "link_invalid")
	// 主人显示名（匿名可读）：只给显示名，不出现邮箱与账号 id；邮箱形态的显示名被清掉。
	r.exec("UPDATE accounts SET display_name = '爱丽丝' WHERE account_id = $1", owner.accountID)
	res = r.do("GET", linkPathOf(link.id), nil, nil)
	r.expect(res, 200, "")
	if res.json["ownerName"] != "爱丽丝" || strings.Contains(string(res.body), owner.accountID) || strings.Contains(string(res.body), "@") {
		t.Fatalf("ownerName：%s", res.body)
	}
	r.exec("UPDATE accounts SET display_name = 'a@example.com' WHERE account_id = $1", owner.accountID)
	if res := r.do("GET", linkPathOf(link.id), nil, nil); res.json["ownerName"] != nil {
		t.Fatalf("邮箱形态的显示名不该泄漏：%s", res.body)
	}
	r.exec("UPDATE accounts SET display_name = '爱丽丝' WHERE account_id = $1", owner.accountID)

	// 接受：秘密不对、匿名成功；断言带 link 声明与范围，没有团队与角色；jti 带设备与源并进了签发索引。
	r.expect(r.do("POST", linkPathOf(link.id, "accept"), map[string]any{"secret": "wrong"}, nil), 403, "link_secret_invalid")
	res = r.acceptLink(link, nil)
	r.expect(res, 200, "")
	accepted := res
	guest := r.guestOfAccept(res)
	claims := payloadOf(t, res.json["assertion"].(string))
	if _, bad := claims["team"]; bad {
		t.Fatalf("访客的断言不带团队：%v", claims)
	}
	if _, bad := claims["role"]; bad {
		t.Fatalf("链接访客的断言不带 role（按链接的角色由 core 判）：%v", claims)
	}
	if linkClaim, _ := claims["link"].(map[string]any); linkClaim["linkId"] != link.id || linkClaim["invitationId"] != "inv-1" ||
		!reflect.DeepEqual(claims["scp"], []any{"ws:ws1", "ro"}) && !reflect.DeepEqual(claims["scp"], []any{"ro", "ws:ws1"}) {
		t.Fatalf("断言声明：%v", claims)
	}
	aJTI, rJTI := jtisOf(t, res)
	if !strings.HasPrefix(aJTI, "s."+guest.deviceID+"."+h.id) || !strings.HasPrefix(rJTI, "r."+guest.deviceID+"."+h.id) {
		t.Fatalf("jti 前缀：%s %s", aJTI, rJTI)
	}
	if n := r.guestIssued(link.id); n != 2 {
		t.Fatalf("签发索引：%d", n)
	}
	if uses, _ := r.linkRow(link.id); uses != 1 {
		t.Fatalf("用量：%d", uses)
	}

	// 访客再要断言：签→索引→复核；索引多两条；审计记 system。
	res2 := r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer())
	r.expect(res2, 200, "")
	if n := r.guestIssued(link.id); n != 4 {
		t.Fatalf("再签之后的签发索引：%d", n)
	}
	if claims := payloadOf(t, res2.json["assertion"].(string)); claims["link"].(map[string]any)["linkId"] != link.id {
		t.Fatalf("再签的断言：%v", claims)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'assertion.issue' AND actor_kind = 'system' AND actor_id LIKE $1 AND source_id = $2", "guest:"+link.id+":%", h.id); n != 1 {
		t.Fatal("访客的断言应记审计")
	}
	// 访客只能要自己链接指向的那台主机。
	other := r.addHost(owner, nil, "0.11.0")
	r.expect(r.do("POST", "/v1/sources/"+other.id+"/assertion", map[string]any{}, guest.bearer()), 403, "source_access_denied")
	// 访客不能管链接。
	r.expect(r.do("GET", "/v1/links", nil, guest.bearer()), 403, "forbidden")
	r.expect(r.do("DELETE", linkPathOf(link.id), nil, guest.bearer()), 403, "forbidden")

	// 列表：所有者看得到；space 过滤；陌生人看不到；按 sourceId 要有管理权。
	res = r.do("GET", "/v1/links", nil, owner.bearer())
	r.expect(res, 200, "")
	list := res.json["links"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["linkId"] != link.id || list[0].(map[string]any)["uses"] != float64(1) ||
		list[0].(map[string]any)["maxUses"] != float64(3) || list[0].(map[string]any)["sourceId"] != h.id {
		t.Fatalf("列表：%s", res.body)
	}
	for _, query := range []string{"?space[kind]=personal", "?sourceId=" + h.id, "?sourceId=" + h.id + "&space[kind]=personal"} {
		if got := r.do("GET", "/v1/links"+query, nil, owner.bearer()); len(got.json["links"].([]any)) != 1 {
			t.Fatalf("%s：%s", query, got.body)
		}
	}
	for _, query := range []string{"?sourceId=" + other.id, "?sourceId=" + h.id + "&space[kind]=team&space[teamId]=x"} {
		got := r.do("GET", "/v1/links"+query, nil, owner.bearer())
		if got.status == 200 && len(got.json["links"].([]any)) != 0 {
			t.Fatalf("%s 应为空：%s", query, got.body)
		}
	}
	if got := r.do("GET", "/v1/links", nil, stranger.bearer()); len(got.json["links"].([]any)) != 0 {
		t.Fatalf("陌生人：%s", got.body)
	}
	r.expect(r.do("GET", "/v1/links?sourceId="+h.id, nil, stranger.bearer()), 404, "not_found")

	// 改备注：只改备注；陌生人 404 / 403。
	res = r.do("PATCH", linkPathOf(link.id), map[string]any{"label": "  新备注 "}, owner.bearer())
	r.expect(res, 200, "")
	if res.json["label"] != "新备注" || res.json["uses"] != float64(1) {
		t.Fatalf("改备注：%s", res.body)
	}
	if got := r.do("PATCH", linkPathOf(link.id), map[string]any{"label": "x"}, stranger.bearer()); got.status != 404 && got.status != 403 {
		t.Fatalf("陌生人改备注：%d %s", got.status, got.body)
	}

	// 撤销：陌生人 404；所有者撤销后访客的令牌进撤销表（内存与 PostgreSQL）、设备作废、按前缀踢、审计；再撤 404；接受 link_invalid。
	r.expect(r.do("DELETE", linkPathOf(link.id), nil, stranger.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", linkPathOf(link.id), nil, owner.bearer()), 200, "")
	if !r.bothRevoked(accepted) || !r.bothRevoked(res2) {
		t.Fatal("链接撤销后访客的令牌（接受时签的与之后再签的）都该撤")
	}
	if _, revoked := r.linkRow(link.id); !revoked {
		t.Fatal("链接应撤销")
	}
	kicked := false
	for _, c := range r.run.Relay.Commands() {
		kicked = kicked || (c.Type == relayctl.CommandKick && c.SourceID == h.id && c.SubjectPrefix == "guest:"+link.id+":")
	}
	if !kicked {
		t.Fatalf("应按前缀踢掉访客：%v", r.run.Relay.Commands())
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'link.revoke' AND target_id = $1", link.id); n != 1 {
		t.Fatal("应记撤销审计")
	}
	r.expect(r.do("DELETE", linkPathOf(link.id), nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 401, "unauthenticated")
	r.expect(r.acceptLink(link, nil), 404, "link_invalid")
	r.expect(r.do("GET", linkPathOf(link.id), nil, nil), 404, "link_invalid")
}

// 撤销链接把它名下访客已签的断言与中继令牌都撤掉（两枚 jti 都进撤销表）；别的链接的访客不受影响。
func TestLinkRevokeRevokesGuestTokens(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	a, b := r.mustLink(owner, h.id, nil), r.mustLink(owner, h.id, nil)
	resA, resB := r.acceptLink(a, nil), r.acceptLink(b, nil)
	r.expect(resA, 200, "")
	r.expect(resB, 200, "")
	guestA := r.guestOfAccept(resA)
	again := r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guestA.bearer())
	r.expect(again, 200, "")
	r.expect(r.do("DELETE", linkPathOf(a.id), nil, owner.bearer()), 200, "")
	if !r.bothRevoked(resA) || !r.bothRevoked(again) {
		t.Fatal("链接撤销后访客的令牌都该撤")
	}
	if !r.neitherRevoked(resB) {
		t.Fatal("别的链接的访客不受影响")
	}
}

// 链接并发 max_uses：12 个同时接受、max_uses = 5，恰好 5 个成功，其余 link_exhausted；用量 5、访客设备 5、索引 10 条。
func TestLinkMaxUsesConcurrent(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	link := r.mustLink(owner, h.id, map[string]any{"maxUses": 5})
	statuses := make([]int, 12)
	codes := make([]any, 12)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := r.acceptLink(link, nil)
			statuses[i], codes[i] = res.status, res.json["code"]
		}()
	}
	wg.Wait()
	ok, exhausted := 0, 0
	for i, status := range statuses {
		switch {
		case status == 200:
			ok++
		case status == 410 && codes[i] == "link_exhausted":
			exhausted++
		default:
			t.Fatalf("意外的结果 %d %v", status, codes[i])
		}
	}
	if ok != 5 || exhausted != 7 {
		t.Fatalf("成功 %d 个、用尽 %d 个", ok, exhausted)
	}
	if uses, _ := r.linkRow(link.id); uses != 5 {
		t.Fatalf("用量：%d", uses)
	}
	if n := r.queryInt("SELECT count(*)::int FROM devices WHERE subject LIKE $1 AND revoked_at_ms IS NULL", "guest:"+link.id+":%"); n != 5 {
		t.Fatalf("访客设备：%d", n)
	}
	if n := r.guestIssued(link.id); n != 10 {
		t.Fatalf("签发索引：%d", n)
	}
	if res := r.do("GET", linkPathOf(link.id), nil, nil); res.json["exhausted"] != true {
		t.Fatalf("落地页应报用尽：%s", res.body)
	}
}

// 团队主机：匿名链接开关（缺省关：匿名 403 anonymous_links_disabled 且不消耗次数；带会话可以；打开后匿名可以）；
// 团队关了 guest：不能签、接受时链接失效；团队停用：访客进不来。
func TestLinksTeamAnonymousSwitchAndGuests(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("ln-1", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.11.0")
	link := r.mustLink(owner, h.id, map[string]any{"maxUses": 10})

	res := r.do("GET", linkPathOf(link.id), nil, nil)
	r.expect(res, 200, "")
	if res.json["requiresAccount"] != true {
		t.Fatalf("团队主机缺省要求登录：%s", res.body)
	}
	// 团队主机的链接给团队名，不给登记主机的成员的显示名。
	var teamName string
	if err := r.run.DB.Pool().QueryRow(ctxBackground, "SELECT name FROM teams WHERE team_id = $1", team).Scan(&teamName); err != nil {
		t.Fatal(err)
	}
	r.exec("UPDATE accounts SET display_name = '登记者甲' WHERE account_id = $1", owner.accountID)
	if res = r.do("GET", linkPathOf(link.id), nil, nil); res.json["ownerName"] != teamName || strings.Contains(string(res.body), "登记者甲") {
		t.Fatalf("团队链接应给团队名 %q：%s", teamName, res.body)
	}
	r.expect(r.acceptLink(link, nil), 403, "anonymous_links_disabled")
	if uses, _ := r.linkRow(link.id); uses != 0 {
		t.Fatalf("被拒的匿名接受不消耗次数：%d", uses)
	}
	res = r.acceptLink(link, &member)
	r.expect(res, 200, "")
	guest := r.guestOfAccept(res)
	if payloadOf(t, res.json["assertion"].(string))["team"] != nil {
		t.Fatal("链接访客的断言不带团队")
	}
	if uses, _ := r.linkRow(link.id); uses != 1 {
		t.Fatalf("用量：%d", uses)
	}
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 200, "")

	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowAnonymousLinks": true}, owner.bearer()), 200, "")
	if r.do("GET", linkPathOf(link.id), nil, nil).json["requiresAccount"] != false {
		t.Fatal("打开匿名链接后不再要求登录")
	}
	r.expect(r.acceptLink(link, nil), 200, "")

	// 团队停用：访客进不来（接受与再要断言）。
	r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
	r.expect(r.acceptLink(link, nil), 403, "team_suspended")
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 403, "team_suspended")
	r.exec("UPDATE teams SET status = 'active' WHERE team_id = $1", team)
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 200, "")

	// 团队关了 guest：签链接 guests_disabled，已有链接接受时当作失效。
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
	r.expect(r.createLink(owner, h.id, nil), 403, "guests_disabled")
	r.expect(r.acceptLink(link, nil), 404, "link_invalid")
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": true}, owner.bearer()), 200, "")
	// 主机设置关了 guest：同样。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"allowGuests": false}}, owner.bearer()), 200, "")
	r.expect(r.createLink(owner, h.id, nil), 403, "guests_disabled")
	r.expect(r.acceptLink(link, nil), 404, "link_invalid")
	// 团队进入恢复期：落地页与接受都失效。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"allowGuests": true}}, owner.bearer()), 200, "")
	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", team)
	r.expect(r.do("GET", linkPathOf(link.id), nil, nil), 404, "link_invalid")
	r.expect(r.acceptLink(link, nil), 404, "link_invalid")
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 403, "source_access_denied")
}

// 谁能签链接（主机设置 linkCreators）与管理权：owner_admin 缺省（团队 admin 可以、member 只管自己的）；
// owner 只许主机所有者（team_policy_denied）；driver 档加有效角色为 driver 的成员（限在工作空间的只能签那些工作空间）；
// 跨团队不可见；列表按 space 过滤。
func TestLinksCreatorsManageAndSpaces(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, driver, scoped, viewer, guest, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("ln-2", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", driver.accountID: "member",
		scoped.accountID: "member", viewer.accountID: "member", guest.accountID: "guest",
	})
	other := r.seedTeam("ln-3", map[string]string{outsider.accountID: "owner"})
	h := r.addHost(owner, teamSpace(team), "0.11.0")
	hMember := r.addHost(member, teamSpace(team), "0.11.0")
	hOther := r.addHost(outsider, teamSpace(other), "0.11.0")
	personal := r.addHost(owner, nil, "0.11.0")
	r.mustGrant(owner, h.id, memberOf(driver.accountID), "driver", nil)
	r.mustGrant(owner, h.id, memberOf(scoped.accountID), "driver", map[string]any{"workspaceIds": []string{"w1"}})
	r.mustGrant(owner, h.id, memberOf(viewer.accountID), "viewer", nil)
	r.mustGrant(owner, h.id, guestOf(guest.accountID), "driver", map[string]any{"workspaceIds": []string{"w1"}})
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{allowGuests}', 'true') WHERE team_id = $1", team)

	// 缺省 owner_admin：admin 可以；member 对别人的主机看不到（404），对自己的可以；driver 角色的成员不行（403）。
	adminLink := r.mustLink(admin, h.id, nil)
	r.mustLink(member, hMember.id, nil)
	r.expect(r.createLink(member, h.id, nil), 404, "not_found")
	r.expect(r.createLink(driver, h.id, nil), 403, "team_role_insufficient")
	r.expect(r.createLink(guest, h.id, nil), 403, "team_role_insufficient") // guest 角色的成员不签链接
	r.expect(r.createLink(outsider, h.id, nil), 404, "not_found")
	r.expect(r.createLink(owner, hOther.id, nil), 404, "not_found")

	// 管理：admin 能撤销、改备注；member 看不到别人的主机的链接；跨团队 404。
	r.expect(r.do("PATCH", linkPathOf(adminLink.id), map[string]any{"label": "x"}, outsider.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", linkPathOf(adminLink.id), nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", linkPathOf(adminLink.id), nil, member.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", linkPathOf(adminLink.id), nil, driver.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("PATCH", linkPathOf(adminLink.id), map[string]any{"label": "y"}, admin.bearer()), 200, "")

	// driver 档。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"linkCreators": "driver"}}, owner.bearer()), 200, "")
	driverLink := r.mustLink(driver, h.id, nil)
	r.expect(r.createLink(viewer, h.id, nil), 403, "team_role_insufficient")
	r.expect(r.createLink(scoped, h.id, nil), 403, "team_role_insufficient")                                                          // 限在工作空间的 driver 不能签整台
	r.expect(r.createLink(scoped, h.id, map[string]any{"scope": map[string]any{"workspaceId": "w2"}}), 403, "team_role_insufficient") // 也不能签别的工作空间
	r.mustLink(scoped, h.id, map[string]any{"scope": map[string]any{"workspaceId": "w1"}})
	// driver@w1 加团队 viewer 整台：合成出来是 driver + 整台，但没有来路真给了整台 driver，整台与 w2 的链接都不能签。
	r.mustGrant(owner, h.id, teamOf(team), "viewer", nil)
	r.expect(r.createLink(scoped, h.id, nil), 403, "team_role_insufficient")
	r.expect(r.createLink(scoped, h.id, map[string]any{"scope": map[string]any{"workspaceId": "w2"}}), 403, "team_role_insufficient")
	r.mustLink(scoped, h.id, map[string]any{"scope": map[string]any{"workspaceId": "w1"}})
	// 链接的到期不能超过 driver 来路的授予到期。
	capped := r.verified()
	r.exec("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'member', 99)", team, capped.accountID)
	r.mustGrant(owner, h.id, memberOf(capped.accountID), "driver", map[string]any{"expiresAtMs": r.clock.Ms() + 2*3600*1000})
	r.expect(r.createLink(capped, h.id, nil), 400, "bad_request")
	r.mustLink(capped, h.id, map[string]any{"expiresAtMs": r.clock.Ms() + 3600*1000})
	// 签链接的人自己没有管理权：不能撤销（要主机管理者来）。
	r.expect(r.do("DELETE", linkPathOf(driverLink.id), nil, driver.bearer()), 403, "team_role_insufficient")
	// owner 档：主机所有者可以，团队 admin 看得到也管得了，但设置不许（team_policy_denied）。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"linkCreators": "owner"}}, owner.bearer()), 200, "")
	r.mustLink(owner, h.id, nil)
	res := r.createLink(admin, h.id, nil)
	r.expect(res, 403, "team_policy_denied")
	if d, _ := res.json["details"].(map[string]any); d["setting"] != "linkCreators" {
		t.Fatalf("details：%s", res.body)
	}
	r.expect(r.createLink(driver, h.id, nil), 403, "team_role_insufficient")

	// 列表与 space 过滤。
	lnPersonal := r.mustLink(owner, personal.id, nil)
	ids := func(res response) map[string]bool {
		r.expect(res, 200, "")
		out := map[string]bool{}
		for _, item := range res.json["links"].([]any) {
			out[item.(map[string]any)["linkId"].(string)] = true
		}
		return out
	}
	all := ids(r.do("GET", "/v1/links", nil, owner.bearer()))
	if !all[lnPersonal.id] || !all[adminLink.id] || len(all) != 8 {
		t.Fatalf("owner 的全部链接（含 driver 签的）：%v", all)
	}
	if got := ids(r.do("GET", "/v1/links?space[kind]=personal", nil, owner.bearer())); !got[lnPersonal.id] || len(got) != 1 {
		t.Fatalf("个人空间：%v", got)
	}
	if got := ids(r.do("GET", "/v1/links?space[kind]=team&space[teamId]="+team, nil, owner.bearer())); got[lnPersonal.id] || len(got) != 7 {
		t.Fatalf("团队空间：%v", got)
	}
	if got := ids(r.do("GET", "/v1/links?space[kind]=team&space[teamId]="+team, nil, member.bearer())); len(got) != 1 {
		t.Fatalf("member 只看得到自己管的主机的链接：%v", got)
	}
	r.expect(r.do("GET", "/v1/links?space[kind]=team&space[teamId]="+other, nil, owner.bearer()), 404, "not_found")
	if got := ids(r.do("GET", "/v1/links", nil, outsider.bearer())); len(got) != 0 {
		t.Fatalf("别的团队什么都看不到：%v", got)
	}
	if got := ids(r.do("GET", "/v1/links", nil, driver.bearer())); len(got) != 0 {
		t.Fatalf("没有管理权的人列表为空：%v", got)
	}
	r.expect(r.do("GET", "/v1/links?sourceId="+h.id, nil, driver.bearer()), 403, "team_role_insufficient")
}

// 访客断言的签→索引→复核：在「令牌已签」与「索引已登记」两点停住，撤销链接，之后答错误且自己签的令牌都已撤销。
func TestLinkGuestAssertionRacesRevoke(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"signed", "indexed"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			g := newGate(stage)
			r := newRig(t, rigOptions{grantsHook: g.hook})
			owner := r.verified()
			h := r.addHost(owner, nil, "0.11.0")
			link := r.mustLink(owner, h.id, nil)
			// 接受时 hook 也会触发一次（链接路径同样停在这两点）：用 once，先让接受那次过去。
			accepted := make(chan response, 1)
			go func() { accepted <- r.acceptLink(link, nil) }()
			<-g.reached
			r.expect(r.do("DELETE", linkPathOf(link.id), nil, owner.bearer()), 200, "")
			close(g.resume)
			res := <-accepted
			if res.status != 404 || res.json["code"] != "link_invalid" {
				t.Fatalf("撤销与接受交错：%d %s", res.status, res.body)
			}
			for _, jti := range r.revokedJTIs() {
				if !r.run.Relay.IsRevoked(jti) {
					t.Fatalf("内存撤销表漏了 %s", jti)
				}
			}
			if len(r.revokedJTIs()) != 2 {
				t.Fatalf("接受时签的两枚令牌都该撤：%v", r.revokedJTIs())
			}
			if n := r.queryInt("SELECT count(*)::int FROM devices WHERE subject LIKE $1 AND revoked_at_ms IS NULL", "guest:"+link.id+":%"); n != 0 {
				t.Fatalf("没有留下活着的访客设备：%d", n)
			}
			if n := r.guestIssued(link.id); n != 0 {
				t.Fatalf("索引里还有 %d 枚", n)
			}
		})
	}
}

// 访客断言的复核看 allowGuests：停在「索引已登记、复核之前」时关掉主机（或团队）的 allowGuests，答 403
// source_access_denied，这次自己签的两枚令牌都已撤（索引里只剩接受时签的两枚）；关 guest 不撤接受时的令牌。
func TestLinkGuestAssertionRacesGuestsOff(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"host", "team"} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()
			g := newGate("indexed")
			// 接受也会走到 indexed：接受完再上闸，只停访客自己要的那次断言。
			var armed atomic.Bool
			r := newRig(t, rigOptions{grantsHook: func(stage string) {
				if armed.Load() {
					g.hook(stage)
				}
			}})
			owner := r.verified()
			team := r.seedTeam("ga-off-"+scope, map[string]string{owner.accountID: "owner"})
			r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowAnonymousLinks": true}, owner.bearer()), 200, "")
			h := r.addHost(owner, teamSpace(team), "0.11.0")
			link := r.mustLink(owner, h.id, nil)
			res := r.acceptLink(link, nil)
			r.expect(res, 200, "")
			guest := r.guestOfAccept(res)
			if n := r.guestIssued(link.id); n != 2 {
				t.Fatalf("接受时签的两枚应在索引里：%d", n)
			}

			armed.Store(true)
			result := make(chan response, 1)
			go func() { result <- r.assertion(guest, h.id) }()
			select {
			case <-g.reached:
			case res := <-result:
				t.Fatalf("断言没停在 indexed：%d %s", res.status, res.body)
			case <-time.After(30 * time.Second):
				t.Fatal("断言没走到 indexed")
			}
			if scope == "host" {
				r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"allowGuests": false}}, owner.bearer()), 200, "")
			} else {
				r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
			}
			close(g.resume)
			r.expect(<-result, 403, "source_access_denied")

			jtis := r.revokedJTIs()
			if len(jtis) != 2 {
				t.Fatalf("这次签出的两枚令牌都该撤：%v", jtis)
			}
			for _, jti := range jtis {
				if !r.run.Relay.IsRevoked(jti) {
					t.Fatalf("内存撤销表漏了 %s", jti)
				}
			}
			if n := r.guestIssued(link.id); n != 2 {
				t.Fatalf("索引里只该剩接受时签的两枚：%d", n)
			}
		})
	}
}

// 读主人账号出错时落地页照常，只是不带 ownerName（不答 500）；账号停用同样不带。
func TestLinkGetOwnerNameOmittedOnFailure(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	link := r.mustLink(owner, h.id, nil)
	r.exec("UPDATE accounts SET display_name = '爱丽丝' WHERE account_id = $1", owner.accountID)
	if res := r.do("GET", linkPathOf(link.id), nil, nil); res.json["ownerName"] != "爱丽丝" {
		t.Fatalf("个人空间给主人显示名：%s", res.body)
	}
	r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", owner.accountID)
	res := r.do("GET", linkPathOf(link.id), nil, nil)
	r.expect(res, 200, "")
	if res.json["ownerName"] != nil {
		t.Fatalf("停用账号不带 ownerName：%s", res.body)
	}
	r.exec("UPDATE accounts SET status = 'active' WHERE account_id = $1", owner.accountID)
	r.exec("ALTER TABLE accounts RENAME TO accounts_gone")
	res = r.do("GET", linkPathOf(link.id), nil, nil)
	r.exec("ALTER TABLE accounts_gone RENAME TO accounts")
	r.expect(res, 200, "")
	if res.json["label"] == nil || res.json["ownerName"] != nil {
		t.Fatalf("读取出错时照常答、不带 ownerName：%s", res.body)
	}
}
