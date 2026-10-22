package personal

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// StoreKind 是存储种类。
type StoreKind string

// 存储种类。file 只在迁移时读（Go 版不再写状态文件）。
const (
	StoreFile     StoreKind = "file"
	StoreSQLite   StoreKind = "sqlite"
	StorePostgres StoreKind = "postgres"
)

// 数据目录里的文件。
const (
	DatabaseFile   = "relay.db"
	PostgresMarker = "postgres.json"
	StateFile      = "state.json"
	RunLockFile    = "serve.pid"
)

// DatabaseOptions 是 PostgreSQL 的连接（口令不进日志与输出）。
type DatabaseOptions struct {
	URL    string
	Schema string
}

// SourceSeen 是源上下线的累计（只在内存，按批落盘）。
type SourceSeen struct {
	AtMs        int64
	CoreVersion string
}

// Store 是 personal 打开的存储：端口、签名钥、设置与外部改动通知（对应 store-types.ts 的 PersonalStore）。
type Store interface {
	Kind() StoreKind
	DataDir() string
	// Path 是存储的位置（relay.db，PostgreSQL 是 postgres:<schema>，不含连接串）。
	Path() string
	Ports() *store.Ports
	Keys() store.KeyStore
	// Tenant 是缓存的租户行（issuer 与设置，每个请求都可能读，同步）：开库时读一遍，
	// Watch 察觉改动与自己写过之后重读。
	Tenant() store.Tenant
	SetIssuer(ctx context.Context, issuer string) error
	// RotateKey 启用新签名钥，旧钥一小时后退役；答新 kid。
	RotateKey(ctx context.Context, nowMs int64) (string, error)
	// RecordSourcesSeen 记源的最近在线与版本（只往后推）。
	RecordSourcesSeen(ctx context.Context, batch map[string]SourceSeen) error
	Snapshot(ctx context.Context) (store.TenantSnapshot, error)
	// Revoked 是已撤销的源与链接 id。
	Revoked(ctx context.Context) (sources, links []string, err error)
	// Watch 别的进程（或节点）改了存储就回调 onChange（SQLite 看 data_version，PostgreSQL 看租户修订号，
	// 同时刷新 Tenant 缓存与签名钥缓存）。答停止函数；interval 为 0 取 1 秒。
	Watch(onChange func(), onError func(error), interval time.Duration) (stop func())
	Close() error
}
