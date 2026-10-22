package personal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/statefile"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

var migrateNow = time.Date(2026, 10, 23, 8, 30, 15, 0, time.UTC)

const migrateStampText = "2026-10-23T08-30-15Z"

func compatFile(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "go", "testdata", "compat", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compatPassword(t testing.TB) string {
	t.Helper()
	var expected struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(compatFile(t, "expected.json"), &expected); err != nil {
		t.Fatal(err)
	}
	return expected.Password
}

// legacyDataDir 是只有 state.json（夹具）的老部署数据目录。
func legacyDataDir(t testing.TB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), compatFile(t, "state.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixtureSnapshot(t testing.TB, dir string) store.TenantSnapshot {
	t.Helper()
	snap, err := statefile.ReadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func names(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func stateCode(t testing.TB, err error) StateCode {
	t.Helper()
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("不是 StateError：%v", err)
	}
	return se.Code
}

func TestMigrateStateFileToSQLite(t *testing.T) {
	ctx := context.Background()
	dir := legacyDataDir(t)
	original, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureSnapshot(t, dir)

	result, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{Now: migrateNow})
	if err != nil {
		t.Fatal(err)
	}
	if result.From != StoreFile || result.Target != filepath.Join(dir, DatabaseFile) {
		t.Fatalf("结果不对：%+v", result)
	}
	if result.Kept != filepath.Join(dir, StateFile+".migrated-"+migrateStampText) {
		t.Fatalf("回退文件名不对：%s", result.Kept)
	}
	kept, err := os.ReadFile(result.Kept)
	if err != nil || string(kept) != string(original) {
		t.Fatalf("回退文件应与原文件一致：%v", err)
	}
	if got := names(t, dir); strings.Join(got, ",") != DatabaseFile+","+StateFile+".migrated-"+migrateStampText {
		t.Fatalf("数据目录里的文件不对：%v", got)
	}
	if info, err := os.Stat(result.Target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("relay.db 应为 0600：%v %v", info, err)
	}

	// 迁完 serve 用的存储直接能开，内容（含修订号 7）与状态文件一致。
	s, err := OpenStore(ctx, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Kind() != StoreSQLite {
		t.Fatalf("存储种类不对：%s", s.Kind())
	}
	got, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSnapshot(got, want) || got.Tenant.Revision != 7 {
		t.Fatalf("迁移后的快照与状态文件不一致（修订号 %d）", got.Tenant.Revision)
	}
	if s.Tenant().Issuer != want.Tenant.Issuer {
		t.Fatalf("租户缓存不对：%+v", s.Tenant())
	}
}

func TestRunMigrateOutput(t *testing.T) {
	dir := legacyDataDir(t)
	var lines []string
	if err := RunMigrate(context.Background(), dir, StoreSQLite, nil, func(l string) { lines = append(lines, l) }, migrateNow); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"已迁移到 SQLite：" + filepath.Join(dir, DatabaseFile),
		"原状态文件留作回退：" + filepath.Join(dir, StateFile+".migrated-"+migrateStampText),
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("输出不对：%q", lines)
	}
}

func TestMigrateRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("目标已存在", func(t *testing.T) {
		dir := legacyDataDir(t)
		if err := os.WriteFile(filepath.Join(dir, DatabaseFile), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{})
		if stateCode(t, err) != Exists || err.Error() != "目标已存在："+filepath.Join(dir, DatabaseFile) {
			t.Fatalf("应拒绝目标已存在：%v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, StateFile)); err != nil {
			t.Fatal("原文件不该动")
		}
	})

	t.Run("postgres 标记已在", func(t *testing.T) {
		dir := legacyDataDir(t)
		if err := os.WriteFile(filepath.Join(dir, PostgresMarker), []byte(`{"store":"postgres"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := MigrateStore(ctx, dir, StorePostgres, MigrateOpts{Database: &DatabaseOptions{URL: "postgres://x"}})
		if stateCode(t, err) != Exists || err.Error() != "数据目录已经在用 PostgreSQL" {
			t.Fatalf("应拒绝：%v", err)
		}
	})

	t.Run("postgres 要连接串", func(t *testing.T) {
		_, err := MigrateStore(ctx, legacyDataDir(t), StorePostgres, MigrateOpts{})
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.HasPrefix(ue.Msg, "要给 --database-url") {
			t.Fatalf("应是用法错误：%v", err)
		}
	})

	t.Run("不再写状态文件", func(t *testing.T) {
		_, err := MigrateStore(ctx, legacyDataDir(t), StoreFile, MigrateOpts{})
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Msg != "Go 版不再写状态文件：用 --to sqlite" {
			t.Fatalf("应是用法错误：%v", err)
		}
	})

	t.Run("没有可迁的存储", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nothing")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{})
		if stateCode(t, err) != NotInitialized || err.Error() != "没有可迁的存储："+filepath.Join(dir, StateFile) {
			t.Fatalf("应拒绝：%v", err)
		}
	})

	t.Run("状态文件认不出：不建库、不动原文件", func(t *testing.T) {
		dir := legacyDataDir(t)
		broken := strings.Replace(string(compatFile(t, "state.json")), `"version": 1`, `"version": 9`, 1)
		if broken == string(compatFile(t, "state.json")) {
			t.Fatal("夹具里没找到 version")
		}
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{})
		if stateCode(t, err) != Invalid {
			t.Fatalf("应是 invalid：%v", err)
		}
		if got := names(t, dir); len(got) != 1 || got[0] != StateFile {
			t.Fatalf("不该留下别的文件：%v", got)
		}
		after, _ := os.ReadFile(filepath.Join(dir, StateFile))
		if string(after) != broken {
			t.Fatal("原文件被改了")
		}
	})

	t.Run("serve 正在运行", func(t *testing.T) {
		dir := legacyDataDir(t)
		// 父进程（go test）活着，且不是本进程。
		pid := os.Getppid()
		lock := filepath.Join(dir, RunLockFile)
		if err := os.WriteFile(lock, []byte(strings.TrimSpace(itoa(pid))+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{})
		var ue *UsageError
		want := "serve 正在运行（进程 " + itoa(pid) + "）：先停掉它再迁移；确认已停止就删掉 " + lock
		if !errors.As(err, &ue) || ue.Msg != want {
			t.Fatalf("应拒绝：%v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, DatabaseFile)); err == nil {
			t.Fatal("不该建库")
		}
		// 进程没了就照常（0x7fffffff 不会是活着的进程）。
		if err := os.WriteFile(lock, []byte("2147483646\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{}); err != nil {
			t.Fatalf("进程已不在，应照常迁：%v", err)
		}
	})

	t.Run("同种存储", func(t *testing.T) {
		dir := legacyDataDir(t)
		if _, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{}); err != nil {
			t.Fatal(err)
		}
		// 目标先于源检查：relay.db 已在。
		if _, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{}); stateCode(t, err) != Exists {
			t.Fatalf("应是 exists：%v", err)
		}
	})
}

func itoa(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestSameSnapshot(t *testing.T) {
	snap := fixtureSnapshot(t, legacyDataDir(t))
	same := snap
	same.Registrations = append([]store.RegistrationToken{}, snap.Registrations...)
	if !sameSnapshot(snap, same) {
		t.Fatal("相同的快照应相等")
	}
	empty := snap
	empty.Registrations, empty.Devices = nil, []store.DeviceRecord{}
	other := snap
	other.Registrations, other.Devices = []store.RegistrationToken{}, nil
	if !sameSnapshot(empty, other) {
		t.Fatal("nil 与空列表应等价")
	}
	revision := snap
	revision.Tenant.Revision++
	if sameSnapshot(snap, revision) {
		t.Fatal("修订号不同应不相等")
	}
	field := snap
	field.Accounts = append([]store.AccountRecord{}, snap.Accounts...)
	field.Accounts[0].Failures++
	if sameSnapshot(snap, field) {
		t.Fatal("字段不同应不相等")
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := legacyDataDir(t)

	t.Run("状态文件拷一份", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "backup.json")
		var lines []string
		if err := RunBackup(ctx, dir, out, nil, func(l string) { lines = append(lines, l) }); err != nil {
			t.Fatal(err)
		}
		if len(lines) != 1 || lines[0] != "已备份："+out {
			t.Fatalf("输出不对：%q", lines)
		}
		info, err := os.Stat(out)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("应为 0600：%v %v", info, err)
		}
		a, _ := os.ReadFile(out)
		if string(a) != string(compatFile(t, "state.json")) {
			t.Fatal("内容应一致")
		}
	})

	if _, err := MigrateStore(ctx, dir, StoreSQLite, MigrateOpts{}); err != nil {
		t.Fatal(err)
	}

	t.Run("SQLite 备份能原样打开", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "backup.db")
		if err := RunBackup(ctx, dir, out, nil, func(string) {}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(out)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("应为 0600：%v %v", info, err)
		}
		restored := t.TempDir()
		data, _ := os.ReadFile(out)
		if err := os.WriteFile(filepath.Join(restored, DatabaseFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStore(ctx, restored, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		got, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !sameSnapshot(got, fixtureSnapshot(t, legacyDataDir(t))) {
			t.Fatal("备份里的内容不对")
		}
		// 不覆盖。
		err = RunBackup(ctx, dir, out, nil, func(string) {})
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Msg != "备份目标已存在："+out {
			t.Fatalf("应拒绝覆盖：%v", err)
		}
	})

	t.Run("没有存储", func(t *testing.T) {
		empty := t.TempDir()
		err := RunBackup(ctx, empty, filepath.Join(t.TempDir(), "x.db"), nil, func(string) {})
		if stateCode(t, err) != NotInitialized {
			t.Fatalf("应是 not_initialized：%v", err)
		}
	})

	t.Run("PostgreSQL 用库自己的备份", func(t *testing.T) {
		pgDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(pgDir, PostgresMarker), []byte(`{"store":"postgres"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := RunBackup(ctx, pgDir, filepath.Join(t.TempDir(), "x"), nil, func(string) {})
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Msg != "PostgreSQL 存储用库自己的备份（pg_dump 或托管服务的快照）" {
			t.Fatalf("应是用法错误：%v", err)
		}
	})
}

func TestMigratePostgresRoundTrip(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db := &DatabaseOptions{URL: url, Schema: schema}
	dir := legacyDataDir(t)
	want := fixtureSnapshot(t, dir)

	// state.json → PostgreSQL。
	result, err := MigrateStore(ctx, dir, StorePostgres, MigrateOpts{Now: migrateNow, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	if result.From != StoreFile || result.Target != "postgres:"+schema ||
		result.Kept != filepath.Join(dir, StateFile+".migrated-"+migrateStampText) {
		t.Fatalf("结果不对：%+v", result)
	}
	marker, err := os.ReadFile(filepath.Join(dir, PostgresMarker))
	if err != nil || strings.TrimSpace(string(marker)) != `{"store":"postgres"}` || strings.Contains(string(marker), "postgres://") {
		t.Fatalf("标记不对：%q %v", marker, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, PostgresMarker)); info.Mode().Perm() != 0o600 {
		t.Fatal("标记应为 0600")
	}
	pg, err := OpenStore(ctx, dir, "", db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pg.Snapshot(ctx)
	_ = pg.Close()
	if err != nil || !sameSnapshot(got, want) {
		t.Fatalf("库里的内容不对：%v", err)
	}

	// 库里已有个人中转：别的数据目录迁进来被拒，什么也不留。
	other := legacyDataDir(t)
	_, err = MigrateStore(ctx, other, StorePostgres, MigrateOpts{Database: db})
	if stateCode(t, err) != Exists || err.Error() != "库里已经有个人中转" {
		t.Fatalf("应拒绝：%v", err)
	}
	if got := names(t, other); len(got) != 1 || got[0] != StateFile {
		t.Fatalf("被拒后不该留下别的文件：%v", got)
	}

	// PostgreSQL → SQLite：库里的数据原样保留，只有标记改名。
	var lines []string
	if err := RunMigrate(ctx, dir, StoreSQLite, db, func(l string) { lines = append(lines, l) }, migrateNow); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, PostgresMarker+".migrated-"+migrateStampText)
	wantLines := "已迁移到 SQLite：" + filepath.Join(dir, DatabaseFile) + "\n" +
		"PostgreSQL 里的数据原样保留；原标记留作回退：" + kept
	if strings.Join(lines, "\n") != wantLines {
		t.Fatalf("输出不对：%q", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, PostgresMarker)); err == nil {
		t.Fatal("标记应改名")
	}
	d, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := d.ListTenants(ctx)
	_ = d.Close()
	if err != nil || len(tenants) != 1 {
		t.Fatalf("库里的租户应原样保留：%v %v", tenants, err)
	}
	lite, err := sqlite.Open(ctx, filepath.Join(dir, DatabaseFile), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := lite.ExportTenant(ctx, store.DefaultTenant)
	_ = lite.Close()
	if err != nil || !sameSnapshot(back, want) {
		t.Fatalf("PostgreSQL → SQLite 后内容不对：%v", err)
	}

	// SQLite → PostgreSQL 已有租户：拒绝，relay.db 不动。
	_, err = MigrateStore(ctx, dir, StorePostgres, MigrateOpts{Database: db})
	if stateCode(t, err) != Exists {
		t.Fatalf("应是 exists：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DatabaseFile)); err != nil {
		t.Fatal("relay.db 不该动")
	}
	if _, err := os.Stat(filepath.Join(dir, PostgresMarker)); err == nil {
		t.Fatal("被拒后不该留标记")
	}
}

func TestMigratePostgresToSQLiteNeedsDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PostgresMarker), []byte(`{"store":"postgres"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := MigrateStore(context.Background(), dir, StoreSQLite, MigrateOpts{})
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.HasPrefix(ue.Msg, "要给 --database-url") {
		t.Fatalf("应是用法错误：%v", err)
	}
}
