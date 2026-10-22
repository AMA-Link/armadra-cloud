package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/storetest"
)

func create(t *testing.T) store.Backend {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// 端口合同（15 例）。
func TestPortsContract(t *testing.T) { storetest.PortsContract(t, create) }

func kindOf(t *testing.T, err error) store.ErrKind {
	t.Helper()
	if err == nil {
		t.Fatal("应该被拒绝")
	}
	kind := store.KindOf(err)
	if kind == "" {
		t.Fatalf("不是库的问题：%v", err)
	}
	return kind
}

func TestNewFileIs0600AndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("新建文件权限 %v，应为 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("应启用 WAL：%v", err)
	}
	// 再新建同一个文件被拒绝，文件不动。
	_, err = sqlite.Open(context.Background(), path, sqlite.Options{Create: true})
	if kindOf(t, err) != store.KindExists {
		t.Fatalf("应为 exists：%v", err)
	}
}

func TestOpenRefusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// not_found：路径不存在。
	_, err := sqlite.Open(ctx, filepath.Join(dir, "none.db"), sqlite.Options{})
	if kindOf(t, err) != store.KindNotFound {
		t.Fatalf("应为 not_found：%v", err)
	}

	// unknown：空文件。
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = sqlite.Open(ctx, empty, sqlite.Options{})
	if kindOf(t, err) != store.KindUnknown {
		t.Fatalf("空文件应为 unknown：%v", err)
	}

	// corrupt：不是 SQLite。
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("这不是一个 SQLite 数据库，只是一段文字，长度要超过一百个字节才像样。0123456789012345678901234567890123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(garbage)
	_, err = sqlite.Open(ctx, garbage, sqlite.Options{})
	if kindOf(t, err) != store.KindCorrupt {
		t.Fatalf("乱码应为 corrupt：%v", err)
	}
	after, _ := os.ReadFile(garbage)
	if string(before) != string(after) {
		t.Fatal("拒绝时不许动原文件")
	}
}

func TestForeignDatabaseIsUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "other.db")
	// 别的程序的库：application_id 是 0，有业务表。用我们自己的驱动造一个。
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw := openRaw(t, path)
	if _, err := raw.Exec("PRAGMA application_id = 7"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	_, err = sqlite.Open(ctx, path, sqlite.Options{})
	if kindOf(t, err) != store.KindUnknown {
		t.Fatalf("application_id 不对应为 unknown：%v", err)
	}
}

func TestLedgerRefusals(t *testing.T) {
	ctx := context.Background()
	migrations, err := sqlite.Migrations()
	if err != nil {
		t.Fatal(err)
	}

	// 账本比程序新：库里多记一条，旧程序（只认识前一个）拒绝。
	t.Run("newer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.db")
		db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		_, err = sqlite.Open(ctx, path, sqlite.Options{Migrations: migrations[:1]})
		if kindOf(t, err) != store.KindNewer {
			t.Fatalf("应为 newer：%v", err)
		}
	})

	// 账本对不上：改一条的 sha384。
	t.Run("mismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.db")
		db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		raw := openRaw(t, path)
		if _, err := raw.Exec("UPDATE relay_migrations SET sha384 = 'x' WHERE id = 1"); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		_, err = sqlite.Open(ctx, path, sqlite.Options{})
		if kindOf(t, err) != store.KindUnknown {
			t.Fatalf("应为 unknown：%v", err)
		}
	})

	// 没有账本却已有业务表。
	t.Run("no ledger", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.db")
		db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		raw := openRaw(t, path)
		if _, err := raw.Exec("DROP TABLE relay_migrations"); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		_, err = sqlite.Open(ctx, path, sqlite.Options{})
		if kindOf(t, err) != store.KindUnknown {
			t.Fatalf("应为 unknown：%v", err)
		}
	})

	// 前缀账本补跑新迁移：先只装第一个，再用全部打开。
	t.Run("prefix catches up", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.db")
		db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true, Migrations: migrations[:1]})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		db, err = sqlite.Open(ctx, path, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Ports().Traffic.Query(ctx, "t", store.TrafficQuery{ToMs: 1, Granularity: "hour", Limit: 1}); err != nil {
			t.Fatalf("补跑后流量表应可用：%v", err)
		}
	})
}

func TestBackupAndDataVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "default", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "backup.db")
	if err := db.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("备份权限 %v，应为 0600", info.Mode().Perm())
	}
	if kindOf(t, db.Backup(ctx, target)) != store.KindExists {
		t.Fatal("备份目标已存在应拒绝")
	}
	// 目标建不出来（目录不存在）不是「已存在」：CLI 要退出 1，不是 64。
	if kindOf(t, db.Backup(ctx, filepath.Join(dir, "none", "backup.db"))) != store.KindCorrupt {
		t.Fatal("目标建不出来应是 corrupt，不是 exists")
	}
	copied, err := sqlite.Open(ctx, target, sqlite.Options{})
	if err != nil {
		t.Fatalf("备份要能原样打开：%v", err)
	}
	if _, ok, _ := copied.Revision(ctx, "default"); !ok {
		t.Fatal("备份里应有租户")
	}
	_ = copied.Close()

	lines, err := db.IntegrityCheck(ctx)
	if err != nil || len(lines) != 1 || lines[0] != "ok" {
		t.Fatalf("integrity_check：%v %v", lines, err)
	}

	// 自己的提交不改 data_version；别的连接提交后才变。
	first, _ := db.DataVersion(ctx)
	if err := db.AddAccount(ctx, "default", store.AccountRecord{AccountID: "u", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.DataVersion(ctx); again != first {
		t.Fatalf("自己的提交 data_version 不该变：%d → %d", first, again)
	}
	other, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.AddAccount(ctx, "default", store.AccountRecord{AccountID: "v", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if changed, _ := db.DataVersion(ctx); changed == first {
		t.Fatal("别的连接提交后 data_version 应该变")
	}
}

func TestRotateKey(t *testing.T) {
	ctx := context.Background()
	db := create(t).(*sqlite.DB)
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "default", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}
	first, err := store.GenerateSigningKey("k1", storetest.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddSigningKey(ctx, "default", first); err != nil {
		t.Fatal(err)
	}
	kid, err := db.RotateKey(ctx, "default", storetest.Now+1000)
	if err != nil || kid != "k2" {
		t.Fatalf("轮换：%q %v", kid, err)
	}
	keys, err := db.Keys("default").SigningKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("应有两把钥：%v %v", keys, err)
	}
	if keys[0].RetiredAtMs == nil || *keys[0].RetiredAtMs != storetest.Now+1000+store.KeyRetireGraceMs {
		t.Fatalf("旧钥应一小时后退役：%v", keys[0].RetiredAtMs)
	}
	if keys[1].RetiredAtMs != nil {
		t.Fatal("新钥不该退役")
	}
	// 再轮换一次，k1 早已退役（一小时后）时被删掉。
	if _, err := db.RotateKey(ctx, "default", storetest.Now+2*store.KeyRetireGraceMs); err != nil {
		t.Fatal(err)
	}
	keys, _ = db.Keys("default").SigningKeys(ctx)
	for _, key := range keys {
		if key.Kid == "k1" {
			t.Fatal("早已退役的钥应被删掉")
		}
	}
}
