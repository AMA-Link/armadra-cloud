package server_test

import (
	"fmt"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
)

// 用已绑定的第三方身份提权（auth.stepUpOAuthStart + auth.stepUp 的 oauth 分支）：假提供方 + 真实的 PostgreSQL / Redis。

func (r *rig) reauthStart(s session, provider string, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/step-up/oauth/"+provider+"/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/" + provider}, h)
}

// reauthBegin 发起提权并让假提供方发 code。
func (r *rig) reauthBegin(f *fakeOAuth, s session, provider string, ident fakeIdentity, h headers) oauthFlow {
	r.t.Helper()
	res := r.reauthStart(s, provider, h)
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

func (r *rig) stepUpOAuth(fl oauthFlow, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/step-up", map[string]any{"oauth": map[string]any{"provider": fl.provider, "state": fl.state, "code": fl.code}}, h)
}

func (r *rig) elevated(deviceID string) bool {
	r.t.Helper()
	return r.queryInt("SELECT count(*)::int FROM devices WHERE device_id = $1 AND elevated_until_ms > $2", deviceID, r.clock.Ms()) == 1
}

// githubSession 用 GitHub 身份登录出一个只有第三方登录的账号。
func (r *rig) githubSession(f *fakeOAuth, subject string) session {
	r.t.Helper()
	return parseSession(r, r.oauthLogin(f, "github", ghIdentity(subject, r.email()), false))
}

func TestStepUpOAuthSucceeds(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
	email := r.email()
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("5001", email), false))
	if r.elevated(s.deviceID) {
		t.Fatal("前提：新会话没有提权")
	}
	flow := r.reauthBegin(f, s, "github", ghIdentity("5001", email), s.bearer())
	res := r.stepUpOAuth(flow, s.bearer())
	r.expect(res, 200, "")
	until := int64(res.json["elevatedUntilMs"].(float64))
	if want := r.clock.Ms() + 10*60_000; until != want {
		t.Fatalf("提权到 %d，应为 %d", until, want)
	}
	if !r.elevated(s.deviceID) {
		t.Fatal("设备应已提权")
	}
	if !contains(r.securityActions(s.accountID), "security.stepUp") {
		t.Fatalf("应写 security.stepUp：%v", r.securityActions(s.accountID))
	}
	if r.queryInt("SELECT count(*)::int FROM account_credentials WHERE account_id = $1 AND kind = 'oauth' AND last_used_at_ms IS NOT NULL", s.accountID) != 1 {
		t.Fatal("该身份的最近使用时间应更新")
	}
	// state 一次性：重放答 401。
	r.expect(r.stepUpOAuth(flow, s.bearer()), 401, "credentials_invalid")
}

func TestStepUpOAuthRejections(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
	email := r.email()
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("5101", email), false))
	other := r.githubSession(f, "5102")

	t.Run("主体不是本账号已绑定的", func(t *testing.T) {
		// 另一个 GitHub 主体（没绑过任何账号）。
		r.expect(r.stepUpOAuth(r.reauthBegin(f, s, "github", ghIdentity("9999", email), s.bearer()), s.bearer()), 401, "credentials_invalid")
		// 绑在别的账号上的主体。
		r.expect(r.stepUpOAuth(r.reauthBegin(f, s, "github", ghIdentity("5102", email), s.bearer()), s.bearer()), 401, "credentials_invalid")
		if r.elevated(s.deviceID) {
			t.Fatal("拒绝时不该提权")
		}
	})
	t.Run("未绑定的提供方", func(t *testing.T) {
		r.expect(r.stepUpOAuth(r.reauthBegin(f, s, "google", googleIdentity("g-5101", email), s.bearer()), s.bearer()), 401, "credentials_invalid")
	})
	t.Run("state 的目的不对", func(t *testing.T) {
		// 登录的 state 不能拿来提权。
		login := r.oauthBegin(f, "github", ghIdentity("5101", email), false)
		r.expect(r.stepUpOAuth(login, s.bearer()), 401, "credentials_invalid")
		// 提权的 state 不能拿来登录。
		reauth := r.reauthBegin(f, s, "github", ghIdentity("5101", email), s.bearer())
		r.expect(r.finishWith(reauth, nil), 401, "credentials_invalid")
	})
	t.Run("账号不是发起者", func(t *testing.T) {
		flow := r.reauthBegin(f, s, "github", ghIdentity("5101", email), s.bearer())
		r.expect(r.stepUpOAuth(flow, other.bearer()), 401, "credentials_invalid")
		if r.elevated(other.deviceID) || r.elevated(s.deviceID) {
			t.Fatal("拒绝时不该提权")
		}
	})
	t.Run("设备不是发起者", func(t *testing.T) {
		second := parseSession(r, r.oauthLogin(f, "github", ghIdentity("5101", email), false))
		if second.deviceID == s.deviceID {
			t.Fatal("前提：第二次登录是另一台设备")
		}
		flow := r.reauthBegin(f, s, "github", ghIdentity("5101", email), s.bearer())
		r.expect(r.stepUpOAuth(flow, second.bearer()), 401, "credentials_invalid")
		if r.elevated(second.deviceID) {
			t.Fatal("拒绝时不该提权")
		}
	})
	t.Run("提供方不认这个 code", func(t *testing.T) {
		flow := r.reauthBegin(f, s, "github", ghIdentity("5101", email), s.bearer())
		flow.code = "bogus"
		r.expect(r.stepUpOAuth(flow, s.bearer()), 401, "credentials_invalid")
	})
	t.Run("过期", func(t *testing.T) {
		flow := r.reauthBegin(f, s, "github", ghIdentity("5101", email), s.bearer())
		r.clock.Advance(oauth.StateTTL + 1)
		r.expect(r.stepUpOAuth(flow, s.bearer()), 401, "credentials_invalid")
	})
}

func TestStepUpOAuthBrowserBinding(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub)
	email := r.email()
	s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("5201", email), true))

	flow := r.reauthBegin(f, s, "github", ghIdentity("5201", email), s.write())
	if flow.nonce == "" {
		t.Fatal("Cookie 传输要下发浏览器绑定随机数")
	}
	// 缺随机数 Cookie：401，state 已作废。
	r.expect(r.stepUpOAuth(flow, s.write()), 401, "credentials_invalid")
	r.expect(r.stepUpOAuth(flow, s.write().add("Cookie", oauthCookie+"="+flow.nonce)), 401, "credentials_invalid")

	// 随机数不一致。
	flow = r.reauthBegin(f, s, "github", ghIdentity("5201", email), s.write())
	r.expect(r.stepUpOAuth(flow, s.write().add("Cookie", oauthCookie+"=other")), 401, "credentials_invalid")

	// 一致：通过，并清掉随机数 Cookie。
	flow = r.reauthBegin(f, s, "github", ghIdentity("5201", email), s.write())
	res := r.stepUpOAuth(flow, s.write().add("Cookie", oauthCookie+"="+flow.nonce))
	r.expect(res, 200, "")
	cleared := false
	for _, c := range setCookies(res) {
		if c.Name == oauthCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("用后要清除浏览器绑定 Cookie")
	}
	if !r.elevated(s.deviceID) {
		t.Fatal("设备应已提权")
	}
}

func TestStepUpOAuthStartErrors(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub)
	s := r.githubSession(f, "5301")
	r.expect(r.do("POST", "/v1/auth/step-up/oauth/github/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/github"}, nil), 401, "")
	r.expect(r.reauthStart(s, "google", s.bearer()), 404, "not_found")
	res := r.do("POST", "/v1/auth/step-up/oauth/github/start", map[string]any{"redirectUri": appOrigin + "/elsewhere"}, s.bearer())
	r.expect(res, 400, "bad_request")
}

func TestStepUpOAuthRateLimit(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub)
	s := r.githubSession(f, "5401")
	// 设备级的 stepup.dev：10 次（含成功与失败）。
	for i := 0; i < 10; i++ {
		r.expect(r.stepUpOAuth(oauthFlow{provider: "github", state: fmt.Sprintf("nope-%d", i), code: "c"}, s.bearer()), 401, "credentials_invalid")
	}
	r.expect(r.stepUpOAuth(oauthFlow{provider: "github", state: "nope", code: "c"}, s.bearer()), 429, "rate_limited")
}

// 口令提权照旧，另写 security.stepUp。
func TestStepUpPasswordLogsSecurityEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	s := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"password": password}, s.bearer()), 200, "")
	if !contains(r.securityActions(s.accountID), "security.stepUp") {
		t.Fatalf("应写 security.stepUp：%v", r.securityActions(s.accountID))
	}
}
