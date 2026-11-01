package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
)

// 第三方登录（GitHub、Google）与口令登录开关的测试：假提供方（oauth_fake_test.go）+ 真实的 PostgreSQL / Redis。

const oauthCookie = "__Host-armadra-oauth"

// bothProviders 的身份：邮箱已验证。
func ghIdentity(subject, email string) fakeIdentity {
	return fakeIdentity{subject: subject, login: "octo-" + subject, name: "Octo Cat", email: email, verified: true}
}

func googleIdentity(subject, email string) fakeIdentity {
	return fakeIdentity{subject: subject, name: "Gina Google", email: email, verified: true}
}

// oauthFlow 是走到回调那一步的一次授权。
type oauthFlow struct {
	provider, state, code, nonce, authorizationURL string
}

// begin 发起授权并让假提供方发 code；cookie = true 用控制台来源（Cookie 传输）。
func (r *rig) oauthBegin(f *fakeOAuth, provider string, ident fakeIdentity, cookie bool) oauthFlow {
	r.t.Helper()
	h := headers(nil)
	if cookie {
		h = hdr("Origin", appOrigin)
	}
	res := r.oauthStart(provider, h)
	r.expect(res, 200, "")
	authURL := res.json["authorizationUrl"].(string)
	code, state := f.grant(provider, authURL, ident)
	if state != res.json["state"] {
		r.t.Fatalf("授权地址里的 state 与答复不一致：%s", res.body)
	}
	flow := oauthFlow{provider: provider, state: state, code: code, authorizationURL: authURL}
	for _, c := range setCookies(res) {
		if c.Name == oauthCookie {
			flow.nonce = c.Value
		}
	}
	return flow
}

func (r *rig) oauthStart(provider string, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/oauth/"+provider+"/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/" + provider}, h)
}

// finish 在回调页那一步完成登录；cookie = true 按 Cookie 传输（带控制台来源与浏览器绑定 Cookie）。
func (r *rig) oauthFinish(fl oauthFlow, cookie bool) response {
	r.t.Helper()
	h := headers(nil)
	if cookie {
		h = hdr("Origin", appOrigin)
		if fl.nonce != "" {
			h = h.add("Cookie", oauthCookie+"="+fl.nonce)
		}
	}
	return r.finishWith(fl, h)
}

func (r *rig) finishWith(fl oauthFlow, h headers) response {
	r.t.Helper()
	return r.do("POST", "/v1/auth/oauth/"+fl.provider+"/finish", map[string]any{
		"state": fl.state, "code": fl.code, "device": device,
	}, h)
}

// oauthLogin 走完整的一次登录。
func (r *rig) oauthLogin(f *fakeOAuth, provider string, ident fakeIdentity, cookie bool) response {
	r.t.Helper()
	return r.oauthFinish(r.oauthBegin(f, provider, ident, cookie), cookie)
}

// insertAccount 直接写一个有口令的账号（口令登录关闭时没法注册）；答账号 id。
func (r *rig) insertAccount(email string, verified bool) string {
	r.t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012x", len(email)*7919+int(time.Now().UnixNano()%1_000_000))
	record, err := identity.HashPassword(password)
	if err != nil {
		r.t.Fatal(err)
	}
	data, _ := json.Marshal(record)
	var verifiedAt any
	if verified {
		verifiedAt = 1
	}
	r.exec(`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms, last_failure_at_ms,
	  created_at_ms, updated_at_ms, email, email_verified_at_ms, display_name) VALUES ('cloud', $1, $1, $2, 0, 0, 0, 1, 1, $3, $4, '本地用户')`,
		id, string(data), email, verifiedAt)
	return id
}

// bindOAuth 直接给账号绑一个第三方身份。
func (r *rig) bindOAuth(accountID, provider, subject string) {
	r.t.Helper()
	r.exec(`INSERT INTO account_credentials (tenant_id, credential_id, account_id, kind, external_id, public_json, created_at_ms)
	  VALUES ('cloud', $1, $2, 'oauth', $3, '{}', 1)`, "cred-"+provider+"-"+subject+"-"+accountID[len(accountID)-6:], accountID, provider+":"+subject)
}

func (r *rig) securityActions(accountID string) []string {
	r.t.Helper()
	rows, err := r.run.DB.Pool().Query(context.Background(), "SELECT action FROM audit_log WHERE account_id = $1 AND action LIKE 'security.%' ORDER BY at_ms, event_id", accountID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func (r *rig) identityCount(provider, subject string) int {
	return r.queryInt("SELECT count(*)::int FROM account_credentials WHERE kind = 'oauth' AND external_id = $1", provider+":"+subject)
}

func (r *rig) accountCount(email string) int {
	return r.queryInt("SELECT count(*)::int FROM accounts WHERE email = $1", email)
}

func oauthRig(t *testing.T, passwordOff bool, providers ...string) (*rig, *fakeOAuth) {
	t.Helper()
	f := newFakeOAuth(t, providers...)
	options := rigOptions{oauth: f}
	if passwordOff {
		options.env = map[string]string{"PASSWORD_AUTH": "off"}
	}
	return newRig(t, options), f
}

func redisClient(t *testing.T) *goredis.Client {
	t.Helper()
	opts, err := goredis.ParseURL(redisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ---------------------------------------------------------------- oauthStart

func TestOAuthStart(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub, oauth.Google)

	t.Run("redirectUri 必须严格等于来源加回调路径", func(t *testing.T) {
		for _, uri := range []string{
			appOrigin + "/login/oauth/github/",
			appOrigin + "/login/oauth/github?x=1",
			appOrigin + "/login/oauth/github#frag",
			appOrigin + "/login/oauth/google",
			appOrigin + "/oauth/github",
			appOrigin,
			"https://evil.example/login/oauth/github",
			"http://localhost:8100/login/oauth/github",
			strings.ToUpper(appOrigin) + "/login/oauth/github",
		} {
			res := r.do("POST", "/v1/auth/oauth/github/start", map[string]any{"redirectUri": uri}, nil)
			r.expect(res, 400, "bad_request")
		}
	})

	t.Run("GitHub：scope、PKCE，Bearer 传输不发 Cookie", func(t *testing.T) {
		before := f.seq
		res := r.oauthStart("github", nil)
		r.expect(res, 200, "")
		if len(setCookies(res)) != 0 {
			t.Fatalf("Bearer 传输不发 Cookie：%v", res.header)
		}
		u, err := url.Parse(res.json["authorizationUrl"].(string))
		if err != nil || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
			t.Fatalf("授权地址：%v %v", u, err)
		}
		q := u.Query()
		if q.Get("scope") != "read:user user:email" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != fakeGitHubID ||
			q.Get("redirect_uri") != appOrigin+"/login/oauth/github" || q.Get("state") != res.json["state"] {
			t.Fatalf("授权参数：%v", q)
		}
		if len(q.Get("code_challenge")) != 43 || len(res.json["state"].(string)) != 43 {
			t.Fatalf("32 字节随机数的 base64url 是 43 个字符：%v", q)
		}
		if int64(res.json["expiresAtMs"].(float64)) != r.clock.Ms()+10*60*1000 {
			t.Fatalf("10 分钟有效：%v", res.json["expiresAtMs"])
		}
		if f.seq != before {
			t.Fatal("发起授权不该访问提供方")
		}
		// 授权状态在 Redis 里，键是 state 的哈希；明文 state 不作键。
		state := res.json["state"].(string)
		sum := sha256.Sum256([]byte(state))
		client := redisClient(t)
		ctx := context.Background()
		ttl, err := client.PTTL(ctx, r.deps.OAuthPrefix+hex.EncodeToString(sum[:])).Result()
		if err != nil || ttl <= 0 || ttl > 11*time.Minute {
			t.Fatalf("授权状态的 TTL：%v %v", ttl, err)
		}
		if n, _ := client.Exists(ctx, r.deps.OAuthPrefix+state).Result(); n != 0 {
			t.Fatal("键里不该放明文 state")
		}
	})

	t.Run("Google：OpenID Connect、选账号", func(t *testing.T) {
		res := r.oauthStart("google", nil)
		r.expect(res, 200, "")
		u, _ := url.Parse(res.json["authorizationUrl"].(string))
		q := u.Query()
		if u.Host != "accounts.google.com" || q.Get("scope") != "openid email profile" || q.Get("prompt") != "select_account" ||
			q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != fakeGoogleID {
			t.Fatalf("Google 授权地址：%v", u)
		}
	})

	t.Run("Cookie 传输下发浏览器绑定随机数", func(t *testing.T) {
		res := r.oauthStart("github", hdr("Origin", appOrigin))
		r.expect(res, 200, "")
		var found []*http.Cookie
		for _, c := range setCookies(res) {
			if c.Name == oauthCookie {
				found = append(found, c)
			}
		}
		if len(found) != 1 {
			t.Fatalf("应下发一枚 %s：%v", oauthCookie, res.header)
		}
		c := found[0]
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 600 || len(c.Value) != 43 {
			t.Fatalf("Cookie 属性：%+v", c)
		}
		// 每次发起的随机数不同。
		other := r.oauthStart("github", hdr("Origin", appOrigin))
		for _, c2 := range setCookies(other) {
			if c2.Name == oauthCookie && c2.Value == c.Value {
				t.Fatal("随机数不该重复")
			}
		}
		// 带 Authorization 的请求按 Bearer 传输，不发 Cookie。
		bearer := r.oauthStart("github", hdr("Origin", appOrigin, "Authorization", "Bearer x"))
		r.expect(bearer, 200, "")
		if len(setCookies(bearer)) != 0 {
			t.Fatal("带 Authorization 不发 Cookie")
		}
	})

	t.Run("没配的提供方答 404", func(t *testing.T) {
		only, _ := oauthRig(t, false, oauth.GitHub)
		r.expect(only.oauthStart("google", nil), 404, "not_found")
		r.expect(only.oauthStart("github", nil), 200, "")
		none := newRig(t, rigOptions{})
		r.expect(none.oauthStart("github", nil), 404, "not_found")
		r.expect(none.oauthStart("google", nil), 404, "not_found")
		// 不认识的提供方被入参校验挡掉。
		r.expect(none.do("POST", "/v1/auth/oauth/gitlab/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/gitlab"}, nil), 400, "bad_request")
	})
}

// 按 IP 每分钟 20 次：发起与完成各一个桶。
func TestOAuthRateLimits(t *testing.T) {
	t.Parallel()
	r, _ := oauthRig(t, false, oauth.GitHub)
	for i := 0; i < 20; i++ {
		r.expect(r.oauthStart("github", nil), 200, "")
	}
	res := r.oauthStart("github", nil)
	r.expect(res, 429, "rate_limited")
	if res.header.Get("Retry-After") == "" {
		t.Fatal("429 要带 Retry-After")
	}
	for i := 0; i < 20; i++ {
		r.expect(r.finishWith(oauthFlow{provider: "github", state: fmt.Sprintf("nope-%d", i), code: "c"}, nil), 401, "credentials_invalid")
	}
	r.expect(r.finishWith(oauthFlow{provider: "github", state: "nope", code: "c"}, nil), 429, "rate_limited")
	// 发起的桶满了不影响完成的桶（另一个 IP 才是新桶，这里只确认两个桶各自计数）。
	other := r.do("POST", "/v1/auth/oauth/github/start", map[string]any{"redirectUri": appOrigin + "/login/oauth/github"}, hdr("X-Forwarded-For", "10.9.9.9"))
	r.expect(other, 200, "")
}

// ---------------------------------------------------------------- oauthFinish：state

func TestOAuthFinishState(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub, oauth.Google)

	t.Run("state 一次性", func(t *testing.T) {
		email := r.email()
		fl := r.oauthBegin(f, "github", ghIdentity("1001", email), false)
		r.expect(r.oauthFinish(fl, false), 200, "")
		// 重放：同一个 state 与 code。
		r.expect(r.oauthFinish(fl, false), 401, "credentials_invalid")
		if f.callsTo("token") != 1 {
			t.Fatalf("重放不该再访问提供方：%d", f.callsTo("token"))
		}
	})

	t.Run("state 不存在", func(t *testing.T) {
		r.expect(r.finishWith(oauthFlow{provider: "github", state: "never-issued", code: "c"}, nil), 401, "credentials_invalid")
	})

	t.Run("过期", func(t *testing.T) {
		before := f.callsTo("token")
		fl := r.oauthBegin(f, "github", ghIdentity("1002", r.email()), false)
		r.clock.Advance(10*time.Minute + time.Second)
		r.expect(r.oauthFinish(fl, false), 401, "credentials_invalid")
		if f.callsTo("token") != before {
			t.Fatal("过期的 state 不该访问提供方")
		}
		// 刚好在有效期内可以。
		fresh := r.oauthBegin(f, "github", ghIdentity("1003", r.email()), false)
		r.clock.Advance(9*time.Minute + 59*time.Second)
		r.expect(r.oauthFinish(fresh, false), 200, "")
	})

	t.Run("提供方对不上：state 作废", func(t *testing.T) {
		fl := r.oauthBegin(f, "github", ghIdentity("1004", r.email()), false)
		wrong := fl
		wrong.provider = "google"
		r.expect(r.oauthFinish(wrong, false), 401, "credentials_invalid")
		r.expect(r.oauthFinish(fl, false), 401, "credentials_invalid")
	})

	t.Run("code 无效", func(t *testing.T) {
		email := r.email()
		fl := r.oauthBegin(f, "google", googleIdentity("g-1", email), false)
		fl.code = "forged-code"
		r.expect(r.oauthFinish(fl, false), 401, "credentials_invalid")
		if r.accountCount(email) != 0 {
			t.Fatal("code 无效不该建账号")
		}
	})

	t.Run("提供方故障答 503，日志不含秘密", func(t *testing.T) {
		fl := r.oauthBegin(f, "github", ghIdentity("1005", r.email()), false)
		f.mu.Lock()
		f.tokenStatus["github"] = 500
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.tokenStatus, "github")
			f.mu.Unlock()
		}()
		r.expect(r.oauthFinish(fl, false), 503, "unavailable")
		logs := r.log.String()
		if !strings.Contains(logs, "第三方登录提供方请求失败") {
			t.Fatalf("应记一条警告：%s", logs)
		}
		for _, secret := range []string{fakeGitHubSecret, fakeGoogleSecret, fl.code, fl.state} {
			if strings.Contains(logs, secret) {
				t.Fatalf("日志里不该有 %q", secret)
			}
		}
	})
}

// ---------------------------------------------------------------- 浏览器绑定

func TestOAuthBrowserBinding(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub)
	cleared := func(res response) bool {
		for _, c := range setCookies(res) {
			if c.Name == oauthCookie && c.Value == "" && c.MaxAge < 0 {
				return true
			}
		}
		return false
	}
	cookieFinish := func(fl oauthFlow, extra ...string) response {
		h := hdr("Origin", appOrigin)
		for _, c := range extra {
			h = h.add("Cookie", c)
		}
		return r.finishWith(fl, h)
	}

	t.Run("Cookie 传输：随机数对得上才登录，用后清除，答 csrfToken", func(t *testing.T) {
		fl := r.oauthBegin(f, "github", ghIdentity("2001", r.email()), true)
		res := r.oauthFinish(fl, true)
		r.expect(res, 200, "")
		s := parseSession(r, res)
		if s.csrf == "" || s.refresh != "" || s.cookies["__Host-armadra-cloud"] == "" || s.cookies["__Host-armadra-cloud-refresh"] == "" {
			t.Fatalf("Cookie 会话：%+v %s", s, res.body)
		}
		if !cleared(res) {
			t.Fatalf("用后要清掉 %s：%v", oauthCookie, res.header)
		}
		// 会话可用：带 Cookie 与 CSRF 调 me.get。
		me := r.do("GET", "/v1/me", nil, hdr("Cookie", s.cookieHeader("__Host-armadra-cloud")))
		r.expect(me, 200, "")
	})

	cases := []struct {
		name   string
		cookie func(fl oauthFlow) []string
	}{
		{"缺 Cookie", func(oauthFlow) []string { return nil }},
		{"随机数不符", func(fl oauthFlow) []string { return []string{oauthCookie + "=" + strings.Repeat("A", len(fl.nonce))} }},
		{"空值", func(oauthFlow) []string { return []string{oauthCookie + "="} }},
		{"重复的同名 Cookie", func(fl oauthFlow) []string {
			return []string{oauthCookie + "=" + fl.nonce, oauthCookie + "=" + fl.nonce}
		}},
		{"别的 Cookie 顶替", func(fl oauthFlow) []string { return []string{"__Host-other=" + fl.nonce} }},
	}
	for _, c := range cases {
		t.Run("Cookie 传输："+c.name+"答 401，state 作废", func(t *testing.T) {
			email := r.email()
			fl := r.oauthBegin(f, "github", ghIdentity("2002", email), true)
			res := cookieFinish(fl, c.cookie(fl)...)
			r.expect(res, 401, "credentials_invalid")
			if len(setCookies(res)) > 0 && !cleared(res) {
				t.Fatalf("失败时也只许清除：%v", res.header)
			}
			if r.accountCount(email) != 0 {
				t.Fatal("不该建账号")
			}
			// 被劫持的 state 不能再用：对的 Cookie 也不行。
			r.expect(r.oauthFinish(fl, true), 401, "credentials_invalid")
			if f.callsTo("token") != 1 { // 只有上一个子测试成功的那次
				t.Fatalf("绑定失败不该访问提供方：%d", f.callsTo("token"))
			}
		})
	}

	t.Run("Bearer 发起的 state 不能在 Cookie 传输下完成", func(t *testing.T) {
		fl := r.oauthBegin(f, "github", ghIdentity("2003", r.email()), false)
		if fl.nonce != "" {
			t.Fatal("Bearer 不发随机数")
		}
		r.expect(cookieFinish(fl), 401, "credentials_invalid")
		fl = r.oauthBegin(f, "github", ghIdentity("2003", r.email()), false)
		r.expect(cookieFinish(fl, oauthCookie+"=anything"), 401, "credentials_invalid")
	})

	t.Run("Cookie 发起的 state 在 Bearer 传输下也要对得上 Cookie", func(t *testing.T) {
		fl := r.oauthBegin(f, "github", ghIdentity("2004", r.email()), true)
		r.expect(r.finishWith(fl, nil), 401, "credentials_invalid")
	})

	t.Run("Bearer 发起、Bearer 完成：带 refreshToken、不带 csrfToken、不发 Cookie", func(t *testing.T) {
		res := r.oauthLogin(f, "github", ghIdentity("2005", r.email()), false)
		r.expect(res, 200, "")
		s := parseSession(r, res)
		if s.refresh == "" || s.csrf != "" || len(setCookies(res)) != 0 {
			t.Fatalf("Bearer 会话：%+v %v", s, res.header)
		}
		r.expect(r.do("GET", "/v1/me", nil, s.bearer()), 200, "")
	})
}

// ---------------------------------------------------------------- 邮箱必须已验证

func TestOAuthEmailVerified(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub, oauth.Google)
	cases := []struct {
		name     string
		provider string
		ident    fakeIdentity
		wantOK   bool
	}{
		{"GitHub 主邮箱已验证", "github", ghIdentity("3001", ""), true},
		{"GitHub 主邮箱未验证", "github", fakeIdentity{subject: "3002", login: "x", email: "", verified: false}, false},
		{"GitHub 只有非主邮箱已验证", "github", fakeIdentity{subject: "3003", login: "x", emails: []map[string]any{
			{"email": "secondary@example.com", "primary": false, "verified": true},
			{"email": "primary@example.com", "primary": true, "verified": false},
		}}, false},
		{"GitHub 没有任何邮箱", "github", fakeIdentity{subject: "3004", login: "x", emails: []map[string]any{}}, false},
		{"Google email_verified = true", "google", googleIdentity("g-3005", ""), true},
		{"Google email_verified = false", "google", fakeIdentity{subject: "g-3006", name: "G", email: "", verified: false}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			email := r.email()
			ident := c.ident
			if ident.email == "" && len(ident.emails) == 0 && c.name != "GitHub 没有任何邮箱" {
				ident.email = email
			}
			if !c.wantOK && len(ident.emails) == 0 && c.name != "GitHub 没有任何邮箱" {
				ident.email = email // 邮箱有，但提供方没验证
			}
			res := r.oauthLogin(f, c.provider, ident, false)
			if c.wantOK {
				r.expect(res, 200, "")
				return
			}
			r.expect(res, 403, "email_unverified")
			if r.accountCount(email) != 0 || r.identityCount(c.provider, ident.subject) != 0 {
				t.Fatal("邮箱未验证不该建账号或身份")
			}
		})
	}
}

// ---------------------------------------------------------------- 新建账号与已绑定登录

func TestOAuthNewAccountThenBoundLogin(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, false, oauth.GitHub, oauth.Google)
	email := strings.ToUpper(r.email())
	lower := strings.ToLower(email)

	fl := r.oauthBegin(f, "github", ghIdentity("4001", email), true)
	res := r.oauthFinish(fl, true)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	account := res.json["session"].(map[string]any)["account"].(map[string]any)
	if account["email"] != lower || account["emailVerified"] != true || account["displayName"] != "Octo Cat" {
		t.Fatalf("新账号：%s", res.body)
	}
	// 库里：邮箱已验证、没有口令、一个第三方身份（只有展示字段，没有令牌）。
	if r.queryString("SELECT password_json::json->>'kdf' FROM accounts WHERE account_id = $1", s.accountID) != "none" {
		t.Fatal("OAuth 建的账号没有口令")
	}
	if r.queryInt("SELECT count(*)::int FROM accounts WHERE account_id = $1 AND email_verified_at_ms IS NOT NULL AND status = 'active'", s.accountID) != 1 {
		t.Fatal("邮箱应已验证")
	}
	if got := r.queryString("SELECT external_id FROM account_credentials WHERE account_id = $1", s.accountID); got != "github:4001" {
		t.Fatalf("external_id：%q", got)
	}
	var public map[string]any
	if err := json.Unmarshal([]byte(r.queryString("SELECT public_json::text FROM account_credentials WHERE account_id = $1", s.accountID)), &public); err != nil ||
		!reflect.DeepEqual(public, map[string]any{"provider": "github", "email": lower, "login": "octo-4001", "name": "Octo Cat"}) {
		t.Fatalf("public_json：%v %v", public, err)
	}
	// 安全日志：注册、登录、第三方登录；第一台设备不发新设备提醒，也不发验证邮件。
	got := r.securityActions(s.accountID)
	sort.Strings(got)
	if want := []string{"security.login", "security.newDevice", "security.oauthLogin", "security.register"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("安全日志：%v", got)
	}
	r.noMail(lower)
	// 没有口令：任何口令都登不进来（含空串），答 credentials_invalid 而不是 500。
	for _, pw := range []string{password, "", "none"} {
		r.expect(r.login(lower, pw, nil), 401, "credentials_invalid")
	}
	// 令牌与授权码不落库、不进日志。
	dump := r.dump()
	for _, secret := range []string{"at-" + fl.code, fl.code, fl.state, fakeGitHubSecret} {
		if strings.Contains(dump, secret) || strings.Contains(r.log.String(), secret) {
			t.Fatalf("库或日志里不该有 %q", secret)
		}
	}
	if f.callsTo("token") != 1 || f.callsTo("user") != 1 || f.callsTo("emails") != 1 {
		t.Fatalf("访问令牌只用一次：token %d user %d emails %d", f.callsTo("token"), f.callsTo("user"), f.callsTo("emails"))
	}

	// 已绑定：同一主体再登录，仍是这个账号；提供方后来改了邮箱也不影响。
	again := r.oauthLogin(f, "github", ghIdentity("4001", r.email()), false)
	r.expect(again, 200, "")
	second := parseSession(r, again)
	if second.accountID != s.accountID || r.identityCount("github", "4001") != 1 {
		t.Fatalf("已绑定应登录同一个账号：%s %d", again.body, r.identityCount("github", "4001"))
	}
	if r.queryString("SELECT email FROM accounts WHERE account_id = $1", s.accountID) != lower {
		t.Fatal("账号邮箱不该被提供方的邮箱改写")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE account_id = $1 AND action = 'security.oauthLogin'", s.accountID); n != 2 {
		t.Fatalf("两次登录两条 security.oauthLogin：%d", n)
	}

	// 显示名缺省：Google 无名字时用邮箱本地部分；GitHub 无名字用登录名。
	gEmail := r.email()
	g := r.oauthLogin(f, "google", fakeIdentity{subject: "g-4002", email: gEmail, verified: true}, false)
	r.expect(g, 200, "")
	if name := g.json["session"].(map[string]any)["account"].(map[string]any)["displayName"]; name != strings.Split(gEmail, "@")[0] {
		t.Fatalf("Google 无名字：%v", name)
	}
	h := r.oauthLogin(f, "github", fakeIdentity{subject: "4003", login: "only-login", email: r.email(), verified: true}, false)
	r.expect(h, 200, "")
	if name := h.json["session"].(map[string]any)["account"].(map[string]any)["displayName"]; name != "only-login" {
		t.Fatalf("GitHub 无名字：%v", name)
	}
}

// ---------------------------------------------------------------- 同邮箱账号：on / off

func TestOAuthSameEmailOnAndOff(t *testing.T) {
	t.Parallel()

	t.Run("口令登录开着：409 account_exists，不合并", func(t *testing.T) {
		r, f := oauthRig(t, false, oauth.GitHub)
		for _, verified := range []bool{true, false} {
			email := r.email()
			local := r.registerBearer(email)
			if verified {
				r.exec("UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = $1", local.accountID)
			}
			res := r.oauthLogin(f, "github", ghIdentity(fmt.Sprintf("5001%t", verified)[:4], strings.ToUpper(email)), false)
			r.expect(res, 409, "conflict")
			if res.json["details"].(map[string]any)["reason"] != "account_exists" {
				t.Fatalf("details：%s", res.body)
			}
			if r.queryInt("SELECT count(*)::int FROM account_credentials WHERE account_id = $1", local.accountID) != 0 {
				t.Fatal("不该绑定")
			}
			// 原账号照常能用口令登录，没有新设备。
			r.expect(r.login(email, password, nil), 200, "")
			if r.queryInt("SELECT count(*)::int FROM devices WHERE subject = $1", local.accountID) != 2 {
				t.Fatal("OAuth 不该给本地账号建设备")
			}
		}
	})

	t.Run("口令登录关闭：已验证的同邮箱账号绑定并登录", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		email := r.email()
		id := r.insertAccount(email, true)
		res := r.oauthLogin(f, "github", ghIdentity("5101", strings.ToUpper(email)), true)
		r.expect(res, 200, "")
		s := parseSession(r, res)
		if s.accountID != id || r.identityCount("github", "5101") != 1 {
			t.Fatalf("应绑到已有账号：%s", res.body)
		}
		if r.queryString("SELECT password_json::json->>'kdf' FROM accounts WHERE account_id = $1", id) != "scrypt" {
			t.Fatal("已验证的账号保留原口令记录")
		}
		if got := r.securityActions(id); !contains(got, "security.oauthLogin") || contains(got, "security.register") {
			t.Fatalf("安全日志：%v", got)
		}
		// 以后用同一主体直接登录。
		r.expect(r.oauthLogin(f, "github", ghIdentity("5101", r.email()), false), 200, "")
		if r.queryInt("SELECT count(*)::int FROM accounts WHERE email = $1", email) != 1 {
			t.Fatal("不该多建账号")
		}
		// 同一账号再绑另一个提供方（Google）。
		g := r.oauthLogin(f, "google", googleIdentity("g-5101", email), false)
		r.expect(g, 200, "")
		if parseSession(r, g).accountID != id || r.queryInt("SELECT count(*)::int FROM account_credentials WHERE account_id = $1 AND kind = 'oauth'", id) != 2 {
			t.Fatal("两个提供方都绑在同一账号")
		}
	})

	t.Run("口令登录关闭：邮箱未验证的本地账号绑定后清口令、撤旧会话", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub)
		email := r.email()
		id := r.insertAccount(email, false)
		r.exec(`INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, rotation, created_at_ms, expires_at_ms)
		  VALUES ('cloud', 'squatter-device', $1, 'browser', '抢注者', 'h', 1, 1, 99999999999999)`, id)
		res := r.oauthLogin(f, "github", ghIdentity("5201", email), false)
		r.expect(res, 200, "")
		if parseSession(r, res).accountID != id {
			t.Fatal("应绑到已有账号")
		}
		if r.queryInt("SELECT count(*)::int FROM accounts WHERE account_id = $1 AND email_verified_at_ms IS NOT NULL", id) != 1 {
			t.Fatal("提供方证明了邮箱所有权，应记已验证")
		}
		if r.queryString("SELECT password_json::json->>'kdf' FROM accounts WHERE account_id = $1", id) != "none" {
			t.Fatal("先前的口令要清掉")
		}
		if r.queryInt("SELECT count(*)::int FROM devices WHERE device_id = 'squatter-device' AND revoked_at_ms IS NOT NULL") != 1 {
			t.Fatal("先前的设备要撤销")
		}
	})
}

// ---------------------------------------------------------------- 一个第三方身份只属于一个账号

func TestOAuthIdentityBelongsToOneAccount(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
	emailA, emailB := r.email(), r.email()
	a, b := r.insertAccount(emailA, true), r.insertAccount(emailB, true)

	r.expect(r.oauthLogin(f, "github", ghIdentity("6001", emailA), false), 200, "")
	// 同一主体带着 B 的邮箱来：登录的仍是 A（身份以主体为准），不会绑到 B。
	res := r.oauthLogin(f, "github", ghIdentity("6001", emailB), false)
	r.expect(res, 200, "")
	if parseSession(r, res).accountID != a || r.identityCount("github", "6001") != 1 {
		t.Fatalf("应登录 A：%s", res.body)
	}
	if r.queryInt("SELECT count(*)::int FROM account_credentials WHERE account_id = $1", b) != 0 {
		t.Fatal("B 不该有身份")
	}
	// A 已有 GitHub 身份，另一个 GitHub 主体带着 A 的邮箱来：409，不绑。
	conflict := r.oauthLogin(f, "github", ghIdentity("6002", emailA), false)
	r.expect(conflict, 409, "conflict")
	if conflict.json["details"].(map[string]any)["reason"] != "provider_linked" || r.identityCount("github", "6002") != 0 {
		t.Fatalf("同提供方第二个身份：%s", conflict.body)
	}
	// 数据库兜底：直接把已绑的身份绑给 B 会被唯一索引拒绝。
	if _, err := r.run.DB.Pool().Exec(context.Background(), `INSERT INTO account_credentials (tenant_id, credential_id, account_id, kind, external_id, created_at_ms)
	  VALUES ('cloud', 'dup', $1, 'oauth', 'github:6001', 1)`, b); err == nil {
		t.Fatal("同一身份绑到第二个账号应被拒绝")
	}
	// 另一个提供方的同名主体是另一个身份。
	g := r.oauthLogin(f, "google", googleIdentity("6001", emailB), false)
	r.expect(g, 200, "")
	if parseSession(r, g).accountID != b {
		t.Fatal("Google 的 6001 与 GitHub 的 6001 不是同一个身份")
	}
}

// ---------------------------------------------------------------- 停用与注册暂停

func TestOAuthDisabledAndPaused(t *testing.T) {
	t.Parallel()

	t.Run("已绑定的停用账号答 account_disabled，不建设备", func(t *testing.T) {
		r, f := oauthRig(t, false, oauth.GitHub)
		email := r.email()
		first := parseSession(r, r.oauthLogin(f, "github", ghIdentity("7001", email), false))
		r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", first.accountID)
		before := r.queryInt("SELECT count(*)::int FROM devices WHERE subject = $1", first.accountID)
		r.expect(r.oauthLogin(f, "github", ghIdentity("7001", email), false), 403, "account_disabled")
		r.expect(r.oauthLogin(f, "github", ghIdentity("7001", email), true), 403, "account_disabled")
		if r.queryInt("SELECT count(*)::int FROM devices WHERE subject = $1", first.accountID) != before {
			t.Fatal("停用账号不该建设备")
		}
	})

	t.Run("口令关闭时按邮箱匹配到停用账号：不绑定", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub)
		email := r.email()
		id := r.insertAccount(email, true)
		r.exec("UPDATE accounts SET status = 'disabled' WHERE account_id = $1", id)
		r.expect(r.oauthLogin(f, "github", ghIdentity("7002", email), false), 403, "account_disabled")
		if r.identityCount("github", "7002") != 0 {
			t.Fatal("停用账号不该被绑定")
		}
	})

	t.Run("注册暂停：不建新账号，已绑定的照常登录", func(t *testing.T) {
		r, f := oauthRig(t, false, oauth.GitHub)
		bound := parseSession(r, r.oauthLogin(f, "github", ghIdentity("7003", r.email()), false))
		r.exec(`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms) VALUES ('cloud', 'incident', '{"registrationsPaused": true}', 1)`)
		email := r.email()
		r.expect(r.oauthLogin(f, "github", ghIdentity("7004", email), false), 503, "registrations_paused")
		if r.accountCount(email) != 0 || r.identityCount("github", "7004") != 0 {
			t.Fatal("暂停时不该建账号")
		}
		again := r.oauthLogin(f, "github", ghIdentity("7003", r.email()), false)
		r.expect(again, 200, "")
		if parseSession(r, again).accountID != bound.accountID {
			t.Fatal("已绑定的账号不受注册暂停影响")
		}
	})
}

// ---------------------------------------------------------------- 口令开关

func TestPasswordAuthOff(t *testing.T) {
	t.Parallel()
	r, f := oauthRig(t, true, oauth.GitHub)
	email := r.email()
	id := r.insertAccount(email, true)

	t.Run("注册、普通登录、改口令答 403", func(t *testing.T) {
		r.expect(r.register(r.email(), nil), 403, "forbidden")
		r.expect(r.register(r.email(), hdr("Origin", appOrigin)), 403, "forbidden")
		// 正确与错误的口令、已注册与未注册的邮箱，答案一致，也不计失败。
		r.expect(r.login(email, password, nil), 403, "forbidden")
		r.expect(r.login(email, "wrong password 12345", nil), 403, "forbidden")
		r.expect(r.login(r.email(), password, nil), 403, "forbidden")
		if r.queryInt("SELECT failures FROM accounts WHERE account_id = $1", id) != 0 {
			t.Fatal("被拒的登录不该计失败")
		}
		s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("8001", email), false))
		r.expect(r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": "another long passphrase here 9"}, s.bearer()), 403, "forbidden")
	})

	t.Run("找回口令照常答成功但不发信", func(t *testing.T) {
		for _, address := range []string{email, r.email()} {
			r.expect(r.do("POST", "/v1/auth/password-reset/start", map[string]any{"email": address}, nil), 200, "")
		}
		time.Sleep(300 * time.Millisecond)
		for _, m := range r.mail.Messages() {
			if m.Subject == subjectReset {
				t.Fatalf("不该发找回口令的信：%s", m.To)
			}
		}
		if n := r.queryInt("SELECT count(*)::int FROM email_tokens WHERE account_id = $1", id); n != 0 {
			t.Fatalf("不该生成令牌：%d", n)
		}
	})

	t.Run("运营登录不受影响", func(t *testing.T) {
		opEmail := r.email()
		opID := r.insertAccount(opEmail, true)
		op := r.operatorFrom(opEmail, opID)
		s := r.operatorLogin(op, true)
		if s.accountID != opID || s.cookies["__Host-armadra-ops"] == "" {
			t.Fatalf("运营会话：%+v", s)
		}
		// 运营账号的普通登录同样 403。
		r.expect(r.login(opEmail, password, nil), 403, "forbidden")
	})
}

// 口令登录开着时找回口令照常发信（对照）。
func TestPasswordResetStillMailsWhenOn(t *testing.T) {
	t.Parallel()
	r, _ := oauthRig(t, false, oauth.GitHub)
	email := r.email()
	r.registerBearer(email)
	r.expect(r.do("POST", "/v1/auth/password-reset/start", map[string]any{"email": email}, nil), 200, "")
	r.waitSubject(email, subjectReset)
}

// ---------------------------------------------------------------- 能力位

func TestOAuthCapabilities(t *testing.T) {
	t.Parallel()
	rest := []any{"auth.device-code", "links.source-invite", "links.update", "links.scope", "me.stream", "teams", "invitations", "grants", "audit", "billing"}
	with := func(auth ...string) []any {
		out := []any{}
		for _, a := range auth {
			out = append(out, a)
		}
		return append(out, rest...)
	}
	cases := []struct {
		name      string
		off       bool
		providers []string
		want      []any
	}{
		{"只配 GitHub", false, []string{oauth.GitHub}, with("auth.password", "auth.password-change", "auth.oauth", "auth.oauth.github")},
		{"两个都配", false, []string{oauth.GitHub, oauth.Google}, with("auth.password", "auth.password-change", "auth.oauth", "auth.oauth.github", "auth.oauth.google")},
		{"口令关、配了 Google", true, []string{oauth.Google}, with("auth.oauth", "auth.oauth.google")},
		{"口令关、两个都配", true, []string{oauth.GitHub, oauth.Google}, with("auth.oauth", "auth.oauth.github", "auth.oauth.google")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, _ := oauthRig(t, c.off, c.providers...)
			res := r.do("GET", "/.well-known/armadra-platform", nil, nil)
			r.expect(res, 200, "")
			if !reflect.DeepEqual(res.json["capabilities"], c.want) {
				t.Fatalf("能力清单：%v", res.json["capabilities"])
			}
		})
	}
}

// ---------------------------------------------------------------- 解绑

func TestOAuthUnlink(t *testing.T) {
	t.Parallel()
	unlink := func(r *rig, s session, provider string) response {
		return r.do("DELETE", "/v1/auth/oauth/"+provider, nil, s.bearer())
	}

	t.Run("口令开着：有口令的账号可以解绑唯一的第三方", func(t *testing.T) {
		r, _ := oauthRig(t, false, oauth.GitHub, oauth.Google)
		s := r.registerBearer(r.email())
		r.bindOAuth(s.accountID, "github", "9001")
		r.expect(unlink(r, s, "github"), 200, "")
		if r.identityCount("github", "9001") != 0 || !contains(r.securityActions(s.accountID), "security.oauthUnlink") {
			t.Fatal("应解绑并记日志")
		}
		r.expect(unlink(r, s, "github"), 404, "not_found")
		r.expect(unlink(r, s, "google"), 404, "not_found")
		// 解绑后别的账号可以绑这个身份。
		other := r.registerBearer(r.email())
		r.bindOAuth(other.accountID, "github", "9001")
	})

	t.Run("没有口令的账号：留不下登录方式答 409", func(t *testing.T) {
		r, f := oauthRig(t, false, oauth.GitHub, oauth.Google)
		s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("9002", r.email()), false))
		r.expect(unlink(r, s, "github"), 409, "conflict")
		if r.identityCount("github", "9002") != 1 {
			t.Fatal("拒绝时不该解绑")
		}
		// 再绑一个 Google 后，可以解绑其中一个，最后一个不行。
		r.bindOAuth(s.accountID, "google", "g-9002")
		r.expect(unlink(r, s, "google"), 200, "")
		r.expect(unlink(r, s, "github"), 409, "conflict")
	})

	t.Run("口令关闭时只看第三方：口令不算数", func(t *testing.T) {
		r, f := oauthRig(t, true, oauth.GitHub, oauth.Google)
		email := r.email()
		r.insertAccount(email, true)
		s := parseSession(r, r.oauthLogin(f, "github", ghIdentity("9003", email), false))
		if r.queryString("SELECT password_json::json->>'kdf' FROM accounts WHERE account_id = $1", s.accountID) != "scrypt" {
			t.Fatal("前提：账号有口令记录")
		}
		r.expect(unlink(r, s, "github"), 409, "conflict")
		r.bindOAuth(s.accountID, "google", "g-9003")
		r.expect(unlink(r, s, "github"), 200, "")
		r.expect(unlink(r, s, "google"), 409, "conflict")
	})

	t.Run("需要会话", func(t *testing.T) {
		r, _ := oauthRig(t, false, oauth.GitHub)
		r.expect(r.do("DELETE", "/v1/auth/oauth/github", nil, nil), 401, "")
	})
}

// ---------------------------------------------------------------- 没配任何提供方

func TestOAuthWithoutProviders(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	r.expect(r.oauthStart("github", nil), 404, "not_found")
	r.expect(r.finishWith(oauthFlow{provider: "google", state: "s", code: "c"}, nil), 401, "credentials_invalid")
	s := r.registerBearer(r.email())
	r.expect(r.do("DELETE", "/v1/auth/oauth/github", nil, s.bearer()), 404, "not_found")
	res := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	for _, have := range res.json["capabilities"].([]any) {
		if strings.HasPrefix(have.(string), "auth.oauth") {
			t.Fatalf("没配提供方不报 %v", have)
		}
	}
}
