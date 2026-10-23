package server_test

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
)

// inviteLink 取邀请邮件的邀请 id 与令牌：链接必须是 <来源>/invite/<id>#<令牌>。
func inviteLink(t *testing.T, m mail.Message) (id, token string) {
	t.Helper()
	link := linkPattern.FindString(m.Text)
	u, err := url.Parse(link)
	if err != nil || u.Fragment == "" || u.RawQuery != "" || !strings.HasPrefix(u.Path, "/invite/") || u.Scheme+"://"+u.Host != appOrigin {
		t.Fatalf("链接要是 %s/invite/<id>#令牌：%q", appOrigin, link)
	}
	return strings.TrimPrefix(u.Path, "/invite/"), u.Fragment
}

// waitInvite 等到发给 email 的、指向这条邀请的邮件（令牌不是 not 的那封），答令牌。
// 注册验证邮件也可能发给同一个地址，所以按链接路径挑。
func (r *rig) waitInvite(email, id string, not ...string) (token string) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range r.mail.Messages() {
			if m.To != email {
				continue
			}
			u, err := url.Parse(linkPattern.FindString(m.Text))
			if err != nil || u.Path != "/invite/"+id {
				continue
			}
			fresh := true
			for _, old := range not {
				fresh = fresh && u.Fragment != old
			}
			if fresh {
				return u.Fragment
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("等不到发给 %s 的邀请 %s 的邮件", email, id)
	return ""
}

func invitePath(id string, rest ...string) string {
	path := "/v1/invitations/" + id
	for _, part := range rest {
		path += "/" + part
	}
	return path
}

func (r *rig) invite(s session, team, email, role string) response {
	r.t.Helper()
	return r.do("POST", "/v1/invitations", map[string]any{"kind": "team", "teamId": team, "email": email, "role": role}, s.bearer())
}

// verifiedAs 注册（可指定邮箱与语言）并验证邮箱。
func (r *rig) verifiedAs(email string, h headers) session {
	r.t.Helper()
	res := r.register(email, h)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", s.accountID)
	return s
}

// 邀请全流程：创建 → 邮件（令牌只在片段、库里只有哈希）→ 匿名摘要 → 接受 → 成员 → 幂等。
func TestInvitationFlow(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("flow", map[string]string{owner.accountID: "owner"})
	bobEmail := r.email()

	res := r.invite(owner, team, strings.ToUpper(bobEmail), "member")
	r.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	if res.json["state"] != "pending" || res.json["email"] != bobEmail || res.json["teamId"] != team || res.json["teamRole"] != "member" ||
		res.json["decidedAtMs"] != nil || res.json["expiresAtMs"] != float64(r.clock.Ms()+7*24*3600*1000) {
		t.Fatalf("创建的邀请：%s", res.body)
	}
	mails := r.waitMails(bobEmail, 1)
	linkID, token := inviteLink(t, mails[0])
	if linkID != id {
		t.Fatalf("链接里的邀请 id %s 不对", linkID)
	}
	// 没有账号的收件人用邀请人的语言（缺省中文）；团队名进主题。
	if !strings.Contains(mails[0].Subject, "团队 flow") || !strings.Contains(mails[0].Text, "成员") {
		t.Fatalf("邀请邮件：%q %q", mails[0].Subject, mails[0].Text)
	}
	r.noPlaintext(token)
	if r.queryString("SELECT token_hash FROM invitations WHERE invitation_id = $1", id) != hashOf(token) {
		t.Fatal("库里应只存令牌哈希")
	}

	// 匿名摘要：遮掩邮箱、不含全文；令牌或 id 不对一律同样的 404。
	res = r.do("GET", invitePath(id)+"?token="+url.QueryEscape(token), nil, nil)
	r.expect(res, 200, "")
	local := strings.SplitN(bobEmail, "@", 2)
	want := map[string]any{
		"kind": "team", "state": "pending", "teamName": "团队 flow", "teamRole": "member", "inviterName": "测试用户",
		"emailHint": local[0][:1] + "***@" + local[1], "expiresAtMs": res.json["expiresAtMs"],
	}
	if !reflect.DeepEqual(res.json, want) {
		t.Fatalf("摘要：%s", res.body)
	}
	bad := r.do("GET", invitePath(id)+"?token=wrong", nil, nil)
	missing := r.do("GET", invitePath("nope")+"?token="+url.QueryEscape(token), nil, nil)
	r.expect(bad, 404, "not_found")
	r.expect(missing, 404, "not_found")
	delete(bad.json, "requestId")
	delete(missing.json, "requestId")
	if !reflect.DeepEqual(bad.json, missing.json) {
		t.Fatalf("不存在与令牌不对应答同样的形状：%v / %v", bad.json, missing.json)
	}

	// Bob 注册（邮箱已验证）后接受：成员、审计、收件箱与已发列表。
	bob := r.verifiedAs(bobEmail, nil)
	res = r.do("GET", "/v1/invitations?box=received", nil, bob.bearer())
	r.expect(res, 200, "")
	if list := res.json["invitations"].([]any); len(list) != 1 || list[0].(map[string]any)["invitationId"] != id {
		t.Fatalf("收件箱：%s", res.body)
	}
	res = r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, bob.bearer())
	r.expect(res, 200, "")
	if res.json["teamId"] != team {
		t.Fatalf("接受的答复：%s", res.body)
	}
	if got := r.queryString("SELECT role FROM team_members WHERE team_id = $1 AND account_id = $2", team, bob.accountID); got != "member" {
		t.Fatalf("Bob 的角色 %s", got)
	}
	if got := r.queryString("SELECT invited_by FROM team_members WHERE team_id = $1 AND account_id = $2", team, bob.accountID); got != owner.accountID {
		t.Fatalf("invited_by %s", got)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE team_id = $1 AND action IN ('invitation.create', 'invitation.accept')", team); n != 2 {
		t.Fatalf("审计：%d", n)
	}
	// 幂等：再接受答原结果；摘要报 accepted；收件箱空了；已发列表里是 accepted。
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, bob.bearer()), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'invitation.accept'"); n != 1 {
		t.Fatal("重复接受不该再写审计")
	}
	res = r.do("GET", invitePath(id)+"?token="+url.QueryEscape(token), nil, nil)
	if res.json["state"] != "accepted" {
		t.Fatalf("摘要的状态：%s", res.body)
	}
	if list := r.do("GET", "/v1/invitations?box=received", nil, bob.bearer()).json["invitations"].([]any); len(list) != 0 {
		t.Fatal("收件箱应空")
	}
	res = r.do("GET", "/v1/invitations?teamId="+team, nil, owner.bearer())
	if list := res.json["invitations"].([]any); len(list) != 1 || list[0].(map[string]any)["state"] != "accepted" || list[0].(map[string]any)["decidedAtMs"] == nil {
		t.Fatalf("已发列表：%s", res.body)
	}
	// 接受后再邀请同一个人：已是成员。
	r.expect(r.invite(owner, team, bobEmail, "admin"), 409, "member_exists")
	r.noPlaintext(token)
}

func TestInvitationMailFollowsInviteeLocale(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("lang", map[string]string{owner.accountID: "owner"})
	email := r.email()
	r.verifiedAs(email, hdr("Accept-Language", "en-US,en;q=0.9")) // 收件人有账号，用他的语言
	r.expect(r.invite(owner, team, email, "guest"), 200, "")
	mails := r.waitMails(email, 1)
	// 注册时还会发验证邮件：取邀请那封。
	var invite mail.Message
	for _, m := range mails {
		if strings.Contains(m.Subject, "Invitation") {
			invite = m
		}
	}
	if invite.To == "" || !strings.Contains(invite.Subject, `"团队 lang"`) || !strings.Contains(invite.Text, "as a guest") {
		for _, m := range r.mail.Messages() {
			t.Logf("%s | %s", m.To, m.Subject)
		}
		t.Fatalf("应按收件账号的语言发英文：%+v", invite)
	}
}

func TestInvitationEmailMismatchDeclineAndRevoke(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, other := r.verified(), r.verified()
	team := r.seedTeam("neg", map[string]string{owner.accountID: "owner"})
	dave := r.verifiedAs(r.email(), nil)
	daveEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", dave.accountID)
	res := r.invite(owner, team, daveEmail, "member")
	r.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	token := r.waitInvite(daveEmail, id)

	// 别的账号拿着链接：邮箱对不上，接受与拒绝都 403，邀请不受影响。
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, other.bearer()), 403, "invitation_email_mismatch")
	r.expect(r.do("POST", invitePath(id, "decline"), map[string]any{"token": token}, other.bearer()), 403, "invitation_email_mismatch")
	// 令牌不对：404，不泄露邀请存在。
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": "wrong"}, dave.bearer()), 404, "not_found")
	// 未验证邮箱不能接受。
	r.exec("UPDATE accounts SET email_verified_at_ms = NULL WHERE account_id = $1", dave.accountID)
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, dave.bearer()), 403, "email_unverified")
	r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", dave.accountID)

	// 拒绝：成功、幂等；之后接受 / 重发 409。
	r.expect(r.do("POST", invitePath(id, "decline"), map[string]any{"token": token}, dave.bearer()), 200, "")
	r.expect(r.do("POST", invitePath(id, "decline"), map[string]any{"token": token}, dave.bearer()), 200, "")
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, dave.bearer()), 409, "conflict")
	r.expect(r.do("POST", invitePath(id, "resend"), nil, owner.bearer()), 409, "conflict")
	r.expect(r.do("DELETE", invitePath(id), nil, owner.bearer()), 409, "conflict")
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1", team); n != 1 {
		t.Fatal("没人加入")
	}

	// 撤销：之后摘要 / 接受 / 拒绝 / 重发都 410 invitation_revoked；再撤销是空操作。
	res = r.invite(owner, team, daveEmail, "member")
	r.expect(res, 200, "")
	id2 := res.json["invitationId"].(string)
	token2 := r.waitInvite(daveEmail, id2)
	r.expect(r.do("DELETE", invitePath(id2), nil, owner.bearer()), 200, "")
	r.expect(r.do("DELETE", invitePath(id2), nil, owner.bearer()), 200, "")
	r.expect(r.do("GET", invitePath(id2)+"?token="+url.QueryEscape(token2), nil, nil), 410, "invitation_revoked")
	r.expect(r.do("POST", invitePath(id2, "accept"), map[string]any{"token": token2}, dave.bearer()), 410, "invitation_revoked")
	r.expect(r.do("POST", invitePath(id2, "decline"), map[string]any{"token": token2}, dave.bearer()), 410, "invitation_revoked")
	r.expect(r.do("POST", invitePath(id2, "resend"), nil, owner.bearer()), 410, "invitation_revoked")
	if got := r.queryString("SELECT state FROM invitations WHERE invitation_id = $1", id2); got != "revoked" {
		t.Fatalf("状态 %s", got)
	}
}

// 过期：读时按过期答 410；重新邀请前先把过期的 pending 落成 expired，不挡新邀请；过期的可重发，旧链接作废。
func TestInvitationExpiryReinviteAndResend(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	ownerEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", owner.accountID)
	team := r.seedTeam("exp", map[string]string{owner.accountID: "owner"})
	email := r.email()
	res := r.invite(owner, team, email, "member")
	r.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	token := r.waitInvite(email, id)
	// 还没过期：同邮箱再邀请是 conflict。
	r.expect(r.invite(owner, team, email, "member"), 409, "conflict")

	r.clock.Advance(7*24*time.Hour + time.Minute)
	r.expect(r.do("GET", invitePath(id)+"?token="+url.QueryEscape(token), nil, nil), 410, "invitation_expired")
	owner2 := parseSession(r, r.login(ownerEmail, password, nil))
	invitee := r.verifiedAs(email, nil)
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, invitee.bearer()), 410, "invitation_expired")
	res = r.do("GET", "/v1/invitations?teamId="+team, nil, owner2.bearer())
	if list := res.json["invitations"].([]any); len(list) != 1 || list[0].(map[string]any)["state"] != "expired" {
		t.Fatalf("过期的邀请列为 expired：%s", res.body)
	}
	// 每小时的作业把它落库成 expired。
	eventuallyDB(t, "invitations.expire 落库", func() bool {
		return r.queryString("SELECT state FROM invitations WHERE invitation_id = $1", id) == "expired"
	})

	// 重发：回到待处理、新 7 天、旧链接作废，新邮件带新令牌。
	res = r.do("POST", invitePath(id, "resend"), nil, owner2.bearer())
	r.expect(res, 200, "")
	if res.json["state"] != "pending" || res.json["expiresAtMs"] != float64(r.clock.Ms()+7*24*3600*1000) {
		t.Fatalf("重发的答复：%s", res.body)
	}
	newToken := r.waitInvite(email, id, token)
	r.expect(r.do("GET", invitePath(id)+"?token="+url.QueryEscape(token), nil, nil), 404, "not_found")
	r.expect(r.do("GET", invitePath(id)+"?token="+url.QueryEscape(newToken), nil, nil), 200, "")
	r.expect(r.do("POST", invitePath(id, "accept"), map[string]any{"token": newToken}, invitee.bearer()), 200, "")

	// 另一个邮箱：过期未处理的 pending 不挡重新邀请。
	other := r.email()
	r.expect(r.invite(owner2, team, other, "guest"), 200, "")
	r.waitMails(other, 1)
	r.clock.Advance(8 * 24 * time.Hour)
	owner3 := parseSession(r, r.login(ownerEmail, password, nil))
	r.expect(r.invite(owner3, team, other, "guest"), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM invitations WHERE email = $1 AND state = 'expired'", other); n != 1 {
		t.Fatalf("旧邀请应落成 expired：%d", n)
	}
	if n := r.queryInt("SELECT count(*)::int FROM invitations WHERE email = $1 AND state = 'pending'", other); n != 1 {
		t.Fatalf("应只有一条 pending：%d", n)
	}
}

// 角色、跨团队与额度。
func TestInvitationPermissionsAndQuotas(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, admin, member, billing, guest, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("perm", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", billing.accountID: "billing", guest.accountID: "guest",
	})
	other := r.seedTeam("perm-other", map[string]string{outsider.accountID: "owner"})
	// Free 成员额度 3：owner、admin、member、billing 已占满（billing 占席位，guest 不占）。先把 billing / guest 的角色门槛测完。
	for name, s := range map[string]session{"member": member, "billing": billing, "guest": guest} {
		r.expect(r.invite(s, team, r.email(), "guest"), 403, "team_role_insufficient")
		_ = name
	}
	r.expect(r.invite(outsider, team, r.email(), "member"), 404, "not_found")
	r.expect(r.do("GET", "/v1/invitations?teamId="+team, nil, member.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("GET", "/v1/invitations?teamId="+team, nil, outsider.bearer()), 404, "not_found")
	if list := r.do("GET", "/v1/invitations", nil, member.bearer()).json["invitations"].([]any); len(list) != 0 {
		t.Fatal("没有管理权的账号，已发列表为空")
	}

	// 成员额度：现有 4 个非 guest（已超 Free 的 3），再邀请成员越限；details 写清用量。
	res := r.invite(admin, team, r.email(), "member")
	r.expect(res, 429, "limit_reached")
	if d := res.json["details"].(map[string]any); d["quota"] != "members" || d["used"] != float64(4) || d["limit"] != float64(3) {
		t.Fatalf("details：%s", res.body)
	}
	// guest 额度 3：现有 1 个 guest，再邀请 2 个，第 4 个越限（待处理的也计入）。
	var ids []string
	for i := 0; i < 2; i++ {
		res = r.invite(admin, team, r.email(), "guest")
		r.expect(res, 200, "")
		ids = append(ids, res.json["invitationId"].(string))
	}
	res = r.invite(admin, team, r.email(), "guest")
	r.expect(res, 429, "limit_reached")
	if d := res.json["details"].(map[string]any); d["quota"] != "guests" || d["used"] != float64(3) {
		t.Fatalf("details：%s", res.body)
	}
	// 撤销一条后空出一个。
	r.expect(r.do("DELETE", invitePath(ids[0]), nil, admin.bearer()), 200, "")
	r.expect(r.invite(admin, team, r.email(), "guest"), 200, "")
	// 跨团队：别的团队的 owner 动不了这些邀请（404，不泄露存在）。
	r.expect(r.do("DELETE", invitePath(ids[1]), nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("POST", invitePath(ids[1], "resend"), nil, outsider.bearer()), 404, "not_found")
	// 成员 / billing 同样不能撤销、重发。
	r.expect(r.do("DELETE", invitePath(ids[1]), nil, member.bearer()), 403, "team_role_insufficient")
	r.expect(r.do("POST", invitePath(ids[1], "resend"), nil, billing.bearer()), 403, "team_role_insufficient")
	// guests 被团队关掉：邀请 guest 答 guests_disabled；已是成员 member_exists。
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
	r.expect(r.invite(admin, team, r.email(), "guest"), 403, "guests_disabled")
	memberEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", member.accountID)
	r.expect(r.invite(admin, team, memberEmail, "admin"), 409, "member_exists")
	_ = other

	// 未验证邮箱不能邀请。
	r.exec("UPDATE accounts SET email_verified_at_ms = NULL WHERE account_id = $1", admin.accountID)
	r.expect(r.invite(admin, team, r.email(), "admin"), 403, "email_unverified")
	// 恢复期内的团队：410。
	r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", admin.accountID)
	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", other)
	r.expect(r.invite(outsider, other, r.email(), "member"), 410, "team_deleted")
	// 主机邀请随 S6：501。
	r.expect(r.do("POST", "/v1/invitations", map[string]any{"kind": "host", "sourceId": strings.Repeat("a", 32), "email": r.email(), "role": "viewer"}, admin.bearer()), 501, "not_implemented")
}

// 接受时再核额度：待处理的邀请占着席位，自己那条不重复计。
func TestInvitationAcceptChecksQuotaAndTeamState(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("acc", map[string]string{owner.accountID: "owner"})
	var invitees []session
	var tokens []string
	var ids []string
	for i := 0; i < 2; i++ {
		email := r.email()
		invitees = append(invitees, r.verifiedAs(email, nil))
		res := r.invite(owner, team, email, "member")
		r.expect(res, 200, "")
		ids = append(ids, res.json["invitationId"].(string))
		tokens = append(tokens, r.waitInvite(email, ids[i]))
	}
	// owner + 2 个待处理 = 3，已满；又有人直接进了团队（比如另一条路径）：接受第一条应越限。
	filler := r.verified()
	r.exec("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'member', 9)", team, filler.accountID)
	res := r.do("POST", invitePath(ids[0], "accept"), map[string]any{"token": tokens[0]}, invitees[0].bearer())
	r.expect(res, 429, "limit_reached")
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1", team); n != 2 {
		t.Fatal("越限不该加成员")
	}
	// 腾出一个：撤销另一条待处理的邀请后，接受成功（自己那条不重复计）。
	r.expect(r.do("DELETE", invitePath(ids[1]), nil, owner.bearer()), 200, "")
	r.expect(r.do("POST", invitePath(ids[0], "accept"), map[string]any{"token": tokens[0]}, invitees[0].bearer()), 200, "")

	// 接受时团队已关 guest：team_policy_denied。
	email := r.email()
	guest := r.verifiedAs(email, nil)
	res = r.invite(owner, team, email, "guest")
	r.expect(res, 200, "")
	gid := res.json["invitationId"].(string)
	gtoken := r.waitInvite(email, gid)
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
	res = r.do("POST", invitePath(gid, "accept"), map[string]any{"token": gtoken}, guest.bearer())
	r.expect(res, 403, "team_policy_denied")
	if res.json["details"].(map[string]any)["setting"] != "allowGuests" {
		t.Fatalf("details：%s", res.body)
	}
	// 团队进恢复期：接受 410 team_deleted；摘要 404（团队已不可见）。
	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", team)
	r.expect(r.do("POST", invitePath(gid, "accept"), map[string]any{"token": gtoken}, guest.bearer()), 410, "team_deleted")
	r.expect(r.do("GET", invitePath(gid)+"?token="+url.QueryEscape(gtoken), nil, nil), 404, "not_found")
}

// 限流：创建按邀请人每小时 50 次，重发按邀请每小时 5 次，匿名摘要按 IP 每分钟 30 次。
func TestInvitationRateLimits(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("rate", map[string]string{owner.accountID: "owner"})
	var last response
	for i := 0; i < 51; i++ {
		last = r.invite(owner, team, fmt.Sprintf("rate%d-%s", i, r.email()), "member")
	}
	r.expect(last, 429, "rate_limited")
	if _, ok := last.json["details"].(map[string]any)["retryAfterMs"]; !ok {
		t.Fatalf("rate_limited 应带 retryAfterMs：%s", last.body)
	}

	r2 := newRig(t, rigOptions{})
	o2 := r2.verified()
	t2 := r2.seedTeam("rate2", map[string]string{o2.accountID: "owner"})
	res := r2.invite(o2, t2, r2.email(), "member")
	r2.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	for i := 0; i < 5; i++ {
		r2.expect(r2.do("POST", invitePath(id, "resend"), nil, o2.bearer()), 200, "")
	}
	r2.expect(r2.do("POST", invitePath(id, "resend"), nil, o2.bearer()), 429, "rate_limited")
	for i := 0; i < 30; i++ {
		r2.expect(r2.do("GET", invitePath(id)+"?token=x", nil, nil), 404, "not_found")
	}
	r2.expect(r2.do("GET", invitePath(id)+"?token=x", nil, nil), 429, "rate_limited")
}

// 发信失败重试换新令牌；邀请在发信前被撤销则不发信。
func TestInvitationMailSkipsRevokedAndRetries(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("mailskip", map[string]string{owner.accountID: "owner"})
	email := r.email()
	r.mail.FailNext(1)
	res := r.invite(owner, team, email, "member")
	r.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	eventuallyDB(t, "第一次发信失败", func() bool { return len(r.mail.Attempts()) == 1 })
	r.clock.Advance(31 * time.Second) // 退避 30 秒
	token := r.waitInvite(email, id)
	if got := len(r.mail.Attempts()); got < 2 {
		t.Fatalf("应有重试：%d", got)
	}
	if r.queryString("SELECT token_hash FROM invitations WHERE invitation_id = $1", id) != hashOf(token) {
		t.Fatal("库里应只认最后一次发出的令牌")
	}

	// 发信前就被撤销：直接插一条已撤销的邀请和它的发件，作业发现不是待处理，丢弃发件。
	r.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, team_id, team_role, state, invited_by, created_at_ms, expires_at_ms)
	  VALUES ('cloud', 'revoked-inv', 'team', 'ghost@example.com', $1, 'member', 'revoked', $2, 1, $3)`, team, owner.accountID, r.clock.Ms()+1000000)
	r.exec(`WITH j AS (INSERT INTO jobs (tenant_id, kind, payload, run_at_ms, created_at_ms)
	    VALUES ('cloud', 'mail.send', '{"emailId":"e-ghost","invitationId":"revoked-inv"}', $1, $1) RETURNING job_id)
	  INSERT INTO outbox_emails (tenant_id, email_id, job_id, template, locale, to_email, params, message_id, created_at_ms)
	  SELECT 'cloud', 'e-ghost', job_id, 'team_invitation', 'zh-CN', 'ghost@example.com', '{"teamName":"x"}', 'e-ghost@x', $1 FROM j`, r.clock.Ms())
	eventuallyDB(t, "撤销的邀请的发件被丢弃", func() bool {
		return r.queryInt("SELECT count(*)::int FROM outbox_emails WHERE email_id = 'e-ghost'") == 0
	})
	r.noMail("ghost@example.com")
}

// 并发创建 / 接受不越过成员额度（锁团队行后计数）：剩一个席位，四个并发邀请恰好一个成功。
func TestInvitationConcurrentCreateRespectsQuota(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, member := r.verified(), r.verified()
	team := r.seedTeam("race", map[string]string{owner.accountID: "owner", member.accountID: "member"})
	statuses := make([]int, 4)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() { defer wg.Done(); statuses[i] = r.invite(owner, team, r.email(), "member").status }()
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

	// 两个受邀者同时接受：待处理的邀请已占着席位，各自不重复计，Free 额度 3 刚好够 owner + 两个。
	r2 := newRig(t, rigOptions{})
	o2 := r2.verified()
	t2 := r2.seedTeam("race2", map[string]string{o2.accountID: "owner"})
	type invitee struct {
		s     session
		id    string
		token string
	}
	var invitees []invitee
	for i := 0; i < 2; i++ {
		email := r2.email()
		s := r2.verifiedAs(email, nil)
		res := r2.invite(o2, t2, email, "member")
		r2.expect(res, 200, "")
		id := res.json["invitationId"].(string)
		invitees = append(invitees, invitee{s, id, r2.waitInvite(email, id)})
	}
	got := make([]int, 2)
	for i := range invitees {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = r2.do("POST", invitePath(invitees[i].id, "accept"), map[string]any{"token": invitees[i].token}, invitees[i].s.bearer()).status
		}()
	}
	wg.Wait()
	if got[0] != 200 || got[1] != 200 {
		t.Fatalf("额度刚好够两个：%v", got)
	}
	if n := r2.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1", t2); n != 3 {
		t.Fatalf("成员数 %d", n)
	}
}
