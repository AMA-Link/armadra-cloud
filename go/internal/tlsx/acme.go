package tlsx

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
)

// ACME 的目录与节奏（与 TS 版逐项一致）。
const (
	LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"
	ACMERenewBefore      = 30 * 24 * time.Hour
	ACMERetry            = 12 * time.Hour
	challengePrefix      = "/.well-known/acme-challenge/"
	issueTimeout         = 5 * time.Minute
)

// Challenges 是签发途中待答的 http-01 挑战（令牌 -> key authorization）。
type Challenges struct{ m sync.Map }

// Set 登记一个挑战；挑战监听据此作答。
func (c *Challenges) Set(token, keyAuth string) { c.m.Store(token, keyAuth) }

// Delete 撤掉一个挑战。
func (c *Challenges) Delete(token string) { c.m.Delete(token) }

func (c *Challenges) get(token string) (string, bool) {
	v, ok := c.m.Load(token)
	if !ok {
		return "", false
	}
	return v.(string), true
}

// IssueRequest 是一次签发的输入。
type IssueRequest struct {
	Names          []string
	Email          string
	AccountKey     *ecdsa.PrivateKey
	CertificateKey *ecdsa.PrivateKey
	Challenges     *Challenges
}

// Issuer 做一次签发，答 PEM 证书链（叶证书在前）；测试换成假的。
type Issuer func(ctx context.Context, req IssueRequest) ([]byte, error)

// ACMEOptions 配置 ACME http-01。
type ACMEOptions struct {
	DataDir string
	Email   string
	// Names 是证书要覆盖的名字（域名或 IP）。
	Names []string
	// DirectoryURL 缺省 LetsEncryptDirectory。
	DirectoryURL string
	// CABundle 是信任 ACME 目录服务器的 PEM（测试 CA）；空则用系统根。
	CABundle []byte
	// HTTPHost、HTTPPort 是挑战监听地址；调用方给缺省（0.0.0.0:80）。
	HTTPHost string
	HTTPPort int
	// RedirectOrigin 是非挑战请求 308 去的对外来源。
	RedirectOrigin string
	Issuer         Issuer
	Now            func() time.Time
	// Retry 缺省 ACMERetry：续期检查与失败重试的间隔。
	Retry time.Duration
	// OnEvent 在续期成功（"renewed"，0）或失败（"failed"，累计失败数）时回调。
	OnEvent func(event string, failures int)
}

// ACME 是 ACME 模式：Manager 加关闭。
type ACME struct {
	Manager
	m      *acmeManager
	server *http.Server
	ln     net.Listener
	cancel context.CancelFunc
	served chan struct{}
	once   sync.Once
}

type acmeManager struct {
	o          ACMEOptions
	now        func() time.Time
	issuer     Issuer
	dir        string
	accountKey *ecdsa.PrivateKey
	challenges *Challenges
	current    atomic.Pointer[Material]
	failures   int
	loopDone   chan struct{}
	loopCtx    context.Context
	started    atomic.Bool
}

// StartACME 起 ACME：先用现有证书（没有就申请）、挑战监听、续期循环。
func StartACME(ctx context.Context, o ACMEOptions) (*ACME, error) {
	if o.DirectoryURL == "" {
		o.DirectoryURL = LetsEncryptDirectory
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Retry <= 0 {
		o.Retry = ACMERetry
	}
	dir := joinDir(o.DataDir, ACMEDir)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	accountKey, err := loadOrCreateAccountKey(filepath.Join(dir, "account.key"))
	if err != nil {
		return nil, err
	}
	m := &acmeManager{o: o, now: o.Now, dir: dir, accountKey: accountKey, challenges: &Challenges{}, loopDone: make(chan struct{})}
	m.issuer = o.Issuer
	if m.issuer == nil {
		m.issuer, err = acmeIssuer(o.DirectoryURL, o.CABundle)
		if err != nil {
			return nil, err
		}
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(o.HTTPHost, strconv.Itoa(o.HTTPPort)))
	if err != nil {
		return nil, err
	}
	a := &ACME{Manager: m, m: m, ln: ln, served: make(chan struct{})}
	a.server = &http.Server{Handler: challengeHandler(m.challenges, o.RedirectOrigin), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(a.served)
		_ = a.server.Serve(ln)
	}()

	if mat, ok := m.existing(); ok {
		m.current.Store(&mat)
	} else {
		mat, err := m.issue(ctx)
		if err != nil {
			_ = a.shutdown()
			var te *Error
			if errors.As(err, &te) {
				return nil, err
			}
			msg := err.Error()
			if r := []rune(msg); len(r) > 200 {
				msg = string(r[:200])
			}
			return nil, tlsErr("ACME 签发失败，拒绝启动：" + msg)
		}
		m.current.Store(&mat)
	}
	var loopCtx context.Context
	loopCtx, a.cancel = context.WithCancel(context.WithoutCancel(ctx))
	m.loopCtx = loopCtx
	// ctx 取消等同 Close。
	go func() {
		select {
		case <-ctx.Done():
			_ = a.Close()
		case <-loopCtx.Done():
		}
	}()
	return a, nil
}

// Addr 答挑战监听实际的地址（端口给 0 时用）。
func (a *ACME) Addr() net.Addr { return a.ln.Addr() }

// Close 停掉挑战监听与续期循环。
func (a *ACME) Close() error {
	var err error
	a.once.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		err = a.shutdown()
		if a.m.started.Load() {
			<-a.m.loopDone
		}
	})
	return err
}

func (a *ACME) shutdown() error {
	err := a.server.Close()
	<-a.served
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

func challengeHandler(ch *Challenges, redirectOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.RequestURI
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		if strings.HasPrefix(path, challengePrefix) {
			answer, ok := ch.get(strings.TrimPrefix(path, challengePrefix))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(answer))
			return
		}
		w.Header().Set("Location", redirectOrigin+r.RequestURI)
		w.WriteHeader(http.StatusPermanentRedirect)
	})
}

func loadOrCreateAccountKey(file string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		key, err := parseECKey(data)
		if err != nil {
			return nil, tlsErr("ACME 账户密钥读不出来：检查 acme/account.key")
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		// 读不到不等于没有：绝不覆盖已有的账户密钥。
		return nil, tlsErr("ACME 账户密钥读不出来：检查 acme/account.key")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pemKey, err := keyPEM(key)
	if err != nil {
		return nil, err
	}
	return key, replaceFile(file, pemKey, 0o600)
}

// existing 读已有证书：没过期、覆盖 Names、私钥对得上才用。
func (m *acmeManager) existing() (Material, bool) {
	certData, err1 := os.ReadFile(filepath.Join(m.dir, "cert.pem"))
	keyData, err2 := os.ReadFile(filepath.Join(m.dir, "key.pem"))
	if err1 != nil || err2 != nil {
		return Material{}, false
	}
	mat, leaf, err := describeACME(certData, keyData)
	if err != nil || !leaf.NotAfter.After(m.now()) || !covers(leaf, m.o.Names) {
		return Material{}, false
	}
	return mat, true
}

func describeACME(chain, key []byte) (Material, *x509.Certificate, error) {
	pair, leaf, err := buildMaterial(chain, key)
	if err != nil {
		return Material{}, nil, err
	}
	return Material{Certificate: pair, NotAfter: leaf.NotAfter, Source: "acme"}, leaf, nil
}

// issue 用新的证书私钥签一次，成功后落盘。
func (m *acmeManager) issue(ctx context.Context) (Material, error) {
	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, err
	}
	chain, err := m.issuer(ctx, IssueRequest{Names: m.o.Names, Email: m.o.Email, AccountKey: m.accountKey, CertificateKey: key, Challenges: m.challenges})
	if err != nil {
		return Material{}, err
	}
	keyData, err := keyPEM(key)
	if err != nil {
		return Material{}, err
	}
	mat, _, err := describeACME(chain, keyData)
	if err != nil {
		return Material{}, err
	}
	if err := replaceFile(filepath.Join(m.dir, "key.pem"), keyData, 0o600); err != nil {
		return Material{}, err
	}
	if err := replaceFile(filepath.Join(m.dir, "cert.pem"), chain, 0o600); err != nil {
		return Material{}, err
	}
	return mat, nil
}

func (m *acmeManager) Current() Material { return *m.current.Load() }

func (m *acmeManager) Start(onChange func(Material), onError func(error)) func() {
	if !m.started.CompareAndSwap(false, true) {
		return func() {}
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(m.loopDone)
		for {
			if m.current.Load().NotAfter.Sub(m.now()) <= ACMERenewBefore {
				mat, err := m.issue(m.loopCtx)
				if err != nil {
					m.failures++
					if m.o.OnEvent != nil {
						m.o.OnEvent("failed", m.failures)
					}
					if onError != nil {
						onError(err)
					}
				} else {
					m.failures = 0
					m.current.Store(&mat)
					if m.o.OnEvent != nil {
						m.o.OnEvent("renewed", 0)
					}
					if onChange != nil {
						onChange(mat)
					}
				}
			}
			t := time.NewTimer(m.o.Retry)
			select {
			case <-stop:
				t.Stop()
				return
			case <-m.loopCtx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

// acmeIssuer 是真的 ACME 流程（RFC 8555，http-01）。
func acmeIssuer(directoryURL string, caBundle []byte) (Issuer, error) {
	httpClient := http.DefaultClient
	if len(caBundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, tlsErr("--acme-ca-bundle 里没有可用的证书")
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		httpClient = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	return func(ctx context.Context, req IssueRequest) ([]byte, error) {
		client := &acme.Client{Key: req.AccountKey, DirectoryURL: directoryURL, HTTPClient: httpClient}
		acct := &acme.Account{Contact: []string{"mailto:" + req.Email}}
		if _, err := client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return nil, err
		}
		ids := make([]acme.AuthzID, len(req.Names))
		for i, n := range req.Names {
			ids[i] = acme.AuthzID{Type: "dns", Value: n}
			if net.ParseIP(n) != nil {
				ids[i].Type = "ip"
			}
		}
		order, err := client.AuthorizeOrder(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, url := range order.AuthzURLs {
			if err := completeAuthz(ctx, client, url, req.Challenges); err != nil {
				return nil, err
			}
		}
		ready, err := client.WaitOrder(ctx, order.URI)
		if err != nil {
			return nil, err
		}
		if ready.FinalizeURL != "" {
			order.FinalizeURL = ready.FinalizeURL
		}
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			DNSNames:    dnsNames(req.Names),
			IPAddresses: ipAddrs(req.Names),
		}, crypto.Signer(req.CertificateKey))
		if err != nil {
			return nil, err
		}
		der, _, err := client.CreateCertFromOrder(ctx, order, csr, true)
		if err != nil {
			return nil, err
		}
		var out []byte
		for _, d := range der {
			out = append(out, certPEM(d)...)
		}
		return out, nil
	}, nil
}

func completeAuthz(ctx context.Context, client *acme.Client, url string, ch *Challenges) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return err
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			chal = c
		}
	}
	if chal == nil {
		return tlsErr(fmt.Sprintf("%s 没有 http-01 挑战", authz.Identifier.Value))
	}
	resp, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return err
	}
	ch.Set(chal.Token, resp)
	defer ch.Delete(chal.Token)
	if _, err := client.Accept(ctx, chal); err != nil {
		return err
	}
	_, err = client.WaitAuthorization(ctx, authz.URI)
	return err
}

func dnsNames(names []string) []string {
	var out []string
	for _, n := range names {
		if net.ParseIP(n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func ipAddrs(names []string) []net.IP {
	var out []net.IP
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
