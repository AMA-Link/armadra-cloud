package edge

import "strings"

// Target 是解析出的目标：sourceId 加转给源的路径（含 query）。
type Target struct {
	SourceID, Path string
}

// Addressing 把客户端请求映射到 sourceId；不匹配返回 false（HTTP 404 / 拒绝升级）。
// sourceId 只认 32 位小写 hex。
type Addressing interface {
	Resolve(url, host string) (Target, bool)
}

const sourceIDLen = 32

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type pathAddressing struct{}

// PathAddressing 是 personal 的写法：路径前缀 `/s/<sourceId>`，剥掉前缀后再转发
// （`/s/<id>` 与 `/s/<id>?q` 都转成 `/…`）。
func PathAddressing() Addressing { return pathAddressing{} }

func (pathAddressing) Resolve(url, _ string) (Target, bool) {
	const prefix = "/s/"
	if !strings.HasPrefix(url, prefix) || len(url) < len(prefix)+sourceIDLen {
		return Target{}, false
	}
	id := url[len(prefix) : len(prefix)+sourceIDLen]
	rest := url[len(prefix)+sourceIDLen:]
	if !isLowerHex(id) || (rest != "" && rest[0] != '/' && rest[0] != '?') {
		return Target{}, false
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return Target{SourceID: id, Path: rest}, true
}

type hostAddressing struct{ suffix string }

// HostAddressing 是 saas 的写法：Host 为 `<sourceId>.src.<domain>`（忽略大小写与端口），路径原样。
func HostAddressing(domain string) Addressing {
	return hostAddressing{suffix: ".src." + strings.ToLower(domain)}
}

func (a hostAddressing) Resolve(url, host string) (Target, bool) {
	if host == "" || !strings.HasPrefix(url, "/") {
		return Target{}, false
	}
	name := stripPort(strings.ToLower(host))
	if len(name) != sourceIDLen+len(a.suffix) || name[sourceIDLen:] != a.suffix {
		return Target{}, false
	}
	id := name[:sourceIDLen]
	if !isLowerHex(id) {
		return Target{}, false
	}
	return Target{SourceID: id, Path: url}, true
}

// stripPort 去掉结尾的 `:\d+`。
func stripPort(s string) string {
	i := strings.LastIndexByte(s, ':')
	if i < 0 || i == len(s)-1 {
		return s
	}
	for j := i + 1; j < len(s); j++ {
		if s[j] < '0' || s[j] > '9' {
			return s
		}
	}
	return s[:i]
}
