package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Options 配置托管页面。
type Options struct {
	// Root 是 apps/web 的构建产物目录；空 = 没有产物。
	Root string
	// CAPEM 是 /ca.crt 发的 PEM；返回 nil = 没有（不认领）。
	CAPEM func() []byte
	// Challenge 开了 Turnstile 才给：页面 CSP 放行 Turnstile，并允许这些来源内嵌 /app/challenge。
	Challenge func() *Challenge
}

// Challenge 是开了人机挑战时页面要的东西。
type Challenge struct{ EmbedOrigins []string }

// Pages 是托管页面的处理器。
type Pages struct {
	opts     Options
	rootOnce sync.Once
	rootReal string
	rootErr  error
}

var linkPath = regexp.MustCompile(`^/j/[0-9a-f]{16}/?$`)

// UsableRoot 目录里有 index.html 才答它（绝对路径），否则答空串。
func UsableRoot(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
		return ""
	}
	return abs
}

// New 建处理器。
func New(o Options) *Pages { return &Pages{opts: o} }

// Available 是有没有页面产物。
func (p *Pages) Available() bool { return p.opts.Root != "" }

func (p *Pages) challenge() *Challenge {
	if p.opts.Challenge == nil {
		return nil
	}
	return p.opts.Challenge()
}

func (p *Pages) caPEM() []byte {
	if p.opts.CAPEM == nil {
		return nil
	}
	return p.opts.CAPEM()
}

func setBase(h http.Header) {
	for k, v := range BaseHeaders {
		h.Set(k, v)
	}
}

// requestPath 取请求目标的原始路径（不解码、不含查询）。
func requestPath(r *http.Request) string {
	target := r.RequestURI
	if !strings.HasPrefix(target, "/") {
		target = r.URL.EscapedPath()
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "/"
	}
	return target
}

// Handle 认领 /、/app、/app/*、/j/<id>、/ca.crt 与根目录文件；认领了返回 true。
func (p *Pages) Handle(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	path := requestPath(r)
	root := p.opts.Root
	var mine bool
	switch {
	case path == "/ca.crt":
		mine = p.caPEM() != nil
	case path == "/" || path == "/app" || strings.HasPrefix(path, "/app/") || linkPath.MatchString(path):
		mine = true
	case root != "" && !strings.HasSuffix(path, "/"):
		_, err := os.Stat(filepath.Join(root, path))
		mine = err == nil
	}
	if !mine {
		return false
	}
	if !p.route(w, r, path) {
		h := w.Header()
		setBase(h)
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"code":"not_found","message":"Not found"}`)
		}
	}
	return true
}

func (p *Pages) route(w http.ResponseWriter, r *http.Request, path string) bool {
	root := p.opts.Root
	switch {
	case path == "/ca.crt":
		pem := p.caPEM()
		if pem == nil {
			return false
		}
		h := w.Header()
		setBase(h)
		h.Set("Content-Type", "application/x-pem-file")
		h.Set("Content-Disposition", `attachment; filename="armadra-relay-ca.crt"`)
		h.Set("Content-Length", strconv.Itoa(len(pem)))
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(pem)
		}
		return true
	case path == "/" || path == "/app":
		setBase(w.Header())
		w.Header().Set("Location", "/app/")
		w.WriteHeader(http.StatusPermanentRedirect)
		return true
	case linkPath.MatchString(path):
		if root != "" {
			p.serveIndex(w, r, path)
		} else {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/j/"), "/")
			sendPage(w, r, http.StatusOK, PageJoin, id)
		}
		return true
	case strings.HasPrefix(path, "/app/"):
		if root == "" {
			sendPage(w, r, http.StatusServiceUnavailable, PageUnavailable, "")
			return true
		}
		if file := p.locate(strings.TrimPrefix(path, "/app/")); file != "" {
			p.serveFile(w, r, file, path)
		} else {
			p.serveIndex(w, r, path)
		}
		return true
	}
	if root == "" || strings.HasSuffix(path, "/") {
		return false
	}
	file := p.locate(path[1:])
	if file == "" {
		return false
	}
	p.serveFile(w, r, file, "/")
	return true
}

func sendPage(w http.ResponseWriter, r *http.Request, status int, kind PageKind, linkID string) {
	html, csp := RenderPage(PickLocale(r.Header.Get("Accept-Language")), kind, linkID)
	h := w.Header()
	setBase(h)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(html)))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, html)
	}
}

func (p *Pages) serveIndex(w http.ResponseWriter, r *http.Request, path string) {
	p.serveFile(w, r, filepath.Join(p.opts.Root, "index.html"), path)
}

func (p *Pages) serveFile(w http.ResponseWriter, r *http.Request, file, path string) {
	f, err := os.Open(file)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	isHTML := strings.EqualFold(filepath.Ext(file), ".html")
	h := w.Header()
	setBase(h)
	h.Set("Content-Type", ContentTypeFor(file))
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	switch {
	case isHTML:
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", PageCSPFor(path, p.challenge()))
	case HashedAsset(filepath.Base(file)):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		h.Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// locate 把请求路径变成根内的文件；越界、不是文件都答空串。
// 先在字符串上收敛到根内，再用 EvalSymlinks 复核（挡符号链接）。
func (p *Pages) locate(relative string) string {
	root := p.opts.Root
	if root == "" {
		return ""
	}
	decoded, ok := percentDecode(relative)
	if !ok || strings.ContainsRune(decoded, 0) {
		return ""
	}
	sep := string(filepath.Separator)
	candidate := filepath.Join(root, "."+sep+decoded)
	if candidate != root && !strings.HasPrefix(candidate, root+sep) {
		return ""
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	p.rootOnce.Do(func() { p.rootReal, p.rootErr = filepath.EvalSymlinks(root) })
	if p.rootErr != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil || !strings.HasPrefix(real, p.rootReal+sep) {
		return ""
	}
	return real
}
