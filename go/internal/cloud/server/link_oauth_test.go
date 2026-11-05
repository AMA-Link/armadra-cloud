package server_test

import (
	"fmt"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
)

// 绑定、解绑第三方登录与 me.signInMethods：假提供方 + 真实的 PostgreSQL / Redis。

// elevate 直接把会话的设备提权（提权本身由 step-up 的测试覆盖）。
func (r *rig) elevate(s session) {
	r.t.Helper()
	r.exec("UPDATE devices SET elevated_until_ms = $2 WHERE device_id = $1", s.deviceID, r.clock.Ms()+10*60_000)
}

func (r *rig) linkStart(provider string, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/oauth/"+provider+"/link/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/" + provider}, h)
}

func (r *rig) linkBegin(f *fakeOAuth, provider string, ident fakeIdentity, h headers) oauthFlow {
	r.t.Helper()
	res := r.linkStart(provider, h)
	r.expect(res, 200, "")
	code, state := f.grant(provider, res.json["authorizationUrl"].(string), ident)
	flow := oauthFlow{provider: provider, state: state, code: code}
	for _, c := range setCookies(res) {
		if c.Name == oauthCookie {
			flow.nonce = c.Value
		}
	}
	return flow
}

func (r *rig) linkFinish(fl oauthFlow, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/oauth/"+fl.provider+"/link/finish", map[string]any{"state": fl.state, "code": fl.code}, h)
}

func (r *rig) queuedMails(to, template string) int {
	r.t.Helper()
	return r.queryInt("SELECT count(*)::int FROM outbox_emails WHERE to_email = $1 AND template = $2", to, template)
}

func reason(res response) string {
	d, _ := res.json["details"].(map[string]any)
	s, _ := d["reason"].(string)
	return s
}

func TestOAuthLink(t *testing.T) {
	t.Parallel()

	t.Run("成功：绑第二个提供方，不改账号邮箱与显示名，写日志并通知", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		email := r.email()
		s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("7001", email), false))
		r.elevate(s)
		before := r.queryString("SELECT display_name FROM accounts WHERE account_id = $1", s.accountID)
		// 提供方邮箱不同于账号邮箱：显式绑定不要求相等。
		flow := r.linkBegin(f, "google", googleIdentity("g-7001", "other-"+email), s.bearer())
		res := r.linkFinish(flow, s.bearer())
		r.expect(res, 200, "")
		if res.json["provider"] != "google" || res.json["email"] != "other-"+email || res.json["linkedAtMs"] == nil {
			t.Fatalf("答复：%s", res.body)
		}
		if r.identityCount("google", "g-7001") != 1 {
			t.Fatal("应已绑定")
		}
		if r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID) != email ||
			r.queryString("SELECT display_name FROM accounts WHERE account_id = $1", s.accountID) != before {
			t.Fatal("不该改账号邮箱与显示名")
		}
		if !contains(r.securityActions(s.accountID), "security.oauthLink") {
			t.Fatalf("安全日志：%v", r.securityActions(s.accountID))
		}
		if r.queuedMails(email, "provider_linked") != 1 {
			t.Fatal("应给账号邮箱发已绑定通知")
		}
		// state 一次性。
		r.expect(r.linkFinish(flow, s.bearer()), 401, "credentials_invalid")
	})

	t.Run("未提权：start 与 finish 都答 mfa_required", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		s := r.githubSession(f, "7101")
		r.expect(r.linkStart("google", s.bearer()), 401, "mfa_required")
		r.elevate(s)
		flow := r.linkBegin(f, "google", googleIdentity("g-7101", r.email()), s.bearer())
		r.exec("UPDATE devices SET elevated_until_ms = NULL WHERE device_id = $1", s.deviceID)
		r.expect(r.linkFinish(flow, s.bearer()), 401, "mfa_required")
		if r.identityCount("google", "g-7101") != 0 {
			t.Fatal("拒绝时不该绑定")
		}
	})

	t.Run("需要会话；没配的提供方 404；回调地址不对 400", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub)
		r.expect(r.linkStart("github", nil), 401, "")
		s := r.githubSession(f, "7201")
		r.elevate(s)
		r.expect(r.linkStart("google", s.bearer()), 404, "not_found")
		bad := r.do("POST", "/v1/auth/oauth/github/link/start", map[string]any{"redirectUri": appOrigin + "/x"}, s.bearer())
		r.expect(bad, 400, "bad_request")
	})

	t.Run("身份已属别的账号：409 identity_linked，不透露是谁", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		other := r.githubSession(f, "7301")
		_ = other
		s := r.githubSession(f, "7302")
		r.bindOAuth(other.accountID, "google", "g-7300")
		r.elevate(s)
		res := r.linkFinish(r.linkBegin(f, "google", googleIdentity("g-7300", r.email()), s.bearer()), s.bearer())
		r.expect(res, 409, "conflict")
		if reason(res) != "identity_linked" {
			t.Fatalf("details：%s", res.body)
		}
	})

	t.Run("本账号该提供方已有身份：409 provider_linked", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub)
		s := r.githubSession(f, "7401")
		r.elevate(s)
		res := r.linkFinish(r.linkBegin(f, "github", ghIdentity("7402", r.email()), s.bearer()), s.bearer())
		r.expect(res, 409, "conflict")
		if reason(res) != "provider_linked" || r.identityCount("github", "7402") != 0 {
			t.Fatalf("details：%s", res.body)
		}
		// 绑自己已绑的同一个身份也是 provider_linked。
		res = r.linkFinish(r.linkBegin(f, "github", ghIdentity("7401", r.email()), s.bearer()), s.bearer())
		r.expect(res, 409, "conflict")
		if reason(res) != "provider_linked" {
			t.Fatalf("details：%s", res.body)
		}
	})

	t.Run("提供方邮箱未验证：403 email_unverified", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		s := r.githubSession(f, "7501")
		r.elevate(s)
		ident := googleIdentity("g-7501", r.email())
		ident.verified = false
		r.expect(r.linkFinish(r.linkBegin(f, "google", ident, s.bearer()), s.bearer()), 403, "email_unverified")
		if r.identityCount("google", "g-7501") != 0 {
			t.Fatal("不该绑定")
		}
	})

	t.Run("state 的目的、账号、设备对不上", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		s := r.githubSession(f, "7601")
		other := r.githubSession(f, "7602")
		second := parseSession(r, r.oauthLogin(f, "github", ghIdentity("7601", r.email()), false))
		for _, one := range []session{s, other, second} {
			r.elevate(one)
		}
		ident := googleIdentity("g-7601", r.email())
		// 账号不是发起者。
		r.expect(r.linkFinish(r.linkBegin(f, "google", ident, s.bearer()), other.bearer()), 401, "credentials_invalid")
		// 设备不是发起者。
		r.expect(r.linkFinish(r.linkBegin(f, "google", ident, s.bearer()), second.bearer()), 401, "credentials_invalid")
		// 提权和登录的 state 不能拿来绑定。
		reauth := r.reauthBegin(f, s, "github", ghIdentity("7601", r.email()), s.bearer())
		r.expect(r.linkFinish(reauth, s.bearer()), 401, "credentials_invalid")
		login := r.oauthBegin(f, "google", ident, false)
		r.expect(r.linkFinish(login, s.bearer()), 401, "credentials_invalid")
		// 绑定的 state 不能拿来提权或登录。
		link := r.linkBegin(f, "github", ghIdentity("7601", r.email()), s.bearer())
		r.expect(r.stepUpOAuth(link, s.bearer()), 401, "credentials_invalid")
		link = r.linkBegin(f, "google", ident, s.bearer())
		r.expect(r.finishWith(link, nil), 401, "credentials_invalid")
		if r.identityCount("google", "g-7601") != 0 {
			t.Fatal("拒绝时不该绑定")
		}
	})

	t.Run("Cookie 传输要浏览器随机数", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("7701", r.email()), true))
		r.elevate(s)
		ident := googleIdentity("g-7701", r.email())
		flow := r.linkBegin(f, "google", ident, s.write())
		if flow.nonce == "" {
			t.Fatal("应下发随机数")
		}
		r.expect(r.linkFinish(flow, s.write()), 401, "credentials_invalid")
		flow = r.linkBegin(f, "google", ident, s.write())
		r.expect(r.linkFinish(flow, s.write().add("Cookie", oauthCookie+"=wrong")), 401, "credentials_invalid")
		flow = r.linkBegin(f, "google", ident, s.write())
		r.expect(r.linkFinish(flow, s.write().add("Cookie", oauthCookie+"="+flow.nonce)), 200, "")
	})

	t.Run("start 按账号每小时 10 次", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		s := r.githubSession(f, "7801")
		r.elevate(s)
		for i := 0; i < 10; i++ {
			// 换来源 IP，避开按 IP 的发起限流，只测按账号的桶。
			r.expect(r.do("POST", "/v1/auth/oauth/google/link/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/google"},
				s.bearer().add("X-Forwarded-For", fmt.Sprintf("10.8.0.%d", i+1))), 200, "")
		}
		res := r.linkStart("google", s.bearer())
		r.expect(res, 429, "rate_limited")
	})
}

func TestOAuthUnlinkElevatedAndNotified(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
	email := r.email()
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("8001", email), false))
	r.bindOAuth(s.accountID, "google", "g-8001")
	unlink := func(p string) response { return r.do("DELETE", "/v1/auth/oauth/"+p, nil, s.bearer()) }
	r.expect(unlink("google"), 401, "mfa_required")
	if r.identityCount("google", "g-8001") != 1 {
		t.Fatal("未提权不该解绑")
	}
	r.elevate(s)
	r.expect(unlink("google"), 200, "")
	if r.queuedMails(email, "provider_unlinked") != 1 || !contains(r.securityActions(s.accountID), "security.oauthUnlink") {
		t.Fatal("应发解绑通知并写日志")
	}
	// 最后一种方式。
	r.expect(unlink("github"), 409, "conflict")
}

// 「至少留一种登录方式」只算此刻能用的：服务端没配的提供方不算，口令登录关着的口令不算。
func TestOAuthUnlinkCountsOnlyUsableMethods(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub) // 只配 GitHub，口令登录开着
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("8101", r.email()), false))
	// 另有一个 Google 身份，但服务端没配 Google：不算。
	r.bindOAuth(s.accountID, "google", "g-8101")
	r.elevate(s)
	r.expect(r.do("DELETE", "/v1/auth/oauth/github", nil, s.bearer()), 409, "conflict")
	// 设了口令且口令登录开着：口令算。
	withPassword := r.insertAccount(r.email(), true)
	r.bindOAuth(withPassword, "github", "8102")
	other := parseSession(r, r.oauthLogin(f, "github", ghIdentity("8102", r.email()), false))
	if other.accountID != withPassword {
		t.Fatal("应登录到有口令的账号")
	}
	r.elevate(other)
	r.expect(r.do("DELETE", "/v1/auth/oauth/github", nil, other.bearer()), 200, "")
}

func TestSignInMethods(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
	get := func(s session) response { return r.do("GET", "/v1/me/sign-in-methods", nil, s.bearer()) }
	r.expect(r.do("GET", "/v1/me/sign-in-methods", nil, nil), 401, "")

	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("8201", r.email()), false))
	res := get(s)
	r.expect(res, 200, "")
	if res.json["password"] != false || res.json["totp"] != false {
		t.Fatalf("答复：%s", res.body)
	}
	providers := res.json["providers"].([]any)
	if len(providers) != 1 || providers[0].(map[string]any)["provider"] != "github" || providers[0].(map[string]any)["login"] != "octo-8201" {
		t.Fatalf("providers：%s", res.body)
	}
	if got := fmt.Sprint(res.json["stepUp"]); got != "[oauth]" {
		t.Fatalf("stepUp：%s", got)
	}
	// 用过之后带最近使用时间。
	r.elevate(s)
	r.expect(r.stepUpOAuth(r.reauthBegin(f, s, "github", ghIdentity("8201", r.email()), s.bearer()), s.bearer()), 200, "")
	if get(s).json["providers"].([]any)[0].(map[string]any)["lastUsedAtMs"] == nil {
		t.Fatal("应带 lastUsedAtMs")
	}

	// 口令登录开着、账号有口令：password 与 stepUp 都含口令；没配的提供方的身份不算可用的提权方式。
	r2, _ := oauthRig(t, false, oauth.GitHub)
	pw := r2.registerBearer(r2.email())
	r2.bindOAuth(pw.accountID, "google", "g-8202")
	got := r2.do("GET", "/v1/me/sign-in-methods", nil, pw.bearer())
	r2.expect(got, 200, "")
	if got.json["password"] != true || fmt.Sprint(got.json["stepUp"]) != "[password]" || len(got.json["providers"].([]any)) != 1 {
		t.Fatalf("答复：%s", got.body)
	}
}
