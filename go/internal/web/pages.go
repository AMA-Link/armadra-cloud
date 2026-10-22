package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 本文件对应 packages/cloud-shared/src/pages.ts：页面 CSP、内容类型、缓存判断与中继自有的两页。

// PageCSP 是托管页的 CSP（与 core Gateway 的服务器版相同）。
const PageCSP = "default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; worker-src 'self' blob:; media-src 'self' blob:; " +
	"frame-src http: https: blob:; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'none'"

// TurnstileOrigin 是 Turnstile 组件的来源。
const TurnstileOrigin = "https://challenges.cloudflare.com"

// ChallengeEmbedOrigins 是客户端内嵌挑战页时固定的受信来源（只放行回环主机的端口）。
var ChallengeEmbedOrigins = []string{
	"https://localhost",
	"capacitor://localhost",
	"http://localhost",
	"http://localhost:*",
	"http://127.0.0.1:*",
}

// BaseHeaders 是每个页面响应都带的头。
var BaseHeaders = map[string]string{
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
}

var originPattern = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://.+$`)

// validEmbedOrigin 判断额外来源能不能安全拼进 CSP：scheme://主机，不含空白、分号、逗号、引号与星号。
func validEmbedOrigin(origin string) bool {
	if !originPattern.MatchString(origin) {
		return false
	}
	rest := origin[strings.Index(origin, "://")+3:]
	for _, r := range rest {
		if unicode.IsSpace(r) || r == '\uFEFF' || strings.ContainsRune(";,'\"*", r) {
			return false
		}
	}
	return true
}

// IsChallengePath 判断是不是 /app/challenge（含子路径）。
func IsChallengePath(p string) bool {
	return p == "/app/challenge" || strings.HasPrefix(p, "/app/challenge/")
}

// PageCSPFor 是托管页（index.html）的 CSP：没配挑战原样是 PageCSP，配了放行 Turnstile，
// 且只有 /app/challenge 的 frame-ancestors 列出受信来源。
func PageCSPFor(p string, challenge *Challenge) string {
	if challenge == nil {
		return PageCSP
	}
	ancestors := []string{"'self'"}
	ancestors = append(ancestors, ChallengeEmbedOrigins...)
	for _, o := range challenge.EmbedOrigins {
		if validEmbedOrigin(o) {
			ancestors = append(ancestors, o)
		}
	}
	seen := map[string]bool{}
	uniq := ancestors[:0:0]
	for _, a := range ancestors {
		if !seen[a] {
			seen[a] = true
			uniq = append(uniq, a)
		}
	}
	directives := strings.Split(PageCSP, "; ")
	for i, d := range directives {
		switch {
		case strings.HasPrefix(d, "connect-src "), strings.HasPrefix(d, "frame-src "):
			directives[i] = d + " " + TurnstileOrigin
		case strings.HasPrefix(d, "frame-ancestors ") && IsChallengePath(p):
			directives[i] = "frame-ancestors " + strings.Join(uniq, " ")
		}
	}
	directives = append(directives, "script-src 'self' "+TurnstileOrigin)
	return strings.Join(directives, "; ")
}

var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".map":         "application/json; charset=utf-8",
	".wasm":        "application/wasm",
	".txt":         "text/plain; charset=utf-8",
}

// extensionOf 取路径最后一段的扩展名（含点，小写）；没有答空串。
func extensionOf(p string) string {
	name := p
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		name = p[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return ""
	}
	return strings.ToLower(name[dot:])
}

// ContentTypeFor 按扩展名给类型。
func ContentTypeFor(p string) string {
	if t, ok := contentTypes[extensionOf(p)]; ok {
		return t
	}
	return "application/octet-stream"
}

var hashedPattern = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// HashedAsset 判断是不是带哈希的文件名（index-D3fK9x2a.js），可以永久缓存。
func HashedAsset(name string) bool { return hashedPattern.MatchString(name) }

// PageLocale 是自有页面的语言。
type PageLocale string

// 自有页面支持的语言。
const (
	LocaleZH PageLocale = "zh-CN"
	LocaleEN PageLocale = "en"
)

// PageKind 是自有页面的种类。
type PageKind string

// 自有页面的种类。
const (
	PageUnavailable PageKind = "unavailable"
	PageJoin        PageKind = "join"
)

type message struct{ key, value string }

// pageMessages 是自有页面的文案（有序，JSON 键序与 TS 版一致：zh-CN 在前）。
var pageMessages = []struct {
	locale PageLocale
	items  []message
}{
	{LocaleZH, []message{
		{"relay.unavailable.title", "页面不可用"},
		{"relay.join.title", "加入共享"},
		{"relay.join.open", "在 Armadra 中打开"},
		{"relay.locale.switch", "English"},
	}},
	{LocaleEN, []message{
		{"relay.unavailable.title", "Page unavailable"},
		{"relay.join.title", "Join shared source"},
		{"relay.join.open", "Open in Armadra"},
		{"relay.locale.switch", "简体中文"},
	}},
}

// PageMessages 返回某语言的文案表。
func PageMessages(locale PageLocale) map[string]string {
	out := map[string]string{}
	for _, l := range pageMessages {
		if l.locale == locale {
			for _, m := range l.items {
				out[m.key] = m.value
			}
		}
	}
	return out
}

// PickLocale 按 Accept-Language 选初始语言。
func PickLocale(acceptLanguage string) PageLocale {
	first, _, _ := strings.Cut(acceptLanguage, ",")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(first)), "en") {
		return LocaleEN
	}
	return LocaleZH
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// messagesJSON 等价 JSON.stringify(PAGE_MESSAGES) 后把 < 换成 \u003c。
func messagesJSON() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range pageMessages {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(string(l.locale)) + ":{")
		for j, m := range l.items {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(m.key) + ":" + jsonString(m.value))
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return strings.ReplaceAll(b.String(), "<", `\u003c`)
}

// RenderPage 渲染中继自有页面，返回 HTML 与配套的 CSP（每次一个新 nonce）。
func RenderPage(locale PageLocale, kind PageKind, linkID string) (html, csp string) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return renderPage(locale, kind, linkID, base64.StdEncoding.EncodeToString(raw))
}

func renderPage(locale PageLocale, kind PageKind, linkID, nonce string) (string, string) {
	t := PageMessages(locale)
	titleKey := "relay.unavailable.title"
	if kind == PageJoin {
		titleKey = "relay.join.title"
	}
	var body string
	if kind == PageJoin {
		body = `<h1 data-i18n="relay.join.title">` + escapeHTML(t["relay.join.title"]) + "</h1>\n" +
			`<a class="primary" id="relay-open" href="armadra://join" data-i18n="relay.join.open">` +
			escapeHTML(t["relay.join.open"]) + "</a>"
	} else {
		body = `<h1 data-i18n="relay.unavailable.title">` + escapeHTML(t["relay.unavailable.title"]) + "</h1>"
	}
	link := ""
	if linkID != "" {
		link = ` data-link="` + escapeHTML(linkID) + `"`
	}
	html := "<!doctype html>\n" +
		`<html lang="` + string(locale) + `" data-theme="dark">` + "\n<head>\n" +
		`<meta charset="utf-8">` + "\n" +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n" +
		`<meta name="color-scheme" content="dark light">` + "\n" +
		"<title>" + escapeHTML(t[titleKey]) + "</title>\n" +
		`<style nonce="` + nonce + `">` + pageStyle + "</style>\n</head>\n" +
		`<body data-title="` + titleKey + `"` + link + ">\n" +
		`<button class="locale" id="relay-locale" type="button" data-i18n="relay.locale.switch">` +
		escapeHTML(t["relay.locale.switch"]) + "</button>\n" +
		"<main><section>\n" + body + "\n</section></main>\n" +
		`<script type="application/json" id="relay-messages">` + messagesJSON() + "</script>\n" +
		`<script nonce="` + nonce + `">` + pageScript + "</script>\n</body>\n</html>\n"
	csp := strings.Join([]string{
		"default-src 'none'",
		"style-src 'nonce-" + nonce + "'",
		"script-src 'nonce-" + nonce + "'",
		"img-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
	return html, csp
}

// percentDecode 等价 decodeURIComponent：非法转义答 false；结果必须是合法 UTF-8。
func percentDecode(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	out, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(out) {
		return "", false
	}
	return out, true
}
