package httperr

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 对应 errors.test.ts：状态行原因短语与标准库逐字相同（注册表里的全部状态码）。
func TestStatusText(t *testing.T) {
	statuses := map[int]bool{101: true, 200: true, 204: true}
	for _, code := range gen.ErrorCodes {
		if s, ok := code.HTTPStatus(); ok {
			statuses[s] = true
		}
	}
	for s := range statuses {
		if got, want := StatusText(s), http.StatusText(s); got != want {
			t.Errorf("%d: %q != %q", s, got, want)
		}
	}
	if StatusText(599) != "" {
		t.Error("未知状态码应答空串")
	}
}

// 对应 errors.test.ts：裸错误响应的状态行、定长 JSON 体、no-store、connection: close 与 CORS 头。
func TestRaw(t *testing.T) {
	got := string(Raw(gen.ErrSourceOffline, map[string]string{
		"access-control-allow-origin": "https://app.example",
	}))
	status, _ := gen.ErrSourceOffline.HTTPStatus()
	body := `{"code":"source_offline","message":"` + Message(gen.ErrSourceOffline) + `"}`
	want := "HTTP/1.1 " + strconv.Itoa(status) + " Service Unavailable\r\n" +
		"content-type: application/json; charset=utf-8\r\n" +
		"content-length: " + strconv.Itoa(len(body)) + "\r\n" +
		"cache-control: no-store\r\nconnection: close\r\n" +
		"access-control-allow-origin: https://app.example\r\n\r\n" + body
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRawCORSSorted(t *testing.T) {
	got := string(Raw(gen.ErrNotFound, map[string]string{"vary": "origin", "access-control-allow-origin": "x"}))
	i := len("HTTP/1.1 404 Not Found\r\n")
	if got[:i] != "HTTP/1.1 404 Not Found\r\n" {
		t.Fatal(got)
	}
	a, v := indexOf(got, "access-control-allow-origin"), indexOf(got, "vary:")
	if a < 0 || v < 0 || a > v {
		t.Fatalf("CORS 头应按键名排序：%q", got)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 对应 errors.test.ts：bad_gateway 登记为 502，有固定文案。
func TestBadGateway(t *testing.T) {
	if s, ok := gen.ErrBadGateway.HTTPStatus(); !ok || s != 502 {
		t.Fatalf("status = %d %v", s, ok)
	}
	if Message(gen.ErrBadGateway) == string(gen.ErrBadGateway) {
		t.Fatal("应有固定文案")
	}
}

func TestMessageFallback(t *testing.T) {
	if Message(gen.ErrorCode("nope")) != "nope" {
		t.Fatal("没登记的码应回退为码本身")
	}
}

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, gen.ErrRateLimited, Options{
		RequestID:  "req-1",
		RetryAfter: 1500 * time.Millisecond,
		CORS:       map[string]string{"access-control-allow-origin": "https://app.example", "vary": "origin"},
	})
	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	h := rec.Header()
	for k, v := range map[string]string{
		"retry-after":                 "2",
		"cache-control":               "no-store",
		"content-type":                "application/json; charset=utf-8",
		"access-control-allow-origin": "https://app.example",
		"vary":                        "origin",
	} {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	want := `{"code":"rate_limited","message":"Too many requests","requestId":"req-1"}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if h.Get("content-length") != strconv.Itoa(len(want)) {
		t.Fatalf("content-length = %s", h.Get("content-length"))
	}
}

func TestWriteRetryAfterMinOne(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, gen.ErrRateLimited, Options{RetryAfter: time.Millisecond})
	if rec.Header().Get("retry-after") != "1" {
		t.Fatalf("retry-after = %q", rec.Header().Get("retry-after"))
	}
	rec = httptest.NewRecorder()
	Write(rec, gen.ErrNotFound, Options{})
	if rec.Header().Get("retry-after") != "" || rec.Body.String() != `{"code":"not_found","message":"Not found"}` {
		t.Fatalf("无重试等待时不应有 retry-after：%v %s", rec.Header(), rec.Body.String())
	}
}
