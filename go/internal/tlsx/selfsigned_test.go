package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNames(t *testing.T) {
	cases := []struct {
		host string
		want []string
	}{
		{"relay.example", []string{"relay.example", "localhost", "127.0.0.1"}},
		{"127.0.0.1", []string{"127.0.0.1", "localhost"}},
		{"localhost", []string{"localhost", "127.0.0.1"}},
		{"[::1]", []string{"::1", "localhost", "127.0.0.1"}},
	}
	for _, c := range cases {
		got := Names(c.host)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %v", c.host, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: %v", c.host, got)
			}
		}
	}
}

var t0 = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

func TestSelfSignedLayout(t *testing.T) {
	dir := t.TempDir()
	m, err := selfSignedAt(dir, Names("203.0.113.5"), t0)
	if err != nil {
		t.Fatal(err)
	}
	caData, _ := os.ReadFile(filepath.Join(dir, "tls", "ca.crt"))
	ca, err := parseCert(caData)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Fatalf("CA 约束不对: %+v", ca)
	}
	if pub := ca.PublicKey.(*ecdsa.PublicKey); pub.Curve != elliptic.P256() {
		t.Fatal("CA 不是 P-256")
	}
	if got := ca.NotAfter.Sub(t0); got < 3649*day {
		t.Fatalf("CA 有效期 %v", got)
	}
	leaf := m.Certificate.Leaf
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		t.Fatal(err)
	}
	if len(leaf.IPAddresses) != 2 || leaf.IPAddresses[0].String() != "203.0.113.5" || leaf.IPAddresses[1].String() != "127.0.0.1" || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Fatalf("SAN: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if d := leaf.NotAfter.Sub(t0).Round(day); d != LeafDays*day {
		t.Fatalf("叶证书有效期 %v", d)
	}
	if m.Fingerprint != fingerprintOf(ca.Raw) || len(m.Fingerprint) != 64 {
		t.Fatalf("指纹 %s", m.Fingerprint)
	}
	if string(m.Anchor) != string(caData) || m.Source != "self-signed" {
		t.Fatal("anchor / source")
	}
	if len(m.Certificate.Certificate) != 2 {
		t.Fatal("链应为叶证书 + CA")
	}
	for _, n := range []string{"ca.crt", "ca.key", "leaf.crt", "leaf.key"} {
		info, err := os.Stat(filepath.Join(dir, "tls", n))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", n, info, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, "tls")); info.Mode().Perm() != 0o700 {
		t.Fatal("tls 目录应为 0700")
	}
	keyData, _ := os.ReadFile(filepath.Join(dir, "tls", "ca.key"))
	if string(keyData[:27]) != "-----BEGIN PRIVATE KEY-----" {
		t.Fatalf("ca.key 应为 PKCS8: %q", keyData[:27])
	}
	if fp, ok := ExistingCAFingerprint(dir); !ok || fp != m.Fingerprint {
		t.Fatal("ExistingCAFingerprint")
	}
	if _, ok := ExistingCAFingerprint(t.TempDir()); ok {
		t.Fatal("空目录不应有指纹")
	}
}

func TestSelfSignedReuseAndRenew(t *testing.T) {
	dir := t.TempDir()
	names := Names("relay.example")
	first, err := selfSignedAt(dir, names, t0)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := selfSignedAt(dir, names, t0)
	if string(again.Certificate.Certificate[0]) != string(first.Certificate.Certificate[0]) {
		t.Fatal("重启应复用叶证书")
	}
	nearEnd := t0.Add((LeafDays-7)*day + time.Second)
	renewed, err := selfSignedAt(dir, names, nearEnd)
	if err != nil {
		t.Fatal(err)
	}
	if string(renewed.Certificate.Certificate[0]) == string(first.Certificate.Certificate[0]) {
		t.Fatal("剩 7 天内应重签")
	}
	if renewed.Fingerprint != first.Fingerprint {
		t.Fatal("CA 指纹应不变")
	}
	moved, err := selfSignedAt(dir, Names("relay2.example"), nearEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !covers(moved.Certificate.Leaf, []string{"relay2.example"}) || moved.Fingerprint != first.Fingerprint {
		t.Fatal("host 变了应只换叶证书")
	}
	// 多一天之前（剩 8 天）不换。
	early := t0.Add((LeafDays - 8) * day)
	r, _ := selfSignedAt(dir, Names("relay2.example"), early)
	if string(r.Certificate.Certificate[0]) != string(moved.Certificate.Certificate[0]) {
		t.Fatal("剩 8 天不应重签")
	}
}

func TestSelfSignedBrokenCA(t *testing.T) {
	var te *Error
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		if _, err := selfSignedAt(dir, Names("127.0.0.1"), t0); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("过期", func(t *testing.T) {
		_, err := selfSignedAt(setup(t), Names("127.0.0.1"), t0.Add(3651*day))
		if !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("证书损坏", func(t *testing.T) {
		dir := setup(t)
		caFile := filepath.Join(dir, "tls", "ca.crt")
		os.WriteFile(caFile, []byte("garbage"), 0o600)
		_, err := selfSignedAt(dir, Names("127.0.0.1"), t0)
		if !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
		if data, _ := os.ReadFile(caFile); string(data) != "garbage" {
			t.Fatal("不能覆盖坏掉的 CA")
		}
	})
	t.Run("缺私钥", func(t *testing.T) {
		dir := setup(t)
		os.Remove(filepath.Join(dir, "tls", "ca.key"))
		_, err := selfSignedAt(dir, Names("127.0.0.1"), t0)
		if !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
		if _, e := os.Stat(filepath.Join(dir, "tls", "ca.key")); e == nil {
			t.Fatal("不能补建私钥")
		}
	})
	t.Run("私钥对不上", func(t *testing.T) {
		dir := setup(t)
		other := t.TempDir()
		if _, err := selfSignedAt(other, Names("127.0.0.1"), t0); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(other, "tls", "ca.key"))
		os.WriteFile(filepath.Join(dir, "tls", "ca.key"), data, 0o600)
		_, err := selfSignedAt(dir, Names("127.0.0.1"), t0)
		if !errors.As(err, &te) {
			t.Fatalf("%v", err)
		}
	})
}

func TestSelfSignedManagerRenews(t *testing.T) {
	dir := t.TempDir()
	var clock = t0
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	now := func() time.Time { <-mu; defer func() { mu <- struct{}{} }(); return clock }
	m, err := newSelfSigned(dir, Names("127.0.0.1"), now, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Current()
	changed := make(chan Material, 1)
	stop := m.Start(func(x Material) { changed <- x }, func(error) {})
	defer stop()
	<-mu
	clock = t0.Add(85 * day)
	mu <- struct{}{}
	select {
	case x := <-changed:
		if string(x.Certificate.Certificate[0]) == string(before.Certificate.Certificate[0]) {
			t.Fatal("证书没换")
		}
		if string(m.Current().Certificate.Certificate[0]) != string(x.Certificate.Certificate[0]) {
			t.Fatal("Current 应是新证书")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有回调")
	}
}

func TestSelfSignedPublicAPI(t *testing.T) {
	dir := t.TempDir()
	m, err := SelfSignedMaterial(dir, Names("127.0.0.1"))
	if err != nil || m.Source != "self-signed" {
		t.Fatal(err)
	}
	mgr, err := SelfSigned(dir, Names("127.0.0.1"))
	if err != nil || mgr.Current().Fingerprint != m.Fingerprint {
		t.Fatal(err)
	}
	stop := mgr.Start(nil, nil)
	stop()
	stop()
}
