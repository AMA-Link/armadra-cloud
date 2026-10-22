package tlsx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIssuer 先经挑战监听取回 key authorization（模拟 CA 的 http-01 验证），再用测试 CA 签 days 天的证书。
func fakeIssuer(t *testing.T, addr func() string, days int, calls *[]string, mu *sync.Mutex) Issuer {
	c, err := createCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, req IssueRequest) ([]byte, error) {
		req.Challenges.Set("tok-1", "tok-1.thumbprint")
		resp, err := http.Get("http://" + addr() + challengePrefix + "tok-1")
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		req.Challenges.Delete("tok-1")
		mu.Lock()
		*calls = append(*calls, resp.Status[:3]+":"+string(body))
		mu.Unlock()
		der, _, err := issueLeaf(c, req.Names, time.Now(), days, req.CertificateKey)
		if err != nil {
			return nil, err
		}
		return certPEM(der), nil
	}
}

func baseOpts(dir string) ACMEOptions {
	return ACMEOptions{DataDir: dir, Email: "ops@relay.test", Names: []string{"relay.test"}, DirectoryURL: "https://acme.invalid/directory",
		HTTPHost: "127.0.0.1", HTTPPort: 0, RedirectOrigin: "https://relay.test:8443"}
}

func TestACMEFirstIssueAndHandler(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	var mu sync.Mutex
	o := baseOpts(dir)
	// 先占一个端口再释放，让 addr 在签发期间可知。
	port := freePort(t)
	o.HTTPPort = port
	addr := "127.0.0.1:" + itoa(port)
	o.Issuer = fakeIssuer(t, func() string { return addr }, 60, &calls, &mu)
	a, err := StartACME(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(calls) != 1 || calls[0] != "200:tok-1.thumbprint" {
		t.Fatalf("calls: %v", calls)
	}
	cur := a.Current()
	if cur.Source != "acme" || cur.Anchor != nil || cur.Fingerprint != "" {
		t.Fatalf("%+v", cur)
	}
	if k := cur.Certificate.PrivateKey.(*ecdsa.PrivateKey); k.Curve != elliptic.P256() {
		t.Fatal("证书私钥应是 P-256")
	}
	for _, n := range []string{"account.key", "cert.pem", "key.pem"} {
		info, err := os.Stat(filepath.Join(dir, "acme", n))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s %v %v", n, info, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, "acme")); info.Mode().Perm() != 0o700 {
		t.Fatal("acme 目录应为 0700")
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get("http://" + a.Addr().String() + "/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 308 || resp.Header.Get("Location") != "https://relay.test:8443/x?y=1" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = http.Get("http://" + a.Addr().String() + challengePrefix + "none")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	// 缓存：再起一次不再签发。
	a.Close()
	o.Issuer = func(context.Context, IssueRequest) ([]byte, error) { return nil, errors.New("不该签发") }
	b, err := StartACME(context.Background(), o)
	if err != nil {
		t.Fatalf("缓存应可用: %v", err)
	}
	defer b.Close()
	if string(b.Current().Certificate.Certificate[0]) != string(cur.Certificate.Certificate[0]) {
		t.Fatal("应复用缓存证书")
	}
}

func TestACMEIssueFailureRefusesAndClosesListener(t *testing.T) {
	o := baseOpts(t.TempDir())
	port := freePort(t)
	o.HTTPPort = port
	o.Issuer = func(context.Context, IssueRequest) ([]byte, error) { return nil, errors.New("unreachable") }
	_, err := StartACME(context.Background(), o)
	var te *Error
	if !errors.As(err, &te) || !strings.Contains(te.Msg, "unreachable") {
		t.Fatalf("%v", err)
	}
	if _, err := http.Get("http://127.0.0.1:" + itoa(port) + "/"); err == nil {
		t.Fatal("监听应已关闭")
	}
}

func TestACMECacheValidity(t *testing.T) {
	dir := t.TempDir()
	o := baseOpts(dir)
	var issued atomic.Int32
	c, _ := createCA(time.Now())
	o.Issuer = func(_ context.Context, req IssueRequest) ([]byte, error) {
		issued.Add(1)
		der, _, err := issueLeaf(c, req.Names, time.Now(), 60, req.CertificateKey)
		return certPEM(der), err
	}
	a, err := StartACME(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	// 名字变了 -> 重签；时间走到过期 -> 重签。
	o.Names = []string{"other.test"}
	a, err = StartACME(context.Background(), o)
	if err != nil || issued.Load() != 2 {
		t.Fatalf("%v %d", err, issued.Load())
	}
	a.Close()
	o.Now = func() time.Time { return time.Now().Add(61 * day) }
	a, err = StartACME(context.Background(), o)
	if err != nil || issued.Load() != 3 {
		t.Fatalf("%v %d", err, issued.Load())
	}
	a.Close()
}

func TestACMERenewAndRetry(t *testing.T) {
	o := baseOpts(t.TempDir())
	c, _ := createCA(time.Now())
	var fail atomic.Bool
	o.Issuer = func(_ context.Context, req IssueRequest) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("boom")
		}
		der, _, err := issueLeaf(c, req.Names, time.Now(), 20, req.CertificateKey)
		return certPEM(der), err
	}
	var mu sync.Mutex
	var events []string
	o.OnEvent = func(e string, n int) { mu.Lock(); events = append(events, e+":"+itoa(n)); mu.Unlock() }
	o.Retry = 50 * time.Millisecond
	a, err := StartACME(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := a.Current().Certificate.Certificate[0]
	fail.Store(true)
	changed := make(chan Material, 4)
	errs := make(chan error, 8)
	stop := a.Start(func(m Material) { changed <- m }, func(e error) { errs <- e })
	defer stop()
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("没有失败回调")
	}
	if string(a.Current().Certificate.Certificate[0]) != string(first) {
		t.Fatal("失败时应继续用旧证书")
	}
	mu.Lock()
	if events[0] != "failed:1" {
		t.Fatalf("%v", events)
	}
	mu.Unlock()
	fail.Store(false)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("没有续期")
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range events {
		if e == "renewed:0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%v", events)
	}
}

func TestACMENoRenewWhenFresh(t *testing.T) {
	o := baseOpts(t.TempDir())
	c, _ := createCA(time.Now())
	var issued atomic.Int32
	o.Issuer = func(_ context.Context, req IssueRequest) ([]byte, error) {
		issued.Add(1)
		der, _, err := issueLeaf(c, req.Names, time.Now(), 60, req.CertificateKey)
		return certPEM(der), err
	}
	o.Retry = 20 * time.Millisecond
	a, err := StartACME(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	stop := a.Start(nil, nil)
	time.Sleep(150 * time.Millisecond)
	stop()
	a.Close()
	if issued.Load() != 1 {
		t.Fatalf("剩 60 天不应续期: %d", issued.Load())
	}
}

func TestACMECloseViaContext(t *testing.T) {
	o := baseOpts(t.TempDir())
	c, _ := createCA(time.Now())
	o.Issuer = func(_ context.Context, req IssueRequest) ([]byte, error) {
		der, _, err := issueLeaf(c, req.Names, time.Now(), 60, req.CertificateKey)
		return certPEM(der), err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := StartACME(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	addr := a.Addr().String()
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/"); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ctx 取消后监听应关闭")
}

func TestDefaultDirectory(t *testing.T) {
	if LetsEncryptDirectory != "https://acme-v02.api.letsencrypt.org/directory" {
		t.Fatal(LetsEncryptDirectory)
	}
}
