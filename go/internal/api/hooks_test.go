package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func hookAPI(t *testing.T, options api.Options) *httptest.Server {
	t.Helper()
	if options.Handlers == nil {
		options.Handlers = map[string]api.Handler{}
	}
	options.Mode = api.ModeCloud
	server, err := api.New(options)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(server.ServeHTTP))
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// Before 在入参校验之后、处理器之前：入参不合格先答 400，不进 Before；Before 的错误带出 call.Header。
func TestBeforeHookRunsAfterValidationAndCarriesHeaders(t *testing.T) {
	var called []string
	ts := hookAPI(t, api.Options{
		Handlers: map[string]api.Handler{
			"auth.logout": func(context.Context, *api.Call) (any, error) {
				called = append(called, "handler")
				return map[string]any{}, nil
			},
		},
		Before: func(call *api.Call) error {
			called = append(called, "before:"+call.Procedure)
			if call.Request.Header.Get("X-Reject") != "" {
				call.Header.Add("Set-Cookie", "a=1")
				call.Header.Add("Set-Cookie", "b=2")
				return core.Fail(gen.ErrUnauthenticated)
			}
			return nil
		},
	})
	// 入参不合格（scope 取值不对）：先 400，Before 不跑。
	resp := do(t, "POST", ts.URL+"/v1/auth/logout", `{"scope":"nope"}`, nil)
	if resp.StatusCode != 400 || len(called) != 0 {
		t.Fatalf("入参校验先于 Before：%d %v", resp.StatusCode, called)
	}
	resp = do(t, "POST", ts.URL+"/v1/auth/logout", `{}`, map[string]string{"X-Reject": "1"})
	if resp.StatusCode != 401 || len(resp.Header.Values("Set-Cookie")) != 2 {
		t.Fatalf("Before 的错误带出多个同名头：%d %v", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	}
	resp = do(t, "POST", ts.URL+"/v1/auth/logout", `{}`, nil)
	if resp.StatusCode != 200 || strings.Join(called, ",") != "before:auth.logout,before:auth.logout,handler" {
		t.Fatalf("正常路径：%d %v", resp.StatusCode, called)
	}
	// 没有处理器的过程（501）不进 Before。
	called = nil
	if resp = do(t, "GET", ts.URL+"/v1/me", "", nil); resp.StatusCode != 501 || len(called) != 0 {
		t.Fatalf("501 不进 Before：%d %v", resp.StatusCode, called)
	}
}

// Authenticate 取代缺省认证，惰性且只跑一次；处理器写进 call.Header 的头在成功与失败的答复里都带出。
func TestAuthenticateHookAndResponseHeaders(t *testing.T) {
	runs := 0
	ts := hookAPI(t, api.Options{
		Handlers: map[string]api.Handler{
			"auth.logout": func(ctx context.Context, call *api.Call) (any, error) {
				if _, err := call.Caller(ctx); err != nil {
					return nil, err
				}
				if _, err := call.Caller(ctx); err != nil {
					return nil, err
				}
				call.Header.Add("Set-Cookie", "x=1; Max-Age=0")
				call.Header.Add("Set-Cookie", "y=1; Max-Age=0")
				return map[string]any{}, nil
			},
		},
		Authenticate: func(_ context.Context, call *api.Call) (core.Caller, error) {
			runs++
			if call.Request.Header.Get("X-User") == "" {
				call.Header.Add("Set-Cookie", "clear=1; Max-Age=0")
				return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
			}
			return core.Caller{Subject: call.Request.Header.Get("X-User")}, nil
		},
	})
	resp := do(t, "POST", ts.URL+"/v1/auth/logout", `{}`, nil)
	if resp.StatusCode != 401 || resp.Header.Get("Set-Cookie") != "clear=1; Max-Age=0" {
		t.Fatalf("失败也带 call.Header：%d %v", resp.StatusCode, resp.Header)
	}
	runs = 0
	resp = do(t, "POST", ts.URL+"/v1/auth/logout", `{}`, map[string]string{"X-User": "u"})
	if resp.StatusCode != 200 || len(resp.Header.Values("Set-Cookie")) != 2 || runs != 1 {
		t.Fatalf("成功带多个同名头、只认证一次：%d %v runs=%d", resp.StatusCode, resp.Header.Values("Set-Cookie"), runs)
	}
}
