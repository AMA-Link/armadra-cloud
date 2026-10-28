package web

import (
	"io"
	"net/http"
	"strings"
)

// 控制台页面（armadra-cloud 的 WEB_ROOT，SaaS 实施设计 §5.3）：单页应用，这些路径回落 index.html。
var appExact = map[string]bool{"/": true, "/login": true, "/register": true, "/verify": true, "/reset": true, "/device": true}

// 带子路径的前缀（含前缀本身）。
var appPrefixes = []string{"/me/", "/t/", "/h/", "/j/", "/invite/", "/ops/"}

// IsAppRoute 判断路径是不是控制台的页面路由（回落 index.html）。
func IsAppRoute(path string) bool {
	if appExact[path] {
		return true
	}
	for _, prefix := range appPrefixes {
		if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}

// ConsoleCSP 是控制台页面的 CSP（SaaS 实施设计 §5.3）；relayDomain 是源地址 `<sourceId>.src.<域>` 的域。
func ConsoleCSP(relayDomain string) string {
	// style-src 'unsafe-inline'：0.10.0 的控制台页面（开屏）有内联样式，实测不放行会被拦。
	// /h/ 打开主机（SaaS 主机打开设计 §5.4）：经中继取回的主机图片、音视频与 Mermaid 预览是 blob:（图表另有 data:），
	// PDF 预览是 blob: 框架；页面装了 Trusted Types 策略 armadra / armadra-sanitize / default，Mermaid 自带的净化器建 dompurify。
	return "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' wss://*.src." + relayDomain + " " + TurnstileOrigin +
		"; script-src 'self' " + TurnstileOrigin + "; frame-src " + TurnstileOrigin + " blob:" +
		"; img-src 'self' blob: data:; media-src 'self' blob:" +
		"; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'" +
		"; require-trusted-types-for 'script'; trusted-types armadra armadra-sanitize default dompurify"
}

// HandleApp 只认 GET / HEAD：页面路由回落 index.html，根目录里真实存在的文件（资源）原样给，
// 其余返回 false（交给调用方答 404）。所有 HTML 带 Referrer-Policy: no-referrer 与 Options.CSP。
func (p *Pages) HandleApp(w http.ResponseWriter, r *http.Request) bool {
	if p.opts.Root == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	path := requestPath(r)
	if IsAppRoute(path) {
		p.serveIndex(w, r, path)
		return true
	}
	if strings.HasSuffix(path, "/") {
		return false
	}
	file := p.locate(path[1:])
	if file == "" {
		return false
	}
	p.serveFile(w, r, file, path)
	return true
}

// NotFound 答控制台范围内的 404（JSON，与 API 的错误形状一致）。
func NotFound(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	setBase(h)
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, `{"code":"not_found","message":"Not found"}`)
	}
}
