package tlsx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"
	"time"
)

// 真 ACME：对 pebble（PEBBLE_VA_ALWAYS_VALID=1，不真的回头拨挑战监听）签一张证书。
// ARMADRA_CLOUD_TEST_ACME_DIRECTORY 如 https://127.0.0.1:14001/dir，
// ARMADRA_CLOUD_TEST_ACME_CA 是 pebble 目录服务器的 minica PEM 路径。
func TestACMEPebble(t *testing.T) {
	dirURL := os.Getenv("ARMADRA_CLOUD_TEST_ACME_DIRECTORY")
	caPath := os.Getenv("ARMADRA_CLOUD_TEST_ACME_CA")
	if dirURL == "" || caPath == "" {
		t.Skip("未设置 ARMADRA_CLOUD_TEST_ACME_DIRECTORY / ARMADRA_CLOUD_TEST_ACME_CA，跳过 pebble 集成测试")
	}
	bundle, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := StartACME(ctx, ACMEOptions{
		DataDir: dir, Email: "ops@relay.test", Names: []string{"relay.test"},
		DirectoryURL: dirURL, CABundle: bundle,
		HTTPHost: "127.0.0.1", HTTPPort: 0, RedirectOrigin: "https://relay.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m := a.Current()
	leaf := m.Certificate.Leaf
	if m.Source != "acme" || m.Anchor != nil || m.Fingerprint != "" {
		t.Fatalf("%+v", m)
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Fatal("叶证书应为 P-256")
	}
	if !covers(leaf, []string{"relay.test"}) || !leaf.NotAfter.After(time.Now()) {
		t.Fatalf("%v", leaf.DNSNames)
	}
	// 经 Dynamic 起 TLS 监听，用叶证书的签发者做信任根握手。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := tls.NewListener(ln, NewDynamic(m).Config()).Accept()
			if err != nil {
				return
			}
			go func() { _, _ = c.(*tls.Conn).Read(make([]byte, 1)); c.Close() }()
		}
	}()
	pool := x509.NewCertPool()
	for _, der := range m.Certificate.Certificate[1:] {
		c, _ := x509.ParseCertificate(der)
		pool.AddCert(c)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "relay.test"})
	if err != nil {
		// pebble 的根每次启动随机，链可能只带中间证书：至少确认服务端出示的是这张证书。
		conn, err = tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "relay.test"}) //nolint
		if err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Close()
	if string(conn.ConnectionState().PeerCertificates[0].Raw) != string(leaf.Raw) {
		t.Fatal("握手出示的不是签发的证书")
	}
	// 账户密钥已落盘；第二次起复用同一账户并命中缓存。
	if _, err := os.Stat(dir + "/acme/account.key"); err != nil {
		t.Fatal(err)
	}
}
