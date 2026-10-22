package edge

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// ClientIP 是客户端 IP 的唯一出处：限流、登录锁定、转给源的 X-Forwarded-For 都走它。
// remoteAddr 形如 "1.2.3.4:5678" 或 "[::1]:5"，返回规范化后的 IP 文本。
//
// 缺省不信任任何代理，只用直连对端地址。配置了受信代理后，只有直连对端落在受信列表内，
// 才读转发头；X-Forwarded-For 自右向左剔除受信跳数，第一个不受信的地址就是客户端，
// 全部受信取最左一个。
type ClientIP func(remoteAddr string, h http.Header) string

// TrustedProxyConfig 是受信代理配置。
type TrustedProxyConfig struct {
	// Proxies 是 CIDR 或单个 IP（`127.0.0.1/32`、`::1/128`、`10.0.0.1`）。
	Proxies []string
	// Prefer 是优先读的头："forwarded-for"（缺省）或 "real-ip"，另一个作备选。
	Prefer string
}

// NormalizeIP 去掉方括号、zone id 与 IPv4 映射前缀并转小写；不是 IP 返回 false。
func NormalizeIP(text string) (string, bool) {
	value := strings.TrimSpace(text)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = value[1 : len(value)-1]
	}
	if i := strings.IndexByte(value, '%'); i >= 0 {
		value = value[:i]
	}
	if len(value) > 7 && strings.EqualFold(value[:7], "::ffff:") && isDottedQuad(value[7:]) {
		value = value[7:]
	}
	if _, err := netip.ParseAddr(value); err != nil {
		return "", false
	}
	return strings.ToLower(value), true
}

// isDottedQuad 对应 `\d+\.\d+\.\d+\.\d+`。
func isDottedQuad(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// ParseProxyList 解析逗号分隔的代理列表，去掉空项。
func ParseProxyList(text string) []string {
	var out []string
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func buildList(proxies []string) ([]netip.Prefix, error) {
	list := make([]netip.Prefix, 0, len(proxies))
	for _, entry := range proxies {
		parts := strings.Split(entry, "/")
		ip, ok := NormalizeIP(parts[0])
		if !ok {
			return nil, errors.New("受信代理不是 IP 或 CIDR：" + entry)
		}
		addr, _ := netip.ParseAddr(ip)
		bits := addr.BitLen()
		if len(parts) > 1 {
			text := parts[1]
			n, err := strconv.Atoi(text)
			if err != nil || text == "" || text[0] < '0' || text[0] > '9' || n > bits {
				return nil, errors.New("受信代理前缀不合法：" + entry)
			}
			bits = n
		}
		list = append(list, netip.PrefixFrom(addr, bits).Masked())
	}
	return list, nil
}

func headerValue(h http.Header, name string) string {
	return strings.Join(h.Values(name), ",")
}

// NewClientIP 按配置创建 ClientIP；cfg 为 nil 或没有代理时只用对端地址。
// 配置不合法返回错误。
func NewClientIP(cfg *TrustedProxyConfig) (ClientIP, error) {
	peerOf := func(remoteAddr string) string {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			host = remoteAddr
		}
		if ip, ok := NormalizeIP(host); ok {
			return ip
		}
		return host
	}
	if cfg == nil || len(cfg.Proxies) == 0 {
		return func(remoteAddr string, _ http.Header) string { return peerOf(remoteAddr) }, nil
	}
	list, err := buildList(cfg.Proxies)
	if err != nil {
		return nil, err
	}
	trusted := func(ip string) bool {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return false
		}
		addr = addr.Unmap() // IPv4 规则同样匹配映射地址
		for _, p := range list {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	preferReal := cfg.Prefer == "real-ip"

	fromForwardedFor := func(header string) (string, bool) {
		if strings.TrimSpace(header) == "" {
			return "", false
		}
		hops := strings.Split(header, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip, ok := NormalizeIP(hops[i])
			if !ok {
				return "", false // 链里有乱码：整条不可信
			}
			if !trusted(ip) || i == 0 {
				return ip, true
			}
		}
		return "", false
	}
	fromRealIP := func(header string) (string, bool) {
		if strings.Contains(header, ",") {
			return "", false
		}
		return NormalizeIP(header)
	}

	return func(remoteAddr string, h http.Header) string {
		peer := peerOf(remoteAddr)
		if !trusted(peer) {
			return peer
		}
		xff, xffOK := fromForwardedFor(headerValue(h, "X-Forwarded-For"))
		realIP, realOK := fromRealIP(headerValue(h, "X-Real-IP"))
		first, second, firstOK, secondOK := xff, realIP, xffOK, realOK
		if preferReal {
			first, second, firstOK, secondOK = realIP, xff, realOK, xffOK
		}
		switch {
		case firstOK:
			return first
		case secondOK:
			return second
		}
		return peer
	}, nil
}
