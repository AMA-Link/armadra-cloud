package store_test

import (
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func TestWrapperRoundTripAndBinding(t *testing.T) {
	key, err := store.ParseMasterKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.NewWrapper(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := w.Seal([]byte("secret"), "aad-1")
	if strings.Contains(sealed, "secret") {
		t.Fatal("密文里不该有明文")
	}
	plain, err := w.Open(sealed, "aad-1")
	if err != nil || string(plain) != "secret" {
		t.Fatalf("往返：%q %v", plain, err)
	}
	if _, err := w.Open(sealed, "aad-2"); err == nil {
		t.Fatal("附加数据不符应解不开")
	}
	if w.Seal([]byte("secret"), "aad-1") == sealed {
		t.Fatal("每次包裹的随机数应不同")
	}
	other, _ := store.NewWrapper(make([]byte, 32))
	if _, err := other.Open(sealed, "aad-1"); err == nil {
		t.Fatal("主密钥不对应解不开")
	}
	tampered := []byte(sealed)
	tampered[len(tampered)-6] ^= 1
	if _, err := w.Open(string(tampered), "aad-1"); err == nil {
		t.Fatal("密文被改应解不开")
	}
	secret := w.SealSecret([]byte("seed"), "totp")
	if !strings.HasPrefix(secret, "aes256gcm-v1:") {
		t.Fatalf("secret_wrap 形状：%q", secret)
	}
	if plain, err := w.OpenSecret(secret, "totp"); err != nil || string(plain) != "seed" {
		t.Fatalf("OpenSecret：%q %v", plain, err)
	}
	if _, err := w.OpenSecret("none:abc", "totp"); err == nil {
		t.Fatal("别的包裹方式应拒绝")
	}
}

func TestParseMasterKeyRejects(t *testing.T) {
	for _, bad := range []string{"", "zz", strings.Repeat("a", 63), strings.Repeat("a", 66)} {
		if _, err := store.ParseMasterKey(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if _, err := store.NewWrapper(make([]byte, 16)); err == nil {
		t.Error("16 字节主密钥应被拒绝")
	}
}
