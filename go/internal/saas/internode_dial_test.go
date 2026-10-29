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

	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
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
