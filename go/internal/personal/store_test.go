package personal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
)

const testPassword = "correct horse battery staple"

var baseTime = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// eachStore 在 SQLite 与 PostgreSQL（没有环境变量就跳过）上各跑一遍；dataDir 是还不存在的子目录。
func eachStore(t *testing.T, fn func(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		fn(t, StoreSQLite, filepath.Join(t.TempDir(), "data"), nil)
	})
	t.Run("postgres", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		fn(t, StorePostgres, filepath.Join(t.TempDir(), "data"), &DatabaseOptions{URL: url, Schema: schema})
	})
}

func createTestStore(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions) Store {
	t.Helper()
	st, err := CreateState(context.Background(), CreateStateInput{
		DataDir: dataDir, Account: "alice", Password: testPassword, Issuer: "https://127.0.0.1:8100",
		Store: kind, Database: db, Now: baseTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o，要 %o", path, got, want)
	}
}

func stateErr(t *testing.T, err error, code StateCode, contains string) {
	t.Helper()
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("要 StateError，得到 %T %v", err, err)
	}
	if se.Code != code || !strings.Contains(se.Msg, contains) {
		t.Fatalf("StateError = %s %q，要 %s 含 %q", se.Code, se.Msg, code, contains)
	}
}

func usageErr(t *testing.T, err error, msg string) {
	t.Helper()
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("要 UsageError，得到 %T %v", err, err)
	}
	if ue.Msg != msg {
		t.Fatalf("UsageError = %q，要 %q", ue.Msg, msg)
	}
}

func TestResolveKind(t *testing.T) {
	type files struct{ sqlite, state, marker bool }
	cases := []struct {
		name      string
		files     files
		requested StoreKind
		kind      StoreKind
		legacy    bool
		code      StateCode
		msg       string
	}{
		{name: "新装缺省 sqlite", kind: StoreSQLite},
		{name: "新装 --store postgres", requested: StorePostgres, kind: StorePostgres},
		{name: "已有 relay.db", files: files{sqlite: true}, kind: StoreSQLite},
		{name: "已有 relay.db 且 --store sqlite", files: files{sqlite: true}, requested: StoreSQLite, kind: StoreSQLite},
		{name: "只有 state.json 是老部署", files: files{state: true}, kind: StoreFile, legacy: true},
		{name: "标记 = postgres", files: files{marker: true}, kind: StorePostgres},
		{name: "两种以上拒绝", files: files{sqlite: true, state: true}, code: Invalid, msg: "数据目录里同时有 relay.db 与 state.json，拒绝启动：只留下要用的那个"},
		{name: "三种都有", files: files{sqlite: true, state: true, marker: true}, code: Invalid, msg: "数据目录里同时有 relay.db 与 state.json 与 postgres.json，拒绝启动"},
		{name: "sqlite 库 + --store file", files: files{sqlite: true}, requested: StoreFile, code: Invalid, msg: "数据目录里是 SQLite 库：去掉 --store file，或先运行 armadra-relay personal migrate --to file"},
		{name: "状态文件 + --store sqlite", files: files{state: true}, requested: StoreSQLite, code: Invalid, msg: "数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite"},
		{name: "sqlite 库 + --store postgres", files: files{sqlite: true}, requested: StorePostgres, code: Invalid, msg: "数据目录里用的是 SQLite：去掉 --store postgres，或先运行 armadra-relay personal migrate --to postgres"},
		{name: "postgres + --store sqlite", files: files{marker: true}, requested: StoreSQLite, code: Invalid, msg: "数据目录里用的是 PostgreSQL：去掉 --store sqlite，或先运行 armadra-relay personal migrate --to sqlite"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, on := range map[string]bool{DatabaseFile: c.files.sqlite, StateFile: c.files.state, PostgresMarker: c.files.marker} {
				if on {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			kind, legacy, err := ResolveKind(dir, c.requested)
			if c.code != "" {
				stateErr(t, err, c.code, c.msg)
				var se *StateError
				_ = errors.As(err, &se)
				if !strings.HasPrefix(se.Msg, c.msg) {
					t.Fatalf("消息 = %q，要 %q", se.Msg, c.msg)
				}
				return
			}
			if err != nil || kind != c.kind || legacy != c.legacy {
				t.Fatalf("ResolveKind = %s %v %v，要 %s %v", kind, legacy, err, c.kind, c.legacy)
			}
			if StoreExists(dir) != (c.files != files{}) {
				t.Fatalf("StoreExists 不对")
			}
		})
	}
}

func TestStoreRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions) {
		ctx := context.Background()
		st := createTestStore(t, kind, dataDir, db)
		if st.Kind() != kind || st.DataDir() != dataDir {
			t.Fatalf("Kind/DataDir = %s %s", st.Kind(), st.DataDir())
		}
		switch kind {
		case StoreSQLite:
			if st.Path() != filepath.Join(dataDir, DatabaseFile) {
				t.Fatalf("Path = %s", st.Path())
			}
			mustMode(t, dataDir, 0o700)
			mustMode(t, st.Path(), 0o600)
		case StorePostgres:
			if st.Path() != "postgres:"+db.Schema {
				t.Fatalf("Path = %s", st.Path())
			}
			mustMode(t, dataDir, 0o700)
			mustMode(t, filepath.Join(dataDir, PostgresMarker), 0o600)
			marker, _ := os.ReadFile(filepath.Join(dataDir, PostgresMarker))
			if string(marker) != "{\"store\":\"postgres\"}\n" || strings.Contains(string(marker), db.URL) {
				t.Fatalf("标记内容 = %q", marker)
			}
		}
		tenant := st.Tenant()
		if tenant.Issuer != "https://127.0.0.1:8100" || !tenant.AllowGuests || len(tenant.TrustedOrigins) != 0 {
			t.Fatalf("Tenant = %+v", tenant)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}

		// 已有存储：再建一遍答 Exists，不覆盖。
		if _, err := CreateState(ctx, CreateStateInput{DataDir: dataDir, Account: "bob", Password: testPassword, Issuer: "https://x", Store: kind, Database: db}); err == nil {
			t.Fatal("重复初始化应当拒绝")
		} else {
			stateErr(t, err, Exists, "已经初始化过")
		}

		// 不写 --store 也按目录里的开。
		st, err := OpenStore(ctx, dataDir, "", db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Accounts) != 1 || snap.Accounts[0].Name != "alice" || snap.Accounts[0].AccountID != "acct:alice" ||
			snap.Accounts[0].CreatedAtMs != baseTime.UnixMilli() {
			t.Fatalf("账号 = %+v", snap.Accounts)
		}
		if len(snap.SigningKeys) != 1 || snap.SigningKeys[0].Kid != "k1" {
			t.Fatalf("签名钥 = %+v", snap.SigningKeys)
		}

		if err := st.SetIssuer(ctx, "https://relay.example.test"); err != nil {
			t.Fatal(err)
		}
		if got := st.Tenant().Issuer; got != "https://relay.example.test" {
			t.Fatalf("SetIssuer 之后 Tenant = %s", got)
		}
		issuer, err := st.Keys().Issuer(ctx)
		if err != nil || issuer != "https://relay.example.test" {
			t.Fatalf("Keys.Issuer = %q %v", issuer, err)
		}

		kid, err := st.RotateKey(ctx, baseTime.UnixMilli())
		if err != nil || kid != "k2" {
			t.Fatalf("RotateKey = %q %v", kid, err)
		}
		keys, err := st.Keys().SigningKeys(ctx)
		if err != nil || len(keys) != 2 || keys[0].RetiredAtMs == nil || keys[1].RetiredAtMs != nil {
			t.Fatalf("轮换后签名钥 = %+v %v", keys, err)
		}
	})
}

func TestStoreRecordSourcesSeenAndRevoked(t *testing.T) {
	eachStore(t, func(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions) {
		ctx := context.Background()
		st := createTestStore(t, kind, dataDir, db)
		defer func() { _ = st.Close() }()
		seen := int64(5_000)
		put := func(id string, last *int64) {
			_, err := st.Ports().Sources.Put(ctx, store.DefaultTenant, id, func(*store.SourceRecord) (store.SourceRecord, error) {
				return testSource(id, last), nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		put("s1", &seen)
		put("s2", nil)
		put("s3", nil)
		err := st.RecordSourcesSeen(ctx, map[string]SourceSeen{
			"s1":      {AtMs: 3_000, CoreVersion: "0.9.2"}, // 往回推不算，版本照记
			"s2":      {AtMs: 7_000},
			"missing": {AtMs: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		get := func(id string) store.SourceRecord {
			row, err := st.Ports().Sources.Get(ctx, store.DefaultTenant, id)
			if err != nil || row == nil {
				t.Fatal(err)
			}
			return *row
		}
		if s1 := get("s1"); *s1.LastSeenAtMs != 5_000 || s1.CoreVersion != "0.9.2" {
			t.Fatalf("s1 = %+v", s1)
		}
		if s2 := get("s2"); *s2.LastSeenAtMs != 7_000 || s2.CoreVersion != "0.9.0" {
			t.Fatalf("s2 = %+v", s2)
		}
		if s3 := get("s3"); s3.LastSeenAtMs != nil {
			t.Fatalf("s3 = %+v", s3)
		}
		// 没变的不写：修订号不动。
		before, _, _ := st.(*dbStore).b.Revision(ctx, store.DefaultTenant)
		if err := st.RecordSourcesSeen(ctx, map[string]SourceSeen{"s2": {AtMs: 7_000}}); err != nil {
			t.Fatal(err)
		}
		if after, _, _ := st.(*dbStore).b.Revision(ctx, store.DefaultTenant); after != before {
			t.Fatalf("没变也写了：%d → %d", before, after)
		}

		// 撤销清单。
		revokedAt := int64(9_000)
		if err := st.Ports().Sources.Update(ctx, store.DefaultTenant, "s3", func(d *store.SourceRecord) error {
			d.RevokedAtMs = &revokedAt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sources, links, err := st.Revoked(ctx)
		if err != nil || len(sources) != 1 || sources[0] != "s3" || len(links) != 0 {
			t.Fatalf("Revoked = %v %v %v", sources, links, err)
		}
	})
}

func testSource(id string, last *int64) store.SourceRecord {
	return store.SourceRecord{
		SourceID: id, Name: "笔记本-" + id, Kind: "desktop", CoreVersion: "0.9.0",
		Capabilities: []string{}, RegisteredAtMs: 1_000, LastSeenAtMs: last, TrustedOrigins: []string{},
		PublicJWK: testJWK(),
	}
}

func testJWK() gen.Ed25519PublicJWK {
	return gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}
}

func TestStoreWatch(t *testing.T) {
	eachStore(t, func(t *testing.T, kind StoreKind, dataDir string, db *DatabaseOptions) {
		ctx := context.Background()
		first := createTestStore(t, kind, dataDir, db)
		defer func() { _ = first.Close() }()
		second, err := OpenStore(ctx, dataDir, "", db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = second.Close() }()

		changed := make(chan struct{}, 8)
		errs := make(chan error, 8)
		stop := first.Watch(func() { changed <- struct{}{} }, func(err error) { errs <- err }, 20*time.Millisecond)
		defer stop()

		// 自己写的，SQLite 不算改动；先让 Watch 跑几轮。
		if err := second.SetIssuer(ctx, "https://other.example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := second.RotateKey(ctx, baseTime.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("Watch 没察觉别的句柄的写")
		}
		// 签名钥缓存与租户缓存都要刷新（可能需要再等一轮把两次写合并掉）。
		deadline := time.Now().Add(5 * time.Second)
		for {
			keys, _ := first.Keys().SigningKeys(ctx)
			if first.Tenant().Issuer == "https://other.example.test" && len(keys) == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("缓存没刷新：issuer=%s keys=%d", first.Tenant().Issuer, len(keys))
			}
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		stop() // 幂等
	})
}

func TestOpenAndCreateRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("没有存储", func(t *testing.T) {
		_, err := OpenStore(ctx, t.TempDir(), "", nil)
		stateErr(t, err, NotInitialized, "还没有初始化：先运行 armadra-relay personal init")
	})
	t.Run("状态文件拒绝并提示迁移", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(ctx, dir, "", nil)
		stateErr(t, err, Invalid, "数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite")
		_, err = OpenStore(ctx, dir, StoreSQLite, nil)
		stateErr(t, err, Invalid, "先运行 armadra-relay personal migrate --to sqlite")
		// 有状态文件时不新建。
		_, err = CreateState(ctx, CreateStateInput{DataDir: dir, Account: "alice", Password: testPassword, Issuer: "https://x"})
		stateErr(t, err, Exists, "已经初始化过：数据目录里已有存储")
		if exists(filepath.Join(dir, DatabaseFile)) {
			t.Fatal("拒绝时不该留下 relay.db")
		}
	})
	t.Run("两种存储并存", func(t *testing.T) {
		dir := t.TempDir()
		st := createTestStore(t, StoreSQLite, dir, nil)
		_ = st.Close()
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(ctx, dir, "", nil)
		stateErr(t, err, Invalid, "同时有 relay.db 与 state.json")
	})
	t.Run("--store 与目录不符", func(t *testing.T) {
		dir := t.TempDir()
		st := createTestStore(t, StoreSQLite, dir, nil)
		_ = st.Close()
		_, err := OpenStore(ctx, dir, StorePostgres, &DatabaseOptions{URL: "postgres://x"})
		stateErr(t, err, Invalid, "数据目录里用的是 SQLite：去掉 --store postgres")
	})
	t.Run("Go 版不写状态文件", func(t *testing.T) {
		_, err := CreateState(ctx, CreateStateInput{DataDir: t.TempDir(), Account: "alice", Password: testPassword, Issuer: "https://x", Store: StoreFile})
		usageErr(t, err, "Go 版不再写状态文件：用 --store sqlite")
	})
	t.Run("postgres 要连接串", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, PostgresMarker), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(ctx, dir, "", nil)
		usageErr(t, err, "数据目录用的是 PostgreSQL：给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
		_, err = CreateState(ctx, CreateStateInput{DataDir: t.TempDir(), Account: "alice", Password: testPassword, Issuer: "https://x", Store: StorePostgres})
		usageErr(t, err, "数据目录用的是 PostgreSQL：给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）")
	})
	t.Run("别的程序的库", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, DatabaseFile), []byte("not a database at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(ctx, dir, "", nil)
		var se *StateError
		if !errors.As(err, &se) || se.Code != Invalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("租户不对的库", func(t *testing.T) {
		dir := t.TempDir()
		st := createTestStore(t, StoreSQLite, dir, nil)
		if err := st.(*dbStore).b.CreateTenant(ctx, "other", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://o", TrustedOrigins: []string{}}}); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		_, err := OpenStore(ctx, dir, "", nil)
		stateErr(t, err, Invalid, "数据库不是个人中转的库（租户不对），拒绝启动")
	})
	t.Run("CreateStore 快照要一个账号", func(t *testing.T) {
		_, err := CreateStore(ctx, t.TempDir(), StoreSQLite, store.TenantSnapshot{}, nil)
		stateErr(t, err, Invalid, "个人中转只有一个账号")
	})
}

func TestPostgresStoreExistsAndMarker(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db := DatabaseOptions{URL: url, Schema: schema}
	has, err := PostgresStoreExists(ctx, db)
	if err != nil || has {
		t.Fatalf("空库 = %v %v", has, err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	// 没初始化：打不开，也不留标记。
	_, err = OpenStore(ctx, dataDir, StorePostgres, &db)
	stateErr(t, err, NotInitialized, "personal init --store postgres")
	if exists(filepath.Join(dataDir, PostgresMarker)) {
		t.Fatal("没初始化不该留标记")
	}
	st := createTestStore(t, StorePostgres, dataDir, &db)
	_ = st.Close()
	if has, err = PostgresStoreExists(ctx, db); err != nil || !has {
		t.Fatalf("已初始化 = %v %v", has, err)
	}
	// 库里已有：换个数据目录再建也拒绝，也不留标记。
	other := filepath.Join(t.TempDir(), "other")
	_, err = CreateState(ctx, CreateStateInput{DataDir: other, Account: "bob", Password: testPassword, Issuer: "https://x", Store: StorePostgres, Database: &db})
	stateErr(t, err, Exists, "PostgreSQL 里已有个人中转")
	if exists(filepath.Join(other, PostgresMarker)) {
		t.Fatal("拒绝时不该留标记")
	}
	// 连不上：只报错误码。
	_, err = PostgresStoreExists(ctx, DatabaseOptions{URL: "postgres://nobody:secret-pw@127.0.0.1:1/none"})
	if err == nil || strings.Contains(err.Error(), "secret-pw") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithStore(t *testing.T) {
	dir := t.TempDir()
	st := createTestStore(t, StoreSQLite, dir, nil)
	_ = st.Close()
	var kind StoreKind
	if err := WithStore(context.Background(), dir, nil, func(s Store) error { kind = s.Kind(); return nil }); err != nil || kind != StoreSQLite {
		t.Fatalf("WithStore = %v %v", kind, err)
	}
	sentinel := errors.New("boom")
	if err := WithStore(context.Background(), dir, nil, func(Store) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}

func TestRevTrackerOwnVersusExternal(t *testing.T) {
	tr := newRevTracker()
	tr.reset(5)
	tr.commit("t", 6)
	tr.commit("t", 7)
	if tr.advance(7) {
		t.Fatal("6、7 都是自己提交的，不算外部改动")
	}
	// 自己提交了 8，但 9 是别的节点写的：不能因为「正好 +1」就当成都是自己的。
	tr.commit("t", 8)
	if !tr.advance(9) {
		t.Fatal("9 不是自己提交的，应察觉外部改动")
	}
	if tr.advance(9) {
		t.Fatal("没有新修订就不该再报")
	}
	// 夹在中间：别人先写了 10，自己写了 11。
	tr.commit("t", 11)
	if !tr.advance(11) {
		t.Fatal("10 是别人的")
	}
}
