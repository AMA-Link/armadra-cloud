package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
)

// schemaLock 的键带着 schema：同一个库里不同 schema 互不排队，同一 schema 里互斥。
func TestSchemaLockIsScopedToSchema(t *testing.T) {
	t.Parallel()
	url, schemaA := pgtest.Schema(t)
	_, schemaB := pgtest.Schema(t)
	ctx := context.Background()
	begin := func(schema string) pgx.Tx {
		t.Helper()
		config, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		config.RuntimeParams["search_path"] = schema
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}
	held := begin(schemaA)
	if err := schemaLock(ctx, held, "migrate"); err != nil {
		t.Fatal(err)
	}

	other := begin(schemaB)
	free, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := schemaLock(free, other, "migrate"); err != nil {
		t.Fatalf("别的 schema 的同名锁不该排队：%v", err)
	}

	same := begin(schemaA)
	blocked, cancelBlocked := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelBlocked()
	if err := schemaLock(blocked, same, "migrate"); err == nil {
		t.Fatal("同一 schema 的同名锁应互斥")
	}
	// 同一 schema 里名字不同的锁互不影响。
	again := begin(schemaA)
	distinct, cancelDistinct := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDistinct()
	if err := schemaLock(distinct, again, "audit"); err != nil {
		t.Fatalf("不同名字的锁不该排队：%v", err)
	}
}
