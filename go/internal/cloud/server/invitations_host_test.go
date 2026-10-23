package server_test

import (
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 辅助 ----

func (r *rig) inviteHost(s session, sourceID, email, role string, extra map[string]any) response {
	r.t.Helper()
	body := map[string]any{"kind": "host", "sourceId": sourceID, "email": email, "role": role}
	for k, v := range extra {
		body[k] = v
	}
	return r.do("POST", "/v1/invitations", body, s.bearer())
}

// mustInviteHost 邀请并等到邮件，答（邀请 id，令牌）。
func (r *rig) mustInviteHost(s session, sourceID, email, role string, extra map[string]any) (id, token string) {
	r.t.Helper()
	res := r.inviteHost(s, sourceID, email, role, extra)
	r.expect(res, 200, "")
	id = res.json["invitationId"].(string)
	return id, r.waitInvite(email, id)
}

func (r *rig) acceptInvite(s session, id, token string) response {
	r.t.Helper()
	return r.do("POST", invitePath(id, "accept"), map[string]any{"token": token}, s.bearer())
}

// grantsOn 是主机上没撤销的授予（主体种类、主体 id、角色、来路、范围、到期）。
func (r *rig) grantRows(sourceID string) [][]string {
	r.t.Helper()
	rows, err := r.run.DB.Pool().Query(ctxBackground,
		`SELECT subject_kind, subject_id, role, origin, array_to_string(workspace_ids, ','), COALESCE(expires_at_ms::text, ''), COALESCE(invitation_id, '')
		 FROM source_grants WHERE source_id = $1 AND revoked_at_ms IS NULL ORDER BY subject_kind, created_at_ms, grant_id`, sourceID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		row := make([]string, 7)
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3], &row[4], &row[5], &row[6]); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// relogin 在时钟拨快之后重新登录（访问令牌只活 15 分钟）。
func (r *rig) relogin(old session, email string) session {
	r.t.Helper()
	res := r.login(email, password, nil)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	if s.accountID != old.accountID {
		r.t.Fatal("重新登录到了别的账号")
	}
	return s
}

// ---- 个人空间 ----

// 个人空间主机邀请全流程：创建（邮件带主机名与角色，令牌在片段里）→ 匿名摘要 → 接受 → guest 授予
// （origin = invitation，授予到期取自邀请的 grant_expires_at_ms，不取邀请自身有效期）→ 幂等。
func TestHostInvitationPersonalFlow(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, stranger := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	friendEmail := r.email()

	// 校验：角色、范围项、到期、不存在 / 别人的主机。
	r.expect(r.inviteHost(owner, h.id, friendEmail, "root", nil), 400, "bad_request")
	r.expect(r.inviteHost(owner, h.id, friendEmail, "viewer", map[string]any{"workspaceIds": []string{"bad id"}}), 400, "bad_request")
	r.expect(r.inviteHost(owner, h.id, friendEmail, "viewer", map[string]any{"expiresAtMs": r.clock.Ms() - 1}), 400, "bad_request")
	r.expect(r.inviteHost(owner, h.id, friendEmail, "viewer", map[string]any{"expiresAtMs": r.clock.Ms() + 31*24*3600*1000}), 400, "bad_request")
	r.expect(r.inviteHost(stranger, h.id, friendEmail, "viewer", nil), 404, "not_found")
	r.expect(r.inviteHost(owner, h.id, r.queryString("SELECT email FROM accounts WHERE account_id = $1", owner.accountID), "viewer", nil), 409, "member_exists")

	grantExpiry := r.clock.Ms() + 2*24*3600*1000
	res := r.inviteHost(owner, h.id, strings.ToUpper(friendEmail), "editor", map[string]any{"workspaceIds": []string{"w2", "w1"}, "expiresAtMs": grantExpiry})
	r.expect(res, 200, "")
	id := res.json["invitationId"].(string)
	if res.json["kind"] != "host" || res.json["sourceId"] != h.id || res.json["hostRole"] != "editor" || res.json["state"] != "pending" ||
		!reflect.DeepEqual(res.json["workspaceIds"], []any{"w1", "w2"}) || res.json["email"] != friendEmail ||
		res.json["expiresAtMs"] != float64(r.clock.Ms()+7*24*3600*1000) || res.json["teamId"] != nil || res.json["teamRole"] != nil {
		t.Fatalf("创建的主机邀请：%s", res.body)
	}
	r.expect(r.inviteHost(owner, h.id, friendEmail, "viewer", nil), 409, "conflict")
	token := r.waitInvite(friendEmail, id)
	mails := r.waitMails(friendEmail, 1)
	if !strings.Contains(mails[0].Subject, "主机") || !strings.Contains(mails[0].Text, "编辑者") {
		t.Fatalf("邀请邮件：%q %q", mails[0].Subject, mails[0].Text)
	}
	r.noPlaintext(token)

	// 匿名摘要（get 与 preview 相同）：主机名与角色，没有团队名。
	pv := r.do("POST", invitePath(id, "preview"), map[string]any{"token": token}, nil)
	r.expect(pv, 200, "")
	local := strings.SplitN(friendEmail, "@", 2)
	want := map[string]any{
		"kind": "host", "state": "pending", "sourceName": "主机 " + h.id[:6], "hostRole": "editor", "inviterName": "测试用户",
		"emailHint": local[0][:1] + "***@" + local[1], "expiresAtMs": pv.json["expiresAtMs"],
	}
	if !reflect.DeepEqual(pv.json, want) {
		t.Fatalf("摘要：%s", pv.body)
	}
	r.expect(r.do("GET", invitePath(id)+"?token="+url.QueryEscape(token), nil, nil), 200, "")
	r.expect(r.do("POST", invitePath(id, "preview"), map[string]any{"token": "wrong"}, nil), 404, "not_found")

	// 邮箱不符 403；未验证邮箱的账号接受 403（个人空间的访客授予要求已验证）。
	other := r.verified()
	r.expect(r.acceptInvite(other, id, token), 403, "invitation_email_mismatch")
	friend := r.registerBearer(friendEmail)
	r.expect(r.acceptInvite(friend, id, token), 403, "email_unverified")
	r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", friend.accountID)

	// 接受：guest 授予，到期是邀请里的授予到期（不是邀请的 7 天）；之后访客看得到主机。
	res = r.acceptInvite(friend, id, token)
	r.expect(res, 200, "")
	if res.json["sourceId"] != h.id || res.json["teamId"] != nil {
		t.Fatalf("接受的答复：%s", res.body)
	}
	rows := r.grantRows(h.id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], []string{"guest", friend.accountID, "editor", "invitation", "w1,w2", strconv.FormatInt(grantExpiry, 10), id}) {
		t.Fatalf("授予：%v", rows)
	}
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'grant.expire' AND run_at_ms = $1", grantExpiry); n != 1 {
		t.Fatal("应排授予到期作业")
	}
	if got := ids(r.do("GET", "/v1/sources", nil, friend.bearer())); !reflect.DeepEqual(got, []string{h.id}) {
		t.Fatalf("访客的目录：%v", got)
	}
	// 幂等：再接受答原结果、不再建授予、不再写审计。
	r.expect(r.acceptInvite(friend, id, token), 200, "")
	if len(r.grantRows(h.id)) != 1 || r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'invitation.accept' AND source_id = $1", h.id) != 1 {
		t.Fatal("重复接受不该再建授予或写审计")
	}
	// 已接受后：摘要报 accepted，再邀请同一个人可以（授予会合并）。
	if r.do("POST", invitePath(id, "preview"), map[string]any{"token": token}, nil).json["state"] != "accepted" {
		t.Fatal("摘要应报 accepted")
	}
	// 管理者的列表里看得到，访客看不到别人的发件。
	res = r.do("GET", "/v1/invitations?sourceId="+h.id, nil, owner.bearer())
	r.expect(res, 200, "")
	if list := res.json["invitations"].([]any); len(list) != 1 || list[0].(map[string]any)["state"] != "accepted" {
		t.Fatalf("已发列表：%s", res.body)
	}
	r.expect(r.do("GET", "/v1/invitations?sourceId="+h.id, nil, friend.bearer()), 403, "source_access_denied")
	r.expect(r.do("GET", "/v1/invitations?sourceId="+h.id, nil, stranger.bearer()), 404, "not_found")
	r.expect(r.do("POST", invitePath(id, "resend"), nil, friend.bearer()), 403, "source_access_denied")
	r.expect(r.do("POST", invitePath(id, "resend"), nil, stranger.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", invitePath(id), nil, stranger.bearer()), 404, "not_found")
}

// 已有同主体的有效授予：按「角色取高、范围并集、到期取晚」合并进那一条，不建第二条；
// 授予已到期却还没被撤销的当作不存在。
func TestHostInvitationMergesIntoExistingGrant(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	friend := r.verified()
	friendEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", friend.accountID)
	h := r.addHost(owner, nil, "0.11.0")

	soon := r.clock.Ms() + 3_600_000
	grantID := r.mustGrant(owner, h.id, guestOf(friend.accountID), "viewer", map[string]any{"workspaceIds": []string{"w1"}, "expiresAtMs": soon})
	id, token := r.mustInviteHost(owner, h.id, friendEmail, "editor", map[string]any{"workspaceIds": []string{"w2"}})
	r.expect(r.acceptInvite(friend, id, token), 200, "")
	rows := r.grantRows(h.id)
	// 邀请不带到期 = 不过期，比原来的晚；角色取高；范围并集；仍是原来那条。
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], []string{"guest", friend.accountID, "editor", "grant", "w1,w2", "", ""}) {
		t.Fatalf("合并后：%v", rows)
	}
	if r.queryString("SELECT grant_id FROM source_grants WHERE source_id = $1 AND revoked_at_ms IS NULL", h.id) != grantID {
		t.Fatal("应更新原来那条授予")
	}
	if n := r.queryInt("SELECT count(*)::int FROM jobs WHERE kind = 'grant.expire' AND dedupe_key = $1 AND done_at_ms IS NULL", grantID); n != 0 {
		t.Fatal("到期清掉后应撤掉到期作业")
	}

	// 再来一份整台、较低角色的邀请：范围放宽成整台，角色不降。
	id2, token2 := r.mustInviteHost(owner, h.id, friendEmail, "viewer", nil)
	r.expect(r.acceptInvite(friend, id2, token2), 200, "")
	rows = r.grantRows(h.id)
	if len(rows) != 1 || rows[0][2] != "editor" || rows[0][4] != "" {
		t.Fatalf("整台并入后：%v", rows)
	}

	// 授予到期了（作业还没跑）：当作不存在，旧的以 system 撤销，建新的。
	later := r.clock.Ms() + 1000
	r.exec("UPDATE source_grants SET expires_at_ms = $1 WHERE grant_id = $2", later, grantID)
	r.clock.Advance(2 * time.Second)
	id3, token3 := r.mustInviteHost(owner, h.id, friendEmail, "viewer", map[string]any{"workspaceIds": []string{"w9"}})
	r.expect(r.acceptInvite(friend, id3, token3), 200, "")
	rows = r.grantRows(h.id)
	if len(rows) != 1 || rows[0][2] != "viewer" || rows[0][4] != "w9" || rows[0][6] != id3 {
		t.Fatalf("到期的授予不并入：%v", rows)
	}
	if at, by := r.grantRow(grantID); at == nil || by != "system" {
		t.Fatalf("旧授予应由 system 撤销：%v %q", at, by)
	}
}

// 邀请自身的有效期（7 天）与授予的到期无关；授予到期早于接受时刻按过期答。撤销、重发、拒绝、过期。
func TestHostInvitationLifecycle(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	ownerEmail := r.queryString("SELECT email FROM accounts WHERE account_id = $1", owner.accountID)
	email := r.email()

	// 授予到期 1 天、邀请 7 天：第 2 天接受，授予已过期 → invitation_expired。
	id, token := r.mustInviteHost(owner, h.id, email, "viewer", map[string]any{"expiresAtMs": r.clock.Ms() + 24*3600*1000})
	guest := r.verifiedAs(email, nil)
	r.clock.Advance(48 * time.Hour)
	owner, guest = r.relogin(owner, ownerEmail), r.relogin(guest, email)
	r.expect(r.acceptInvite(guest, id, token), 410, "invitation_expired")
	if len(r.grantRows(h.id)) != 0 {
		t.Fatal("过期的邀请不该建授予")
	}
	r.expect(r.inviteHost(owner, h.id, email, "viewer", nil), 409, "conflict") // 邀请自己（7 天）还没过期
	r.clock.Advance(6 * 24 * time.Hour)
	owner, guest = r.relogin(owner, ownerEmail), r.relogin(guest, email)

	// 邀请自己的 7 天也过了：重新邀请时先把过期的 pending 改 expired（否则唯一索引挡住）；旧的重发则回到 pending 并换新令牌。
	id2, token2 := r.mustInviteHost(owner, h.id, email, "viewer", nil)
	if r.queryString("SELECT state FROM invitations WHERE invitation_id = $1", id) != "expired" {
		t.Fatal("重新邀请前应把同主机同邮箱过期的 pending 改成 expired")
	}
	r.clock.Advance(8 * 24 * time.Hour)
	owner, guest = r.relogin(owner, ownerEmail), r.relogin(guest, email)
	r.expect(r.acceptInvite(guest, id2, token2), 410, "invitation_expired")
	res := r.do("POST", invitePath(id2, "resend"), nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["state"] != "pending" || res.json["sourceId"] != h.id || res.json["expiresAtMs"] != float64(r.clock.Ms()+7*24*3600*1000) {
		t.Fatalf("重发：%s", res.body)
	}
	fresh := r.waitInvite(email, id2, token2)
	r.expect(r.acceptInvite(guest, id2, token2), 404, "not_found") // 旧令牌作废
	r.expect(r.acceptInvite(guest, id2, fresh), 200, "")
	r.expect(r.do("DELETE", invitePath(id2), nil, owner.bearer()), 409, "conflict") // 已接受的不能撤销
	r.expect(r.do("POST", invitePath(id2, "resend"), nil, owner.bearer()), 409, "conflict")

	// 撤销：之后接受 410；撤销是幂等的；重发已撤销的 410。
	email2 := r.email()
	id3, token3 := r.mustInviteHost(owner, h.id, email2, "viewer", nil)
	r.expect(r.do("DELETE", invitePath(id3), nil, owner.bearer()), 200, "")
	r.expect(r.do("DELETE", invitePath(id3), nil, owner.bearer()), 200, "")
	other := r.verifiedAs(email2, nil)
	r.expect(r.acceptInvite(other, id3, token3), 410, "invitation_revoked")
	r.expect(r.do("POST", invitePath(id3, "resend"), nil, owner.bearer()), 410, "invitation_revoked")
	if r.do("POST", invitePath(id3, "preview"), map[string]any{"token": token3}, nil).status != 410 {
		t.Fatal("预览已撤销的邀请应 410")
	}

	// 拒绝：待处理改 declined；之后接受 409；邮箱不符 403。
	email3 := r.email()
	id4, token4 := r.mustInviteHost(owner, h.id, email3, "viewer", nil)
	decliner := r.verifiedAs(email3, nil)
	r.expect(r.do("POST", invitePath(id4, "decline"), map[string]any{"token": token4}, other.bearer()), 403, "invitation_email_mismatch")
	r.expect(r.do("POST", invitePath(id4, "decline"), map[string]any{"token": token4}, decliner.bearer()), 200, "")
	r.expect(r.acceptInvite(decliner, id4, token4), 409, "conflict")

	// 主机撤销后：预览 404、接受 404、收件箱不列。
	email4 := r.email()
	id5, token5 := r.mustInviteHost(owner, h.id, email4, "viewer", nil)
	late := r.verifiedAs(email4, nil)
	r.expect(r.do("DELETE", sourcePath(h.id), nil, owner.bearer()), 200, "")
	r.expect(r.do("POST", invitePath(id5, "preview"), map[string]any{"token": token5}, nil), 404, "not_found")
	r.expect(r.acceptInvite(late, id5, token5), 404, "not_found")
	if list := r.do("GET", "/v1/invitations?box=received", nil, late.bearer()).json["invitations"].([]any); len(list) != 0 {
		t.Fatalf("主机撤销后收件箱不该列它：%v", list)
	}
}

// 团队主机：接受者先成为 guest 成员（allowGuests、guest 额度）再得授予；团队 guest 只能被授予具体工作空间；
// 已是 owner / admin / member 的接受者得 member 授予；billing 不行；跨团队互相不可见。
func TestHostInvitationTeamHost(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, billing, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("hi-1", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", billing.accountID: "billing"})
	other := r.seedTeam("hi-2", map[string]string{outsider.accountID: "owner"})
	h := r.addHost(owner, teamSpace(team), "0.11.0")
	hOwnedByMember := r.addHost(member, teamSpace(team), "0.11.0")
	hOther := r.addHost(outsider, teamSpace(other), "0.11.0")
	emailOf := func(s session) string {
		return r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID)
	}

	// 管理权：admin 管团队主机；member 只管自己的；别的团队看不到（404）。
	r.expect(r.inviteHost(member, h.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}}), 404, "not_found") // member 看不到别人登记的团队主机
	r.expect(r.inviteHost(outsider, h.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}}), 404, "not_found")
	r.expect(r.inviteHost(owner, hOther.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}}), 404, "not_found")
	r.mustInviteHost(member, hOwnedByMember.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}})

	// 邀请团队外的人：必须给具体工作空间（接受者会成为 guest）。
	ext := r.verified()
	extEmail := emailOf(ext)
	r.expect(r.inviteHost(admin, h.id, extEmail, "viewer", nil), 400, "bad_request")
	id, token := r.mustInviteHost(admin, h.id, extEmail, "operator", map[string]any{"workspaceIds": []string{"ws1"}})
	// 受邀者是已有的团队成员（member）：整台主机也行，得 member 授予。
	idMember, tokenMember := r.mustInviteHost(admin, h.id, emailOf(member), "editor", nil)
	// billing 不访问主机。
	r.expect(r.inviteHost(admin, h.id, emailOf(billing), "viewer", map[string]any{"workspaceIds": []string{"w"}}), 409, "conflict")
	// 主机所有者自己。
	r.expect(r.inviteHost(admin, h.id, emailOf(owner), "viewer", nil), 409, "member_exists")

	// 接受前团队关了 guest：team_policy_denied；打开后接受成功，接受者成为 guest 成员。
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
	res := r.acceptInvite(ext, id, token)
	r.expect(res, 403, "team_policy_denied")
	r.expect(r.inviteHost(admin, h.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}}), 403, "guests_disabled")
	r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": true}, owner.bearer()), 200, "")
	res = r.acceptInvite(ext, id, token)
	r.expect(res, 200, "")
	if res.json["sourceId"] != h.id || res.json["teamId"] != team {
		t.Fatalf("接受的答复：%s", res.body)
	}
	if got := r.queryString("SELECT role FROM team_members WHERE team_id = $1 AND account_id = $2", team, ext.accountID); got != "guest" {
		t.Fatalf("接受者应成为 guest 成员：%s", got)
	}
	if got := r.queryString("SELECT invited_by FROM team_members WHERE team_id = $1 AND account_id = $2", team, ext.accountID); got != admin.accountID {
		t.Fatalf("invited_by：%s", got)
	}
	rows := r.grantRows(h.id)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], []string{"guest", ext.accountID, "operator", "invitation", "ws1", "", id}) {
		t.Fatalf("guest 授予：%v", rows)
	}
	// guest 只看得到被授予的主机，别的团队主机看不到。
	if got := ids(r.do("GET", "/v1/sources", nil, ext.bearer())); !reflect.DeepEqual(got, []string{h.id}) {
		t.Fatalf("guest 的目录：%v", got)
	}
	r.expect(r.do("GET", sourcePath(hOther.id), nil, ext.bearer()), 404, "not_found")
	// member 接受：member 授予、整台。
	r.expect(r.acceptInvite(member, idMember, tokenMember), 200, "")
	rows = r.grantRows(h.id)
	if len(rows) != 2 || !reflect.DeepEqual(rows[1], []string{"member", member.accountID, "editor", "invitation", "", "", idMember}) {
		t.Fatalf("member 授予：%v", rows)
	}

	// 邀请人被降级后，待处理的邀请不能再接受（409）。
	ext2 := r.verified()
	id2, token2 := r.mustInviteHost(admin, h.id, emailOf(ext2), "viewer", map[string]any{"workspaceIds": []string{"w"}})
	r.exec("UPDATE team_members SET role = 'member' WHERE team_id = $1 AND account_id = $2", team, admin.accountID)
	r.expect(r.acceptInvite(ext2, id2, token2), 409, "conflict")
	if n := r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND account_id = $2", team, ext2.accountID); n != 0 {
		t.Fatal("失败的接受不该留下成员")
	}
	r.exec("UPDATE team_members SET role = 'admin' WHERE team_id = $1 AND account_id = $2", team, admin.accountID)

	// guest 额度：免费版 3 个 guest，ext 已占一个；再补两个就满了，第四个接受 429，成员关系回滚。
	for i := 0; i < 2; i++ {
		filler := r.verified()
		r.exec("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'guest', 99)", team, filler.accountID)
	}
	ext3 := r.verified()
	id3, token3 := r.mustInviteHost(admin, h.id, emailOf(ext3), "viewer", map[string]any{"workspaceIds": []string{"w"}})
	r.expect(r.acceptInvite(ext3, id3, token3), 429, "limit_reached")
	if r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND account_id = $2", team, ext3.accountID) != 0 {
		t.Fatal("额度满了不该留下成员")
	}
	// 已经是 guest 的人再被邀请到另一台主机不占新额度。
	h2 := r.addHost(owner, teamSpace(team), "0.11.0")
	id4, token4 := r.mustInviteHost(admin, h2.id, emailOf(ext), "viewer", map[string]any{"workspaceIds": []string{"w"}})
	r.expect(r.acceptInvite(ext, id4, token4), 200, "")
}

// 同一条邀请并发接受：恰好建一条授予、一个 guest 成员，各次都成功（幂等）。
func TestHostInvitationConcurrentAccept(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("hi-c", map[string]string{owner.accountID: "owner"})
	h := r.addHost(owner, teamSpace(team), "0.11.0")
	ext := r.verified()
	id, token := r.mustInviteHost(owner, h.id, r.queryString("SELECT email FROM accounts WHERE account_id = $1", ext.accountID), "viewer",
		map[string]any{"workspaceIds": []string{"w1"}})
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = r.acceptInvite(ext, id, token).status
		}()
	}
	wg.Wait()
	for _, status := range statuses {
		if status != 200 {
			t.Fatalf("并发接受的状态：%v", statuses)
		}
	}
	if len(r.grantRows(h.id)) != 1 || r.queryInt("SELECT count(*)::int FROM team_members WHERE team_id = $1 AND account_id = $2", team, ext.accountID) != 1 {
		t.Fatal("并发接受应只建一条授予、一个成员")
	}
}

// invitations.list：不带过滤时含自己管得了的主机邀请（个人与团队），别人的不出现；每小时 50 次共用团队邀请的额度。
func TestHostInvitationListAndRateLimit(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, stranger := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("hi-l", map[string]string{owner.accountID: "owner", admin.accountID: "admin"})
	personal := r.addHost(admin, nil, "0.11.0")
	teamHost := r.addHost(owner, teamSpace(team), "0.11.0")
	idPersonal, _ := r.mustInviteHost(admin, personal.id, r.email(), "viewer", nil)
	idTeam, _ := r.mustInviteHost(admin, teamHost.id, r.email(), "viewer", map[string]any{"workspaceIds": []string{"w"}})
	teamInv := r.invite(admin, team, r.email(), "member")
	r.expect(teamInv, 200, "")

	collect := func(res response) map[string]bool {
		got := map[string]bool{}
		for _, item := range res.json["invitations"].([]any) {
			got[item.(map[string]any)["invitationId"].(string)] = true
		}
		return got
	}
	res := r.do("GET", "/v1/invitations", nil, admin.bearer())
	r.expect(res, 200, "")
	if got := collect(res); !got[idPersonal] || !got[idTeam] || !got[teamInv.json["invitationId"].(string)] || len(got) != 3 {
		t.Fatalf("admin 的全部已发邀请：%s", res.body)
	}
	// 只按主机、只按团队过滤。
	if got := collect(r.do("GET", "/v1/invitations?sourceId="+teamHost.id, nil, admin.bearer())); !got[idTeam] || len(got) != 1 {
		t.Fatalf("按主机：%v", got)
	}
	if got := collect(r.do("GET", "/v1/invitations?teamId="+team, nil, admin.bearer())); !got[teamInv.json["invitationId"].(string)] || len(got) != 1 {
		t.Fatalf("按团队只含团队邀请：%v", got)
	}
	// 外人什么都看不到。
	if got := collect(r.do("GET", "/v1/invitations", nil, stranger.bearer())); len(got) != 0 {
		t.Fatalf("外人：%v", got)
	}

	// 额度 50 / 小时 / 邀请人：前面已用 3 次。
	for i := 0; i < 47; i++ {
		r.expect(r.inviteHost(admin, personal.id, r.email(), "viewer", nil), 200, "")
	}
	r.expect(r.inviteHost(admin, personal.id, r.email(), "viewer", nil), 429, "rate_limited")
}

// 成员被移出团队、授予被撤销或收窄之后，发给他的待处理主机邀请随之作废，不能拿旧邀请接受回来；
// 授予到期已过的主机邀请不能重发。
func TestHostInvitationsDieWithAccessChanges(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, member, guest, friend := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("hi-d", map[string]string{owner.accountID: "owner", member.accountID: "member", guest.accountID: "guest"})
	th := r.addHost(owner, teamSpace(team), "0.11.0")
	ph := r.addHost(owner, nil, "0.11.0")
	emailOf := func(s session) string {
		return r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID)
	}

	// 移出团队：member 的旧邀请作废。
	idM, tokenM := r.mustInviteHost(owner, th.id, emailOf(member), "editor", nil)
	r.expect(r.do("DELETE", teamPath(team, "members", member.accountID), nil, owner.bearer()), 200, "")
	r.expect(r.acceptInvite(member, idM, tokenM), 410, "invitation_revoked")
	if len(r.grantRows(th.id)) != 0 {
		t.Fatal("作废的邀请不该建授予")
	}
	// 降为 guest：member 角色的邀请同样作废（先重新加回成员再降级）。
	r.exec("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'member', 50)", team, member.accountID)
	idM2, tokenM2 := r.mustInviteHost(owner, th.id, emailOf(member), "editor", nil)
	r.expect(r.do("PATCH", teamPath(team, "members", member.accountID), map[string]any{"role": "guest"}, owner.bearer()), 200, "")
	r.expect(r.acceptInvite(member, idM2, tokenM2), 410, "invitation_revoked")

	// 撤授予：旧邀请作废。
	g := r.mustGrant(owner, ph.id, guestOf(friend.accountID), "viewer", nil)
	idF, tokenF := r.mustInviteHost(owner, ph.id, emailOf(friend), "editor", nil)
	r.expect(r.do("DELETE", "/v1/grants/"+g, nil, owner.bearer()), 200, "")
	r.expect(r.acceptInvite(friend, idF, tokenF), 410, "invitation_revoked")

	// 收窄（加到期）：旧邀请作废，到期不会被它清掉。
	soon := r.clock.Ms() + 3_600_000
	g2 := r.mustGrant(owner, ph.id, guestOf(friend.accountID), "viewer", nil)
	idF2, tokenF2 := r.mustInviteHost(owner, ph.id, emailOf(friend), "viewer", nil)
	r.expect(r.do("PATCH", "/v1/grants/"+g2, map[string]any{"expiresAtMs": soon}, owner.bearer()), 200, "")
	r.expect(r.acceptInvite(friend, idF2, tokenF2), 410, "invitation_revoked")
	if rows := r.grantRows(ph.id); len(rows) != 1 || rows[0][5] != strconv.FormatInt(soon, 10) {
		t.Fatalf("到期应保留：%v", rows)
	}

	// 授予到期已过的邀请不能重发。
	stranger := r.verified()
	idX, _ := r.mustInviteHost(owner, ph.id, emailOf(stranger), "viewer", map[string]any{"expiresAtMs": r.clock.Ms() + 3_600_000})
	r.clock.Advance(2 * time.Hour)
	owner = r.relogin(owner, emailOf(owner))
	r.expect(r.do("POST", invitePath(idX, "resend"), nil, owner.bearer()), 400, "bad_request")
}
