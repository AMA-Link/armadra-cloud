package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

func masterKey(fill byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return key
}

// 带主密钥时签名私钥以 aes256gcm-v1 包裹落库，读时解开，照常签发与轮换。
func TestSigningKeysWrappedWithMasterKey(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	open := func(key []byte) (*postgres.DB, error) {
		return postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, MasterKey: key})
	}
	db, err := open(masterKey(1))
	if err != nil {
		t.Fatal(err)
	}
	settings := store.TenantSettings{Issuer: "https://app.armadra.test", TrustedOrigins: []string{}}
	if err := db.CreateTenant(ctx, "cloud", store.Tenant{TenantSettings: settings}); err != nil {
		t.Fatal(err)
	}
	key, err := store.GenerateSigningKey("k1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddSigningKey(ctx, "cloud", key); err != nil {
		t.Fatal(err)
	}
	var stored, wrap string
	if err := db.Pool().QueryRow(ctx, "SELECT private_key_pkcs8, private_key_wrap FROM signing_keys WHERE kid = 'k1'").Scan(&stored, &wrap); err != nil {
		t.Fatal(err)
	}
	if wrap != "aes256gcm-v1" || stored == key.PrivateKeyPKCS8 || strings.Contains(stored, key.PrivateKeyPKCS8[:20]) {
		t.Fatalf("私钥没有被包裹：%q %q", wrap, stored[:12])
	}
	keys, err := db.Keys("cloud").SigningKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].PrivateKeyPKCS8 != key.PrivateKeyPKCS8 {
		t.Fatalf("读时应解开：%v %v", len(keys), err)
	}
	signer := core.NewSigner(db.Keys("cloud"), nil)
	if _, err := signer.Sign(ctx, "armadra-access", core.Claims{{"sub", "x"}}, core.SignOptions{TTL: 60_000}); err != nil {
		t.Fatalf("包裹的钥应能签发：%v", err)
	}
	if _, err := db.RotateKey(ctx, "cloud", 2); err != nil {
		t.Fatal(err)
	}
	var wrapped int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*)::int FROM signing_keys WHERE private_key_wrap = 'aes256gcm-v1'").Scan(&wrapped); err != nil || wrapped != 2 {
		t.Fatalf("轮换出的新钥也要包裹：%d %v", wrapped, err)
	}
	// 导出的快照里是明文，导入到别处再包裹。
	snapshot, err := db.ExportTenant(ctx, "cloud")
	if err != nil || len(snapshot.SigningKeys) != 2 || snapshot.SigningKeys[0].PrivateKeyPKCS8 != key.PrivateKeyPKCS8 {
		t.Fatalf("导出：%v", err)
	}
	_ = db.Close()

	// 没给主密钥：读到包裹过的拒绝启动。
	plain, err := open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Keys("cloud").SigningKeys(ctx); store.KindOf(err) != store.KindUnknown {
		t.Fatalf("没给主密钥应拒绝：%v", err)
	}
	_ = plain.Close()

	// 主密钥不对：解不开。
	wrong, err := open(masterKey(2))
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
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, MasterKey: masterKey(3)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "cloud", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://app.armadra.test", TrustedOrigins: []string{}}}); err != nil {
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
	if _, err := db.Pool().Exec(ctx, "UPDATE signing_keys SET private_key_pkcs8 = (SELECT private_key_pkcs8 FROM signing_keys WHERE kid = 'k1') WHERE kid = 'k2'"); err != nil {
		t.Fatal(err)
	}
	_ = db.RefreshKeys(ctx, "cloud")
	if _, err := db.Keys("cloud").SigningKeys(ctx); store.KindOf(err) != store.KindCorrupt {
		t.Fatalf("抄到别的 kid 应解不开：%v", err)
	}
}
