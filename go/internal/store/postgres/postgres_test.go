package postgres_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/storetest"
)

func create(t *testing.T) store.Backend {
	t.Helper()
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// 端口合同（15 例）。
func TestPortsContract(t *testing.T) { storetest.PortsContract(t, create) }

func kindOf(t *testing.T, err error) store.ErrKind {
	t.Helper()
	if err == nil {
		t.Fatal("应该被拒绝")
	}
	kind := store.KindOf(err)
	if kind == "" {
		t.Fatalf("不是库的问题：%v", err)
	}
	return kind
}

func TestUnreachableErrorHasNoConnectionString(t *testing.T) {
	_, err := postgres.Open(context.Background(), postgres.Options{
		URL: "postgres://armadra:s3cret-pass@127.0.0.1:1/armadra_cloud?connect_timeout=1",
	})
	if kindOf(t, err) != store.KindUnreachable {
		t.Fatalf("应为 unreachable：%v", err)
	}
	for _, leak := range []string{"s3cret-pass", "armadra:", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误信息泄露了连接串片段 %q：%v", leak, err)
		}
	}
	_, err = postgres.Open(context.Background(), postgres.Options{URL: "://坏的 s3cret-pass"})
	if kindOf(t, err) != store.KindUnreachable || strings.Contains(err.Error(), "s3cret-pass") {
		t.Fatalf("坏连接串的错误：%v", err)
	}
}

func TestSchemaNameIsValidated(t *testing.T) {
	_, err := postgres.Open(context.Background(), postgres.Options{URL: pgtest.URL(t), Schema: "Bad-Name; DROP"})
	if kindOf(t, err) != store.KindUnknown {
		t.Fatalf("不合格的 schema 名应被拒绝：%v", err)
	}
}

func TestLedgerRefusals(t *testing.T) {
	ctx := context.Background()
	migrations, err := postgres.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(t *testing.T, url, schema, statement string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("newer", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		_, err = postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, Migrations: migrations[:1]})
		if kindOf(t, err) != store.KindNewer {
			t.Fatalf("应为 newer：%v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		exec(t, url, schema, "UPDATE cloud_migrations SET sha384 = 'x' WHERE id = 1")
		_, err = postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
		if kindOf(t, err) != store.KindUnknown {
			t.Fatalf("应为 unknown：%v", err)
		}
	})

	t.Run("tables without ledger", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		exec(t, url, schema, "CREATE TABLE somebody_elses (id int)")
		_, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
		if kindOf(t, err) != store.KindUnknown {
			t.Fatalf("表存在而账本缺失应拒绝：%v", err)
		}
		// 拒绝时不许碰那张表。
		exec(t, url, schema, "SELECT 1 FROM somebody_elses")
	})

	t.Run("prefix catches up", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, Migrations: migrations[:1]})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		db, err = postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Ports().Traffic.Query(ctx, "t", store.TrafficQuery{ToMs: 1, Granularity: "hour", Limit: 1}); err != nil {
			t.Fatalf("补跑后流量表应可用：%v", err)
		}
	})

	// 多个节点同时启动：advisory lock 保证只有一个真正执行，其余对账为最新。
	t.Run("concurrent starts", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
				if err == nil {
					_ = db.Close()
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("并发启动应都成功：%v", err)
			}
		}
	})
}

func TestRevisionAndRotateKey(t *testing.T) {
	ctx := context.Background()
	db := create(t).(*postgres.DB)
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "default", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}
	first, err := store.GenerateSigningKey("k1", storetest.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddSigningKey(ctx, "default", first); err != nil {
		t.Fatal(err)
	}
	before, _, _ := db.Revision(ctx, "default")
	if _, err := db.Keys("default").SigningKeys(ctx); err != nil {
		t.Fatal(err)
	}
	kid, err := db.RotateKey(ctx, "default", storetest.Now+1000)
	if err != nil || kid != "k2" {
		t.Fatalf("轮换：%q %v", kid, err)
	}
	after, _, _ := db.Revision(ctx, "default")
	if after != before+1 {
		t.Fatalf("轮换应让修订号加一：%d → %d", before, after)
	}
	keys, err := db.Keys("default").SigningKeys(ctx)
	if err != nil || len(keys) != 2 || keys[0].RetiredAtMs == nil {
		t.Fatalf("缓存应随轮换失效：%+v %v", keys, err)
	}
	// 没改任何东西的 Update 不加修订。
	if err := db.Ports().Accounts.Update(ctx, "default", "nobody", func(*store.AccountRecord) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := db.Revision(ctx, "default"); again != after {
		t.Fatalf("没有改动不该加修订：%d → %d", after, again)
	}
}
