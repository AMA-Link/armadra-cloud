package personal

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

const postgresPoolMax = 5

// revTracker 认「自己的写」：写事务提交时报来新的修订号（OnCommit），轮询时逐个对账——
// 从上次见到的修订号到现在的每一号都是自己提交的才算没有外部改动；不从「+1」去猜。
type revTracker struct {
	mu    sync.Mutex
	known int64
	own   map[int64]struct{}
}

func newRevTracker() *revTracker { return &revTracker{own: map[int64]struct{}{}} }

// commit 记下一次自己提交的修订号。
func (t *revTracker) commit(_ string, revision int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if revision > t.known {
		t.own[revision] = struct{}{}
	}
}

// reset 定下起点（开库时读到的修订号）。
func (t *revTracker) reset(revision int64) {
	t.mu.Lock()
	t.known = revision
	t.mu.Unlock()
}

// advance 推进到 now，答 (known, now] 里有没有不是自己提交的修订。
func (t *revTracker) advance(now int64) (external bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now <= t.known {
		return false
	}
	for r := t.known + 1; r <= now; r++ {
		if _, ok := t.own[r]; ok {
			delete(t.own, r)
		} else {
			external = true
		}
	}
	t.known = now
	return external
}

func connectPostgres(ctx context.Context, db DatabaseOptions, tracker *revTracker) (*postgres.DB, error) {
	pg, err := postgres.Open(ctx, postgres.Options{
		URL: db.URL, Schema: db.Schema, MaxConns: postgresPoolMax, ApplicationName: "armadra-relay-personal", OnError: db.OnError, OnCommit: tracker.commit,
	})
	if err != nil {
		return nil, mapStoreError(StorePostgres, err)
	}
	return pg, nil
}

// writePostgresMarker 在数据目录留 postgres.json（0600，不含连接串）：之后不写 --store 也按 PostgreSQL 开。
func writePostgresMarker(dataDir string) error {
	path := filepath.Join(dataDir, PostgresMarker)
	if exists(path) {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Statef(Invalid, "数据目录建不出来：%s", dataDir)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Statef(Invalid, "标记文件写不出来：%s", PostgresMarker)
	}
	_, err = file.WriteString("{\"store\":\"postgres\"}\n")
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Statef(Invalid, "标记文件写不出来：%s", PostgresMarker)
	}
	return os.Chmod(path, 0o600)
}

// checkTenants：库里只能有个人中转的那一个租户；没有它答 false。
func checkTenants(ctx context.Context, pg *postgres.DB) (bool, error) {
	tenants, err := pg.ListTenants(ctx)
	if err != nil {
		return false, mapStoreError(StorePostgres, err)
	}
	if len(tenants) == 0 {
		return false, nil
	}
	if len(tenants) != 1 || tenants[0] != store.DefaultTenant {
		return false, Statef(Invalid, "数据库不是个人中转的库（租户不对），拒绝启动")
	}
	return true, nil
}

func wrapPostgres(ctx context.Context, dataDir string, pg *postgres.DB, schema string, tracker *revTracker) (Store, error) {
	known, _, err := pg.Revision(ctx, store.DefaultTenant)
	if err != nil {
		return nil, mapStoreError(StorePostgres, err)
	}
	tracker.reset(known)
	if schema == "" {
		schema = "public"
	}
	s := &dbStore{kind: StorePostgres, dataDir: dataDir, path: "postgres:" + schema, b: pg}
	s.refreshKeys = func(ctx context.Context) error { return pg.RefreshKeys(ctx, store.DefaultTenant) }
	s.poll = func(ctx context.Context) (bool, error) {
		now, ok, err := pg.Revision(ctx, store.DefaultTenant)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		return tracker.advance(now), nil
	}
	if _, err := newDBStore(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// openPostgres 打开库并核对它是一个个人中转；顺手留下标记。
func openPostgres(ctx context.Context, dataDir string, db DatabaseOptions) (Store, error) {
	tracker := newRevTracker()
	pg, err := connectPostgres(ctx, db, tracker)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (Store, error) {
		_ = pg.Close()
		return nil, err
	}
	has, err := checkTenants(ctx, pg)
	if err != nil {
		return fail(err)
	}
	if !has {
		return fail(Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init --store postgres"))
	}
	if err := verify(ctx, StorePostgres, pg); err != nil {
		return fail(err)
	}
	st, err := wrapPostgres(ctx, dataDir, pg, db.Schema, tracker)
	if err != nil {
		return fail(err)
	}
	if err := writePostgresMarker(dataDir); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// createPostgres 在一个事务里写入整份快照；库里已有个人中转就拒绝。
func createPostgres(ctx context.Context, dataDir string, snap store.TenantSnapshot, db DatabaseOptions) (Store, error) {
	tracker := newRevTracker()
	pg, err := connectPostgres(ctx, db, tracker)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (Store, error) {
		_ = pg.Close()
		return nil, err
	}
	has, err := checkTenants(ctx, pg)
	if err != nil {
		return fail(err)
	}
	if has {
		return fail(Statef(Exists, "已经初始化过：PostgreSQL 里已有个人中转"))
	}
	if err := pg.ImportTenant(ctx, store.DefaultTenant, snap); err != nil {
		return fail(mapStoreError(StorePostgres, err))
	}
	st, err := wrapPostgres(ctx, dataDir, pg, db.Schema, tracker)
	if err != nil {
		return fail(err)
	}
	if err := writePostgresMarker(dataDir); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}
