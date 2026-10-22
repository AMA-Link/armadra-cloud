package tlsx

import (
	"crypto/ecdsa"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair 用一张新的 CA 签叶证书并写成链（叶 + CA）与私钥。
func writePair(t *testing.T, certPath, keyPath string, names []string, withCA bool) (chain, key []byte) {
	t.Helper()
	c, err := createCA(t0)
	if err != nil {
		t.Fatal(err)
	}
	der, k, err := issueLeaf(c, names, time.Now(), 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	chain = certPEM(der)
	if withCA {
		chain = append(chain, c.certPEM...)
	}
	key, _ = keyPEM(k)
	if err := replaceFile(certPath, chain, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return chain, key
}

func TestFileLoad(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	t.Run("单张证书指纹是它自己且无 anchor", func(t *testing.T) {
		writePair(t, cp, kp, []string{"a.test"}, false)
		m, err := File(cp, kp)
		if err != nil {
			t.Fatal(err)
		}
		cur := m.Current()
		if cur.Source != "file" || cur.Anchor != nil || cur.Fingerprint != fingerprintOf(cur.Certificate.Certificate[0]) {
			t.Fatalf("%+v", cur)
		}
	})
	t.Run("多张时最后一张是锚", func(t *testing.T) {
		chain, _ := writePair(t, cp, kp, []string{"a.test"}, true)
		m, err := File(cp, kp)
		if err != nil {
			t.Fatal(err)
		}
		cur := m.Current()
		blocks := pemCertBlocks(chain)
		if string(cur.Anchor) != string(blocks[1]) || cur.Fingerprint != fingerprintOf(cur.Certificate.Certificate[1]) {
			t.Fatalf("%+v", cur)
		}
	})
	t.Run("对不上与读不出都是 *Error", func(t *testing.T) {
		writePair(t, cp, kp, []string{"a.test"}, false)
		other := filepath.Join(dir, "o.pem")
		okey := filepath.Join(dir, "ok.pem")
		writePair(t, other, okey, []string{"b.test"}, false)
		var te *Error
		if _, err := File(cp, okey); !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
		if _, err := File(filepath.Join(dir, "none"), kp); !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
		if _, err := File(cp, kp); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("SEC1 私钥也能读", func(t *testing.T) {
		chain, key := writePair(t, cp, kp, []string{"a.test"}, false)
		_ = chain
		k, _ := parseECKey(key)
		der, _ := marshalSEC1(k)
		os.WriteFile(kp, der, 0o600)
		if _, err := File(cp, kp); err != nil {
			t.Fatal(err)
		}
	})
}

func marshalSEC1(k *ecdsa.PrivateKey) ([]byte, error) { return encodeSEC1(k) }

func TestFileHotReload(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writePair(t, cp, kp, []string{"a.test"}, false)
	m, err := FileWith(cp, kp, FileOptions{Poll: 10 * time.Millisecond, Debounce: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	first := m.Current()
	changed := make(chan Material, 4)
	errs := make(chan error, 4)
	stop := m.Start(func(x Material) { changed <- x }, func(e error) { errs <- e })
	defer stop()

	// 换到一半：证书新、私钥旧 -> 对不上，保留旧的并报错。
	newCert, newKey := func() ([]byte, []byte) {
		c := filepath.Join(dir, "n.pem")
		k := filepath.Join(dir, "nk.pem")
		return writePair(t, c, k, []string{"b.test"}, false)
	}()
	if err := replaceFile(cp, newCert, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-errs:
		var te *Error
		if !errors.As(e, &te) {
			t.Fatalf("%v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有报错")
	}
	if string(m.Current().Certificate.Certificate[0]) != string(first.Certificate.Certificate[0]) {
		t.Fatal("对不上时应保留旧证书")
	}
	// 私钥也换上：热换。
	if err := replaceFile(kp, newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case x := <-changed:
		if !covers(x.Certificate.Leaf, []string{"b.test"}) {
			t.Fatal("应换成 b.test")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有热换")
	}
	if !covers(m.Current().Certificate.Leaf, []string{"b.test"}) {
		t.Fatal("Current 应是新证书")
	}
}

func TestFileDebounce(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writePair(t, cp, kp, []string{"a.test"}, false)
	m, _ := FileWith(cp, kp, FileOptions{Poll: 10 * time.Millisecond, Debounce: 200 * time.Millisecond})
	changed := make(chan time.Time, 4)
	stop := m.Start(func(Material) { changed <- time.Now() }, nil)
	defer stop()
	start := time.Now()
	writePair(t, cp, kp, []string{"b.test"}, false)
	select {
	case at := <-changed:
		if at.Sub(start) < 200*time.Millisecond {
			t.Fatalf("防抖不足: %v", at.Sub(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有热换")
	}
}
