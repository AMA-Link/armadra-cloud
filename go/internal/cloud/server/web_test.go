package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WEB_ROOT 托管控制台页面（设计 §5.3）：页面路由回落 index.html，资源原样给，CSP 与 Referrer-Policy 固定，
// API 路径不受影响；没配或目录里没有 index.html 时 / 答 404 且 platform.info 不报 webApp。
func TestWebRootHostsConsole(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<!doctype html><title>console</title>", "assets/app-AbCdEf12.js": "console.log(1)"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, rigOptions{env: map[string]string{"WEB_ROOT": root}})
	const csp = "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' wss://*.src.src.localhost https://challenges.cloudflare.com; " +
		"script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com blob:; img-src 'self' blob: data:; media-src 'self' blob:; " +
		"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'; " +
		"require-trusted-types-for 'script'; trusted-types armadra armadra-sanitize default dompurify"
	for _, path := range []string{"/", "/login", "/register", "/verify", "/reset", "/device", "/me", "/me/hosts", "/me/hosts/h1", "/me/links", "/me/account", "/me/tokens", "/t/abc", "/t/abc/members", "/h/abc/x", "/j/abc", "/invite/i1", "/ops/teams", "/ops"} {
		res := r.do("GET", path, nil, nil)
		r.expect(res, 200, "")
		if !strings.Contains(string(res.body), "<title>console</title>") ||
			res.header.Get("Content-Security-Policy") != csp ||
			res.header.Get("Referrer-Policy") != "no-referrer" ||
			res.header.Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(res.header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s：%d %v %s", path, res.status, res.header, res.body)
		}
	}
	asset := r.do("GET", "/assets/app-AbCdEf12.js", nil, nil)
	r.expect(asset, 200, "")
	if string(asset.body) != "console.log(1)" || !strings.Contains(asset.header.Get("Cache-Control"), "immutable") || asset.header.Get("Content-Security-Policy") != "" {
		t.Fatalf("资源：%v %s", asset.header, asset.body)
	}
	r.expect(r.do("GET", "/assets/missing.js", nil, nil), 404, "not_found")
	r.expect(r.do("GET", "/nowhere", nil, nil), 404, "not_found")
	r.expect(r.do("GET", "/assets/", nil, nil), 404, "not_found")
	r.expect(r.do("GET", "/..%2f..%2fetc/passwd", nil, nil), 404, "not_found")
	r.expect(r.do("POST", "/login", map[string]any{}, nil), 404, "not_found")
	r.expect(r.do("GET", "/health", nil, nil), 200, "")
	info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
	r.expect(info, 200, "")
	if info.json["webApp"] != appOrigin {
		t.Fatalf("platform.info.webApp：%s", info.body)
	}
	for _, path := range []string{"/v1/nowhere", "/v1/me/nowhere", "/v1/unknown/x"} {
		res := r.do("GET", path, nil, nil)
		if strings.Contains(string(res.body), "<title>console</title>") {
			t.Fatalf("%s 不该回落成 index.html", path)
		}
	}
	if r.do("GET", "/v1/me", nil, nil).status == 200 {
		t.Fatal("API 路径不该被页面吞掉")
	}
}

func TestWebRootWithoutIndexIsIgnored(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"没配":      nil,
		"目录里没有页面": {"WEB_ROOT": t.TempDir()},
		"目录不存在":   {"WEB_ROOT": filepath.Join(t.TempDir(), "none")},
	} {
		r := newRig(t, rigOptions{env: env})
		r.expect(r.do("GET", "/", nil, nil), 404, "not_found")
		r.expect(r.do("GET", "/login", nil, nil), 404, "not_found")
		info := r.do("GET", "/.well-known/armadra-platform", nil, nil)
		if info.json["webApp"] != nil {
			t.Fatalf("%s：不该报 webApp：%s", name, info.body)
		}
	}
}
