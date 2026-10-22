package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"testing"
)

func TestDynamicHotSwap(t *testing.T) {
	a, err := selfSignedAt(t.TempDir(), Names("127.0.0.1"), t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := selfSignedAt(t.TempDir(), Names("127.0.0.1"), t0)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDynamic(a)
	cfg := d.Config()
	if cfg.MinVersion != tls.VersionTLS12 || len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("config: %+v", cfg)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	defer srv.Close()
	addr := ln.Addr().String()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(a.Anchor)
	pool.AppendCertsFromPEM(b.Anchor)
	seen := func(c *tls.Conn) string {
		return fingerprintOf(c.ConnectionState().PeerCertificates[len(c.ConnectionState().PeerCertificates)-1].Raw)
	}
	dial := func() *tls.Conn {
		c, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	old := dial()
	defer old.Close()
	if seen(old) != a.Fingerprint {
		t.Fatal("首次握手应是 a")
	}
	d.Set(b)
	fresh := dial()
	defer fresh.Close()
	if seen(fresh) != b.Fingerprint {
		t.Fatal("Set 之后的握手应是 b")
	}
	// 旧连接还能收发数据。
	if _, err := old.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(old)
	if err != nil || len(data) == 0 {
		t.Fatalf("旧连接断了: %v", err)
	}
	// 只走 http/1.1。
	h2, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if p := h2.ConnectionState().NegotiatedProtocol; p != "http/1.1" {
		t.Fatalf("协商到 %q", p)
	}
	// TLS 1.1 被拒。
	if c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}); err == nil { //nolint
		c.Close()
		t.Fatal("TLS1.1 不应成功")
	}
}
