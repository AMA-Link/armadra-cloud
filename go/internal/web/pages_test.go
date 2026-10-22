package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMessagesAligned(t *testing.T) {
	zh, en := PageMessages(LocaleZH), PageMessages(LocaleEN)
	if len(zh) != len(en) || len(zh) == 0 {
		t.Fatal("size")
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
}

func TestPickLocale(t *testing.T) {
	for in, want := range map[string]PageLocale{
		"en-US,en;q=0.9": LocaleEN, "en-GB,en": LocaleEN, " EN": LocaleEN,
		"zh-CN,zh;q=0.9": LocaleZH, "": LocaleZH, "fr,en": LocaleZH,
	} {
		if got := PickLocale(in); got != want {
			t.Errorf("%q -> %s", in, got)
		}
	}
}

func TestContentTypeAndHash(t *testing.T) {
	for in, want := range map[string]string{
		"/assets/index-D3fK9x2a.js": "text/javascript; charset=utf-8",
		"dir.v1/README":             "application/octet-stream",
		"ICON.PNG":                  "image/png",
		".gitignore":                "application/octet-stream",
		`a\b.css`:                   "text/css; charset=utf-8",
	} {
		if got := ContentTypeFor(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if !HashedAsset("index-D3fK9x2a.js") || HashedAsset("index.html") || HashedAsset("a-short.js") {
		t.Fatal("hashed")
	}
}

func TestRenderNonce(t *testing.T) {
	html, csp := RenderPage(LocaleEN, PageJoin, link)
	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
	if m == nil || !strings.Contains(html, `nonce="`+m[1]+`"`) {
		t.Fatal("nonce")
	}
	for _, want := range []string{`data-link="` + link + `"`, "Open in Armadra"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %s", want)
		}
	}
	other, ocsp := RenderPage(LocaleZH, PageUnavailable, "")
	if ocsp == csp || !strings.Contains(other, "页面不可用") {
		t.Fatal("other")
	}
}

func directive(csp, name string) string {
	for _, d := range strings.Split(csp, "; ") {
		if strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	return ""
}

func TestPageCSPFor(t *testing.T) {
	if PageCSPFor("/app/challenge", nil) != PageCSP || PageCSPFor("/app/", nil) != PageCSP ||
		strings.Contains(PageCSP, "challenges.cloudflare.com") || directive(PageCSP, "frame-ancestors") != "frame-ancestors 'none'" {
		t.Fatal("off")
	}
	csp := PageCSPFor("/app/challenge", &Challenge{EmbedOrigins: []string{"https://relay.test", "https://x.test;script-src *", "*", "https://relay.test", "https://a b.test"}})
	if directive(csp, "script-src") != "script-src 'self' https://challenges.cloudflare.com" ||
		!strings.Contains(directive(csp, "connect-src"), TurnstileOrigin) ||
		!strings.Contains(directive(csp, "frame-src"), TurnstileOrigin) ||
		directive(csp, "frame-ancestors") != "frame-ancestors 'self' https://localhost capacitor://localhost http://localhost http://localhost:* http://127.0.0.1:* https://relay.test" ||
		strings.Contains(csp, "*;") {
		t.Fatalf("on %q", csp)
	}
	for _, p := range []string{"/app/", "/app/board/1", "/app/challengex"} {
		if directive(PageCSPFor(p, &Challenge{}), "frame-ancestors") != "frame-ancestors 'none'" {
			t.Fatalf("%s", p)
		}
	}
	if !IsChallengePath("/app/challenge/x") || IsChallengePath("/app/challengex") {
		t.Fatal("IsChallengePath")
	}
	allowed := regexp.MustCompile(`^('self'|(https?|capacitor)://(localhost|127\.0\.0\.1)(:\*)?)$`)
	for _, s := range strings.Fields(directive(PageCSPFor("/app/challenge", &Challenge{}), "frame-ancestors"))[1:] {
		if !allowed.MatchString(s) {
			t.Fatalf("source %s", s)
		}
	}
}

// golden 对照 TS 版 renderPage 的输出（testdata/dump-golden.mjs 生成，nonce 统一成 NONCE）。
func TestGolden(t *testing.T) {
	for _, locale := range []PageLocale{LocaleZH, LocaleEN} {
		for _, kind := range []PageKind{PageUnavailable, PageJoin} {
			id := ""
			if kind == PageJoin {
				id = link
			}
			raw, err := os.ReadFile(filepath.Join("testdata", string(locale)+"_"+string(kind)+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			html, csp := renderPage(locale, kind, id, "NONCE")
			if want := csp + "\n=====\n" + html; string(raw) != want {
				t.Errorf("%s/%s differs from golden", locale, kind)
			}
		}
	}
}
