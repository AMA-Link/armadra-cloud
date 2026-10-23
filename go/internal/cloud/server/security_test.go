package server_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// stubHandlers 是测试挂上的假处理器：只做认证，用来检查凭据闸对 admin.* 与别的过程的隔离。
func stubHandlers() map[string]api.Handler {
	h := func(ctx context.Context, call *api.Call) (any, error) {
		if _, err := call.Caller(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	return map[string]api.Handler{"admin.tenants": h}
}

// expectedCSRF 按设计 §5.2 独立重算：HKDF-SHA256(主密钥, info "armadra-cloud/csrf") 派生钥，
// HMAC-SHA256(钥, "armadra/csrf\0" + deviceId)，base64url 无填充。
func expectedCSRF(t *testing.T, deviceID string) string {
	t.Helper()
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i%16) * 0x11
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte("armadra-cloud/csrf")), key); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("armadra/csrf\x00" + deviceID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// operator 造一个运营账号（scopes 含 operator）和它的运营会话：1 小时绝对寿命，传输由参数定。
func (r *rig) operator(transport core.Transport) (session, string) {
	r.t.Helper()
	email := r.email()
	base := r.registerBearer(email)
	r.exec("UPDATE accounts SET scopes = '{operator}' WHERE account_id = $1", base.accountID)
	nowMs := r.clock.Ms()
	dev, refresh := core.NewDevice(base.accountID, core.DeviceInput{Platform: "browser", Name: "运营"}, transport, core.SessionScopeOperator, nowMs, core.OperatorSessionTTL)
	if err := r.run.Core.Ports.Devices.Insert(context.Background(), r.run.Core.TenantID, dev, nowMs); err != nil {
		r.t.Fatal(err)
	}
	sess, err := core.SessionFor(context.Background(), r.run.Core, dev, refresh)
	if err != nil {
		r.t.Fatal(err)
	}
	out := session{
		access: sess.AccessToken, refresh: refresh, deviceID: dev.DeviceID, accountID: base.accountID,
		cookies: map[string]string{"__Host-armadra-ops": sess.AccessToken, "__Host-armadra-ops-refresh": refresh},
		csrf:    expectedCSRF(r.t, dev.DeviceID), expiresAtMs: dev.ExpiresAtMs,
	}
	return out, email
}

func (s session) opsWrite() headers {
	return hdr("Origin", appOrigin, "X-Armadra-CSRF", s.csrf, "Cookie", s.cookieHeader("__Host-armadra-ops"))
}

func TestCSRFValueDerivation(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	if c.csrf != expectedCSRF(t, c.deviceID) {
		t.Fatalf("csrfToken 不是 HMAC(HKDF(主密钥), deviceId)：%s", c.csrf)
	}
	other := r.registerCookie(r.email())
	if other.csrf == c.csrf {
		t.Fatal("不同设备的值不同")
	}
	// 刷新后同一设备同一个值。
	res := r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", c.cookieHeader("__Host-armadra-cloud-refresh")))
	r.expect(res, 200, "")
	if res.json["csrfToken"] != c.csrf {
		t.Fatal("刷新返回同一个 csrfToken")
	}
}

func TestCSRFOnCookieWrites(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	patch := func(h headers) response {
		return r.do("PATCH", "/v1/me", map[string]any{"displayName": "改名"}, h)
	}
	cookie := c.cookieHeader("__Host-armadra-cloud")
	// 缺头 / 错值 / 别的设备的值 / 空值 → 403。
	r.expect(patch(hdr("Origin", appOrigin, "Cookie", cookie)), 403, "forbidden")
	r.expect(patch(hdr("Origin", appOrigin, "Cookie", cookie, "X-Armadra-CSRF", "wrong")), 403, "forbidden")
	r.expect(patch(hdr("Origin", appOrigin, "Cookie", cookie, "X-Armadra-CSRF", "")), 403, "forbidden")
	another := r.registerCookie(r.email())
	r.expect(patch(hdr("Origin", appOrigin, "Cookie", cookie, "X-Armadra-CSRF", another.csrf)), 403, "forbidden")
	r.expect(patch(c.write()), 200, "")
	// 读请求不要 CSRF 与 Origin。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", cookie)), 200, "")
	// Bearer 会话不用 CSRF。
	b := r.registerBearer(r.email())
	r.expect(r.do("PATCH", "/v1/me", map[string]any{"displayName": "改名"}, b.bearer()), 200, "")
	// auth.refresh 不要求 CSRF（已在刷新用例里验过 Origin 的要求）。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", c.cookieHeader("__Host-armadra-cloud-refresh"))), 200, "")
}

func TestOriginChecks(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	patch := func(origins ...string) response {
		h := hdr("X-Armadra-CSRF", c.csrf, "Cookie", c.cookieHeader("__Host-armadra-cloud"))
		for _, origin := range origins {
			h = h.add("Origin", origin)
		}
		return r.do("PATCH", "/v1/me", map[string]any{"displayName": "改名"}, h)
	}
	r.expect(patch(), 403, "forbidden")                                     // 缺失
	r.expect(patch(appOrigin, appOrigin), 403, "forbidden")                 // 重复
	r.expect(patch("https://abc.src.localhost"), 403, "forbidden")          // 兄弟子域（同站，SameSite 挡不住）
	r.expect(patch("http://127.0.0.1:8100.evil.example"), 403, "forbidden") // 前缀相同的别的来源
	r.expect(patch("null"), 403, "forbidden")                               // 沙箱 iframe
	r.expect(patch(strings.ToUpper(appOrigin)), 403, "forbidden")           // 必须逐字相等
	r.expect(patch(appOrigin), 200, "")
	// 登录类过程：Origin 不是控制台来源就走 Bearer 传输（体里给 refreshToken、不下发 Cookie）。
	res := r.login(r.email(), password, hdr("Origin", "https://abc.src.localhost"))
	r.expect(res, 401, "credentials_invalid")
	email := r.email()
	r.registerBearer(email)
	res = r.login(email, password, hdr("Origin", "https://abc.src.localhost"))
	r.expect(res, 200, "")
	if parseSession(r, res).refresh == "" || len(setCookies(res)) != 0 {
		t.Fatalf("别的来源登录走 Bearer：%s", res.body)
	}
	// 同源但带 Authorization：也是 Bearer。
	b := r.registerBearer(r.email())
	res = r.login(email, password, hdr("Origin", appOrigin, "Authorization", "Bearer "+b.access))
	if res.status != 200 || len(setCookies(res)) != 0 {
		t.Fatalf("同源但带 Authorization 走 Bearer：%d %s", res.status, res.body)
	}
}

func TestCORSNeverAllowsCredentials(t *testing.T) {
	r := newRig(t, rigOptions{})
	for _, origin := range []string{"https://evil.example", appOrigin, "https://abc.src.localhost"} {
		pre := r.do("OPTIONS", "/v1/me", nil, hdr("Origin", origin, "Access-Control-Request-Method", "GET"))
		if pre.status != 204 || pre.header.Get("Access-Control-Allow-Origin") != "*" || pre.header.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("预检（%s）：%d %v", origin, pre.status, pre.header)
		}
		if got := strings.ToLower(pre.header.Get("Access-Control-Allow-Headers")); got != "authorization, content-type" {
			t.Fatalf("允许的头：%q", got)
		}
		res := r.do("GET", "/.well-known/armadra-platform", nil, hdr("Origin", origin))
		if res.header.Get("Access-Control-Allow-Origin") != "*" || res.header.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("普通请求（%s）：%v", origin, res.header)
		}
		// 错误答复同样不发 Allow-Credentials。
		denied := r.do("GET", "/v1/me", nil, hdr("Origin", origin))
		if denied.status != 401 || denied.header.Get("Access-Control-Allow-Credentials") != "" || denied.header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("401 答复（%s）：%d %v", origin, denied.status, denied.header)
		}
	}
}

func TestCookieAttributesAndScopes(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	r.registerBearer(email)
	check := func(res response, names ...string) {
		t.Helper()
		cookies := setCookies(res)
		if len(cookies) != len(names) {
			t.Fatalf("Set-Cookie 数量：%d，应为 %d：%v", len(cookies), len(names), res.header.Values("Set-Cookie"))
		}
		for i, cookie := range cookies {
			if cookie.Name != names[i] || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
				cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge <= 0 || !strings.HasPrefix(cookie.Name, "__Host-") {
				t.Fatalf("%s 的属性不对：%+v", names[i], cookie)
			}
		}
		if cookies[0].MaxAge > 900 || cookies[1].MaxAge > 2592000 {
			t.Fatalf("Max-Age 超过寿命：%d %d", cookies[0].MaxAge, cookies[1].MaxAge)
		}
	}
	login := r.login(email, password, hdr("Origin", appOrigin))
	r.expect(login, 200, "")
	check(login, "__Host-armadra-cloud", "__Host-armadra-cloud-refresh")
	if cookies := setCookies(login); cookies[0].MaxAge < 800 || cookies[0].MaxAge > 900 || cookies[1].MaxAge < 2592000-5 {
		t.Fatalf("普通会话的 Max-Age 应是 900 与 2592000：%d %d", cookies[0].MaxAge, cookies[1].MaxAge)
	}

	// 运营那一对：经 auth.refresh（scope: operator）下发，Max-Age 不超过会话剩余寿命（1 小时）。
	op, _ := r.operator(core.TransportCookie)
	r.clock.Advance(50 * time.Minute)
	res := r.do("POST", "/v1/auth/refresh", map[string]any{"scope": "operator"}, hdr("Origin", appOrigin, "Cookie", op.cookieHeader("__Host-armadra-ops-refresh")))
	r.expect(res, 200, "")
	check(res, "__Host-armadra-ops", "__Host-armadra-ops-refresh")
	if cookies := setCookies(res); cookies[0].MaxAge > 600 || cookies[1].MaxAge > 600 {
		t.Fatalf("运营会话 Max-Age 不超过剩余寿命（约 10 分钟）：%d %d", cookies[0].MaxAge, cookies[1].MaxAge)
	}
}

func TestDuplicateCookiesRejectedAndCleared(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	access := c.cookies["__Host-armadra-cloud"]
	for name, h := range map[string]headers{
		"两行 Cookie 头": hdr("Cookie", "__Host-armadra-cloud="+access).add("Cookie", "__Host-armadra-cloud="+access),
		"同一行里写两次":     hdr("Cookie", "__Host-armadra-cloud="+access+"; __Host-armadra-cloud=other"),
	} {
		res := r.do("GET", "/v1/me", nil, h)
		r.expect(res, 401, "unauthenticated")
		cookies := setCookies(res)
		if len(cookies) != 1 || cookies[0].Name != "__Host-armadra-cloud" || cookies[0].MaxAge != -1 {
			t.Fatalf("%s：重复的 Cookie 要被清掉：%v", name, res.header.Values("Set-Cookie"))
		}
	}
	// 重复的运营 Cookie；匿名端点也一样拒绝。
	res := r.do("GET", "/.well-known/armadra-platform", nil, hdr("Cookie", "__Host-armadra-ops=a; __Host-armadra-ops=b"))
	r.expect(res, 401, "unauthenticated")
	if cookies := setCookies(res); len(cookies) != 1 || cookies[0].Name != "__Host-armadra-ops" {
		t.Fatalf("%v", res.header.Values("Set-Cookie"))
	}
	// 无关 Cookie 重复不管。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", "theme=a; theme=b; __Host-armadra-cloud="+access)), 200, "")
}

func TestCookieAndBearerNeverMixed(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	b := r.registerBearer(r.email())
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud"), "Authorization", "Bearer "+b.access)), 400, "bad_request")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": b.refresh}, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud-refresh"), "Origin", appOrigin).add("Authorization", "Bearer x")), 400, "bad_request")
	// 任一枚会话 Cookie 都算（刷新 Cookie 每个请求都会带上）。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud-refresh"), "Authorization", "Bearer "+b.access)), 400, "bad_request")
}

func TestTransportBinding(t *testing.T) {
	r := newRig(t, rigOptions{})
	c := r.registerCookie(r.email())
	b := r.registerBearer(r.email())
	// Cookie 设备的访问令牌（登录答复里就给了）放进 Authorization：拒绝。
	r.expect(r.do("GET", "/v1/me", nil, c.bearer()), 401, "unauthenticated")
	// Bearer 设备的访问令牌放进 Cookie：拒绝。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", "__Host-armadra-cloud="+b.access)), 401, "unauthenticated")
	// 各走各的传输：通过。
	r.expect(r.do("GET", "/v1/me", nil, b.bearer()), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", c.cookieHeader("__Host-armadra-cloud"))), 200, "")
	// 没有任何凭据、乱编的令牌：401。
	r.expect(r.do("GET", "/v1/me", nil, nil), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/me", nil, hdr("Authorization", "Bearer not.a.jwt")), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/me", nil, hdr("Authorization", "Basic abc")), 401, "unauthenticated")
}

func TestOperatorIsolation(t *testing.T) {
	r := newRig(t, rigOptions{extra: stubHandlers()})
	normalCookie := r.registerCookie(r.email())
	normalBearer := r.registerBearer(r.email())
	opCookie, _ := r.operator(core.TransportCookie)
	opBearer, _ := r.operator(core.TransportBearer)

	// 普通会话调 admin.* → 401（Cookie 与 Bearer 都一样）。
	r.expect(r.do("GET", "/v1/admin/tenants", nil, hdr("Cookie", normalCookie.cookieHeader("__Host-armadra-cloud"))), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/admin/tenants", nil, normalBearer.bearer()), 401, "unauthenticated")
	// 运营会话调别的过程 → 401。
	r.expect(r.do("GET", "/v1/teams", nil, hdr("Cookie", opCookie.cookieHeader("__Host-armadra-ops"))), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", opCookie.cookieHeader("__Host-armadra-ops"))), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/teams", nil, opBearer.bearer()), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/me", nil, opBearer.bearer()), 401, "unauthenticated")
	r.expect(r.do("POST", "/v1/auth/step-up", map[string]any{"password": password}, opBearer.bearer()), 401, "unauthenticated")
	// 运营会话调 admin.*：Cookie 与 Bearer 都行。
	r.expect(r.do("GET", "/v1/admin/tenants", nil, hdr("Cookie", opCookie.cookieHeader("__Host-armadra-ops"))), 200, "")
	r.expect(r.do("GET", "/v1/admin/tenants", nil, opBearer.bearer()), 200, "")
	// 普通 Cookie 里放运营设备的令牌（或反过来）：对不上。
	r.expect(r.do("GET", "/v1/me", nil, hdr("Cookie", "__Host-armadra-cloud="+opCookie.access)), 401, "unauthenticated")
	r.expect(r.do("GET", "/v1/admin/tenants", nil, hdr("Cookie", "__Host-armadra-ops="+normalCookie.access)), 401, "unauthenticated")
	// 两套会话 Cookie 同时存在：按过程挑。
	both := hdr("Cookie", normalCookie.cookieHeader("__Host-armadra-cloud")+"; "+opCookie.cookieHeader("__Host-armadra-ops"))
	r.expect(r.do("GET", "/v1/me", nil, both), 200, "")
	r.expect(r.do("GET", "/v1/admin/tenants", nil, both), 200, "") // admin.* 用运营那一枚
}

func TestOperatorCookieCSRFUsesOwnValue(t *testing.T) {
	r := newRig(t, rigOptions{extra: map[string]api.Handler{
		"admin.accountStatus": func(ctx context.Context, call *api.Call) (any, error) {
			if _, err := call.Caller(ctx); err != nil {
				return nil, err
			}
			return map[string]any{}, nil
		},
	}})
	normal := r.registerCookie(r.email())
	op, _ := r.operator(core.TransportCookie)
	body := map[string]any{"status": "disabled", "reason": "测试"}
	// 运营 Cookie 带普通会话的 CSRF 值 → 403；用自己的值 → 通过。
	wrong := hdr("Origin", appOrigin, "X-Armadra-CSRF", normal.csrf, "Cookie", op.cookieHeader("__Host-armadra-ops"))
	r.expect(r.do("PUT", "/v1/admin/accounts/abc/status", body, wrong), 403, "forbidden")
	r.expect(r.do("PUT", "/v1/admin/accounts/abc/status", body, op.opsWrite()), 200, "")
	// 运营 Cookie 带 scope: operator 登出但缺 CSRF → 403；带自己的 CSRF，清掉运营那一对（普通会话的 Cookie 不动）。
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{"scope": "operator"}, hdr("Origin", appOrigin, "Cookie", op.cookieHeader("__Host-armadra-ops"))), 403, "forbidden")
	res := r.do("POST", "/v1/auth/logout", map[string]any{"scope": "operator"}, op.opsWrite())
	r.expect(res, 200, "")
	names := map[string]bool{}
	for _, cookie := range setCookies(res) {
		names[cookie.Name] = cookie.MaxAge == -1
	}
	if len(names) != 2 || !names["__Host-armadra-ops"] || !names["__Host-armadra-ops-refresh"] {
		t.Fatalf("只清运营那一对：%v", res.header.Values("Set-Cookie"))
	}
	// 登出之后运营令牌失效。
	r.expect(r.do("PUT", "/v1/admin/accounts/abc/status", body, op.opsWrite()), 401, "unauthenticated")
}

func TestOperatorSessionHasOneHourAbsoluteLifetime(t *testing.T) {
	r := newRig(t, rigOptions{})
	op, _ := r.operator(core.TransportCookie)
	refresh := func() response {
		return r.do("POST", "/v1/auth/refresh", map[string]any{"scope": "operator"}, hdr("Origin", appOrigin, "Cookie", op.cookieHeader("__Host-armadra-ops-refresh")))
	}
	r.clock.Advance(30 * time.Minute)
	res := refresh()
	r.expect(res, 200, "")
	next := parseSession(r, res)
	if int64(res.json["session"].(map[string]any)["expiresAtMs"].(float64)) != op.expiresAtMs {
		t.Fatalf("刷新不延长运营会话的绝对寿命：%s", res.body)
	}
	op.cookies["__Host-armadra-ops-refresh"] = next.cookies["__Host-armadra-ops-refresh"]
	r.clock.Advance(31 * time.Minute)
	r.expect(refresh(), 401, "session_expired")
	// 同一台设备的普通 Cookie 路径取不到运营刷新令牌。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{}, hdr("Origin", appOrigin, "Cookie", "__Host-armadra-cloud-refresh="+op.cookies["__Host-armadra-ops-refresh"])), 401, "session_revoked")
}

func TestOperatorLoginScope(t *testing.T) {
	r := newRig(t, rigOptions{})
	normalEmail := r.email()
	r.registerBearer(normalEmail)
	login := func(email, pw string) response {
		return r.do("POST", "/v1/auth/login", map[string]any{"account": email, "password": pw, "device": device, "scope": "operator"}, hdr("Origin", appOrigin))
	}
	// 口令对了才看范围：口令不对仍是 credentials_invalid，不泄露谁是运营账号。
	r.expect(login(normalEmail, "wrong-password-123"), 401, "credentials_invalid")
	r.expect(login(normalEmail, password), 403, "forbidden")
	_, opEmail := r.operator(core.TransportBearer)
	r.expect(login(opEmail, password), 403, "operator_mfa_required")
	// 已登记两步验证的运营账号走挑战（auth.mfaVerify）：口令对了答挑战，不建会话（完整流程见 operator_test.go）。
	var accountID string
	if err := r.run.DB.Pool().QueryRow(context.Background(), "SELECT account_id FROM accounts WHERE email = $1", opEmail).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	r.exec(`INSERT INTO account_credentials (tenant_id, credential_id, account_id, kind, secret_wrap, created_at_ms) VALUES ('cloud', 'c1', $1, 'totp', 'x', 1)`, accountID)
	if res := login(opEmail, password); res.status != 200 || res.json["mfa"] == nil || res.json["session"] != nil {
		t.Fatalf("运营登录答挑战：%d %s", res.status, res.body)
	}
	if got := r.do("GET", "/v1/me", nil, nil); got.status != 401 {
		t.Fatal(got.status)
	}
	r.expect(r.do("POST", "/v1/auth/mfa/verify", map[string]any{"challengeId": "x", "code": "123456"}, nil), 410, "mfa_expired")
	// 普通登录不带 scope 的运营账号：拿到普通会话，能看到 operator 范围。
	res := r.login(opEmail, password, nil)
	r.expect(res, 200, "")
	s := parseSession(r, res)
	me := r.do("GET", "/v1/me", nil, s.bearer())
	r.expect(me, 200, "")
	if me.json["mfaEnabled"] != true {
		t.Fatalf("登记了两步验证：%s", me.body)
	}
	// 运营会话不出现在普通设备列表里。
	devs := r.do("GET", "/v1/me/devices", nil, s.bearer())
	for _, d := range devs.json["devices"].([]any) {
		if d.(map[string]any)["name"] == "运营" {
			t.Fatalf("运营会话不该出现在普通设备列表：%s", devs.body)
		}
	}
	r.expect(r.do("DELETE", "/v1/me/devices/"+s.deviceID, nil, s.bearer()), 200, "")
}

func TestDisabledAccountCannotRefreshOrUseSession(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	s := r.registerBearer(email)
	r.exec("UPDATE accounts SET status = 'disabled' WHERE email = $1", email)
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": s.refresh}, nil), 401, "session_revoked")
	r.expect(r.do("GET", "/v1/me", nil, s.bearer()), 401, "unauthenticated")
}

func TestNoSecretsInLogsOrStorage(t *testing.T) {
	r := newRig(t, rigOptions{})
	email := r.email()
	b := r.registerBearer(email)
	c := r.registerCookie(r.email())
	r.login(email, "wrong-password-123", nil)
	r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": b.refresh}, nil)
	r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": "kestrel-ember-orchard-77"}, b.bearer())
	r.do("POST", "/v1/auth/step-up", map[string]any{"password": password}, b.bearer())
	secrets := []string{password, "wrong-password-123", "kestrel-ember-orchard-77", b.access, b.refresh, c.access, c.csrf,
		c.cookies["__Host-armadra-cloud"], c.cookies["__Host-armadra-cloud-refresh"], masterHex}
	logs := r.log.String()
	if logs == "" {
		t.Fatal("应有启动日志")
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(logs, secret) {
			t.Fatalf("日志里有秘密：%s", secret[:8])
		}
	}
	// 库里：账号、设备、签名钥、事故开关、作业的整行转成文本，不含口令、令牌与主密钥。
	dump := func(table string) string {
		t.Helper()
		var out strings.Builder
		rs, err := r.run.DB.Pool().Query(context.Background(), "SELECT row_to_json(t)::text FROM "+table+" t")
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var line string
			if err := rs.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out.WriteString(line)
		}
		return out.String()
	}
	for _, table := range []string{"accounts", "devices", "signing_keys", "tenants", "platform_settings", "jobs", "outbox_emails", "audit_log"} {
		text := dump(table)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(text, secret) {
				t.Fatalf("表 %s 里有秘密：%s", table, secret[:8])
			}
		}
	}
	// 签名私钥是包裹的，库里没有 PKCS#8 明文。
	keys, err := r.run.DB.Keys("cloud").SigningKeys(context.Background())
	if err != nil || len(keys) == 0 {
		t.Fatal(err)
	}
	if strings.Contains(dump("signing_keys"), keys[0].PrivateKeyPKCS8) {
		t.Fatal("签名私钥不该以明文落库")
	}
}
