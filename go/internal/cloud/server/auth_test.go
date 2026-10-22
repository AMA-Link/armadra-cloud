package server_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func setCookies(res response) []*http.Cookie { return (&http.Response{Header: res.header}).Cookies() }

type acceptAll struct{ requests []core.VerifyRequest }

func (a *acceptAll) Verify(_ context.Context, request core.VerifyRequest) (core.Verdict, error) {
	a.requests = append(a.requests, request)
	return core.Verdict{OK: true}, nil
}

func TestPlatformInfoAndJWKS(t *testing.T) {
	r := newRig(t, rigOptions{})
	res := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(res, 200, "")
	if res.json["mode"] != "saas" || res.json["issuer"] != appOrigin || res.json["name"] != "测试控制面" {
		t.Fatalf("基本信息：%s", res.body)
	}
	protocol := res.json["protocol"].(map[string]any)
	if int(protocol["major"].(float64)) != gen.ProtocolMajor || int(protocol["minor"].(float64)) != gen.ProtocolMinor {
		t.Fatalf("协议版本：%v", protocol)
	}
	// 设计 §5.3 的能力清单，逐项照抄（不引用实现里的变量，防漂移）。
	want := []any{
		"auth.password", "auth.password-change", "auth.device-code", "links.source-invite", "links.update",
		"links.scope", "me.stream", "teams", "invitations", "grants", "audit", "billing",
	}
	if !reflect.DeepEqual(res.json["capabilities"], want) {
		t.Fatalf("能力清单：%v", res.json["capabilities"])
	}
	for _, absent := range []string{"auth.passkey", "auth.totp", "auth.oauth", "groups", "access-requests", "api-tokens", "links.team-join", "me.traffic", "sources.hosted", "orgs", "challenge.turnstile"} {
		for _, have := range res.json["capabilities"].([]any) {
			if have == absent {
				t.Fatalf("不该报 %s", absent)
			}
		}
	}
	if res.json["relay"].(map[string]any)["addressing"] != "subdomain" || res.json["webApp"] != nil || res.json["registrationsOpen"] != true {
		t.Fatalf("寻址、页面与注册开关：%s", res.body)
	}
	if _, has := res.json["challenge"]; has {
		t.Fatal("没配挑战就不报 challenge")
	}
	if res.header.Get("Armadra-Protocol") == "" || res.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("协议头与 CORS：%v", res.header)
	}

	jwks := r.do("GET", "/.well-known/jwks.json", nil, nil)
	r.expect(jwks, 200, "")
	keys := jwks.json["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["kid"] != "k1" || keys[0].(map[string]any)["kty"] != "OKP" {
		t.Fatalf("JWKS：%s", jwks.body)
	}
	if _, leaked := keys[0].(map[string]any)["d"]; leaked || strings.Contains(string(jwks.body), "PKCS") {
		t.Fatal("JWKS 里不该有私钥")
	}

	// 事故开关关闭注册：info 如实报。
	r.exec(`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms) VALUES ('cloud', 'incident', '{"registrationsPaused": true}', 1)`)
	res = r.do("GET", "/.well-known/armadra-platform", nil, nil)
	if res.json["registrationsOpen"] != false {
		t.Fatalf("注册关闭态：%s", res.body)
	}
	r.expect(r.register(r.email(), nil), 503, "registrations_paused")
}

func TestPlatformInfoReportsChallenge(t *testing.T) {
	r := newRig(t, rigOptions{turnstile: true, verifier: &acceptAll{}})
	res := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(res, 200, "")
	capabilities := res.json["capabilities"].([]any)
	if capabilities[len(capabilities)-1] != "challenge.turnstile" {
		t.Fatalf("配了挑战加 challenge.turnstile：%v", capabilities)
	}
	challenge := res.json["challenge"].(map[string]any)
	if challenge["provider"] != "turnstile" || challenge["siteKey"] != "site-key" || !reflect.DeepEqual(challenge["scope"], []any{"auth.login", "auth.changePassword"}) {
		t.Fatalf("挑战：%v", challenge)
	}
	// 登录要挑战：缺令牌 → challenge_required；带上 → 过。
	email := r.email()
	r.registerBearer(email)
	r.expect(r.login(email, password, nil), 400, "challenge_required")
	res = r.do("POST", "/v1/auth/login", map[string]any{
		"account": email, "password": password, "device": device, "challenge": map[string]any{"provider": "turnstile", "token": "ok"},
	}, nil)
	r.expect(res, 200, "")
}

func TestRegister(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	res := r.register(strings.ToUpper(email), nil)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	account := res.json["session"].(map[string]any)["account"].(map[string]any)
	if account["email"] != email || account["emailVerified"] != false || account["displayName"] != "测试用户" || s.refresh == "" || s.csrf != "" {
		t.Fatalf("Bearer 传输：%s", res.body)
	}
	if len(setCookies(res)) != 0 {
		t.Fatal("Bearer 传输不下发 Cookie")
	}

	// 邮箱转小写后唯一。
	r.expect(r.register(email, nil), 409, "email_taken")
	r.expect(r.register(strings.ToUpper(email), nil), 409, "email_taken")

	// 库里：账号 id 是 uuid，name 等于 account_id，邮箱小写，口令只存派生。
	var id, name, stored string
	if err := r.run.DB.Pool().QueryRow(context.Background(),
		"SELECT account_id, name, email FROM accounts WHERE email = $1", email).Scan(&id, &name, &stored); err != nil {
		t.Fatal(err)
	}
	if id != s.accountID || name != id || stored != email || len(id) != 36 || id[14] != '4' {
		t.Fatalf("账号行：%q %q %q", id, name, stored)
	}

	// Cookie 传输：不带 refreshToken，两枚 Cookie，带 csrfToken。
	cookie := r.registerCookie(r.email())
	if cookie.refresh != "" || cookie.csrf == "" || cookie.cookies["__Host-armadra-cloud"] == "" || cookie.cookies["__Host-armadra-cloud-refresh"] == "" {
		t.Fatalf("Cookie 传输：%+v", cookie)
	}
	var transport string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT transport FROM devices WHERE device_id = $1", cookie.deviceID).Scan(&transport); err != nil || transport != "cookie" {
		t.Fatalf("设备传输：%q %v", transport, err)
	}
}

func TestRegisterPasswordPolicyAndInput(t *testing.T) {
	r := newRig(t, rigOptions{})
	post := func(email, pw, name string) response {
		return r.do("POST", "/v1/auth/register", map[string]any{"email": email, "password": pw, "displayName": name, "device": device}, nil)
	}
	email := r.email()
	local, _, _ := strings.Cut(email, "@")
	r.expect(post(email, "short", "测试用户"), 400, "password_too_short")
	r.expect(post(email, "password1234", "测试用户"), 400, "password_too_common")
	r.expect(post(email, "xx-"+local+"-lantern", "测试用户"), 400, "password_contains_name")
	r.expect(post(email, "xx-测试用户-lantern-77", "测试用户"), 400, "password_contains_name")
	// 入参形状：不是邮箱、显示名空白、缺字段。
	r.expect(post("not-an-email", password, "测试用户"), 400, "bad_request")
	r.expect(post(email, password, "   "), 400, "bad_request")
	r.expect(r.do("POST", "/v1/auth/register", map[string]any{"email": email}, nil), 400, "bad_request")
	// 都没成功过：邮箱还能用。
	r.expect(post(email, password, "测试用户"), 200, "")
}

func TestRegisterRateLimitPerIP(t *testing.T) {
	r := newRig(t, rigOptions{})
	for i := 0; i < 10; i++ {
		r.expect(r.register(r.email(), nil), 200, "")
	}
	res := r.register(r.email(), nil)
	r.expect(res, 429, "rate_limited")
	if res.header.Get("Retry-After") == "" || res.json["details"].(map[string]any)["retryAfterMs"] == nil {
		t.Fatalf("429 带重试等待：%v %s", res.header, res.body)
	}
}

func TestLogin(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)

	res := r.login(strings.ToUpper(email), password, nil)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	if s.refresh == "" || s.csrf != "" {
		t.Fatalf("Bearer 登录：%s", res.body)
	}
	cookie := r.login(email, password, hdr("Origin", appOrigin))
	r.expect(cookie, 200, "")
	cs := parseSession(r, cookie)
	if cs.refresh != "" || cs.csrf == "" || len(setCookies(cookie)) != 2 {
		t.Fatalf("Cookie 登录：%s %v", cookie.body, cookie.header)
	}

	// 口令不对与账号不存在：同样的状态、码、消息。
	wrong := r.login(email, "wrong-password-123", nil)
	unknown := r.login("nobody-"+email, password, nil)
	r.expect(wrong, 401, "credentials_invalid")
	r.expect(unknown, 401, "credentials_invalid")
	if wrong.json["message"] != unknown.json["message"] || len(wrong.json) != len(unknown.json) {
		t.Fatalf("枚举：形状要一样：%s / %s", wrong.body, unknown.body)
	}
}

func TestLoginLockoutDisabledAndRateLimit(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	for i := 0; i < 5; i++ {
		r.expect(r.login(email, "wrong-password-123", nil), 401, "credentials_invalid")
	}
	locked := r.login(email, password, nil)
	r.expect(locked, 429, "account_locked")
	if locked.json["details"].(map[string]any)["retryAfterMs"] == nil || locked.header.Get("Retry-After") == "" {
		t.Fatalf("锁定带重试等待：%s", locked.body)
	}
	r.clock.Advance(2 * time.Minute)
	r.expect(r.login(email, password, nil), 200, "")

	// 停用：口令对了才答 account_disabled；口令不对仍是 credentials_invalid，不泄露停用。
	r.exec("UPDATE accounts SET status = 'disabled' WHERE email = $1", email)
	r.expect(r.login(email, "wrong-password-123", nil), 401, "credentials_invalid")
	r.expect(r.login(email, password, nil), 403, "account_disabled")

	// login.acct：同一邮箱每分钟 20 次。
	other := r.email()
	var last response
	for i := 0; i < 21; i++ {
		last = r.login(other, "wrong-password-123", nil)
	}
	r.expect(last, 429, "rate_limited")
}

func TestRefreshBearerRotatesAndReplayRevokes(t *testing.T) {
	r := newRig(t, rigOptions{})
	s := r.registerBearer(r.email())
	// 先给这台设备记两枚已签令牌，重放时应被撤销。
	exp := r.clock.Ms() + 3_600_000
	_ = r.run.Relay.IndexIssued(context.Background(), relayctl.Issued{Subject: s.accountID, SourceID: "srcA", DeviceID: s.deviceID, JTI: "r." + s.deviceID + ".srcA.x", ExpMs: exp})

	res := r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": s.refresh}, nil)
	r.expect(res, 200, "")
	next := parseSession(r, res)
	if next.refresh == "" || next.refresh == s.refresh || next.deviceID != s.deviceID || next.csrf != "" {
		t.Fatalf("刷新要旋转刷新令牌：%s", res.body)
	}
	if r.do("GET", "/v1/me", nil, next.bearer()).status != 200 {
		t.Fatal("新访问令牌可用")
	}
	// 旧刷新令牌再来是重放：撤销整台设备。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": s.refresh}, nil), 401, "session_revoked")
	r.expect(r.do("GET", "/v1/me", nil, next.bearer()), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": next.refresh}, nil), 401, "session_revoked")
	if !r.run.Relay.IsRevoked("r." + s.deviceID + ".srcA.x") {
		t.Fatal("重放撤销整台设备，要经撤销钩子撤已签令牌")
	}
	cmds := r.run.Relay.Commands()
	if len(cmds) != 1 || cmds[0].Type != relayctl.CommandKick || cmds[0].SourceID != "srcA" || cmds[0].DeviceID != s.deviceID {
		t.Fatalf("要到有令牌的主机去踢：%+v", cmds)
	}
	// 没带刷新令牌、乱编的令牌：session_revoked。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, nil), 401, "session_revoked")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": "deadbeef.xxx"}, nil), 401, "session_revoked")
}

func TestRefreshCookieNeedsOriginNotCSRFAndKeepsCSRFValue(t *testing.T) {
	r := newRig(t, rigOptions{})
	s := r.registerCookie(r.email())
	refreshCookie := s.cookieHeader("__Host-armadra-cloud-refresh")
	// 免 CSRF，但要 Origin。
	res := r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", refreshCookie))
	r.expect(res, 200, "")
	next := parseSession(r, res)
	if next.refresh != "" || next.csrf != s.csrf || next.cookies["__Host-armadra-cloud-refresh"] == s.cookies["__Host-armadra-cloud-refresh"] {
		t.Fatalf("两枚 Cookie 一起换新，csrfToken 同设备不变：%s", res.body)
	}
	if len(setCookies(res)) != 2 {
		t.Fatal("刷新要下发两枚 Cookie")
	}
	// 旧的刷新 Cookie 值再来是重放。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", refreshCookie)), 401, "session_revoked")

	// 没有 Origin / Origin 重复 / 别的来源：403。
	s2 := r.registerCookie(r.email())
	cookie := s2.cookieHeader("__Host-armadra-cloud-refresh")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Cookie", cookie)), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Cookie", cookie, "Origin", appOrigin).add("Origin", appOrigin)), 403, "forbidden")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Cookie", cookie, "Origin", "https://abc.src.localhost")), 403, "forbidden")
	// Cookie 设备的刷新令牌放进请求体（Bearer 路径）：对不上传输，拒绝。
	value := s2.cookies["__Host-armadra-cloud-refresh"]
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": value}, nil), 401, "session_revoked")
	// 反过来：Bearer 设备的刷新令牌不能走 Cookie 路径（没有对应的 Cookie 就是 session_revoked）。
	b := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", "__Host-armadra-cloud-refresh="+b.refresh)), 401, "session_revoked")
	// scope: operator 要运营的刷新 Cookie，普通会话的不行。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"scope": "operator"}, hdr("Origin", appOrigin, "Cookie", cookie)), 401, "session_revoked")
}

func TestLogoutBearerAndCookie(t *testing.T) {
	r := newRig(t, rigOptions{})
	b := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, b.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, b.bearer()), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, b.bearer()), 401, "unauthenticated")

	c := r.registerCookie(r.email())
	// Cookie 登出要 CSRF。
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, hdr("Origin", appOrigin, "Cookie", c.cookieHeader("__Host-armadra-cloud"))), 403, "forbidden")
	res := r.do("POST", "/v1/auth/logout", map[string]any{}, c.write())
	r.expect(res, 200, "")
	cleared := map[string]bool{}
	for _, cookie := range setCookies(res) {
		if cookie.MaxAge == -1 && cookie.Value == "" && cookie.Path == "/" && cookie.Secure && cookie.HttpOnly {
			cleared[cookie.Name] = true
		}
	}
	if !cleared["__Host-armadra-cloud"] || !cleared["__Host-armadra-cloud-refresh"] || len(cleared) != 2 {
		t.Fatalf("登出两枚都清：%v", res.header.Values("Set-Cookie"))
	}
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud"))), 401, "unauthenticated")
	// Bearer 会话带 scope: operator 登出：不是运营会话，403。
	b2 := r.registerBearer(r.email())
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{"scope": "operator"}, b2.bearer()), 403, "forbidden")
}

func TestChangePassword(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	a := r.registerBearer(email)
	other := parseSession(r, r.login(email, password, nil))
	// 给另一台设备记一枚已签令牌：改口令后应被撤销、并到主机去踢。
	exp := r.clock.Ms() + 3_600_000
	_ = r.run.Relay.IndexIssued(context.Background(), relayctl.Issued{Subject: a.accountID, SourceID: "srcA", DeviceID: other.deviceID, JTI: "r." + other.deviceID + ".srcA.x", ExpMs: exp})
	_ = r.run.Relay.IndexIssued(context.Background(), relayctl.Issued{Subject: a.accountID, SourceID: "srcA", DeviceID: a.deviceID, JTI: "r." + a.deviceID + ".srcA.x", ExpMs: exp})

	const next = "kestrel-ember-orchard-77"
	change := func(old, fresh string) response {
		return r.do("POST", "/v1/auth/password", map[string]any{"password": old, "newPassword": fresh}, a.bearer())
	}
	r.expect(change("wrong-password-123", next), 401, "credentials_invalid")
	r.expect(change(password, "short"), 400, "password_too_short")
	local, _, _ := strings.Cut(email, "@")
	r.expect(change(password, "xx-"+local+"-lantern"), 400, "password_contains_name")
	res := change(password, next)
	r.expect(res, 200, "")
	fresh := parseSession(r, res)
	if fresh.deviceID != a.deviceID || fresh.refresh == "" || fresh.refresh == a.refresh {
		t.Fatalf("当前设备留下并换新刷新令牌：%s", res.body)
	}
	// 当前设备继续可用；本人其他设备被撤销；旧刷新令牌作废。
	r.expect(r.do("GET", "/v1/me", nil, a.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, other.bearer()), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": a.refresh}, nil), 401, "session_revoked")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": fresh.refresh}, nil), 200, "")
	r.expect(r.login(email, password, nil), 401, "credentials_invalid")
	r.expect(r.login(email, next, nil), 200, "")
	// 撤销钩子：另一台设备的令牌撤了、到主机去踢；当前设备的令牌还在。
	if !r.run.Relay.IsRevoked("r."+other.deviceID+".srcA.x") || r.run.Relay.IsRevoked("r."+a.deviceID+".srcA.x") {
		t.Fatal("只撤其他设备的已签令牌")
	}
	var kicks []relayctl.Command
	for _, c := range r.run.Relay.Commands() {
		if c.Type == relayctl.CommandKick {
			kicks = append(kicks, c)
		}
	}
	if len(kicks) != 1 || kicks[0].DeviceID != other.deviceID || kicks[0].SourceID != "srcA" {
		t.Fatalf("按设备踢：%+v", kicks)
	}
}

func TestChangePasswordCookieSessionRotatesCookies(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	res := r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": "kestrel-ember-orchard-77"}, c.write())
	r.expect(res, 200, "")
	next := parseSession(r, res)
	if next.refresh != "" || next.csrf != c.csrf || len(setCookies(res)) != 2 {
		t.Fatalf("Cookie 会话：下发两枚新 Cookie，不带 refreshToken，csrfToken 不变：%s", res.body)
	}
	// 缺 CSRF 的改口令请求被挡在处理器之外，口令不变。
	r.expect(r.do("POST", "/v1/auth/password", map[string]any{"password": "kestrel-ember-orchard-77", "newPassword": "heron-quartz-meadow-99"}, hdr("Origin", appOrigin, "Cookie", next.cookieHeader("__Host-armadra-cloud"))), 403, "forbidden")
}

func TestStepUp(t *testing.T) {
	r := newRig(t, rigOptions{})
	s := r.registerBearer(r.email())
	step := func(body map[string]any) response { return r.do("POST", "/v1/auth/step-up", body, s.bearer()) }
	r.expect(step(map[string]any{}), 400, "bad_request")
	r.expect(step(map[string]any{"totpCode": "123456"}), 401, "mfa_invalid")
	r.expect(step(map[string]any{"password": "wrong-password-123"}), 401, "credentials_invalid")
	res := step(map[string]any{"password": password})
	r.expect(res, 200, "")
	until := int64(res.json["elevatedUntilMs"].(float64))
	if until != r.clock.Ms()+10*60*1000 {
		t.Fatalf("提权 10 分钟：%d", until)
	}
	// 过期后失效（可控时钟）。
	auth := core.NewAuthenticator(r.run.Core)
	caller, err := auth.AuthenticateToken(context.Background(), s.access, core.TransportBearer)
	if err != nil {
		t.Fatal(err)
	}
	if !caller.Elevated(r.clock.Ms()) || caller.ElevatedUntilMs != until {
		t.Fatalf("提权有效：%+v", caller)
	}
	r.clock.Advance(10*time.Minute + time.Second)
	if caller.Elevated(r.clock.Ms()) {
		t.Fatal("10 分钟后提权失效")
	}
	// 没有会话：401。
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"password": password}, nil), 401, "unauthenticated")
}

func TestStepUpFailuresCountTowardsRateLimit(t *testing.T) {
	r := newRig(t, rigOptions{})
	s := r.registerBearer(r.email())
	step := func(pw string) response {
		return r.do("POST", "/v1/auth/step-up", map[string]any{"password": pw}, s.bearer())
	}
	for i := 0; i < 5; i++ {
		r.expect(step("wrong-password-123"), 401, "credentials_invalid")
	}
	// 第 5 次失败起账号锁定，但每次尝试仍计入按设备的限流。
	for i := 5; i < 10; i++ {
		r.expect(step(password), 429, "account_locked")
	}
	r.expect(step(password), 429, "rate_limited")
}

func TestMeGetUpdateDevices(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	s := r.registerBearer(email)
	res := r.do("GET", "/v1/me", nil, s.bearer())
	r.expect(res, 200, "")
	if res.json["accountId"] != s.accountID || res.json["email"] != email || res.json["emailVerified"] != false || res.json["mfaEnabled"] != false {
		t.Fatalf("me.get：%s", res.body)
	}
	if !reflect.DeepEqual(res.json["teams"], []any{}) || !reflect.DeepEqual(res.json["scopes"], []any{}) || !reflect.DeepEqual(res.json["orgs"], []any{}) {
		t.Fatalf("没有团队与范围：%s", res.body)
	}
	if _, has := res.json["csrfToken"]; has {
		t.Fatal("Bearer 会话不报 csrfToken")
	}

	// 团队：成员关系 ⋈ 团队，已删除的排除；scopes 来自账号行。
	r.execTx(
		[2]any{`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms) VALUES
		  ('cloud', 't1', 'one', '一队', '{}', $1, 1, 1), ('cloud', 't2', 'two', '二队', '{}', $1, 1, 1)`, []any{s.accountID}},
		[2]any{`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES
		  ('cloud', 't1', $1, 'owner', 1), ('cloud', 't2', $1, 'owner', 2)`, []any{s.accountID}},
		[2]any{"UPDATE teams SET deleted_at_ms = 5, purge_at_ms = 9 WHERE team_id = 't2'", nil},
	)
	r.exec("UPDATE accounts SET scopes = '{operator}' WHERE account_id = $1", s.accountID)
	res = r.do("GET", "/v1/me", nil, s.bearer())
	r.expect(res, 200, "")
	teams := res.json["teams"].([]any)
	if len(teams) != 1 || teams[0].(map[string]any)["teamId"] != "t1" || teams[0].(map[string]any)["role"] != "owner" || teams[0].(map[string]any)["slug"] != "one" {
		t.Fatalf("团队：%s", res.body)
	}
	if !reflect.DeepEqual(res.json["scopes"], []any{"operator"}) {
		t.Fatalf("scopes：%s", res.body)
	}

	// me.update：显示名；空白拒绝。
	res = r.do("PATCH", "/v1/me", map[string]any{"displayName": "  新名字  "}, s.bearer())
	r.expect(res, 200, "")
	if res.json["displayName"] != "新名字" {
		t.Fatalf("me.update：%s", res.body)
	}
	r.expect(r.do("PATCH", "/v1/me", map[string]any{"displayName": "   "}, s.bearer()), 400, "bad_request")
	r.expect(r.do("PATCH", "/v1/me", map[string]any{"displayName": ""}, s.bearer()), 400, "bad_request")
	if got := r.do("GET", "/v1/me", nil, s.bearer()); got.json["displayName"] != "新名字" {
		t.Fatal("显示名应已持久化")
	}

	// 设备列表：当前设备标 current。
	second := parseSession(r, r.login(email, password, nil))
	res = r.do("GET", "/v1/me/devices", nil, s.bearer())
	r.expect(res, 200, "")
	devices := res.json["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("两台设备：%s", res.body)
	}
	current := 0
	for _, d := range devices {
		if d.(map[string]any)["current"] == true {
			current++
			if d.(map[string]any)["deviceId"] != s.deviceID {
				t.Fatalf("current 应是当前设备：%s", res.body)
			}
		}
	}
	if current != 1 {
		t.Fatalf("恰好一台 current：%s", res.body)
	}

	// 撤销另一台：它的令牌立即失效；撤销不存在的、别人的设备 404。
	r.expect(r.do("DELETE", "/v1/me/devices/"+second.deviceID, nil, s.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, second.bearer()), 401, "unauthenticated")
	r.expect(r.do("DELETE", "/v1/me/devices/"+second.deviceID, nil, s.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", "/v1/me/devices/00000000000000000000000000000000", nil, s.bearer()), 404, "not_found")
	stranger := r.registerBearer(r.email())
	r.expect(r.do("DELETE", "/v1/me/devices/"+stranger.deviceID, nil, s.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/me", nil, stranger.bearer()), 200, "")
}

func TestMeCookieSessionCSRFAndSelfRevoke(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	res := r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud")))
	r.expect(res, 200, "")
	if res.json["csrfToken"] != c.csrf {
		t.Fatalf("me.get 取回 csrfToken：%s", res.body)
	}
	// 撤销自己这一台：两枚 Cookie 一起清。
	res = r.do("DELETE", "/v1/me/devices/"+c.deviceID, nil, c.write())
	r.expect(res, 200, "")
	if len(setCookies(res)) != 2 {
		t.Fatalf("撤销自己要清 Cookie：%v", res.header.Values("Set-Cookie"))
	}
}

// 锁定不暴露邮箱有没有注册：未注册的邮箱同样计失败、同样在第 6 次答 account_locked，形状一致。
func TestUnregisteredEmailLocksLikeRegistered(t *testing.T) {
	r := newRig(t, rigOptions{})
	registered := r.email()
	r.registerBearer(registered)
	unregistered := "ghost-" + r.email()
	for i := 0; i < 5; i++ {
		r.expect(r.login(registered, "wrong-password-123", nil), 401, "credentials_invalid")
		r.expect(r.login(unregistered, "wrong-password-123", nil), 401, "credentials_invalid")
	}
	// 已注册的账号用对口令也被锁着；未注册的同样答 account_locked。
	a := r.login(registered, password, nil)
	b := r.login(unregistered, password, nil)
	r.expect(a, 429, "account_locked")
	r.expect(b, 429, "account_locked")
	if len(a.json) != len(b.json) || a.json["message"] != b.json["message"] {
		t.Fatalf("形状要一致：%s / %s", a.body, b.body)
	}
	for _, res := range []response{a, b} {
		details, ok := res.json["details"].(map[string]any)
		if !ok || details["retryAfterMs"] == nil || res.header.Get("Retry-After") == "" {
			t.Fatalf("锁定带重试等待：%s", res.body)
		}
		if wait := details["retryAfterMs"].(float64); wait <= 0 || wait > 60_000 {
			t.Fatalf("首次锁定约 1 分钟：%v", wait)
		}
	}
	// 过了锁定期，未注册的邮箱同样解锁、可以再试。
	r.clock.Advance(2 * time.Minute)
	r.expect(r.login(unregistered, "wrong-password-123", nil), 401, "credentials_invalid")
	r.expect(r.login(registered, password, nil), 200, "")
}

// 访问 Cookie 过期后，用刷新 Cookie 也能登出并撤销设备；无论成败都清两枚 Cookie。
func TestLogoutWithExpiredAccessCookieUsesRefreshCookie(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	r.clock.Advance(20 * time.Minute) // 访问令牌 15 分钟到期
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud"))), 401, "unauthenticated")

	cookies := c.cookieHeader("__Host-armadra-cloud-refresh") // 只有刷新 Cookie（访问 Cookie 已过期被浏览器丢掉）
	cleared := func(res response) bool {
		names := map[string]bool{}
		for _, cookie := range setCookies(res) {
			names[cookie.Name] = cookie.MaxAge == -1
		}
		return len(names) == 2 && names["__Host-armadra-cloud"] && names["__Host-armadra-cloud-refresh"]
	}
	// 缺 Origin、缺 / 错 CSRF：拒绝，设备不撤销，Cookie 照清。
	for name, h := range map[string]headers{
		"缺 Origin": hdr("Cookie", cookies, "X-Armadra-CSRF", c.csrf),
		"缺 CSRF":   hdr("Cookie", cookies, "Origin", appOrigin),
		"错 CSRF":   hdr("Cookie", cookies, "Origin", appOrigin, "X-Armadra-CSRF", "wrong"),
	} {
		res := r.do("POST", "/v1/auth/logout", map[string]any{}, h)
		r.expect(res, 403, "forbidden")
		if !cleared(res) {
			t.Fatalf("%s：失败也要清 Cookie：%v", name, res.header.Values("Set-Cookie"))
		}
	}
	other := r.registerCookie(r.email())
	res := r.do("POST", "/v1/auth/logout", map[string]any{}, hdr("Cookie", cookies, "Origin", appOrigin, "X-Armadra-CSRF", other.csrf))
	r.expect(res, 403, "forbidden") // 别的设备的 CSRF 值
	res = r.do("POST", "/v1/auth/logout", map[string]any{}, hdr("Cookie", cookies, "Origin", appOrigin, "X-Armadra-CSRF", c.csrf))
	r.expect(res, 200, "")
	if !cleared(res) {
		t.Fatalf("登出要清两枚：%v", res.header.Values("Set-Cookie"))
	}
	// 设备已撤销：刷新 Cookie 不能再换新。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", cookies)), 401, "session_revoked")
	// 什么凭据都没有的登出：401，同样不留 Cookie。
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, hdr("Origin", appOrigin)), 401, "unauthenticated")
}
