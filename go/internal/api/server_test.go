package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 用真处理器把 personal 的 25 个过程各跑一遍：每个 200 的答复都按 cloud-openapi.json 的输出 schema 校验，
// 最后核对 api.PersonalProcedures 全部被命中。
func TestPersonalFlowCoversEveryProcedure(t *testing.T) {
	r := newRig(t, rigOptions{webApp: "https://cloud.armadra.test/app/", name: "  Home relay  "})
	r.addOwner()

	info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(info, 200, "")
	if info.json["mode"] != "personal" || info.json["name"] != "Home relay" || info.json["webApp"] != "https://cloud.armadra.test/app/" {
		t.Fatalf("platform.info：%s", info.body)
	}
	if _, present := info.json["challenge"]; present {
		t.Fatal("没配挑战不该报 challenge")
	}
	if jwks := r.do("GET", "/.well-known/jwks.json", nil, nil); jwks.status != 200 || len(jwks.json["keys"].([]any)) != 1 {
		t.Fatalf("platform.jwks：%s", jwks.body)
	}

	access, refresh, session := r.login()
	if account := session["account"].(map[string]any); account["accountId"] != "acct:alice" || account["email"] != nil {
		t.Fatalf("会话里的账号：%+v", account)
	}
	auth := bearer(access)

	me := r.do("GET", "/v1/me", nil, auth)
	r.expect(me, 200, "")
	if me.json["displayName"] != "alice" || me.json["mfaEnabled"] != false {
		t.Fatalf("me.get：%s", me.body)
	}
	r.registerSource(access)

	// 源用 Source <jws> 换隧道令牌；jti 重放被拒。
	hint := r.do("GET", "/v1/sources/me/relay", nil, map[string]string{"Authorization": "Source " + r.sourceJWS("jti-0001-aaaa", 60)})
	r.expect(hint, 200, "")
	if hint.json["tunnelToken"] == "" || len(hint.json["nodes"].([]any)) != 1 {
		t.Fatalf("sources.heartbeatHint：%s", hint.body)
	}
	replay := r.do("GET", "/v1/sources/me/relay", nil, map[string]string{"Authorization": "Source " + r.sourceJWS("jti-0001-aaaa", 60)})
	r.expect(replay, 401, "unauthenticated")
	tooLong := r.do("GET", "/v1/sources/me/relay", nil, map[string]string{"Authorization": "Source " + r.sourceJWS("jti-0002-bbbb", 121)})
	r.expect(tooLong, 401, "unauthenticated")

	r.expect(r.do("GET", "/v1/me/sources", nil, auth), 200, "")
	list := r.do("GET", "/v1/sources", nil, auth)
	r.expect(list, 200, "")
	if len(list.json["sources"].([]any)) != 1 {
		t.Fatalf("sources.list：%s", list.body)
	}
	r.expect(r.do("GET", "/v1/sources/"+sourceID, nil, auth), 200, "")
	assertion := r.do("POST", "/v1/sources/"+sourceID+"/assertion", map[string]any{
		"device": map[string]any{"platform": "desktop", "name": "laptop"},
	}, auth)
	r.expect(assertion, 200, "")
	if assertion.json["relayBaseUrl"] != "https://cloud.armadra.test/s/"+sourceID {
		t.Fatalf("relayBaseUrl：%s", assertion.body)
	}

	// 链接：create → get → accept → list → update → revoke。
	created := r.do("POST", "/v1/links", map[string]any{
		"kind": "source_invite", "sourceId": sourceID, "invitationId": "inv", "label": "friend", "role": "viewer",
		"expiresAtMs": r.h.Clock.Ms() + 3_600_000, "maxUses": 2, "scope": map[string]any{"workspaceId": "ws_01", "readOnly": true},
	}, auth)
	r.expect(created, 200, "")
	linkID := created.json["linkId"].(string)
	if !strings.HasPrefix(created.json["url"].(string), "https://cloud.armadra.test/j/") || strings.Contains(created.json["url"].(string), "#") {
		t.Fatalf("links.create 的 url 不带片段：%s", created.body)
	}
	got := r.do("GET", "/v1/links/"+linkID, nil, nil)
	r.expect(got, 200, "")
	if got.json["sourceName"] != "mac" || got.json["exhausted"] != false {
		t.Fatalf("links.get：%s", got.body)
	}
	wrong := r.do("POST", "/v1/links/"+linkID+"/accept", map[string]any{"secret": "不对"}, nil)
	r.expect(wrong, 403, "link_secret_invalid")
	accepted := r.do("POST", "/v1/links/"+linkID+"/accept", map[string]any{
		"secret": created.json["secret"], "device": map[string]any{"platform": "browser", "name": "friend"},
	}, nil)
	r.expect(accepted, 200, "")
	guestAccess := accepted.json["guestSession"].(map[string]any)["accessToken"].(string)

	// 访客只看链接指向的源，不能要注册令牌、不能列链接。
	guest := bearer(guestAccess)
	guestSources := r.do("GET", "/v1/me/sources", nil, guest)
	r.expect(guestSources, 200, "")
	if items := guestSources.json["sources"].([]any); len(items) != 1 || items[0].(map[string]any)["via"] != "link" || items[0].(map[string]any)["owner"] != false {
		t.Fatalf("访客的源：%s", guestSources.body)
	}
	r.expect(r.do("POST", "/v1/sources/registration-tokens", map[string]any{}, guest), 403, "forbidden")
	r.expect(r.do("GET", "/v1/links", nil, guest), 403, "forbidden")
	guestMe := r.do("GET", "/v1/me", nil, guest)
	r.expect(guestMe, 200, "")
	if guestMe.json["displayName"] != "friend" {
		t.Fatalf("访客的 me.get：%s", guestMe.body)
	}
	r.expect(r.do("GET", "/v1/me/traffic?fromMs=0&toMs=100", nil, guest), 200, "")

	links := r.do("GET", "/v1/links?sourceId="+sourceID, nil, auth)
	r.expect(links, 200, "")
	if items := links.json["links"].([]any); len(items) != 1 || items[0].(map[string]any)["uses"].(float64) != 1 {
		t.Fatalf("links.list：%s", links.body)
	}
	updated := r.do("PATCH", "/v1/links/"+linkID, map[string]any{"label": "  老朋友  "}, auth)
	r.expect(updated, 200, "")
	if updated.json["label"] != "老朋友" {
		t.Fatalf("links.update 应去首尾空白：%s", updated.body)
	}

	// 设备、流量、me-stream 票。
	devices := r.do("GET", "/v1/me/devices", nil, auth)
	r.expect(devices, 200, "")
	if items := devices.json["devices"].([]any); len(items) != 1 || items[0].(map[string]any)["current"] != true {
		t.Fatalf("me.devices：%s", devices.body)
	}
	ticket := r.do("POST", "/v1/me/stream/ticket", map[string]any{}, auth)
	r.expect(ticket, 200, "")
	traffic := r.do("GET", fmt.Sprintf("/v1/me/traffic?fromMs=0&toMs=%d&granularity=day&sourceId=%s", r.h.Clock.Ms()+1, sourceID), nil, auth)
	r.expect(traffic, 200, "")
	if len(traffic.json["buckets"].([]any)) != 0 {
		t.Fatalf("还没有流量：%s", traffic.body)
	}

	// 撤销链接：访客的访问令牌即时失效。
	r.expect(r.do("DELETE", "/v1/links/"+linkID, nil, auth), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, guest), 401, "unauthenticated")
	r.expect(r.do("DELETE", "/v1/links/"+linkID, nil, auth), 404, "not_found")

	// 会话：改口令 → 新会话；刷新；再开一台设备撤销它；登出。
	changed := r.do("POST", "/v1/auth/password", map[string]any{"password": password, "newPassword": "saffron-glacier-meadow-77"}, auth)
	r.expect(changed, 200, "")
	renewed := changed.json["session"].(map[string]any)
	if renewed["refreshToken"] == refresh {
		t.Fatal("改口令应换新刷新令牌")
	}
	refreshed := r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": renewed["refreshToken"]}, nil)
	r.expect(refreshed, 200, "")
	// personal 没有运营账号：scope operator 一律 403。
	r.expect(r.do("POST", "/v1/auth/login", map[string]any{
		"account": "alice", "password": "saffron-glacier-meadow-77", "scope": "operator",
		"device": map[string]any{"platform": "ios", "name": "phone"},
	}, nil), 403, "forbidden")
	secondLogin := r.do("POST", "/v1/auth/login", map[string]any{
		"account": "alice", "password": "saffron-glacier-meadow-77", "device": map[string]any{"platform": "ios", "name": "phone"},
	}, nil)
	r.expect(secondLogin, 200, "")
	other := secondLogin.json["session"].(map[string]any)["device"].(map[string]any)["deviceId"].(string)
	current := refreshed.json["session"].(map[string]any)
	r.expect(r.do("DELETE", "/v1/me/devices/"+other, nil, bearer(current["accessToken"].(string))), 200, "")
	r.expect(r.do("DELETE", "/v1/me/devices/"+other, nil, bearer(current["accessToken"].(string))), 404, "not_found")

	// 撤销源，再登出。
	r.expect(r.do("DELETE", "/v1/sources/"+sourceID, nil, bearer(current["accessToken"].(string))), 200, "")
	r.expect(r.do("POST", "/v1/auth/logout", map[string]any{}, bearer(current["accessToken"].(string))), 200, "")
	r.expect(r.do("GET", "/v1/me", nil, bearer(current["accessToken"].(string))), 401, "unauthenticated")

	var missing []string
	for _, procedure := range api.PersonalProcedures {
		if !r.hits[procedure] {
			missing = append(missing, procedure)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("这些过程没被真处理器跑到：%v", missing)
	}
	if len(api.PersonalProcedures) != 25 {
		t.Fatalf("personal 应有 25 个过程：%d", len(api.PersonalProcedures))
	}
}

func TestPlatformInfoReportsChallenge(t *testing.T) {
	r := newRig(t, rigOptions{challenge: true})
	info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(info, 200, "")
	caps := fmt.Sprint(info.json["capabilities"])
	if !strings.Contains(caps, "challenge.turnstile") {
		t.Fatalf("应报 challenge.turnstile：%s", caps)
	}
	challenge := info.json["challenge"].(map[string]any)
	if challenge["provider"] != "turnstile" || challenge["siteKey"] != "test-site-key" || fmt.Sprint(challenge["scope"]) != "[auth.login]" {
		t.Fatalf("challenge：%+v", challenge)
	}
	if info.json["webApp"] != nil {
		t.Fatalf("没有页面产物时 webApp 为 null：%v", info.json["webApp"])
	}
	// 配了挑战：登录缺令牌 → challenge_required；带令牌（验证器放行）→ 通过。
	r.addOwner()
	body := map[string]any{"account": "alice", "password": password, "device": map[string]any{"platform": "desktop", "name": "t"}}
	r.expect(r.do("POST", "/v1/auth/login", body, nil), 400, "challenge_required")
	body["challenge"] = map[string]any{"provider": "turnstile", "token": "tok"}
	r.expect(r.do("POST", "/v1/auth/login", body, nil), 200, "")
	// 同一个令牌再用 → 本地防重放表拒绝。
	r.expect(r.do("POST", "/v1/auth/login", body, nil), 400, "challenge_invalid")
}

func TestErrorShapesAndHeaders(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.addOwner()

	res := r.do("GET", "/v1/me", nil, nil)
	r.expect(res, 401, "unauthenticated")
	if res.json["message"] != "Unauthenticated" || res.json["requestId"] == "" || res.json["details"] != nil {
		t.Fatalf("错误形状：%s", res.body)
	}
	for key, want := range map[string]string{
		"Armadra-Protocol":             fmt.Sprintf("%d.%d", gen.ProtocolMajor, gen.ProtocolMinor),
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, POST, PATCH, DELETE, OPTIONS",
		"Access-Control-Allow-Headers": "authorization, content-type",
		"Cache-Control":                "no-store",
		"Content-Type":                 "application/json; charset=utf-8",
	} {
		if got := res.header.Get(key); got != want {
			t.Errorf("头 %s = %q，应为 %q", key, got, want)
		}
	}

	t.Run("预检", func(t *testing.T) {
		res := r.do("OPTIONS", "/v1/auth/login", nil, nil)
		if res.status != 204 || res.header.Get("Access-Control-Allow-Origin") != "*" || res.header.Get("Access-Control-Max-Age") != "7200" || len(res.body) != 0 {
			t.Fatalf("预检：%d %v %q", res.status, res.header, res.body)
		}
	})
	t.Run("personal 没实现的过程与契约外的路径答 404", func(t *testing.T) {
		for _, path := range []string{"/v1/teams/abc", "/v1/auth/register", "/v1/nope", "/v1/sources/"} {
			method := "GET"
			if path == "/v1/auth/register" {
				method = "POST"
			}
			r.expect(r.do(method, path, nil, nil), 404, "not_found")
		}
		r.expect(r.do("POST", "/v1/teams", map[string]any{"slug": "x"}, nil), 404, "not_found")
	})
	t.Run("入参不合格式", func(t *testing.T) {
		login := "/v1/auth/login"
		device := map[string]any{"platform": "desktop", "name": "t"}
		for name, body := range map[string]any{
			"不是 JSON":    "{坏的",
			"JSON 数组":    "[1]",
			"缺必填字段":      map[string]any{"account": "alice"},
			"枚举越界":       map[string]any{"account": "alice", "password": password, "device": map[string]any{"platform": "palm", "name": "t"}},
			"账号名为空":      map[string]any{"account": "", "password": password, "device": device},
			"令牌过长":       map[string]any{"account": "alice", "password": password, "device": device, "challenge": map[string]any{"provider": "turnstile", "token": strings.Repeat("x", 2049)}},
			"体超过 64 KiB": `{"account":"` + strings.Repeat("a", 64*1024) + `"}`,
		} {
			r.expect(r.do("POST", login, body, nil), 400, "bad_request")
			_ = name
		}
		r.expect(r.do("GET", "/v1/me/traffic?fromMs=abc&toMs=1", nil, bearerOf(t, r)), 400, "bad_request")
		r.expect(r.do("GET", "/v1/me/traffic?fromMs=5&toMs=5", nil, bearerOf(t, r)), 400, "bad_request")
		r.expect(r.do("GET", "/v1/me/traffic?fromMs=0&toMs=5&granularity=week", nil, bearerOf(t, r)), 400, "bad_request")
	})
	t.Run("HEAD 按 GET 处理", func(t *testing.T) {
		res := r.do("HEAD", "/.well-known/jwks.json", nil, nil)
		if res.status != 200 || len(res.body) != 0 {
			t.Fatalf("HEAD：%d %q", res.status, res.body)
		}
	})
	t.Run("路径参数里的斜杠被解码后仍按参数校验", func(t *testing.T) {
		auth := bearerOf(t, r)
		r.expect(r.do("GET", "/v1/sources/a%2Fb", nil, auth), 400, "bad_request")
		r.expect(r.do("GET", "/v1/sources/"+sourceID, nil, auth), 404, "not_found")
	})
}

func bearerOf(t *testing.T, r *rig) map[string]string {
	t.Helper()
	access, _, _ := r.login()
	return bearer(access)
}

func TestLockoutAnswersRetryAfter(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.addOwner()
	body := map[string]any{"account": "alice", "password": "wrong-password-here", "device": map[string]any{"platform": "desktop", "name": "t"}}
	for i := 0; i < 5; i++ {
		r.expect(r.do("POST", "/v1/auth/login", body, nil), 401, "credentials_invalid")
	}
	res := r.do("POST", "/v1/auth/login", body, nil)
	r.expect(res, 429, "account_locked")
	details := res.json["details"].(map[string]any)
	if details["retryAfterMs"].(float64) != 60_000 || res.header.Get("Retry-After") != "60" {
		t.Fatalf("锁定 1 分钟：%+v %q", details, res.header.Get("Retry-After"))
	}
	// 锁着的时候正确口令也进不去；过了时间就进得去。
	body["password"] = password
	r.expect(r.do("POST", "/v1/auth/login", body, nil), 429, "account_locked")
	r.h.Clock.Set(r.h.Clock.Ms() + 61_000)
	r.expect(r.do("POST", "/v1/auth/login", body, nil), 200, "")
}

func TestRateLimitAnswersRetryAfter(t *testing.T) {
	r := newRig(t, rigOptions{limit: func(core.LimitKind, string) core.LimitResult {
		return core.LimitResult{Allowed: false, RetryAfter: 1500 * time.Millisecond}
	}})
	res := r.do("POST", "/v1/auth/login", map[string]any{"account": "a", "password": "b", "device": map[string]any{"platform": "desktop", "name": "t"}}, nil)
	r.expect(res, 429, "rate_limited")
	if res.json["details"].(map[string]any)["retryAfterMs"].(float64) != 1500 || res.header.Get("Retry-After") != "2" {
		t.Fatalf("限流答复：%s %q", res.body, res.header.Get("Retry-After"))
	}
}

func TestCloudModeAnswers501ForReserved(t *testing.T) {
	r := newRig(t, rigOptions{mode: api.ModeCloud})
	res := r.do("GET", "/v1/teams/abc", nil, nil)
	r.expect(res, 501, "not_implemented")
	if res.json["message"] != "not_implemented" {
		t.Fatalf("not_implemented 的 message 沿用码本身：%s", res.body)
	}
	// 契约外的路径仍是 404。
	r.expect(r.do("GET", "/v1/nope", nil, nil), 404, "not_found")
	// 启用的过程照常答。
	r.expect(r.do("GET", "/.well-known/jwks.json", nil, nil), 200, "")
}

func TestClaimsOnlyOwnedPaths(t *testing.T) {
	r := newRig(t, rigOptions{})
	for path, want := range map[string]bool{
		"/v1/me": true, "/v1/": true, "/.well-known/armadra-platform": true, "/.well-known/jwks.json": true,
		"/.well-known/other": false, "/app/": false, "/": false, "/v1": false,
	} {
		if got := r.api.Claims(path); got != want {
			t.Errorf("Claims(%q) = %v，应为 %v", path, got, want)
		}
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	var reported error
	r := newRig(t, rigOptions{})
	// 用一个会出错的处理器顶替：错误只进 OnError，答复是 500 internal 与固定文案。
	a, err := api.New(api.Options{
		Handlers: map[string]api.Handler{"platform.jwks": func(_ context.Context, _ *api.Call) (any, error) {
			return nil, fmt.Errorf("数据库连接串 postgres://u:secret@host 出错了")
		}},
		OnError: func(err error) { reported = err },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = r
	srv := newServer(t, a)
	res := srv.do("GET", "/.well-known/jwks.json", nil, nil)
	if res.status != http.StatusInternalServerError || res.json["code"] != "internal" || strings.Contains(string(res.body), "secret") {
		t.Fatalf("内部错误的答复：%d %s", res.status, res.body)
	}
	if reported == nil {
		t.Fatal("内部错误应报给 OnError")
	}
}

// 处理器答了编不出 JSON 的值：兜底的 500 与别处同形（gen 的码、固定文案、带 requestId）。
func TestUnencodableOutputFallsBackToInternalWithRequestID(t *testing.T) {
	a, err := api.New(api.Options{
		Handlers: map[string]api.Handler{"platform.jwks": func(_ context.Context, _ *api.Call) (any, error) {
			return map[string]any{"bad": make(chan int)}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := srv(t, a)
	if res.status != http.StatusInternalServerError || res.json["code"] != string(gen.ErrInternal) ||
		res.json["message"] != core.MessageOf(gen.ErrInternal) {
		t.Fatalf("兜底答复：%d %s", res.status, res.body)
	}
	if id, _ := res.json["requestId"].(string); id == "" {
		t.Fatalf("兜底答复缺 requestId：%s", res.body)
	}
}

func srv(t *testing.T, a *api.API) response {
	t.Helper()
	return newServer(t, a).do("GET", "/.well-known/jwks.json", nil, nil)
}

// 契约里 personal 不实现的过程（含协议 1.4 的团队、邀请、授权、计费、管理员等 SaaS 各组）：
// personal 一律答 404 not_found，cloud 一律答 501 not_implemented；预留路由不会遮住启用的路由。
func TestEveryReservedProcedureAnswersByMode(t *testing.T) {
	personal := map[string]bool{}
	for _, name := range api.PersonalProcedures {
		personal[name] = true
	}
	for name, mode := range map[string]api.Mode{"personal": api.ModePersonal, "cloud": api.ModeCloud} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, rigOptions{mode: mode})
			wantStatus, wantCode := 404, "not_found"
			if mode == api.ModeCloud {
				wantStatus, wantCode = 501, "not_implemented"
			}
			checked := 0
			for _, route := range gen.Routes {
				if personal[route.Procedure] || route.Procedure == "platform.health" {
					continue
				}
				path := route.Path
				for strings.Contains(path, "{") {
					start := strings.Index(path, "{")
					end := strings.Index(path, "}")
					path = path[:start] + "x" + path[end+1:]
				}
				var body any
				if route.Method == "POST" || route.Method == "PATCH" {
					body = map[string]any{}
				}
				res := r.do(route.Method, path, body, nil)
				if res.status != wantStatus || res.json["code"] != wantCode {
					t.Errorf("%s %s（%s）：%d %v，应为 %d %s", route.Method, path, route.Procedure, res.status, res.json["code"], wantStatus, wantCode)
				}
				checked++
			}
			if checked < 100 {
				t.Fatalf("只检查了 %d 个预留过程", checked)
			}
		})
	}
}
