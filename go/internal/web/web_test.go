package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const link = "0123456789abcdef"

type fixture struct {
	root, outside string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "dist")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "assets"), 0o755))
	must(os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>armadra</title>"), 0o644))
	must(os.WriteFile(filepath.Join(root, "assets", "index-B9KJZmSX.js"), []byte("console.log(1)"), 0o644))
	must(os.WriteFile(filepath.Join(root, "icon.png"), []byte("png"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".hidden"), []byte("dot"), 0o644))
	must(os.WriteFile(filepath.Join(base, "secret.txt"), []byte("outside"), 0o644))
	must(os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(root, "assets", "leak.txt")))
	must(os.Symlink(base, filepath.Join(root, "up")))
	return fixture{root: root, outside: base}
}

func serve(p *Pages) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.Handle(w, r) {
			w.WriteHeader(http.StatusTeapot)
		}
	})
}

type resp struct {
	code int
	h    http.Header
	body string
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) resp {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return resp{rec.Code, rec.Header(), string(b)}
}

func TestUsableRoot(t *testing.T) {
	fx := newFixture(t)
	if got := UsableRoot(fx.root); got != fx.root {
		t.Fatalf("got %q", got)
	}
	for _, dir := range []string{"", filepath.Join(fx.outside, "none"), filepath.Join(fx.root, "assets")} {
		if got := UsableRoot(dir); got != "" {
			t.Fatalf("%q -> %q", dir, got)
		}
	}
}

func TestWithRoot(t *testing.T) {
	fx := newFixture(t)
	pem := []byte("PEM")
	p := New(Options{Root: fx.root, CAPEM: func() []byte { return pem }})
	h := serve(p)
	if !p.Available() {
		t.Fatal("available")
	}

	for _, path := range []string{"/", "/app"} {
		r := do(t, h, "GET", path, nil)
		if r.code != 308 || r.h.Get("Location") != "/app/" {
			t.Fatalf("%s: %d %v", path, r.code, r.h)
		}
	}
	for _, path := range []string{"/app/", "/app/board/42", "/j/" + link, "/j/" + link + "/", "/app/?x=1"} {
		r := do(t, h, "GET", path, nil)
		if r.code != 200 || !strings.Contains(r.body, "<title>armadra</title>") {
			t.Fatalf("%s: %d %q", path, r.code, r.body)
		}
		if r.h.Get("Content-Security-Policy") != PageCSP || r.h.Get("Cache-Control") != "no-store" ||
			r.h.Get("Referrer-Policy") != "no-referrer" || r.h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: headers %v", path, r.h)
		}
	}
	if !strings.Contains(PageCSP, "connect-src 'self'") {
		t.Fatal("csp")
	}

	asset := do(t, h, "GET", "/assets/index-B9KJZmSX.js", nil)
	if asset.code != 200 || !strings.Contains(asset.h.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(asset.h.Get("Cache-Control"), "immutable") || asset.body != "console.log(1)" {
		t.Fatalf("asset %+v", asset)
	}
	if asset.h.Get("Content-Length") != "14" {
		t.Fatalf("length %q", asset.h.Get("Content-Length"))
	}
	icon := do(t, h, "GET", "/app/icon.png", nil)
	if icon.h.Get("Content-Type") != "image/png" || icon.h.Get("Cache-Control") != "no-cache" {
		t.Fatalf("icon %v", icon.h)
	}
	if do(t, h, "GET", "/icon.png", nil).body != "png" {
		t.Fatal("root file")
	}
	if do(t, h, "GET", "/.hidden", nil).body != "dot" {
		t.Fatal("dotfile is served like web.ts")
	}

	head := do(t, h, "HEAD", "/assets/index-B9KJZmSX.js", nil)
	if head.code != 200 || head.body != "" || head.h.Get("Content-Length") != "14" {
		t.Fatalf("head %+v", head)
	}
	if r := do(t, h, "POST", "/app/", nil); r.code != http.StatusTeapot {
		t.Fatalf("post %d", r.code)
	}

	ca := do(t, h, "GET", "/ca.crt", nil)
	if ca.body != "PEM" || ca.h.Get("Content-Type") != "application/x-pem-file" ||
		ca.h.Get("Content-Disposition") != `attachment; filename="armadra-relay-ca.crt"` ||
		ca.h.Get("Cache-Control") != "no-store" {
		t.Fatalf("ca %+v", ca)
	}
	if r := do(t, h, "HEAD", "/ca.crt", nil); r.body != "" || r.h.Get("Content-Length") != "3" {
		t.Fatalf("ca head %+v", r)
	}

	// 未知路径不认领；根下存在的目录被认领后 404 JSON。
	if r := do(t, h, "GET", "/nope.js", nil); r.code != http.StatusTeapot {
		t.Fatalf("unknown %d", r.code)
	}
	if r := do(t, h, "GET", "/assets", nil); r.code != 404 || !strings.Contains(r.body, `"code":"not_found"`) {
		t.Fatalf("dir %+v", r)
	}
}

func TestConfinement(t *testing.T) {
	fx := newFixture(t)
	h := serve(New(Options{Root: fx.root}))
	for _, path := range []string{
		"/app/..%2f..%2fsecret.txt",
		"/app/..%2fsecret.txt",
		"/app/%2e%2e/secret.txt",
		"/app/%2e%2e%2fsecret.txt",
		"/app/../secret.txt",
		"/app/..%5csecret.txt",
		"/app/assets/leak.txt",
		"/assets/leak.txt",
		"/up/secret.txt",
		"/app/up/secret.txt",
		"/../secret.txt",
		"/app/%00",
		"/app/index.html%00.png",
	} {
		r := do(t, h, "GET", path, nil)
		if strings.Contains(r.body, "outside") {
			t.Fatalf("%s leaked", path)
		}
	}
	// 目录与编码后的斜杠都退回 index.html。
	if r := do(t, h, "GET", "/app/assets", nil); !strings.Contains(r.body, "<title>armadra</title>") {
		t.Fatalf("dir -> %q", r.body)
	}
}

func TestWithoutRoot(t *testing.T) {
	p := New(Options{Root: ""})
	h := serve(p)
	if p.Available() {
		t.Fatal("available")
	}
	zh := do(t, h, "GET", "/app/", nil)
	if zh.code != 503 || !strings.Contains(zh.body, `lang="zh-CN"`) || !strings.Contains(zh.body, "页面不可用") ||
		!strings.Contains(zh.body, `id="relay-locale"`) {
		t.Fatalf("zh %d", zh.code)
	}
	csp := zh.h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !regexp.MustCompile(`script-src 'nonce-[A-Za-z0-9+/=]+'`).MatchString(csp) {
		t.Fatalf("csp %q", csp)
	}
	if zh.h.Get("Cache-Control") != "no-store" {
		t.Fatal("cache")
	}
	en := do(t, h, "GET", "/app/x", map[string]string{"Accept-Language": "en-US,en;q=0.9"})
	if !strings.Contains(en.body, "Page unavailable") {
		t.Fatal("en")
	}
	landing := do(t, h, "GET", "/j/"+link, nil)
	if landing.code != 200 || !strings.Contains(landing.body, `data-link="`+link+`"`) ||
		!strings.Contains(landing.body, "在 Armadra 中打开") || !strings.Contains(landing.body, "armadra://join?") ||
		!strings.Contains(landing.body, "location.hash") {
		t.Fatalf("landing %d", landing.code)
	}
	if r := do(t, h, "HEAD", "/j/"+link, nil); r.body != "" || r.h.Get("Content-Length") == "" {
		t.Fatalf("head %+v", r)
	}
	if r := do(t, h, "GET", "/ca.crt", nil); r.code != http.StatusTeapot {
		t.Fatalf("ca %d", r.code)
	}
	if r := do(t, h, "GET", "/assets/x.js", nil); r.code != http.StatusTeapot {
		t.Fatalf("asset %d", r.code)
	}
	if r := do(t, h, "GET", "/j/short", nil); r.code != http.StatusTeapot {
		t.Fatalf("bad link %d", r.code)
	}
}

func TestChallenge(t *testing.T) {
	fx := newFixture(t)
	with := serve(New(Options{Root: fx.root, Challenge: func() *Challenge {
		return &Challenge{EmbedOrigins: []string{"https://relay.test"}}
	}}))
	c := do(t, with, "GET", "/app/challenge", nil).h.Get("Content-Security-Policy")
	if !strings.Contains(c, "script-src 'self' https://challenges.cloudflare.com") ||
		!strings.Contains(c, "frame-ancestors 'self' https://localhost capacitor://localhost http://localhost http://localhost:* http://127.0.0.1:* https://relay.test") {
		t.Fatalf("csp %q", c)
	}
	if o := do(t, with, "GET", "/app/board/1", nil).h.Get("Content-Security-Policy"); !strings.Contains(o, "frame-ancestors 'none'") {
		t.Fatalf("other %q", o)
	}
	without := serve(New(Options{Root: fx.root}))
	if got := do(t, without, "GET", "/app/challenge", nil).h.Get("Content-Security-Policy"); got != PageCSP {
		t.Fatalf("off %q", got)
	}
}

func TestPercentDecode(t *testing.T) {
	if got, ok := percentDecode("a%20b+c"); !ok || got != "a b+c" {
		t.Fatalf("%q %v", got, ok)
	}
	for _, bad := range []string{"%zz", "%", "%ff"} {
		if _, ok := percentDecode(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}
