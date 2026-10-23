package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const appOrigin = "https://app.armadra.test"

func masterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func request(method string, headers http.Header, cookies ...string) *http.Request {
	r := httptest.NewRequest(method, "/v1/me", nil)
	for name, values := range headers {
		r.Header[name] = values
	}
	for _, cookie := range cookies {
		r.Header.Add("Cookie", cookie)
	}
	return r
}

func TestParseCookiesCountsSessionCookiesOnly(t *testing.T) {
	r := request("GET", nil,
		"__Host-armadra-cloud=a1; other=x", "__Host-armadra-cloud=a2", "__Host-armadra-ops=o1", "__Host-armadra-cloud-refresh=r1")
	c := httpx.ParseCookies(r)
	if got := c.Duplicated(); len(got) != 1 || got[0] != httpx.CookieAccess {
		t.Fatalf("重复的名字：%v", got)
	}
	if c.Value(httpx.CookieAccess) != "a1" || !c.Has(httpx.CookieOpsAccess) || !c.Has(httpx.CookieRefresh) || c.Has(httpx.CookieOpsRefresh) {
		t.Fatal("取第一个值，别的名字各自独立")
	}
	if !c.Any() || httpx.ParseCookies(request("GET", nil, "other=x")).Any() {
		t.Fatal("Any 只看会话 Cookie")
	}
}

func TestSetCookieAttributes(t *testing.T) {
	for _, name := range []string{httpx.CookieAccess, httpx.CookieRefresh, httpx.CookieOpsAccess, httpx.CookieOpsRefresh} {
		if !strings.HasPrefix(name, "__Host-") {
			t.Fatalf("%s 要有 __Host- 前缀", name)
		}
		w := httptest.NewRecorder()
		w.Header().Add("Set-Cookie", httpx.Set(name, "v", 900))
		w.Header().Add("Set-Cookie", httpx.Clear(name))
		cookies := w.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatalf("%s：%d", name, len(cookies))
		}
		set, cleared := cookies[0], cookies[1]
		if !set.Secure || !set.HttpOnly || set.SameSite != http.SameSiteStrictMode || set.Path != "/" || set.Domain != "" || set.MaxAge != 900 || set.Value != "v" {
			t.Fatalf("%s 属性：%+v", name, set)
		}
		if !cleared.Secure || !cleared.HttpOnly || cleared.SameSite != http.SameSiteStrictMode || cleared.Path != "/" || cleared.Domain != "" || cleared.MaxAge != -1 || cleared.Value != "" {
			t.Fatalf("%s 清除：%+v", name, cleared)
		}
		if raw := w.Header().Values("Set-Cookie")[1]; !strings.Contains(raw, "Max-Age=0") {
			t.Fatalf("清除要写 Max-Age=0：%s", raw)
		}
	}
}

func TestOriginOnce(t *testing.T) {
	cases := []struct {
		name    string
		origins []string
		ok      bool
	}{
		{"一次", []string{appOrigin}, true},
		{"缺失", nil, false},
		{"重复", []string{appOrigin, appOrigin}, false},
	}
	for _, c := range cases {
		r := request("POST", http.Header{"Origin": c.origins})
		origin, ok := httpx.OriginOnce(r)
		if ok != c.ok || (ok && origin != appOrigin) {
			t.Errorf("%s：%q %v", c.name, origin, ok)
		}
	}
}

func TestCSRFIsStatelessPerDeviceAndKeyed(t *testing.T) {
	csrf, err := httpx.NewCSRF(masterKey())
	if err != nil {
		t.Fatal(err)
	}
	a, b := csrf.Token("device-a"), csrf.Token("device-b")
	if a == b || a != csrf.Token("device-a") {
		t.Fatal("同一设备不变，不同设备不同")
	}
	if !csrf.Check("device-a", a) || csrf.Check("device-a", b) || csrf.Check("device-a", "") || csrf.Check("device-a", a+"x") {
		t.Fatal("核对")
	}
	other := make([]byte, 32)
	otherCSRF, _ := httpx.NewCSRF(other)
	if otherCSRF.Token("device-a") == a {
		t.Fatal("换主密钥值要变")
	}
	if strings.ContainsAny(a, "+/=") || len(a) != 43 {
		t.Fatalf("base64url 无填充：%q", a)
	}
}

func gate(t *testing.T) *httpx.Gate {
	t.Helper()
	g, err := httpx.NewGate(appOrigin, masterKey(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBeforeRejectsDuplicateCookiesAndMixedCredentials(t *testing.T) {
	g := gate(t)
	call := func(r *http.Request) (*api.Call, error) {
		c := &api.Call{Request: r, Header: http.Header{}}
		return c, g.Before(c)
	}
	// 重复 Cookie → 401，并清掉这些 Cookie。
	c, err := call(request("POST", nil, "__Host-armadra-cloud=a1", "__Host-armadra-cloud=a2"))
	if code, _ := core.CodeOf(err); code != gen.ErrUnauthenticated {
		t.Fatalf("重复 Cookie：%v", err)
	}
	if got := c.Header.Values("Set-Cookie"); len(got) != 1 || !strings.HasPrefix(got[0], "__Host-armadra-cloud=;") {
		t.Fatalf("应清掉重复的 Cookie：%v", got)
	}
	// 同一个名字写在一个 Cookie 头里也算重复。
	if _, err := call(request("POST", nil, "__Host-armadra-ops=a; __Host-armadra-ops=b")); err == nil {
		t.Fatal("同一头里的重复也要拒绝")
	}
	// Cookie + Authorization → 400。
	_, err = call(request("POST", http.Header{"Authorization": {"Bearer x"}}, "__Host-armadra-cloud=a1"))
	if code, _ := core.CodeOf(err); code != gen.ErrBadRequest {
		t.Fatalf("混用：%v", err)
	}
	// 只有 Authorization，或只有无关 Cookie：放行。两套会话 Cookie 同时存在也放行。
	for _, r := range []*http.Request{
		request("POST", http.Header{"Authorization": {"Bearer x"}}, "theme=dark"),
		request("POST", nil, "__Host-armadra-cloud=a", "__Host-armadra-ops=o"),
		request("POST", nil),
	} {
		if _, err := call(r); err != nil {
			t.Fatalf("应放行：%v", err)
		}
	}
}

func TestGateSessionCookiesAndTransport(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	g, _ := httpx.NewGate(appOrigin, masterKey(), nil, func() time.Time { return now })
	session := core.Session{
		AccessToken: "access", AccessExpiresAtMs: now.UnixMilli() + 900_000, ExpiresAtMs: now.UnixMilli() + 3_600_000,
	}
	h := http.Header{}
	g.SetSession(h, core.SessionScopeOperator, session, "refresh")
	w := httptest.NewRecorder()
	for _, value := range h.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", value)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != httpx.CookieOpsAccess || cookies[1].Name != httpx.CookieOpsRefresh {
		t.Fatalf("运营会话用运营那两枚：%+v", cookies)
	}
	if cookies[0].MaxAge != 900 || cookies[1].MaxAge != 3600 {
		t.Fatalf("Max-Age 不超过会话剩余寿命：%d %d", cookies[0].MaxAge, cookies[1].MaxAge)
	}
	// 访问 Cookie 的寿命也不超过设备剩余寿命。
	short := core.Session{AccessToken: "a", AccessExpiresAtMs: now.UnixMilli() + 900_000, ExpiresAtMs: now.UnixMilli() + 120_000}
	h = http.Header{}
	g.SetSession(h, core.SessionScopeSession, short, "r")
	w = httptest.NewRecorder()
	for _, value := range h.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", value)
	}
	if got := w.Result().Cookies(); got[0].Name != httpx.CookieAccess || got[0].MaxAge != 120 || got[1].Name != httpx.CookieRefresh {
		t.Fatalf("普通会话：%+v", got)
	}
	h = http.Header{}
	g.ClearSession(h, core.SessionScopeSession)
	if len(h.Values("Set-Cookie")) != 2 {
		t.Fatal("登出两枚都清")
	}

	for _, c := range []struct {
		name    string
		headers http.Header
		want    core.Transport
	}{
		{"同源且没有 Authorization", http.Header{"Origin": {appOrigin}}, core.TransportCookie},
		{"同源但带 Authorization", http.Header{"Origin": {appOrigin}, "Authorization": {"Bearer x"}}, core.TransportBearer},
		{"别的来源", http.Header{"Origin": {"https://x.src.armadra.test"}}, core.TransportBearer},
		{"没有 Origin", nil, core.TransportBearer},
		{"Origin 重复", http.Header{"Origin": {appOrigin, appOrigin}}, core.TransportBearer},
	} {
		if got := g.TransportFor(request("POST", c.headers)); got != c.want {
			t.Errorf("%s：%s", c.name, got)
		}
	}
}

func TestRefreshTokenSourceAndOrigin(t *testing.T) {
	g := gate(t)
	call := func(r *http.Request, input map[string]any) *api.Call {
		return &api.Call{Request: r, Input: input, Header: http.Header{}}
	}
	given := "dev.token"
	if token, viaCookie, err := g.RefreshToken(call(request("POST", nil), map[string]any{}), &given); err != nil || viaCookie || token != given {
		t.Fatalf("请求体里的刷新令牌：%q %v %v", token, viaCookie, err)
	}
	cookie := "__Host-armadra-cloud-refresh=cookie.token"
	r := request("POST", http.Header{"Origin": {appOrigin}}, cookie)
	if token, viaCookie, err := g.RefreshToken(call(r, map[string]any{}), nil); err != nil || !viaCookie || token != "cookie.token" {
		t.Fatalf("刷新 Cookie：%q %v %v", token, viaCookie, err)
	}
	// Cookie 路径要 Origin 恰好一次且等于控制台来源。
	for name, origins := range map[string][]string{"缺": nil, "别的来源": {"https://x.src.armadra.test"}, "重复": {appOrigin, appOrigin}} {
		_, _, err := g.RefreshToken(call(request("POST", http.Header{"Origin": origins}, cookie), map[string]any{}), nil)
		if code, _ := core.CodeOf(err); code != gen.ErrForbidden {
			t.Errorf("Origin %s：%v", name, err)
		}
	}
	// 运营范围取运营的刷新 Cookie，不用普通那枚。
	r = request("POST", http.Header{"Origin": {appOrigin}}, cookie)
	if _, _, err := g.RefreshToken(call(r, map[string]any{"scope": "operator"}), nil); err == nil {
		t.Fatal("scope=operator 不该读普通刷新 Cookie")
	} else if code, _ := core.CodeOf(err); code != gen.ErrSessionRevoked {
		t.Fatalf("没有对应的刷新 Cookie 答 session_revoked：%v", err)
	}
	r = request("POST", http.Header{"Origin": {appOrigin}}, "__Host-armadra-ops-refresh=ops.token")
	if token, _, err := g.RefreshToken(call(r, map[string]any{"scope": "operator"}), nil); err != nil || token != "ops.token" {
		t.Fatalf("运营刷新 Cookie：%q %v", token, err)
	}
	if _, _, err := g.RefreshToken(call(request("POST", nil), map[string]any{}), nil); err == nil {
		t.Fatal("两处都没有应拒绝")
	}
}

func TestLimitsCheckReturnsRetryAfter(t *testing.T) {
	memory := control.NewMemory(control.MemoryOptions{Limits: httpx.Rules})
	limits := httpx.NewLimits(memory.Limits())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := limits.Check(ctx, httpx.LimitResetEmail, "a@example.com"); err != nil {
			t.Fatalf("前三次放行：%v", err)
		}
	}
	err := limits.Check(ctx, httpx.LimitResetEmail, "a@example.com")
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != gen.ErrRateLimited || failure.RetryAfter <= 0 {
		t.Fatalf("第四次 429 并带重试等待：%v", err)
	}
	if err := limits.Check(ctx, httpx.LimitResetEmail, "b@example.com"); err != nil {
		t.Fatal("别的键有自己的桶")
	}
	// core.RateLimiter 视图。
	result, err := limits.Hit(ctx, core.LimitLoginIP, "198.51.100.1")
	if err != nil || !result.Allowed {
		t.Fatalf("core.LimitLoginIP：%+v %v", result, err)
	}
}

func TestRulesMatchDesignTable(t *testing.T) {
	want := map[control.LimitKind]control.LimitRule{
		httpx.LimitLoginIP:      {Max: 20, Window: time.Minute},
		httpx.LimitRegisterIP:   {Max: 10, Window: time.Hour},
		httpx.LimitResetEmail:   {Max: 3, Window: time.Hour},
		httpx.LimitStepUpDevice: {Max: 10, Window: 10 * time.Minute},
		httpx.LimitMFAAcct:      {Max: 10, Window: 10 * time.Minute},
		httpx.LimitAssertAcct:   {Max: 60, Window: time.Minute},
		httpx.LimitDeviceStart:  {Max: 30, Window: time.Hour},
		httpx.LimitInviteResend: {Max: 5, Window: time.Hour},
	}
	for kind, rule := range want {
		if httpx.Rules[kind] != rule {
			t.Errorf("%s：%+v", kind, httpx.Rules[kind])
		}
	}
	if len(httpx.Rules) != 16 {
		t.Errorf("§5.4 共 16 种：%d", len(httpx.Rules))
	}
}

func TestMapErrorTurnsStoreConflictsIntoCoreErrors(t *testing.T) {
	err := httpx.MapError(&saas.Error{Code: gen.ErrEmailTaken, Constraint: "accounts_email"})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != gen.ErrEmailTaken {
		t.Fatalf("%v", err)
	}
	plain := errors.New("x")
	if httpx.MapError(plain) != plain || httpx.MapError(nil) != nil {
		t.Fatal("其余原样返回")
	}
}

// 按 IP 的限流键：IPv6 截到 /64，同一段里换地址绕不开；IPv4 与别的 /64 各有自己的桶。
func TestIPv6RateKeysUseSlash64(t *testing.T) {
	memory := control.NewMemory(control.MemoryOptions{Limits: httpx.Rules})
	limits := httpx.NewLimits(memory.Limits())
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := limits.Check(ctx, httpx.LimitRegisterIP, fmt.Sprintf("2001:db8:1:2::%x", i+1)); err != nil {
			t.Fatalf("前十次放行（同一 /64 里每次换地址）：%v", err)
		}
	}
	if err := limits.Check(ctx, httpx.LimitRegisterIP, "2001:db8:1:2:ffff:ffff:ffff:ffff"); err == nil {
		t.Fatal("同一 /64 共用一个桶，第 11 次应被限")
	}
	if err := limits.Check(ctx, httpx.LimitRegisterIP, "2001:db8:1:3::1"); err != nil {
		t.Fatalf("另一个 /64 有自己的桶：%v", err)
	}
	if err := limits.Check(ctx, httpx.LimitRegisterIP, "198.51.100.7"); err != nil {
		t.Fatalf("IPv4 不受影响：%v", err)
	}
	// 不是 .ip 的键（账号、设备）不规整。
	for i := 0; i < 3; i++ {
		if err := limits.Check(ctx, httpx.LimitResetEmail, fmt.Sprintf("2001:db8:1:2::%x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := limits.Check(ctx, httpx.LimitResetEmail, "2001:db8:1:2::d"); err != nil {
		t.Fatalf("邮箱键原样：%v", err)
	}
}
