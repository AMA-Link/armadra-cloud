// Package factory 按 --control 选哪种控制面：memory（缺省，单节点）、redis、postgres（多节点）。
// 装配方（personal、saas）只调 Open，拿到 control.Cluster 后对模式零分支。
package factory

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
)

// Kind 是控制面的种类（--control 的取值）。
type Kind string

// 控制面的种类。
const (
	Memory   Kind = "memory"
	Redis    Kind = "redis"
	Postgres Kind = "postgres"
)

// Kinds 是 --control 的全部合法取值。
var Kinds = []Kind{Memory, Redis, Postgres}

// Parse 解析 --control 的取值；空串是 memory。
func Parse(text string) (Kind, error) {
	if text == "" {
		return Memory, nil
	}
	for _, kind := range Kinds {
		if string(kind) == text {
			return kind, nil
		}
	}
	return "", fmt.Errorf("--control 只能是 memory、redis 或 postgres：%s", text)
}

// Config 是 Open 的入参。
type Config struct {
	Kind Kind
	// Options 是各种类共用的选项：Issuer、Mode、NodeID（--node-id）、JWKS、Limits、Revoked、OnError。
	Options cluster.Options

	// Redis：URL 必填；Prefix 缺省 "relay:"。
	RedisURL    string
	RedisPrefix string

	// PostgreSQL：Pool（与存储共用同一个库时传存储的 Pool，调用方管它）或 URL + Schema（自建）。
	PostgresPool   *pgxpool.Pool
	PostgresURL    string
	PostgresSchema string
	// PostgresChannelPrefix 加在 NOTIFY 频道名前（频道是整库共用的；同库跑多套中继或并行的测试时用）。
	PostgresChannelPrefix string
}

// Open 建控制面。redis / postgres 连不上、表不在都在这里报错（拒绝启动）。
func Open(ctx context.Context, c Config) (control.Cluster, error) {
	switch c.Kind {
	case Memory, "":
		return control.NewMemory(control.MemoryOptions{
			Issuer: c.Options.Issuer, Mode: c.Options.Mode, JWKS: c.Options.JWKS,
			Now: c.Options.Now, Limits: c.Options.Limits, Revoked: c.Options.Revoked,
		}), nil
	case Redis:
		return redis.New(ctx, redis.Options{Options: c.Options, URL: c.RedisURL, Prefix: c.RedisPrefix})
	case Postgres:
		return postgres.New(ctx, postgres.Options{
			Options: c.Options, Pool: c.PostgresPool, URL: c.PostgresURL, Schema: c.PostgresSchema,
			ChannelPrefix: c.PostgresChannelPrefix,
		})
	}
	return nil, fmt.Errorf("--control 只能是 memory、redis 或 postgres：%s", c.Kind)
}
