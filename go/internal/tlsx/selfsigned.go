package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // 只用于 SKI，与 RFC 5280 的做法一致
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const day = 24 * time.Hour

// Names 是自签证书的 SAN：host、localhost、127.0.0.1（去重，host 本身是 IP 时按 IP 记）。
func Names(host string) []string {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	out := []string{}
	seen := map[string]bool{}
	for _, n := range []string{host, "localhost", "127.0.0.1"} {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func keyID(key *ecdsa.PublicKey) []byte {
	pub, _ := key.Bytes()
	sum := sha1.Sum(pub) //nolint:gosec
	return sum[:]
}

type ca struct {
	certPEM []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// createCA 建一张新的 CA（P-256，10 年，pathLen 0）；只在内存里，落盘由调用方。
func createCA(now time.Time) (*ca, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Armadra Relay CA " + hex.EncodeToString(suffix[:])},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CADays * day),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          keyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &ca{certPEM: certPEM(der), cert: cert, key: key}, nil
}

// issueLeaf 用 CA 签一张叶证书；key 为空时新生成。答证书 DER 与私钥。
func issueLeaf(c *ca, names []string, now time.Time, days int, key *ecdsa.PrivateKey) ([]byte, *ecdsa.PrivateKey, error) {
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, nil, err
		}
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	cn := "localhost"
	if len(names) > 0 {
		cn = names[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Duration(days) * day),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SubjectKeyId:          keyID(&key.PublicKey),
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	return der, key, err
}

func tlsErr(msg string) error { return &Error{Msg: msg} }

func loadOrCreateCA(dir string, now time.Time) (*ca, error) {
	certFile, keyFile := filepath.Join(dir, CACert), filepath.Join(dir, CAKey)
	if exists(certFile) || exists(keyFile) {
		c, err := loadCA(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		if !c.cert.NotAfter.After(now) {
			return nil, tlsErr("自签 CA 已过期，拒绝启动：删除 tls/ 目录后客户端需要重新确认指纹")
		}
		return c, nil
	}
	c, err := createCA(now)
	if err != nil {
		return nil, err
	}
	pemKey, err := keyPEM(c.key)
	if err != nil {
		return nil, err
	}
	if err := replaceFile(keyFile, pemKey, 0o600); err != nil {
		return nil, err
	}
	if err := replaceFile(certFile, c.certPEM, 0o600); err != nil {
		return nil, err
	}
	return c, nil
}

func loadCA(certFile, keyFile string) (*ca, error) {
	bad := tlsErr("自签 CA 读不出来，拒绝启动：检查 tls/ca.crt 与 tls/ca.key")
	certData, err := os.ReadFile(certFile)
	if err != nil {
		return nil, bad
	}
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, bad
	}
	cert, err := parseCert(certData)
	if err != nil {
		return nil, bad
	}
	key, err := parseECKey(keyData)
	if err != nil {
		return nil, bad
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, tlsErr("自签 CA 的证书与私钥对不上，拒绝启动")
	}
	return &ca{certPEM: certData, cert: cert, key: key}, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// usableLeaf 读已有叶证书；剩余不足、不是这张 CA 签的、私钥对不上或没覆盖 names 时答 nil。
func usableLeaf(dir string, c *ca, names []string, now time.Time) (certData, keyData []byte) {
	certData, err := os.ReadFile(filepath.Join(dir, LeafCert))
	if err != nil {
		return nil, nil
	}
	keyData, err = os.ReadFile(filepath.Join(dir, LeafKey))
	if err != nil {
		return nil, nil
	}
	cert, err := parseCert(certData)
	if err != nil {
		return nil, nil
	}
	if cert.NotAfter.Sub(now) <= LeafRenew {
		return nil, nil
	}
	if cert.CheckSignatureFrom(c.cert) != nil {
		return nil, nil
	}
	key, err := parseECKey(keyData)
	if err != nil {
		return nil, nil
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil
	}
	if !covers(cert, names) {
		return nil, nil
	}
	return certData, keyData
}

func selfSignedAt(dataDir string, names []string, now time.Time) (Material, error) {
	dir := joinDir(dataDir, TLSDir)
	if err := privateDir(dir); err != nil {
		return Material{}, err
	}
	c, err := loadOrCreateCA(dir, now)
	if err != nil {
		return Material{}, err
	}
	leafCert, leafKey := usableLeaf(dir, c, names, now)
	if leafCert == nil {
		der, key, err := issueLeaf(c, names, now, LeafDays, nil)
		if err != nil {
			return Material{}, err
		}
		if leafKey, err = keyPEM(key); err != nil {
			return Material{}, err
		}
		leafCert = certPEM(der)
		if err := replaceFile(filepath.Join(dir, LeafKey), leafKey, 0o600); err != nil {
			return Material{}, err
		}
		if err := replaceFile(filepath.Join(dir, LeafCert), leafCert, 0o600); err != nil {
			return Material{}, err
		}
	}
	// 链：叶证书 + CA，客户端拿到完整链。
	pair, leaf, err := buildMaterial(append(append([]byte{}, leafCert...), c.certPEM...), leafKey)
	if err != nil {
		return Material{}, tlsErr("自签叶证书读不出来：" + err.Error())
	}
	return Material{
		Certificate: pair,
		Anchor:      c.certPEM,
		Fingerprint: fingerprintOf(c.cert.Raw),
		NotAfter:    leaf.NotAfter,
		Source:      "self-signed",
	}, nil
}

// SelfSignedMaterial 读（没有就建）数据目录里的 CA 与叶证书并答在用的一套；坏的 CA 拒绝（*Error）。
func SelfSignedMaterial(dataDir string, names []string) (Material, error) {
	return selfSignedAt(dataDir, names, time.Now())
}

// ExistingCAFingerprint 读 tls/ca.crt 的指纹；没有或读不出答 false。
func ExistingCAFingerprint(dataDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, TLSDir, CACert))
	if err != nil {
		return "", false
	}
	cert, err := parseCert(data)
	if err != nil {
		return "", false
	}
	return fingerprintOf(cert.Raw), true
}

type selfSigned struct {
	dataDir  string
	names    []string
	now      func() time.Time
	interval time.Duration
	current  atomic.Pointer[Material]
}

// SelfSigned 是自签模式的 Manager：叶证书剩 7 天时换。
func SelfSigned(dataDir string, names []string) (Manager, error) {
	return newSelfSigned(dataDir, names, time.Now, RenewCheck)
}

func newSelfSigned(dataDir string, names []string, now func() time.Time, interval time.Duration) (*selfSigned, error) {
	m, err := selfSignedAt(dataDir, names, now())
	if err != nil {
		return nil, err
	}
	s := &selfSigned{dataDir: dataDir, names: names, now: now, interval: interval}
	s.current.Store(&m)
	return s, nil
}

func (s *selfSigned) Current() Material { return *s.current.Load() }

func (s *selfSigned) Start(onChange func(Material), onError func(error)) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			next, err := selfSignedAt(s.dataDir, s.names, s.now())
			if err != nil {
				if onError != nil {
					onError(err)
				}
				continue
			}
			old := s.current.Load()
			if string(next.Certificate.Certificate[0]) != string(old.Certificate.Certificate[0]) {
				s.current.Store(&next)
				if onChange != nil {
					onChange(next)
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
