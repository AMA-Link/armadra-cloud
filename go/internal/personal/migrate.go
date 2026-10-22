package personal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/statefile"
)

// 迁移只换存储、不改数据（对应 migrate.ts）：整份写入目标（一个事务），写完重新读出来与源的快照
// 逐字段比对，对不上就删掉新写的本地文件、原存储不动。成功后原文件改名为 `<文件>.migrated-<时间>`
// 留作回退；从 PostgreSQL 迁走时库里的数据原样保留（绝不清库），只把 postgres.json 标记改名。
//
// 与 Node 版的差别（有意）：
//   - Go 不再写状态文件，所以没有 --to file（UsageError）；state.json 只作为迁移的源；
//   - 读 state.json 不再取目录锁（Go 没有会写它的 passwd / rotate-key；serve 在跑已经拒绝）；
//   - 写进 PostgreSQL 的比对失败时，刚写进库的租户同样不删（不碰库里的数据），只是不写 postgres.json 标记
//     （Node 先写标记再撤）。
//   - 修订号：ImportTenant 把快照的修订号原样写进租户行，导入的各行不再加一，所以读回来的修订号与源相等，
//     和其余字段一样逐字段比对（与 Node 的 isDeepStrictEqual 一致）。

// MigrateOpts 是 MigrateStore 的选项。
type MigrateOpts struct {
	// Now 缺省取当前时间（用于回退文件名里的时间戳）。
	Now      time.Time
	Database *DatabaseOptions
}

// MigrateResult 是一次迁移的结果。
type MigrateResult struct {
	From StoreKind
	// Target 是新存储的位置（relay.db，或 postgres:<schema>）。
	Target string
	// Kept 是留下的回退文件。
	Kept string
}

func migrateStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15-04-05Z") }

func removeDatabase(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func markerPath(dataDir string) string { return filepath.Join(dataDir, PostgresMarker) }

// labelOf 是「迁移到 SQLite」「迁移到状态文件」：拉丁字母前留一个空格。
func labelOf(kind StoreKind) string {
	switch kind {
	case StoreFile:
		return "状态文件"
	case StoreSQLite:
		return " SQLite"
	default:
		return " PostgreSQL"
	}
}

// asStateError 把存储层的问题换成 StateError（CLI 的退出码由它定）。
func asStateError(err error) error {
	if err == nil {
		return nil
	}
	var se *StateError
	var ue *UsageError
	if errors.As(err, &se) || errors.As(err, &ue) {
		return err
	}
	var file *statefile.Error
	if errors.As(err, &file) {
		if file.Code == statefile.NotInitialized {
			return &StateError{Code: NotInitialized, Msg: file.Msg}
		}
		return &StateError{Code: Invalid, Msg: file.Msg}
	}
	var sErr *store.Error
	if errors.As(err, &sErr) {
		switch sErr.Kind {
		case store.KindNotFound:
			return &StateError{Code: NotInitialized, Msg: sErr.Msg}
		case store.KindExists:
			return &StateError{Code: Exists, Msg: sErr.Msg}
		default:
			return &StateError{Code: Invalid, Msg: sErr.Msg}
		}
	}
	return err
}

func refuseWhileServing(dataDir string) error {
	if pid, ok := RunningServe(dataDir); ok {
		return Usagef("serve 正在运行（进程 %d）：先停掉它再迁移；确认已停止就删掉 %s", pid, filepath.Join(dataDir, RunLockFile))
	}
	return nil
}

func migrateRequireDatabase(db *DatabaseOptions) (*DatabaseOptions, error) {
	if db == nil {
		return nil, Usagef("要给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
	}
	return db, nil
}

func postgresOptions(db *DatabaseOptions) postgres.Options {
	return postgres.Options{URL: db.URL, Schema: db.Schema, ApplicationName: "armadra-relay-personal"}
}

func postgresPath(db *DatabaseOptions) string {
	schema := db.Schema
	if schema == "" {
		schema = "public"
	}
	return "postgres:" + schema
}

// canonical 把快照换成可比较的通用值：字段按 JSON 名，数字保精度，顶层列表 nil 与空一样。
func canonical(snap store.TenantSnapshot) (any, error) {
	lists := func(v any) any {
		if v == nil || reflect.ValueOf(v).IsNil() {
			return []any{}
		}
		return v
	}
	data, err := json.Marshal(map[string]any{
		"tenant":        snap.Tenant,
		"accounts":      lists(snap.Accounts),
		"signingKeys":   lists(snap.SigningKeys),
		"sources":       lists(snap.Sources),
		"registrations": lists(snap.Registrations),
		"devices":       lists(snap.Devices),
		"links":         lists(snap.Links),
	})
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out any
	err = decoder.Decode(&out)
	return out, err
}

func sameSnapshot(a, b store.TenantSnapshot) bool {
	left, err := canonical(a)
	if err != nil {
		return false
	}
	right, err := canonical(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// migrateSource 是迁移的源：整份快照，成功后把它退下来（改名留作回退）。
type migrateSource struct {
	kind   StoreKind
	snap   store.TenantSnapshot
	retire func(now time.Time) (string, error)
}

func openMigrateSource(ctx context.Context, dataDir string, db *DatabaseOptions) (*migrateSource, error) {
	state, database, marker := filepath.Join(dataDir, StateFile), filepath.Join(dataDir, DatabaseFile), markerPath(dataDir)
	if !exists(state) && !exists(database) && !exists(marker) {
		return nil, Statef(NotInitialized, "没有可迁的存储：%s", state)
	}
	kind, _, err := ResolveKind(dataDir, "")
	if err != nil {
		return nil, asStateError(err)
	}
	switch kind {
	case StoreFile:
		snap, err := statefile.ReadSnapshot(dataDir)
		if err != nil {
			return nil, asStateError(err)
		}
		return &migrateSource{kind: kind, snap: snap, retire: func(now time.Time) (string, error) {
			kept := state + ".migrated-" + migrateStamp(now)
			return kept, os.Rename(state, kept)
		}}, nil
	case StoreSQLite:
		d, err := sqlite.Open(ctx, database, sqlite.Options{})
		if err != nil {
			return nil, asStateError(err)
		}
		// 先核对它是个人中转的库（只有 default 一个租户、一个账号），免得只迁了其中一个租户。
		if err := verify(ctx, kind, d); err != nil {
			_ = d.Close()
			return nil, err
		}
		snap, err := d.ExportTenant(ctx, store.DefaultTenant)
		_ = d.Close()
		if err != nil {
			return nil, exportError(err)
		}
		return &migrateSource{kind: kind, snap: snap, retire: func(now time.Time) (string, error) {
			kept := database + ".migrated-" + migrateStamp(now)
			if err := os.Rename(database, kept); err != nil {
				return kept, err
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if exists(database + suffix) {
					if err := os.Rename(database+suffix, kept+suffix); err != nil {
						return kept, err
					}
				}
			}
			return kept, nil
		}}, nil
	default:
		db, err := migrateRequireDatabase(db)
		if err != nil {
			return nil, err
		}
		d, err := postgres.Open(ctx, postgresOptions(db))
		if err != nil {
			return nil, asStateError(err)
		}
		if err := verify(ctx, kind, d); err != nil {
			_ = d.Close()
			return nil, err
		}
		snap, err := d.ExportTenant(ctx, store.DefaultTenant)
		_ = d.Close()
		if err != nil {
			return nil, exportError(err)
		}
		return &migrateSource{kind: kind, snap: snap, retire: func(now time.Time) (string, error) {
			// 库里的数据原样保留：只把标记改名，数据目录之后按新存储开。
			kept := marker + ".migrated-" + migrateStamp(now)
			return kept, os.Rename(marker, kept)
		}}, nil
	}
}

func exportError(err error) error {
	if errors.Is(err, store.ErrNoTenant) {
		return Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init")
	}
	return asStateError(err)
}

// verifySQLite 打开 path（create 时先把快照整份写进去）读回来与快照比对。
func verifySQLite(ctx context.Context, path string, create bool, snap store.TenantSnapshot, what string) error {
	d, err := sqlite.Open(ctx, path, sqlite.Options{Create: create})
	if err != nil {
		return asStateError(err)
	}
	defer func() { _ = d.Close() }()
	if create {
		if err := d.ImportTenant(ctx, store.DefaultTenant, snap); err != nil {
			return asStateError(err)
		}
	}
	got, err := d.ExportTenant(ctx, store.DefaultTenant)
	if err != nil {
		return exportError(err)
	}
	if !sameSnapshot(got, snap) {
		return Statef(Invalid, "迁移后读出的内容与%s不一致", what)
	}
	return nil
}

// writeSQLite 写成 relay.db：先写临时库，比对后整体改名，再按正式路径核对一次。
func writeSQLite(ctx context.Context, dataDir string, snap store.TenantSnapshot) (string, error) {
	database := filepath.Join(dataDir, DatabaseFile)
	temporary := fmt.Sprintf("%s.migrating-%d", database, os.Getpid())
	removeDatabase(temporary)
	renamed := false
	err := func() error {
		if err := verifySQLite(ctx, temporary, true, snap, "原存储"); err != nil {
			return err
		}
		// 关库时 WAL 已并入主文件：只剩主文件才能整体改名。
		if info, err := os.Stat(temporary + "-wal"); err == nil && info.Size() > 0 {
			return Statef(Invalid, "迁移后的数据库没有落盘完整")
		}
		_ = os.Remove(temporary + "-wal")
		_ = os.Remove(temporary + "-shm")
		if err := os.Rename(temporary, database); err != nil {
			return err
		}
		renamed = true
		return verifySQLite(ctx, database, false, snap, "原存储")
	}()
	if err != nil {
		if renamed {
			removeDatabase(database)
		}
		removeDatabase(temporary)
		return "", err
	}
	return database, nil
}

// writePostgres 写进 PostgreSQL（一个事务）；库里已有个人中转就拒绝；写成并读回一致才留下标记。
func writePostgres(ctx context.Context, dataDir string, snap store.TenantSnapshot, db *DatabaseOptions) (string, error) {
	d, err := postgres.Open(ctx, postgresOptions(db))
	if err != nil {
		return "", asStateError(err)
	}
	defer func() { _ = d.Close() }()
	tenants, err := d.ListTenants(ctx)
	if err != nil {
		return "", asStateError(err)
	}
	if len(tenants) > 1 || (len(tenants) == 1 && tenants[0] != store.DefaultTenant) {
		return "", Statef(Invalid, "数据库不是个人中转的库（租户不对），拒绝启动")
	}
	if len(tenants) == 1 {
		return "", Statef(Exists, "库里已经有个人中转")
	}
	if err := d.ImportTenant(ctx, store.DefaultTenant, snap); err != nil {
		return "", asStateError(err)
	}
	// 比对失败：刚写进库的租户不自动删（不碰库里的数据），不留标记，数据目录照旧用原存储。
	got, err := d.ExportTenant(ctx, store.DefaultTenant)
	if err != nil {
		return "", exportError(err)
	}
	if !sameSnapshot(got, snap) {
		return "", Statef(Invalid, "迁移后读出的内容与原存储不一致")
	}
	if err := writePostgresMarker(dataDir); err != nil {
		return "", err
	}
	return postgresPath(db), nil
}

// MigrateStore 换存储：答新存储的位置与留下的回退文件。
func MigrateStore(ctx context.Context, dataDir string, to StoreKind, o MigrateOpts) (MigrateResult, error) {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	if to == StoreFile {
		return MigrateResult{}, Usagef("Go 版不再写状态文件：用 --to sqlite")
	}
	if err := refuseWhileServing(dataDir); err != nil {
		return MigrateResult{}, err
	}
	if to == StoreSQLite && exists(filepath.Join(dataDir, DatabaseFile)) {
		return MigrateResult{}, Statef(Exists, "目标已存在：%s", filepath.Join(dataDir, DatabaseFile))
	}
	if to == StorePostgres && exists(markerPath(dataDir)) {
		return MigrateResult{}, Statef(Exists, "数据目录已经在用 PostgreSQL")
	}
	if to == StorePostgres {
		if _, err := migrateRequireDatabase(o.Database); err != nil {
			return MigrateResult{}, err
		}
	}
	source, err := openMigrateSource(ctx, dataDir, o.Database)
	if err != nil {
		return MigrateResult{}, err
	}
	if source.kind == to {
		return MigrateResult{}, Statef(Exists, "已经在用%s", labelOf(to))
	}
	var target string
	if to == StoreSQLite {
		target, err = writeSQLite(ctx, dataDir, source.snap)
	} else {
		target, err = writePostgres(ctx, dataDir, source.snap, o.Database)
	}
	if err != nil {
		return MigrateResult{}, err
	}
	kept, err := source.retire(now)
	if err != nil {
		return MigrateResult{}, err
	}
	return MigrateResult{From: source.kind, Target: target, Kept: kept}, nil
}

// RunMigrate 是 `personal migrate`：迁移并打印结果。
func RunMigrate(ctx context.Context, dataDir string, to StoreKind, db *DatabaseOptions, out Print, now time.Time) error {
	result, err := MigrateStore(ctx, dataDir, to, MigrateOpts{Now: now, Database: db})
	if err != nil {
		return err
	}
	out(fmt.Sprintf("已迁移到%s：%s", labelOf(to), result.Target))
	if result.From == StorePostgres {
		out(fmt.Sprintf("PostgreSQL 里的数据原样保留；原标记留作回退：%s", result.Kept))
	} else {
		what := "数据库"
		if result.From == StoreFile {
			what = "状态文件"
		}
		out(fmt.Sprintf("原%s留作回退：%s", what, result.Kept))
	}
	return nil
}

// copyExclusive 把 from 拷到 to（不覆盖，0600）。
func copyExclusive(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(to)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(to)
		return err
	}
	return os.Chmod(to, 0o600)
}

// RunBackup 是 `personal backup`：备份到 outFile（不覆盖已有文件）。
func RunBackup(ctx context.Context, dataDir, outFile string, db *DatabaseOptions, out Print) error {
	if exists(outFile) {
		return Usagef("备份目标已存在：%s", outFile)
	}
	kind, _, err := ResolveKind(dataDir, "")
	if err != nil {
		return asStateError(err)
	}
	switch kind {
	case StorePostgres:
		return Usagef("PostgreSQL 存储用库自己的备份（pg_dump 或托管服务的快照）")
	case StoreFile:
		// 老部署的状态文件：直接拷一份（不打开存储）。
		state := filepath.Join(dataDir, StateFile)
		if !exists(state) {
			return Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init")
		}
		if err := copyExclusive(state, outFile); err != nil {
			return err
		}
	default:
		database := filepath.Join(dataDir, DatabaseFile)
		if !exists(database) {
			return Statef(NotInitialized, "还没有初始化：先运行 armadra-relay personal init")
		}
		if err := backupSQLite(ctx, database, outFile); err != nil {
			return err
		}
	}
	out(fmt.Sprintf("已备份：%s", outFile))
	return nil
}

func backupSQLite(ctx context.Context, database, outFile string) error {
	d, err := sqlite.Open(ctx, database, sqlite.Options{})
	if err != nil {
		return asStateError(err)
	}
	err = d.Backup(ctx, outFile)
	_ = d.Close()
	if err != nil {
		return asStateError(err)
	}
	// 备份要能原样打开。
	check, err := sqlite.Open(ctx, outFile, sqlite.Options{})
	if err != nil {
		removeDatabase(outFile)
		return Statef(Invalid, "备份读不回来：%s", outFile)
	}
	_ = check.Close()
	return os.Chmod(outFile, 0o600)
}
