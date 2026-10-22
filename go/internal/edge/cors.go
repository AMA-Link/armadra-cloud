package edge

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// NativeAppOrigins 是原生 App 的 WebView 来源（iOS / Android）。
var NativeAppOrigins = []string{
	"capacitor://localhost",
	"https://localhost",
}

// ExposedHeaders 是页面需要读的中继响应头（限流的重试等待）。
const ExposedHeaders = "retry-after"

// canonical 返回 http(s) 来源的规范拼法（同 WHATWG `new URL(v).origin`：协议与主机小写、
// 默认端口去掉）；带路径、凭据、查询、片段或别的协议则返回 false。
// 与浏览器实现的差别：主机只认 ASCII（不做 IDNA）、不展开十六进制/八进制 IPv4 写法。
func canonical(value string) (string, bool) {
	// URL 解析先去掉首尾的控制字符与空格，并删掉所有的 tab / 换行。
	value = strings.Trim(value, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	value = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, value)
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok {
		return "", false
	}
	scheme = strings.ToLower(scheme)
	var defaultPort string
	switch scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	default:
		return "", false
	}
	end := strings.IndexAny(rest, "/\\?#")
	authority, tail := rest, ""
	if end >= 0 {
		authority, tail = rest[:end], rest[end:]
	}
	// 路径必须是 "/"（或没有），query 与 fragment 必须为空。
	if tail != "" {
		if tail[0] == '/' || tail[0] == '\\' {
			tail = tail[1:]
		}
		if strings.HasPrefix(tail, "?") {
			tail = tail[1:] // query 必须为空：要么到头，要么紧跟 #
		}
		if tail != "#" && tail != "" {
			return "", false
		}
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		if authority[:i] != "" && authority[:i] != ":" {
			return "", false
		}
		authority = authority[i+1:]
	}
	host, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		rb := strings.IndexByte(authority, ']')
		if rb < 0 {
			return "", false
		}
		addr, err := netip.ParseAddr(authority[1:rb])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		host = "[" + addr.String() + "]"
		after := authority[rb+1:]
		if after != "" {
			if after[0] != ':' {
				return "", false
			}
			port = after[1:]
		}
	} else {
		if i := strings.LastIndexByte(authority, ':'); i >= 0 {
			host, port = authority[:i], authority[i+1:]
		}
		host = strings.ToLower(host)
		if host == "" {
			return "", false
		}
		for i := 0; i < len(host); i++ {
			c := host[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
				return "", false
			}
		}
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		port = strconv.FormatUint(n, 10)
		if port == defaultPort {
			port = ""
		}
	}
	out := scheme + "://" + host
	if port != "" {
		out += ":" + port
	}
	return out, true
}

// OriginAllowed 判断来源在不在名单里：原生 App 逐字比，其余按规范拼法比；
// 浏览器发来的 Origin 必须已是规范拼法。
func OriginAllowed(origin string, trusted []string) bool {
	if slices.Contains(NativeAppOrigins, origin) {
		return true
	}
	if c, ok := canonical(origin); !ok || c != origin {
		return false
	}
	for _, entry := range trusted {
		if c, ok := canonical(entry); ok && c == origin {
			return true
		}
	}
	return false
}

// ErrorCORSHeaders 返回中继自己答的错误（401 / 403 / 404 / 429 / 503）的 CORS 头：
// 来源在名单里才有，按 Origin 回显、带 `Vary: Origin`、暴露 retry-after；
// 名单外或没有 Origin 返回 nil。只加头，不影响任何鉴权判定。
func ErrorCORSHeaders(origin string, hasOrigin bool, trusted []string) map[string]string {
	if !hasOrigin || !OriginAllowed(origin, trusted) {
		return nil
	}
	return map[string]string{
		"access-control-allow-origin":   origin,
		"access-control-expose-headers": ExposedHeaders,
		"vary":                          "origin",
	}
}
