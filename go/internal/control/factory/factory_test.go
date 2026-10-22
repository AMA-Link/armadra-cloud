package factory_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/factory"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

func TestParse(t *testing.T) {
	for text, want := range map[string]factory.Kind{"": factory.Memory, "memory": factory.Memory, "redis": factory.Redis, "postgres": factory.Postgres} {
		if got, err := factory.Parse(text); err != nil || got != want {
			t.Fatalf("%q → %q %v", text, got, err)
		}
	}
	if _, err := factory.Parse("etcd"); err == nil {
		t.Fatal("未知取值应报错")
	}
}

// 三种实现都满足同一个接口，装配方拿到之后用法相同。
func TestOpenMemory(t *testing.T) {
	c, err := factory.Open(context.Background(), factory.Config{Kind: factory.Memory, Options: cluster.Options{Issuer: "https://x.test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.Issuer() != "https://x.test" || c.Mode() != "personal" || c.NodeCommands() == nil {
		t.Fatalf("%q %q", c.Issuer(), c.Mode())
	}
}

func TestOpenRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis")
	}
	c, err := factory.Open(context.Background(), factory.Config{Kind: factory.Redis, RedisURL: url, RedisPrefix: "relay-test:factory:",
		Options: cluster.Options{Issuer: "https://x.test", NodeID: "n"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var _ control.Cluster = c
}

func TestOpenPostgresSharesTheStorePool(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c, err := factory.Open(ctx, factory.Config{Kind: factory.Postgres, PostgresPool: db.Pool(),
		Options: cluster.Options{Issuer: "https://x.test", NodeID: "n"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Routes().Set(ctx, "s", control.Route{Node: "n", TunnelID: "t"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	// 共用的连接池不归控制面关：Close 之后存储还能用。
	if _, err := db.Pool().Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("控制面的 Close 不该关存储的连接池：%v", err)
	}
	// 缺表（空 schema）拒绝启动。
	url2, schema2 := pgtest.Schema(t)
	if _, err := factory.Open(ctx, factory.Config{Kind: factory.Postgres, PostgresURL: url2, PostgresSchema: schema2}); err == nil {
		t.Fatal("没迁移应拒绝启动")
	}
}
