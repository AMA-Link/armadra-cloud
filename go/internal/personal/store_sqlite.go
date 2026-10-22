package personal

import (
	"context"
	"os"
	"path/filepath"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
)

func wrapSQLite(ctx context.Context, dataDir string, db *sqlite.DB) (Store, error) {
	version, err := db.DataVersion(ctx)
	if err != nil {
		return nil, mapStoreError(StoreSQLite, err)
	}
	s := &dbStore{kind: StoreSQLite, dataDir: dataDir, path: db.Path(), b: db}
	s.poll = func(ctx context.Context) (bool, error) {
		next, err := db.DataVersion(ctx)
		if err != nil {
			return false, err
		}
		if next == version {
			return false, nil
		}
		version = next
		return true, nil
	}
	if _, err := newDBStore(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// openSQLite 打开 relay.db 并核对它是一个个人中转的库。
func openSQLite(ctx context.Context, dataDir string) (Store, error) {
	db, err := sqlite.Open(ctx, filepath.Join(dataDir, DatabaseFile), sqlite.Options{})
	if err != nil {
		return nil, mapStoreError(StoreSQLite, err)
	}
	if err := verify(ctx, StoreSQLite, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	st, err := wrapSQLite(ctx, dataDir, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

// createSQLite 新建 relay.db 并在一个事务里写入整份快照；没写成的删掉，不留半个库。
func createSQLite(ctx context.Context, dataDir string, snap store.TenantSnapshot) (Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, Statef(Invalid, "数据目录建不出来：%s", dataDir)
	}
	path := filepath.Join(dataDir, DatabaseFile)
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
	if err != nil {
		return nil, mapStoreError(StoreSQLite, err)
	}
	fail := func(err error) (Store, error) {
		_ = db.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
		return nil, mapStoreError(StoreSQLite, err)
	}
	if err := db.ImportTenant(ctx, store.DefaultTenant, snap); err != nil {
		return fail(err)
	}
	st, err := wrapSQLite(ctx, dataDir, db)
	if err != nil {
		return fail(err)
	}
	return st, nil
}
