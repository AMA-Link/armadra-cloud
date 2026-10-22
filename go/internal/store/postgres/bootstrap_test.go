package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

func TestBootstrapTenantCreatesOnceAndRefusesIssuerChange(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	open := func(key []byte) *postgres.DB {
		t.Helper()
		db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, MasterKey: key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open(masterKey(7))
	// 没主密钥的实例不许引导。
	if _, err := open(nil).BootstrapTenant(ctx, "cloud", "https://app.armadra.test", 1); err == nil {
		t.Fatal("没有主密钥应拒绝引导")
	}
	// 多个实例同时启动：只有一个建，其余看到已存在。
	var wg sync.WaitGroup
	results := make(chan bool, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := db.BootstrapTenant(ctx, "cloud", "https://app.armadra.test", 5)
			results <- created
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("恰好一个实例建租户：%d", createdCount)
	}
	tenant, err := db.ReadTenant(ctx, "cloud")
	if err != nil || tenant == nil || tenant.Issuer != "https://app.armadra.test" {
		t.Fatalf("租户：%+v %v", tenant, err)
	}
	keys, err := db.Keys("cloud").SigningKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Kid != "k1" {
		t.Fatalf("第一把签名钥：%+v %v", keys, err)
	}
	var wrap string
	if err := db.Pool().QueryRow(ctx, "SELECT private_key_wrap FROM signing_keys").Scan(&wrap); err != nil || wrap != "aes256gcm-v1" {
		t.Fatalf("签名钥应被包裹：%q %v", wrap, err)
	}
	// 再来一次：不新建、不改。
	created, err := db.BootstrapTenant(ctx, "cloud", "https://app.armadra.test", 99)
	if err != nil || created {
		t.Fatalf("第二次不新建：%v %v", created, err)
	}
	// issuer 不一致：拒绝，且不改库。
	_, err = db.BootstrapTenant(ctx, "cloud", "https://other.armadra.test", 99)
	if store.KindOf(err) != store.KindUnknown {
		t.Fatalf("issuer 不一致应拒绝：%v", err)
	}
	tenant, _ = db.ReadTenant(ctx, "cloud")
	if tenant.Issuer != "https://app.armadra.test" {
		t.Fatalf("不得改写已有租户的 issuer：%s", tenant.Issuer)
	}
}

func TestEnsureAuditPartitions(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	names, err := db.EnsureAuditPartitions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"audit_log_2026_11", "audit_log_2026_12", "audit_log_2027_01"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("分区名（跨年）：%v", names)
	}
	count := func() int {
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*)::int FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent WHERE p.oid = to_regclass('audit_log') AND c.relname LIKE 'audit_log_2%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 3 {
		t.Fatalf("应有 3 个月分区：%d", count())
	}
	// 幂等；并发两个实例同时来也不出错。
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.EnsureAuditPartitions(ctx, now)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count() != 3 {
		t.Fatalf("幂等：%d", count())
	}
	// 兜底分区里已有下个月的行：被搬进新分区。
	if _, err := db.Pool().Exec(ctx, `INSERT INTO audit_log (at_ms, event_id, tenant_id, actor_kind, action)
	  VALUES ($1, 'e1', 'cloud', 'system', 'x')`, time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureAuditPartitions(ctx, now.AddDate(0, 2, 0)); err != nil {
		t.Fatal(err)
	}
	var inDefault int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*)::int FROM audit_log_default").Scan(&inDefault); err != nil || inDefault != 0 {
		t.Fatalf("兜底分区应已搬空：%d %v", inDefault, err)
	}
}

func TestOpenNoMigrateRefusesBehindLedger(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	// 空 schema（没有账本）：落后。
	_, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, NoMigrate: true})
	if store.KindOf(err) != store.KindBehind {
		t.Fatalf("空库应答落后：%v", err)
	}
	// 迁移到最新后 NoMigrate 照常打开。
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, NoMigrate: true})
	if err != nil {
		t.Fatalf("最新的库应能打开：%v", err)
	}
	_ = db.Close()
	// 账本落后（程序带的比库里多）：答落后。
	all, err := postgres.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	url2, schema2 := pgtest.Schema(t)
	old, err := postgres.Open(ctx, postgres.Options{URL: url2, Schema: schema2, Migrations: all[:3]})
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	_, err = postgres.Open(ctx, postgres.Options{URL: url2, Schema: schema2, NoMigrate: true})
	if store.KindOf(err) != store.KindBehind {
		t.Fatalf("账本落后应拒绝：%v", err)
	}
	// 库比程序新：拒绝（KindNewer）。
	_, err = postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, NoMigrate: true, Migrations: all[:3]})
	if store.KindOf(err) != store.KindNewer {
		t.Fatalf("库比程序新应拒绝：%v", err)
	}
}
