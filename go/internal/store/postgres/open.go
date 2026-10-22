// Package postgres 是控制核存储端口的 PostgreSQL 实现（pgx/v5），与 packages/control-store-postgres
// 读写同一套表（cloud 与中继 --store postgres 共用）。
//
// 写都是一个事务：先锁租户行（SELECT … FOR UPDATE，同一租户的写串行）→ 读行 → 在副本上跑 fn →
// 变了才写（并给租户修订加一）；fn 返回错误就回滚。连不上、库不认识、账本对不上、库比程序新
// ——一律拒绝启动（store.Error），绝不自动清库或重建。
//
// 连接串里有口令：错误消息、日志都不带它（只报驱动的错误码）。
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations 是随包的迁移（拷自 packages/control-store-postgres/migrations）。
func Migrations() ([]store.Migration, error) {
	return store.LoadMigrations(migrationFS, "migrations")
}

// Options 是打开库的选项。
type Options struct {
	// URL 是 postgres://…（含口令，不进日志与错误）。
	URL string
	// Schema 是用哪个 schema（search_path）；缺省是连接的缺省 schema（通常 public）。
	Schema string
	// MaxConns 是连接池上限；缺省 10。
	MaxConns int32
	// ApplicationName 在 pg_stat_activity 里看得到。
	ApplicationName string
	// Migrations 缺省用随包的。
	Migrations []store.Migration
	// KeyRefreshGap 是验签查不到 kid 时重读签名钥的最短间隔；缺省 5 秒。
	KeyRefreshGap time.Duration
	// Now 缺省 time.Now；账本的 applied_at_ms 用它。
	Now func() int64
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// DB 是一个打开的库（连接池）。
type DB struct {
	pool       *pgxpool.Pool
	schema     string
	refreshGap time.Duration

	keysMu sync.Mutex
	keys   map[string]*keyCache
}

var (
	_ store.Backend           = (*DB)(nil)
	_ store.KeyCacheRefresher = (*DB)(nil)
)

// Schema 是打开时指定的 schema（缺省为空串）。
func (d *DB) Schema() string { return d.schema }

// unreachable 把驱动错误换成不带连接串的短句，只报错误码。
func unreachable(err error) *store.Error {
	code := "unknown"
	var pgErr *pgconn.PgError
	var connect *pgconn.ConnectError
	switch {
	case errors.As(err, &pgErr):
		code = pgErr.Code
	case errors.As(err, &connect):
		code = "connect"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	}
	return store.Errorf(store.KindUnreachable, "PostgreSQL 连不上：%s", code)
}

func checkSchema(schema string) error {
	if schema != "" && !schemaName.MatchString(schema) {
		return store.Errorf(store.KindUnknown, "schema 名只能是小写字母、数字与下划线：%s", schema)
	}
	return nil
}

func connect(ctx context.Context, options Options) (*pgxpool.Pool, error) {
	if err := checkSchema(options.Schema); err != nil {
		return nil, err
	}
	config, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		// 解析错误可能带连接串片段：不转述。
		return nil, store.Errorf(store.KindUnreachable, "PostgreSQL 连接串不合格式")
	}
	config.MaxConns = 10
	if options.MaxConns > 0 {
		config.MaxConns = options.MaxConns
	}
	if options.Schema != "" {
		config.ConnConfig.RuntimeParams["search_path"] = options.Schema
	}
	if options.ApplicationName != "" {
		config.ConnConfig.RuntimeParams["application_name"] = options.ApplicationName
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, unreachable(err)
	}
	return pool, nil
}

// Open 建连接池 → 探活 →（给了 schema 就建它）→ 迁移到最新。
func Open(ctx context.Context, options Options) (*DB, error) {
	migrations := options.Migrations
	if migrations == nil {
		var err error
		if migrations, err = Migrations(); err != nil {
			return nil, store.Errorf(store.KindUnknown, "迁移文件打包有误：%v，拒绝执行", err)
		}
	}
	now := options.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	pool, err := connect(ctx, options)
	if err != nil {
		return nil, err
	}
	gap := options.KeyRefreshGap
	if gap <= 0 {
		gap = refreshOnMissGap
	}
	db := &DB{pool: pool, schema: options.Schema, refreshGap: gap, keys: map[string]*keyCache{}}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, unreachable(err)
	}
	if options.Schema != "" {
		// 显式给了 schema 时先建它（空 schema，不动已有的任何东西）；之后的迁移照样对账。
		if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", options.Schema)); err != nil {
			pool.Close()
			return nil, unreachable(err)
		}
	}
	if _, err := db.migrate(ctx, migrations, now); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

// Close 关连接池。
func (d *DB) Close() error {
	d.pool.Close()
	return nil
}

// Pool 给集成测试与运维探针用（读自己的 schema 里有哪些表之类）。
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// tx 开一个事务，fn 返回错误就回滚并原样返回。
func (d *DB) tx(ctx context.Context, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

// write 是一个租户的写事务：锁租户行后跑 fn；fn 答 changed 为真时修订加一。
func (d *DB) write(ctx context.Context, tenantID string, fn func(tx pgx.Tx) (changed bool, err error)) error {
	return d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM tenants WHERE tenant_id = $1 FOR UPDATE", tenantID); err != nil {
			return err
		}
		changed, err := fn(tx)
		if err != nil {
			return err
		}
		if changed {
			return bumpRevision(ctx, tx, tenantID)
		}
		return nil
	})
}

// bumpRevision：租户的修订加一（每个改了东西的写事务一次）。
func bumpRevision(ctx context.Context, q querier, tenantID string) error {
	_, err := q.Exec(ctx, "UPDATE tenants SET revision = revision + 1 WHERE tenant_id = $1", tenantID)
	return err
}
