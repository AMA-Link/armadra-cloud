package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// storeLabels 是存储的显示名（status / doctor / 日志）。
var storeLabels = map[StoreKind]string{
	StoreFile:     "状态文件",
	StoreSQLite:   "SQLite",
	StorePostgres: "PostgreSQL",
}

// StoreLabel 是存储种类的显示名。
func StoreLabel(kind StoreKind) string {
	if label, ok := storeLabels[kind]; ok {
		return label
	}
	return string(kind)
}

var kindFiles = map[StoreKind]string{
	StoreFile:     StateFile,
	StoreSQLite:   DatabaseFile,
	StorePostgres: PostgresMarker,
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// present 是数据目录里已有哪些存储。
func present(dataDir string) []StoreKind {
	var kinds []StoreKind
	for _, kind := range []StoreKind{StoreSQLite, StoreFile, StorePostgres} {
		if exists(filepath.Join(dataDir, kindFiles[kind])) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// ResolveKind 按数据目录里已有的文件与 --store 定存储（规则同 store.ts 的 resolveStoreKind）；
// legacy = 只有 state.json（老部署）。requested 为空 = 没给 --store。
func ResolveKind(dataDir string, requested StoreKind) (kind StoreKind, legacy bool, err error) {
	kinds := present(dataDir)
	if len(kinds) > 1 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = kindFiles[k]
		}
		return "", false, Statef(Invalid, "数据目录里同时有 %s，拒绝启动：只留下要用的那个", strings.Join(names, " 与 "))
	}
	var existing StoreKind
	if len(kinds) == 1 {
		existing = kinds[0]
	}
	if requested != "" && existing != "" && requested != existing {
		switch {
		case requested == StoreFile && existing == StoreSQLite:
			return "", false, Statef(Invalid, "数据目录里是 SQLite 库：去掉 --store file，或先运行 armadra-relay personal migrate --to file")
		case requested == StoreSQLite && existing == StoreFile:
			return "", false, Statef(Invalid, "数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite")
		}
		return "", false, Statef(Invalid, "数据目录里用的是 %s：去掉 --store %s，或先运行 armadra-relay personal migrate --to %s",
			StoreLabel(existing), requested, requested)
	}
	if requested != "" {
		return requested, false, nil
	}
	if existing != "" {
		return existing, existing == StoreFile, nil
	}
	return StoreSQLite, false, nil
}

// StoreExists 数据目录里已经有存储了吗（任一种；postgres 只看标记，库里有没有由调用方再问）。
func StoreExists(dataDir string) bool { return len(present(dataDir)) > 0 }

func requireDatabase(db *DatabaseOptions) (*DatabaseOptions, error) {
	if db == nil || db.URL == "" {
		return nil, Usagef("数据目录用的是 PostgreSQL：给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
	}
	return db, nil
}

// OpenStore 打开已有的存储；没有 → NotInitialized，坏的 / 不认识的 → Invalid。
// state.json 的数据目录拒绝并提示 personal migrate --to sqlite（读它是 migrate 的事）。
func OpenStore(ctx context.Context, dataDir string, requested StoreKind, db *DatabaseOptions) (Store, error) {
	kind, _, err := ResolveKind(dataDir, requested)
	if err != nil {
		return nil, err
	}
	switch kind {
	case StoreFile:
		return nil, Statef(Invalid, "数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite")
	case StoreSQLite:
		return openSQLite(ctx, dataDir)
	default:
		opts, err := requireDatabase(db)
		if err != nil {
			return nil, err
		}
		return openPostgres(ctx, dataDir, *opts)
	}
}

// CreateStore 首次写入整份快照（init、自动初始化、migrate 目标）；已有任一种存储答 Exists。
func CreateStore(ctx context.Context, dataDir string, kind StoreKind, snap store.TenantSnapshot, db *DatabaseOptions) (Store, error) {
	if StoreExists(dataDir) {
		return nil, Statef(Exists, "已经初始化过：数据目录里已有存储")
	}
	if kind == StoreFile {
		return nil, Usagef("Go 版不再写状态文件：用 --store sqlite")
	}
	if len(snap.Accounts) != 1 {
		return nil, Statef(Invalid, "个人中转只有一个账号，快照里有 %d 个", len(snap.Accounts))
	}
	switch kind {
	case StoreSQLite, "":
		return createSQLite(ctx, dataDir, snap)
	case StorePostgres:
		opts, err := requireDatabase(db)
		if err != nil {
			return nil, err
		}
		return createPostgres(ctx, dataDir, snap, *opts)
	}
	return nil, Usagef("不认识的存储：%s", kind)
}

// PostgresStoreExists 库里已经有个人中转的租户了吗（serve 自动初始化前问）；连不上照样拒绝。
func PostgresStoreExists(ctx context.Context, db DatabaseOptions) (bool, error) {
	pg, err := connectPostgres(ctx, db)
	if err != nil {
		return false, err
	}
	defer func() { _ = pg.Close() }()
	return checkTenants(ctx, pg)
}

// WithStore 打开存储跑一下就关。
func WithStore(ctx context.Context, dataDir string, db *DatabaseOptions, fn func(Store) error) error {
	st, err := OpenStore(ctx, dataDir, "", db)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(st)
}

// ------------------------------------------------------------ 两种库共用的实现

// backend 是两种库都有的操作（store.Backend 之外的租户读写与撤销清单）。
type backend interface {
	store.Backend
	ListTenants(ctx context.Context) ([]string, error)
	ReadTenant(ctx context.Context, tenantID string) (*store.Tenant, error)
	UpdateTenant(ctx context.Context, tenantID string, fn func(draft *store.Tenant) error) error
	RevokedIDs(ctx context.Context, tenantID string) (sources, links []string, err error)
}

// dbStore 是 Store 在 backend 上的实现；SQLite 与 PostgreSQL 只在察觉改动的办法上不同。
type dbStore struct {
	kind    StoreKind
	dataDir string
	path    string
	b       backend
	ports   *store.Ports
	keys    store.KeyStore
	tenant  atomic.Pointer[store.Tenant]
	// poll 看库有没有被别人改过（SQLite：data_version；PostgreSQL：租户修订号）。
	poll func(ctx context.Context) (changed bool, err error)
	// refreshKeys 重读签名钥缓存（PostgreSQL）；nil = 不缓存。
	refreshKeys func(ctx context.Context) error
	// ownWrite 在自己写过之后调（PostgreSQL 记下接上的修订号，免得把自己的写当成别人的）。
	ownWrite  func(ctx context.Context)
	closeOnce sync.Once
	closeErr  error
}

// PostgresPool 是存储为 PostgreSQL 时的连接池（--control postgres 共用）；其余答 nil。
func (s *dbStore) PostgresPool() *pgxpool.Pool {
	if pg, ok := s.b.(*postgres.DB); ok {
		return pg.Pool()
	}
	return nil
}

func (s *dbStore) Kind() StoreKind      { return s.kind }
func (s *dbStore) DataDir() string      { return s.dataDir }
func (s *dbStore) Path() string         { return s.path }
func (s *dbStore) Ports() *store.Ports  { return s.ports }
func (s *dbStore) Keys() store.KeyStore { return s.keys }
func (s *dbStore) Tenant() store.Tenant { return *s.tenant.Load() }
func (s *dbStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.b.Close() })
	return s.closeErr
}
func (s *dbStore) wrapErr(err error) error { return mapStoreError(s.kind, err) }

// reload 重读租户行与（PostgreSQL 的）签名钥缓存。
func (s *dbStore) reload(ctx context.Context) error {
	row, err := s.b.ReadTenant(ctx, store.DefaultTenant)
	if err != nil {
		return s.wrapErr(err)
	}
	if row == nil {
		return Statef(Invalid, "数据库里没有个人中转的租户")
	}
	if s.refreshKeys != nil {
		if err := s.refreshKeys(ctx); err != nil {
			return s.wrapErr(err)
		}
	}
	s.tenant.Store(row)
	return nil
}

func (s *dbStore) afterWrite(ctx context.Context) error {
	if s.ownWrite != nil {
		s.ownWrite(ctx)
	}
	return s.reload(ctx)
}

func (s *dbStore) SetIssuer(ctx context.Context, issuer string) error {
	err := s.b.UpdateTenant(ctx, store.DefaultTenant, func(draft *store.Tenant) error {
		draft.Issuer = issuer
		return nil
	})
	if err != nil {
		return s.wrapErr(err)
	}
	return s.afterWrite(ctx)
}

func (s *dbStore) RotateKey(ctx context.Context, nowMs int64) (string, error) {
	kid, err := s.b.RotateKey(ctx, store.DefaultTenant, nowMs)
	if err != nil {
		return "", s.wrapErr(err)
	}
	if err := s.afterWrite(ctx); err != nil {
		return "", err
	}
	return kid, nil
}

// RecordSourcesSeen 只往后推最近在线、有版本才改版本；没变的源不写。
func (s *dbStore) RecordSourcesSeen(ctx context.Context, batch map[string]SourceSeen) error {
	for id, seen := range batch {
		err := s.ports.Sources.Update(ctx, store.DefaultTenant, id, func(draft *store.SourceRecord) error {
			if draft == nil {
				return nil
			}
			applySeen(draft, seen)
			return nil
		})
		if err != nil {
			return s.wrapErr(err)
		}
	}
	if s.ownWrite != nil && len(batch) > 0 {
		s.ownWrite(ctx)
	}
	return nil
}

// applySeen 按累计的最近在线改一条源记录（只往后推）。
func applySeen(source *store.SourceRecord, seen SourceSeen) {
	if source.LastSeenAtMs == nil || *source.LastSeenAtMs < seen.AtMs {
		at := seen.AtMs
		source.LastSeenAtMs = &at
	}
	if seen.CoreVersion != "" {
		source.CoreVersion = seen.CoreVersion
	}
}

func (s *dbStore) Snapshot(ctx context.Context) (store.TenantSnapshot, error) {
	snap, err := s.b.ExportTenant(ctx, store.DefaultTenant)
	if err != nil {
		return store.TenantSnapshot{}, s.wrapErr(err)
	}
	return snap, nil
}

func (s *dbStore) Revoked(ctx context.Context) (sources, links []string, err error) {
	sources, links, err = s.b.RevokedIDs(ctx, store.DefaultTenant)
	return sources, links, s.wrapErr(err)
}

func (s *dbStore) Watch(onChange func(), onError func(error), interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			changed, err := s.poll(ctx)
			if err == nil && changed {
				err = s.reload(ctx)
			}
			if err != nil {
				if ctx.Err() == nil && onError != nil {
					onError(s.wrapErr(err))
				}
				continue
			}
			if changed && ctx.Err() == nil && onChange != nil {
				onChange()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// verify 核对库是个人中转的库：只有 default 一个租户、一个账号。
func verify(ctx context.Context, kind StoreKind, b backend) error {
	tenants, err := b.ListTenants(ctx)
	if err != nil {
		return mapStoreError(kind, err)
	}
	if len(tenants) != 1 || tenants[0] != store.DefaultTenant {
		return Statef(Invalid, "数据库不是个人中转的库（租户不对），拒绝启动")
	}
	snap, err := b.ExportTenant(ctx, store.DefaultTenant)
	if err != nil {
		return mapStoreError(kind, err)
	}
	if len(snap.Accounts) != 1 {
		return Statef(Invalid, "个人中转只有一个账号，库里有 %d 个，拒绝启动", len(snap.Accounts))
	}
	return nil
}

// mapStoreError 把 store.Error 换成 StateError；别的错误原样。
func mapStoreError(kind StoreKind, err error) error {
	if err == nil {
		return nil
	}
	var se *store.Error
	if !errors.As(err, &se) {
		if errors.Is(err, store.ErrNoTenant) {
			return Statef(Invalid, "数据库里没有个人中转的租户")
		}
		return err
	}
	switch se.Kind {
	case store.KindNotFound:
		if kind == StorePostgres {
			return Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init --store postgres")
		}
		return Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init")
	case store.KindExists:
		if kind == StorePostgres {
			return Statef(Exists, "已经初始化过：PostgreSQL 里已有个人中转")
		}
		return Statef(Exists, "已经初始化过：数据库已存在")
	}
	return Statef(Invalid, "%s", se.Msg)
}

// newDBStore 装配并读第一遍租户行。
func newDBStore(ctx context.Context, s *dbStore) (*dbStore, error) {
	s.ports = s.b.Ports()
	s.keys = s.b.Keys(store.DefaultTenant)
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
