package core_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

type scripted struct {
	calls    atomic.Int32
	requests []core.VerifyRequest
	answers  []core.Verdict
	errs     []error
}

func (s *scripted) Verify(_ context.Context, request core.VerifyRequest) (core.Verdict, error) {
	i := int(s.calls.Add(1)) - 1
	s.requests = append(s.requests, request)
	if i < len(s.errs) && s.errs[i] != nil {
		return core.Verdict{}, s.errs[i]
	}
	if i < len(s.answers) {
		return s.answers[i], nil
	}
	return core.Verdict{OK: true}, nil
}

func gate(t *testing.T, verifier core.ChallengeVerifier, scope ...core.ChallengeScope) (*core.ChallengeGate, *[][]string) {
	t.Helper()
	var rejections [][]string
	now := time.UnixMilli(1_760_000_000_000)
	g, err := core.NewChallengeGate(core.ChallengeGateOptions{
		SiteKey: " site ", Scope: scope, Hostnames: []string{"Relay.Armadra.Test", ""}, Verifier: verifier,
		Now:        func() time.Time { return now },
		OnRejected: func(_ core.ChallengeScope, reasons []string) { rejections = append(rejections, reasons) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, &rejections
}

func token(value string) *core.ChallengeToken {
	return &core.ChallengeToken{Provider: "turnstile", Token: value}
}

func TestChallengeGateRules(t *testing.T) {
	ctx := context.Background()
	verifier := &scripted{}
	g, rejections := gate(t, verifier)

	info := g.Info()
	if info.Provider != "turnstile" || info.SiteKey != "site" || !reflect.DeepEqual(info.Scope, []core.ChallengeScope{core.ScopeLogin}) {
		t.Fatalf("info：%+v", info)
	}
	// 不在范围里的入口不要求，也不问提供方。
	eq(t, code(g.Check(ctx, core.ScopeLinksAccept, nil, "ip")), "ok", "不在范围")
	eq(t, int(verifier.calls.Load()), 0, "没问提供方")
	// 缺令牌。
	eq(t, code(g.Check(ctx, core.ScopeLogin, nil, "ip")), "challenge_required", "缺令牌")
	// 提供方不对、令牌空、过长：失败，且不问提供方。
	eq(t, code(g.Check(ctx, core.ScopeLogin, &core.ChallengeToken{Provider: "hcaptcha", Token: "x"}, "ip")), "challenge_invalid", "提供方")
	eq(t, code(g.Check(ctx, core.ScopeLogin, token(""), "ip")), "challenge_invalid", "空令牌")
	eq(t, code(g.Check(ctx, core.ScopeLogin, token(strings.Repeat("x", 2049)), "ip")), "challenge_invalid", "过长")
	eq(t, int(verifier.calls.Load()), 0, "没问提供方")
	// 通过：请求里带 action、主机名白名单（小写、去空）、IP 与幂等键。
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("good"), "198.51.100.7")), "ok", "通过")
	request := verifier.requests[0]
	if request.Action != "armadra-login" || !reflect.DeepEqual(request.Hostnames, []string{"relay.armadra.test"}) ||
		request.RemoteIP != "198.51.100.7" || len(request.IdempotencyKey) != 36 || request.Token != "good" {
		t.Fatalf("校验请求：%+v", request)
	}
	// 同一个令牌再来：本地防重放表拒绝，不再问提供方。
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("good"), "ip")), "challenge_invalid", "重放")
	eq(t, int(verifier.calls.Load()), 1, "重放不再问提供方")
	if len(*rejections) == 0 || (*rejections)[len(*rejections)-1][0] != "replayed" {
		t.Fatalf("拒绝原因：%v", *rejections)
	}
}

func TestChallengeGateFailsClosed(t *testing.T) {
	ctx := context.Background()
	// 提供方拒绝：不重试。
	verifier := &scripted{answers: []core.Verdict{{Reasons: []string{"invalid-input-response"}}}}
	g, rejections := gate(t, verifier)
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("t1"), "ip")), "challenge_invalid", "提供方拒绝")
	eq(t, int(verifier.calls.Load()), 1, "不重试")
	eq(t, (*rejections)[0][0], "invalid-input-response", "原因")

	// 网络错：同一个幂等键重试一次；重试成功就通过。
	verifier = &scripted{errs: []error{errors.New("连不上 https://challenges.cloudflare.com 的细节")}, answers: []core.Verdict{{}, {OK: true}}}
	g, _ = gate(t, verifier)
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("t2"), "ip")), "ok", "重试成功")
	eq(t, int(verifier.calls.Load()), 2, "重试一次")
	eq(t, verifier.requests[0].IdempotencyKey, verifier.requests[1].IdempotencyKey, "同一个幂等键")

	// 重试仍不行：challenge_invalid，只试两次。
	verifier = &scripted{errs: []error{errors.New("a"), errors.New("b"), errors.New("c")}}
	g, rejections = gate(t, verifier)
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("t3"), "ip")), "challenge_invalid", "重试仍不行")
	eq(t, int(verifier.calls.Load()), 2, "只试两次")
	eq(t, (*rejections)[0][0], "network", "原因不含错误细节")

	// 5xx 可重试，4xx 不可。
	verifier = &scripted{answers: []core.Verdict{{Reasons: []string{"network"}, Retryable: true}, {Reasons: []string{"network"}, Retryable: true}}}
	g, _ = gate(t, verifier)
	eq(t, code(g.Check(ctx, core.ScopeLogin, token("t4"), "ip")), "challenge_invalid", "两次都 5xx")
	eq(t, int(verifier.calls.Load()), 2, "5xx 重试一次")
}

func TestChallengeGateScopeAndNilGate(t *testing.T) {
	ctx := context.Background()
	g, _ := gate(t, &scripted{}, core.ScopeLogin, core.ScopeChangePassword)
	eq(t, code(g.Check(ctx, core.ScopeChangePassword, nil, "ip")), "challenge_required", "改口令在范围里")
	eq(t, code(g.Check(ctx, core.ScopeChangePassword, token("p"), "ip")), "ok", "改口令带令牌")
	var none *core.ChallengeGate
	eq(t, code(none.Check(ctx, core.ScopeLogin, nil, "ip")), "ok", "没配置的闸不要求")
	if _, err := core.NewChallengeGate(core.ChallengeGateOptions{SiteKey: "  "}); err == nil {
		t.Fatal("缺 siteKey 应报错")
	}
	if _, err := core.NewChallengeGate(core.ChallengeGateOptions{SiteKey: "k", Scope: []core.ChallengeScope{}}); err == nil {
		t.Fatal("范围为空应报错")
	}
	// action 每个入口一个。
	if core.ChallengeActions[core.ScopeLogin] != "armadra-login" || core.ChallengeActions[core.ScopeLinksAccept] != "armadra-accept" ||
		core.ChallengeActions[core.ScopeChangePassword] != "armadra-password" {
		t.Fatalf("actions：%+v", core.ChallengeActions)
	}
}

func TestReadSiteverify(t *testing.T) {
	hostnames := []string{"relay.armadra.test"}
	read := func(status int, body string) core.Verdict {
		return core.ReadSiteverify(status, []byte(body), "armadra-login", hostnames)
	}
	if v := read(200, `{"success":true,"action":"armadra-login","hostname":"Relay.Armadra.Test"}`); !v.OK {
		t.Fatalf("应通过：%+v", v)
	}
	for name, v := range map[string]core.Verdict{
		"5xx":       read(502, `{}`),
		"4xx":       read(400, `{}`),
		"不是 JSON":   read(200, `坏的`),
		"缺 success": read(200, `{"action":"armadra-login"}`),
		"success 假": read(200, `{"success":false,"error-codes":["timeout-or-duplicate"]}`),
		"没有 action": read(200, `{"success":true,"hostname":"relay.armadra.test"}`),
		"action 不对": read(200, `{"success":true,"action":"armadra-accept","hostname":"relay.armadra.test"}`),
		"没有主机名":     read(200, `{"success":true,"action":"armadra-login"}`),
		"主机名不在名单":   read(200, `{"success":true,"action":"armadra-login","hostname":"evil.test"}`),
	} {
		if v.OK {
			t.Errorf("%s 不该通过", name)
		}
	}
	if v := read(502, `{}`); !v.Retryable || v.Reasons[0] != "network" {
		t.Errorf("5xx 可重试：%+v", v)
	}
	if v := read(400, `{}`); v.Retryable || v.Reasons[0] != "bad-status" {
		t.Errorf("4xx 不可重试：%+v", v)
	}
	if v := read(200, `{"success":false,"error-codes":["internal-error"]}`); !v.Retryable {
		t.Errorf("提供方内部错误可重试：%+v", v)
	}
	if v := read(200, `{"success":false}`); v.Reasons[0] != "rejected" {
		t.Errorf("没给原因答 rejected：%+v", v)
	}
	if v := read(200, `{"success":false,"error-codes":["a","b","c","d","e","f","g","h","i","j"]}`); len(v.Reasons) != 8 {
		t.Errorf("原因最多 8 条：%d", len(v.Reasons))
	}
}

func TestSiteverifyBodyAndConfigParsing(t *testing.T) {
	body := core.SiteverifyBody("s3cret", "tok", "198.51.100.7", "key-1")
	want := url.Values{"secret": {"s3cret"}, "response": {"tok"}, "remoteip": {"198.51.100.7"}, "idempotency_key": {"key-1"}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("请求体：%v", body)
	}
	if body = core.SiteverifyBody("s", "t", "unknown", "k"); body.Has("remoteip") {
		t.Fatal("IP 未知时不带 remoteip")
	}

	hosts, err := core.ParseChallengeHostnames(" Relay.Armadra.Test, app.armadra.test ,relay.armadra.test,", false)
	if err != nil || !reflect.DeepEqual(hosts, []string{"relay.armadra.test", "app.armadra.test"}) {
		t.Fatalf("主机名：%v %v", hosts, err)
	}
	if empty, err := core.ParseChallengeHostnames("", false); err != nil || len(empty) != 0 {
		t.Fatalf("空串给空列表：%v %v", empty, err)
	}
	for _, bad := range []string{"https://x.test", "x.test:8080", "x.test/path", "-x.test"} {
		if _, err := core.ParseChallengeHostnames(bad, true); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	for _, local := range []string{"localhost", "a.localhost", "127.0.0.1", "::1"} {
		if _, err := core.ParseChallengeHostnames(local, false); err == nil {
			t.Errorf("生产环境不许 %q", local)
		}
		if _, err := core.ParseChallengeHostnames(local, true); err != nil {
			t.Errorf("开发环境许 %q：%v", local, err)
		}
	}

	scope, err := core.ParseChallengeScope("auth.login, links.accept,auth.login")
	if err != nil || !reflect.DeepEqual(scope, []core.ChallengeScope{core.ScopeLogin, core.ScopeLinksAccept}) {
		t.Fatalf("范围：%v %v", scope, err)
	}
	for _, bad := range []string{"", " , ", "auth.nope"} {
		if _, err := core.ParseChallengeScope(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestHTTPVerifierTalksToSiteverify(t *testing.T) {
	var seen url.Values
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen, contentType = r.PostForm, r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"success":true,"action":"armadra-login","hostname":"relay.armadra.test"}`))
	}))
	defer server.Close()
	verifier := core.HTTPVerifier{Secret: "s3cret", URL: server.URL}
	verdict, err := verifier.Verify(context.Background(), core.VerifyRequest{
		Token: "tok", RemoteIP: "198.51.100.7", IdempotencyKey: "k", Action: "armadra-login", Hostnames: []string{"relay.armadra.test"},
	})
	if err != nil || !verdict.OK {
		t.Fatalf("应通过：%+v %v", verdict, err)
	}
	if seen.Get("secret") != "s3cret" || seen.Get("response") != "tok" || contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("siteverify 收到的：%v %q", seen, contentType)
	}

	// 连不上：结论是可重试的 network，原因里没有地址。
	server.Close()
	verdict, err = verifier.Verify(context.Background(), core.VerifyRequest{Token: "tok", Action: "armadra-login"})
	if err != nil || verdict.OK || !verdict.Retryable || strings.Contains(strings.Join(verdict.Reasons, ","), "127.0.0.1") {
		t.Fatalf("连不上：%+v %v", verdict, err)
	}

	// 5xx 可重试，答复不成形失败关闭。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	verdict, _ = core.HTTPVerifier{Secret: "s", URL: bad.URL}.Verify(context.Background(), core.VerifyRequest{Token: "t"})
	if verdict.OK || !verdict.Retryable {
		t.Fatalf("5xx：%+v", verdict)
	}
}
