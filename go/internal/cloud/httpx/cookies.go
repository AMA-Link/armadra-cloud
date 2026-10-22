// Package httpx 是 SaaS 控制面的 HTTP 层（设计 §5）：四枚 __Host- Cookie 的读写与重复检查、
// 传输绑定与凭据选择、CSRF、Origin、独立限流器与错误映射。它不知道账号怎么登录，只管「这一枚
// 凭据能不能用于这个请求」。
package httpx

import (
	"net/http"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// 四枚 Cookie 的名字（契约 §1，设计 Q3）：普通会话的访问 / 刷新，运营会话的访问 / 刷新。
// 都是 Secure、HttpOnly、SameSite=Strict、Path=/、不带 Domain（__Host- 前缀的要求）。
const (
	CookieAccess     = "__Host-armadra-cloud"
	CookieRefresh    = "__Host-armadra-cloud-refresh"
	CookieOpsAccess  = "__Host-armadra-ops"
	CookieOpsRefresh = "__Host-armadra-ops-refresh"
)

// HeaderCSRF 是 Cookie 会话的写请求要带的头。
const HeaderCSRF = "X-Armadra-CSRF"

var sessionCookieNames = []string{CookieAccess, CookieRefresh, CookieOpsAccess, CookieOpsRefresh}

// CookieNames 是某个范围的会话用的两枚 Cookie（访问、刷新）。
func CookieNames(scope core.SessionScope) (access, refresh string) {
	if scope == core.SessionScopeOperator {
		return CookieOpsAccess, CookieOpsRefresh
	}
	return CookieAccess, CookieRefresh
}

// Cookies 是一个请求里的会话 Cookie：每个名字出现了几次与（第一次的）值。
type Cookies struct {
	values map[string]string
	counts map[string]int
}

// ParseCookies 读出请求里的四枚会话 Cookie。别的 Cookie 一概不看。
func ParseCookies(r *http.Request) Cookies {
	c := Cookies{values: map[string]string{}, counts: map[string]int{}}
	wanted := map[string]bool{}
	for _, name := range sessionCookieNames {
		wanted[name] = true
	}
	for _, cookie := range r.Cookies() {
		if !wanted[cookie.Name] {
			continue
		}
		if c.counts[cookie.Name] == 0 {
			c.values[cookie.Name] = cookie.Value
		}
		c.counts[cookie.Name]++
	}
	return c
}

// Has 报告是否带了这枚 Cookie（至少一次且值非空）。
func (c Cookies) Has(name string) bool { return c.values[name] != "" }

// Value 是 Cookie 的值；没有答空串。
func (c Cookies) Value(name string) string { return c.values[name] }

// Any 报告是否带了任何一枚会话 Cookie（包括空值的）。
func (c Cookies) Any() bool { return len(c.counts) > 0 }

// Duplicated 是同一个名字出现了两次以上的那些 Cookie 名（按固定顺序）。
func (c Cookies) Duplicated() []string {
	var out []string
	for _, name := range sessionCookieNames {
		if c.counts[name] > 1 {
			out = append(out, name)
		}
	}
	return out
}

// Set 造一枚会话 Cookie 的 Set-Cookie 值。maxAgeSeconds <= 0 时是清除（Max-Age=0）。
func Set(name, value string, maxAgeSeconds int) string {
	cookie := &http.Cookie{
		Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
	if maxAgeSeconds > 0 {
		cookie.MaxAge = maxAgeSeconds
	} else {
		cookie.MaxAge = -1
	}
	return cookie.String()
}

// Clear 造清除一枚 Cookie 的 Set-Cookie 值。
func Clear(name string) string { return Set(name, "", 0) }

// OriginOnce 取请求的 Origin：恰好出现一次才算；缺失或重复答 false。
func OriginOnce(r *http.Request) (string, bool) {
	values := r.Header.Values("Origin")
	if len(values) != 1 {
		return "", false
	}
	return strings.TrimSpace(values[0]), true
}
