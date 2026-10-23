package clientip

import (
	"net/http"
	"net/netip"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func mustClientIP(t *testing.T, cfg *TrustedProxyConfig) ClientIP {
	t.Helper()
	c, err := NewClientIP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// 对应 client-ip.test.ts「未配置：永远用对端地址，转发头一律忽略」。
func TestClientIPUnconfigured(t *testing.T) {
	c := mustClientIP(t, nil)
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "1.2.3.4", "X-Real-IP", "5.6.7.8")), "127.0.0.1")
	expect(t, mustClientIP(t, &TrustedProxyConfig{})("[::ffff:10.0.0.1]:9", nil), "10.0.0.1")
}

// 对应 client-ip.test.ts「对端不在受信列表：伪造的 XFF / X-Real-IP 被忽略」。
func TestClientIPUntrustedPeer(t *testing.T) {
	c := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1/32"}})
	expect(t, c("203.0.113.9:1", hdr("X-Forwarded-For", "1.1.1.1", "X-Real-IP", "2.2.2.2")), "203.0.113.9")
}

// 对应 client-ip.test.ts「受信对端：取 XFF 里自右向左第一个不受信的地址」。
func TestClientIPForwardedForWalk(t *testing.T) {
	c := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1/32", "10.0.0.0/8"}})
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "198.51.100.7")), "198.51.100.7")
	// 客户端自己塞的左侧伪造项不会被取到：右边第一个不受信的是 198.51.100.7
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "9.9.9.9, 198.51.100.7, 10.1.1.1, 10.2.2.2")), "198.51.100.7")
	// 全部受信：取最左
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "10.0.0.5, 10.0.0.6")), "10.0.0.5")
}

// 对应 client-ip.test.ts「多个 XFF 头行合并按顺序处理」。
func TestClientIPMultipleHeaderLines(t *testing.T) {
	c := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1"}})
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "8.8.8.8", "X-Forwarded-For", "198.51.100.7")), "198.51.100.7")
}

// 对应 client-ip.test.ts「IPv6：CIDR、映射地址、方括号」。
func TestClientIPIPv6(t *testing.T) {
	c := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"::1/128", "fd00::/8", "127.0.0.1"}})
	expect(t, c("[::1]:5", hdr("X-Forwarded-For", "2001:db8::7")), "2001:db8::7")
	expect(t, c("[::1]:5", hdr("X-Forwarded-For", "2001:DB8::7, fd00::2")), "2001:db8::7")
	expect(t, c("[::ffff:127.0.0.1]:5", hdr("X-Forwarded-For", "[2001:db8::8]")), "2001:db8::8")
	expect(t, c("[2001:db8::99]:5", hdr("X-Forwarded-For", "1.1.1.1")), "2001:db8::99")
	expect(t, mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1"}})("[::ffff:127.0.0.1]:5", hdr("X-Forwarded-For", "4.4.4.4")), "4.4.4.4")
}

// 对应 client-ip.test.ts「X-Real-IP 作备选，prefer 可调」。
func TestClientIPRealIPAndPrefer(t *testing.T) {
	xffOnly := hdr("X-Forwarded-For", "1.1.1.1")
	both := hdr("X-Forwarded-For", "1.1.1.1", "X-Real-IP", "2.2.2.2")
	first := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1"}})
	real := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1"}, Prefer: "real-ip"})
	expect(t, first("127.0.0.1:1", both), "1.1.1.1")
	expect(t, real("127.0.0.1:1", both), "2.2.2.2")
	expect(t, first("127.0.0.1:1", hdr("X-Real-IP", "2.2.2.2")), "2.2.2.2")
	expect(t, real("127.0.0.1:1", xffOnly), "1.1.1.1")
}

// 对应 client-ip.test.ts「头不合法：回退到对端」。
func TestClientIPInvalidHeaders(t *testing.T) {
	c := mustClientIP(t, &TrustedProxyConfig{Proxies: []string{"127.0.0.1"}})
	expect(t, c("127.0.0.1:1", hdr("X-Forwarded-For", "1.1.1.1, evil")), "127.0.0.1")
	expect(t, c("127.0.0.1:1", hdr("X-Real-IP", "1.1.1.1, 2.2.2.2")), "127.0.0.1")
	expect(t, c("127.0.0.1:1", nil), "127.0.0.1")
}

// 对应 client-ip.test.ts「配置不合法直接抛」。
func TestClientIPBadConfig(t *testing.T) {
	cases := map[string]string{
		"nope":        "受信代理不是 IP 或 CIDR：nope",
		"10.0.0.0/33": "受信代理前缀不合法：10.0.0.0/33",
		"::1/129":     "受信代理前缀不合法：::1/129",
		"1.2.3.4/x":   "受信代理前缀不合法：1.2.3.4/x",
	}
	for bad, msg := range cases {
		_, err := NewClientIP(&TrustedProxyConfig{Proxies: []string{bad}})
		if err == nil || err.Error() != msg {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// 对应 client-ip.test.ts「normalizeIp」。
func TestNormalizeIP(t *testing.T) {
	for in, want := range map[string]string{" ::FFFF:1.2.3.4 ": "1.2.3.4", "fe80::1%en0": "fe80::1", "[::1]": "::1"} {
		if got, ok := NormalizeIP(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"1.2.3", "", "%", "evil", "01.1.1.1"} {
		if got, ok := NormalizeIP(in); ok {
			t.Errorf("%q 应不是 IP：%q", in, got)
		}
	}
}

func TestParseProxyList(t *testing.T) {
	got := ParseProxyList(" 10.0.0.0/8, ,::1 ,")
	if len(got) != 2 || got[0] != "10.0.0.0/8" || got[1] != "::1" {
		t.Fatalf("%q", got)
	}
}

func FuzzClientIP(f *testing.F) {
	f.Add("1.2.3.4:5", "9.9.9.9, 10.0.0.1", "2.2.2.2")
	f.Add("[::1]:5", "[2001:db8::1],::ffff:10.0.0.1", "")
	f.Add("127.0.0.1", ",,,", "a,b")
	f.Add("garbage", "\x00%zz", "1.2.3.4%en0")
	c, err := NewClientIP(&TrustedProxyConfig{Proxies: []string{"127.0.0.1", "::1/128", "10.0.0.0/8"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, remote, xff, realIP string) {
		h := http.Header{}
		h.Add("X-Forwarded-For", xff)
		h.Add("X-Real-IP", realIP)
		got := c(remote, h)
		if _, err := netip.ParseAddr(got); err == nil {
			return
		}
		// 不是 IP 时只能是对端字符串本身。
		peer := c(remote, nil)
		if got != peer {
			t.Fatalf("结果既非 IP 也非对端：%q（对端 %q）", got, peer)
		}
	})
}
