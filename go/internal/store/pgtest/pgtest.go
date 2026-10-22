// Package pgtest 给 PostgreSQL 相关的测试要一个隔离的 schema。只在 _test.go 里用。
//
// 连接串取自环境变量 ARMADRA_CLOUD_TEST_DATABASE_URL（pnpm dev:up 起的库，或 CI 的服务容器）；
// 没有就跳过。每个测试建一个随机名字的 schema，结束时只删这个 schema，不碰库里别的东西。
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL 是测试库连接串的环境变量。
const EnvURL = "ARMADRA_CLOUD_TEST_DATABASE_URL"

// URL 返回测试库连接串；没有就跳过当前测试，并说明怎么开。
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("没有 PostgreSQL：设置 %s（pnpm dev:up 的库），跳过", EnvURL)
	}
	return url
}

// Schema 建一个随机名字的 schema 并在测试结束时删掉；答它的名字。
func Schema(t testing.TB) (url, schema string) {
	t.Helper()
	url = URL(t)
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema = "gotest_" + hex.EncodeToString(suffix[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("测试库连不上：%v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Logf("清理 schema 时连不上：%v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("清理 schema %s 失败：%v", schema, err)
		}
	})
	return url, schema
}
