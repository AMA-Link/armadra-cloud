package saas

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// #142：连对端失败的日志要能排障——带分类（reason）、拒绝升级时的状态码（code）与对端的主机与端口（host），
// 仍不记错误的 message 与节点令牌。
func TestLinkDialFailureIsClassified(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddr := closed.Addr().String()
	_ = closed.Close()
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "对端给的内容 leak-body", http.StatusUnauthorized)
	}))
	t.Cleanup(unauthorized.Close)
	tlsPeer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsPeer.Close)
	tlsAddr := strings.TrimPrefix(tlsPeer.URL, "https://")
	unauthorizedAddr := strings.TrimPrefix(unauthorized.URL, "http://")

	const token = "node-token-secret-value"
	for _, tc := range []struct {
		name, peer, url string
		insecure        bool
		reason, host    string
		code            int
	}{
		{name: "不在节点表", peer: "ghost", reason: dialNodeUnknown},
		{name: "不是 wss", peer: "plain", url: "ws://" + refusedAddr + "/x/v1", reason: dialInsecureURL, host: refusedAddr},
		{name: "拒绝连接", peer: "refused", url: "ws://" + refusedAddr + "/x/v1", insecure: true, reason: dialRefused, host: refusedAddr},
		{name: "拒绝升级", peer: "deny", url: "ws://" + unauthorizedAddr + "/x/v1", insecure: true, reason: dialRejected, host: unauthorizedAddr, code: 401},
		{name: "证书不受信任", peer: "tls", url: "wss://" + tlsAddr + "/x/v1", reason: dialTLS, host: tlsAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.url != "" {
				entry := fmt.Sprintf(`{"url":"wss://peer.example.test/t/v1","weight":1,"version":"x","draining":false,"connections":0,"tunnels":0,"internodeUrl":%q}`, tc.url)
				if err := r.admin.HSet(ctx, r.prefix+"nodes", tc.peer, entry).Err(); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			l := newLinks("self", token, tc.insecure, r.admin, r.prefix, obs.NewLogger(&logs, obs.LevelDebug), &internodeMetrics{})
			call := &dialCall{done: make(chan struct{})}
			l.dial(tc.peer, call)
			if call.t != nil {
				t.Fatal("不该连上")
			}
			line := logs.String()
			want := `"msg":"连不上对端节点，通道不转发","node":"` + tc.peer + `","reason":"` + tc.reason + `"`
			if tc.code != 0 {
				want += fmt.Sprintf(`,"code":%d`, tc.code)
			}
			if tc.host != "" {
				want += `,"host":"` + tc.host + `"`
			}
			if !strings.Contains(line, want) {
				t.Fatalf("日志应含 %s：%s", want, line)
			}
			for _, leak := range []string{token, "leak-body", "/x/v1"} {
				if strings.Contains(line, leak) {
					t.Fatalf("日志里不该有 %q：%s", leak, line)
				}
			}
		})
	}
}

// classifyDial 只看错误类型与系统错误码；包了几层也认得出。
func TestClassifyDial(t *testing.T) {
	op := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: err}}
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "no such host", Name: "b.edge.example.test", IsNotFound: true}, dialDNS},
		{&net.DNSError{Err: "i/o timeout", Name: "b.edge.example.test", IsTimeout: true}, dialDNS},
		{op(syscall.ECONNREFUSED), dialRefused},
		{op(syscall.ECONNRESET), dialReset},
		{op(syscall.EHOSTUNREACH), dialUnreachable},
		{op(syscall.ENETUNREACH), dialUnreachable},
		{context.DeadlineExceeded, dialTimeout},
		{fmt.Errorf("握手：%w", &timeoutErr{}), dialTimeout},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, dialTLS},
		{x509.HostnameError{Host: "b.edge.example.test"}, dialTLS},
		{tls.AlertError(42), dialTLS},
		{tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, dialTLS},
		{errors.New("unexpected EOF"), dialNetwork},
	} {
		if got := classifyDial(tc.err); got != tc.want {
			t.Errorf("%T %v：%s，应为 %s", tc.err, tc.err, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

// fixedRoute 是只答一条固定路由的路由表。
type fixedRoute struct{ route control.Route }

func (f fixedRoute) Get(context.Context, string) (*control.Route, error) {
	r := f.route
	return &r, nil
}
func (fixedRoute) Set(context.Context, string, control.Route, time.Duration) error {
	return nil
}
func (fixedRoute) Touch(context.Context, string, time.Duration) error { return nil }
func (fixedRoute) Delete(context.Context, string, string) error       { return nil }

// 路由指向别的节点却拿不到到那个节点的连接（对端不在节点表、没配节点令牌）：答 nil（边缘 4404），
// 计进 relay_routes_remote_failed_total，不计 relay_routes_remote_total。
func TestLookupCountsRemoteFailures(t *testing.T) {
	r := newRig(t)
	routes := fixedRoute{control.Route{Node: "ghost", TunnelID: "t1"}}
	noLocal := func(string) *tunnel.Tunnel { return nil }
	m := &internodeMetrics{streams: func() int { return 0 }}
	withLinks := lookup(routes, "self", noLocal, newLinks("self", "tok", false, r.admin, r.prefix, obs.Nop(), m), m)
	withoutToken := lookup(routes, "self", noLocal, nil, m)
	for _, find := range []func(context.Context, string) (*tunnel.Tunnel, error){withLinks, withoutToken} {
		if got, err := find(context.Background(), "src1"); got != nil || err != nil {
			t.Fatalf("应答 nil：%v %v", got, err)
		}
	}
	text := m.render()
	if metricLine(text, "relay_routes_remote_failed_total") != "2" || metricLine(text, "relay_routes_remote_total") != "0" {
		t.Fatal(text)
	}
	// 路由指向本节点而本地没有隧道（旧路由）不是转发失败。
	self := lookup(fixedRoute{control.Route{Node: "self", TunnelID: "t1"}}, "self", noLocal, nil, m)
	if got, _ := self(context.Background(), "src1"); got != nil || metricLine(m.render(), "relay_routes_remote_failed_total") != "2" {
		t.Fatal(m.render())
	}
}
