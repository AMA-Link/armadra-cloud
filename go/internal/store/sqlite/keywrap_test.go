package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
)

func masterKey(fill byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return key
}

func tenantSettings() store.Tenant {
	return store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://relay.armadra.test", TrustedOrigins: []string{}}}
}

func rawKey(t *testing.T, path, kid string) (body, wrap string) {
	t.Helper()
	raw := openRaw(t, path)
	defer func() { _ = raw.Close() }()
	if err := raw.QueryRow("SELECT private_key_pkcs8, private_key_wrap FROM signing_keys WHERE kid = ?", kid).Scan(&body, &wrap); err != nil {
		t.Fatal(err)
	}
	return body, wrap
}

// 带主密钥时新写的签名私钥以 aes256gcm-v1 包裹落库，读时解开，照常签发与轮换；
// 没给主密钥读到包裹的答 ErrWrapped，主密钥不对解不开。
func TestSigningKeysWrappedWithMasterKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true, MasterKey: masterKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTenant(ctx, "cloud", tenantSettings()); err != nil {
		t.Fatal(err)
	}
	key, err := store.GenerateSigningKey("k1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddSigningKey(ctx, "cloud", key); err != nil {
		t.Fatal(err)
	}
	body, wrap := rawKey(t, path, "k1")
	if wrap != store.KeyWrapAESGCM || body == key.PrivateKeyPKCS8 || strings.Contains(body, key.PrivateKeyPKCS8[:20]) {
		t.Fatalf("私钥没有被包裹：%q", wrap)
	}
	keys, err := db.Keys("cloud").SigningKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].PrivateKeyPKCS8 != key.PrivateKeyPKCS8 {
		t.Fatalf("读时应解开：%d %v", len(keys), err)
	}
	signer := core.NewSigner(db.Keys("cloud"), nil)
	if _, err := signer.Sign(ctx, "armadra-access", core.Claims{{Key: "sub", Value: "x"}}, core.SignOptions{TTL: 60_000}); err != nil {
		t.Fatalf("包裹的钥应能签发：%v", err)
	}
	newKid, err := db.RotateKey(ctx, "cloud", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, wrap := rawKey(t, path, newKid); wrap != store.KeyWrapAESGCM {
		t.Fatalf("轮换出的新钥也要包裹：%q", wrap)
	}
	snapshot, err := db.ExportTenant(ctx, "cloud")
	if err != nil || len(snapshot.SigningKeys) != 2 || snapshot.SigningKeys[0].PrivateKeyPKCS8 != key.PrivateKeyPKCS8 {
		t.Fatalf("导出的快照里是明文：%v", err)
	}
	_ = db.Close()

	plain, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Keys("cloud").SigningKeys(ctx); !errors.Is(err, store.ErrWrapped) {
		t.Fatalf("没给主密钥应答 ErrWrapped：%v", err)
	}
	_ = plain.Close()

	wrong, err := sqlite.Open(ctx, path, sqlite.Options{MasterKey: masterKey(2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Keys("cloud").SigningKeys(ctx); store.KindOf(err) != store.KindCorrupt {
		t.Fatalf("主密钥不对应拒绝：%v", err)
	}
	_ = wrong.Close()
}

// 包裹绑定租户与 kid：把密文抄到别的 kid 上解不开。
func TestWrappedKeyBoundToKid(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true, MasterKey: masterKey(3)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "cloud", tenantSettings()); err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"k1", "k2"} {
		key, err := store.GenerateSigningKey(kid, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddSigningKey(ctx, "cloud", key); err != nil {
			t.Fatal(err)
		}
	}
	raw := openRaw(t, path)
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("UPDATE signing_keys SET private_key_pkcs8 = (SELECT private_key_pkcs8 FROM signing_keys WHERE kid = 'k1') WHERE kid = 'k2'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Keys("cloud").SigningKeys(ctx); store.KindOf(err) != store.KindCorrupt {
		t.Fatalf("抄到别的 kid 应解不开：%v", err)
	}
}

// WrapSigningKeys 把明文私钥就地包裹，可重复执行，包裹后签发照常；没给主密钥拒绝。
func TestWrapSigningKeysIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTenant(ctx, "cloud", tenantSettings()); err != nil {
		t.Fatal(err)
	}
	var originals []string
	for _, kid := range []string{"k1", "k2"} {
		key, _ := store.GenerateSigningKey(kid, 1)
		originals = append(originals, key.PrivateKeyPKCS8)
		if err := db.AddSigningKey(ctx, "cloud", key); err != nil {
			t.Fatal(err)
		}
	}
	if plain, wrapped, err := db.KeyWrapStats(ctx); err != nil || plain != 2 || wrapped != 0 {
		t.Fatalf("统计：%d %d %v", plain, wrapped, err)
	}
	if _, err := db.WrapSigningKeys(ctx); err == nil {
		t.Fatal("没给主密钥应拒绝")
	}
	_ = db.Close()

	db, err = sqlite.Open(ctx, path, sqlite.Options{MasterKey: masterKey(4)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, want := range []int{2, 0} {
		n, err := db.WrapSigningKeys(ctx)
		if err != nil || n != want {
			t.Fatalf("第 %d 次包裹 %d 把：%v", i+1, n, err)
		}
	}
	if plain, wrapped, err := db.KeyWrapStats(ctx); err != nil || plain != 0 || wrapped != 2 {
		t.Fatalf("统计：%d %d %v", plain, wrapped, err)
	}
	keys, err := db.Keys("cloud").SigningKeys(ctx)
	if err != nil || len(keys) != 2 || keys[0].PrivateKeyPKCS8 != originals[0] || keys[1].PrivateKeyPKCS8 != originals[1] {
		t.Fatalf("包裹后读回应与原来一致：%v", err)
	}
}
