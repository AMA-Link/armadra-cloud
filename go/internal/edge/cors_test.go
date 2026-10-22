package edge

import (
	"reflect"
	"testing"
)

// 对应 cors.test.ts「名单：原生 App 两个来源总放行；其余按规范拼法与可信来源比」。
func TestCORSOriginAllowed(t *testing.T) {
	for _, origin := range NativeAppOrigins {
		if !OriginAllowed(origin, nil) {
			t.Errorf("%s 应放行", origin)
		}
	}
	trusted := []string{"https://Relay.Example:443", "http://127.0.0.1:5173"}
	for origin, want := range map[string]bool{
		"https://relay.example":      true,
		"http://127.0.0.1:5173":      true,
		"https://evil.example":       false,
		"https://relay.example:8443": false,
		// 不是规范拼法的 Origin（浏览器不会这样发）一律不认。
		"https://relay.example/": false,
		"https://RELAY.example":  false,
		"null":                   false,
		"*":                      false,
		"capacitor://evil":       false,
		"http://localhost":       false,
	} {
		if got := OriginAllowed(origin, trusted); got != want {
			t.Errorf("%s: %v", origin, got)
		}
	}
	if OriginAllowed("*", []string{"*"}) {
		t.Error("* 不能进名单")
	}
}

// 对应 cors.test.ts「错误答复：名单内回显 + Vary + 暴露 retry-after；名单外、无 Origin、* 都不加」。
func TestCORSErrorHeaders(t *testing.T) {
	want := map[string]string{
		"access-control-allow-origin":   "https://app.example",
		"access-control-expose-headers": "retry-after",
		"vary":                          "origin",
	}
	if got := ErrorCORSHeaders("https://app.example", true, []string{"https://app.example"}); !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
	if got := ErrorCORSHeaders("capacitor://localhost", true, nil); got["access-control-allow-origin"] != "capacitor://localhost" {
		t.Errorf("%v", got)
	}
	if ErrorCORSHeaders("https://evil.example", true, []string{"https://app.example"}) != nil {
		t.Error("名单外应为 nil")
	}
	if ErrorCORSHeaders("", false, []string{"https://app.example"}) != nil {
		t.Error("无 Origin 应为 nil")
	}
	if ErrorCORSHeaders("*", true, []string{"*"}) != nil {
		t.Error("* 应为 nil")
	}
}

// WHATWG `new URL(v).origin` 的规范化行为。
func TestCORSCanonical(t *testing.T) {
	good := map[string]string{
		"https://Relay.Example:443": "https://relay.example",
		"HTTP://a.b:80":             "http://a.b",
		"http://a.b:0080":           "http://a.b",
		"http://a.b:8080/":          "http://a.b:8080",
		"https://a.b?":              "https://a.b",
		"https://a.b#":              "https://a.b",
		"https://[::1]:3000":        "https://[::1]:3000",
		"https://[0:0:0:0:0:0:0:1]": "https://[::1]",
		" https://a.b ":             "https://a.b",
		"https://a.b:":              "https://a.b",
	}
	for in, want := range good {
		if got, ok := canonical(in); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"https://u@a.b", "https://u:p@a.b", "https://a.b/x", "https://a.b//", "https://a.b?q=1", "https://a.b#f",
		"ftp://a.b", "capacitor://localhost", "null", "", "https://", "https://a.b:99999", "https://a.b:x", "https://a b",
		"https://[::1", "https://[zz]",
	} {
		if got, ok := canonical(in); ok {
			t.Errorf("%q 应被拒绝：%q", in, got)
		}
	}
}
