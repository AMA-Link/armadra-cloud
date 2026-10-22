package personal

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/factory"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// 启动时连 Redis / PostgreSQL 最多等多久；/ready 里每项探针的上限由 obs 定（2 秒）。
const connectTimeout = 5 * time.Second

// Cluster 是装配好的控制面：控制面本身（Plane + Command + Close）与 /ready 里多出来的依赖探针
// （多节点时探外部服务：redis、database）。
type Cluster struct {
	control.Cluster
	Probes map[string]obs.Probe

	closeExtra func()
}

// Close 关控制面，再关探针自己持有的连接。
func (c *Cluster) Close() error {
	err := c.Cluster.Close()
	if c.closeExtra != nil {
		c.closeExtra()
	}
	return err
}

// ClusterInput 是创建控制面的输入。
type ClusterInput struct {
	Kind     factory.Kind
	NodeID   string
	RedisURL string
	Database *DatabaseOptions
	// Pool 是存储为 PostgreSQL 时存储的连接池：--control postgres 共用它（控制面 Close 不关借来的池）。
	Pool    *pgxpool.Pool
	Options cluster.Options
}

// poolOwner 是自己持有 PostgreSQL 连接池的存储。
type poolOwner interface{ PostgresPool() *pgxpool.Pool }

// openCluster 按 --control 建控制面：memory 是内核的内存实现；redis / postgres 连不上、表不在都在这里拒绝启动。
// 错误只报种类与驱动的错误码，不带地址与口令。
func openCluster(ctx context.Context, in ClusterInput) (*Cluster, error) {
	config := factory.Config{Kind: in.Kind, Options: in.Options}
	config.Options.NodeID = in.NodeID
	switch in.Kind {
	case factory.Redis:
		config.RedisURL = in.RedisURL
	case factory.Postgres:
		if in.Database == nil {
			return nil, Usagef("--control postgres 要给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
		}
		if in.Pool != nil {
			config.PostgresPool = in.Pool
		} else {
			config.PostgresURL, config.PostgresSchema = in.Database.URL, in.Database.Schema
		}
	}
	opened, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	plane, err := factory.Open(opened, config)
	if err != nil {
		return nil, Statef(Invalid, "%s", clusterFailure(in.Kind, err))
	}
	handle := &Cluster{Cluster: plane, Probes: map[string]obs.Probe{}}
	switch in.Kind {
	case factory.Redis:
		client, err := redisProbeClient(in.RedisURL)
		if err != nil {
			_ = plane.Close()
			return nil, Statef(Invalid, "%s", clusterFailure(in.Kind, err))
		}
		handle.Probes["redis"] = func(ctx context.Context) error { return client.Ping(ctx).Err() }
		handle.closeExtra = func() { _ = client.Close() }
	case factory.Postgres:
		if in.Pool != nil {
			handle.Probes["database"] = func(ctx context.Context) error { return in.Pool.Ping(ctx) }
		} else {
			url, schema := in.Database.URL, in.Database.Schema
			handle.Probes["database"] = func(ctx context.Context) error { return pingPostgres(ctx, url, schema) }
		}
	}
	return handle, nil
}

// clusterFailure 把控制面的错误换成不带连接串的短句（G3a 的实现保证错误里没有地址与口令）。
func clusterFailure(kind factory.Kind, err error) string {
	switch kind {
	case factory.Redis:
		return "Redis 控制面起不来：" + err.Error()
	case factory.Postgres:
		return "PostgreSQL 控制面起不来：" + err.Error()
	}
	return err.Error()
}

func redisProbeClient(url string) (*goredis.Client, error) {
	options, err := goredis.ParseURL(url)
	if err != nil {
		return nil, errors.New("--redis-url 不是合法的地址")
	}
	return goredis.NewClient(options), nil
}

// pingPostgres 单开一条连接探一下 SELECT 1（--control postgres 但存储不是 PostgreSQL 时）。
func pingPostgres(ctx context.Context, url, schema string) error {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("连接串不合格式")
	}
	if schema != "" {
		config.RuntimeParams["search_path"] = schema
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return errors.New("连不上")
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, "SELECT 1")
	return err
}
