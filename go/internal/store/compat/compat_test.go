// Package compat 证明 Go 读得动已部署的库，账本对得上：
//
//   - go/testdata/compat/ 里是冻结的静态夹具（由已删除的 TypeScript 存储写出，不再重新生成）：
//     relay.db（SQLite）、postgres.sql（PostgreSQL schema 的转储）、state.json（状态文件，schema v1）、
//     expected.json（测试值）；
//   - 夹具账本里每条迁移的名字与 SHA-384 必须与 Go 内嵌的迁移一致——迁移文件改了一个字节，
//     已部署的库就会拒绝启动，这里先失败；
//   - Go 打开夹具库、读出的内容与 state.json 导入的快照逐字段相同；再让 Go 写（轮换钥、旋转刷新
//     令牌、撤销链接、加流量……），关掉重开后读回逐字段相同。Go 新建的库账本与夹具逐条相同。
//
// PostgreSQL 那一半要 ARMADRA_CLOUD_TEST_DATABASE_URL（pnpm dev:up 的库），没有就整段跳过。
package compat_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/statefile"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

const tenant = "default"

func compatDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(testutil.RepoRoot(t), "go", "testdata", "compat")
}

type expected struct {
	NowMs    int64  `json:"nowMs"`
	Password string `json:"password"`
	IDs      struct {
		Source        string `json:"source"`
		RevokedSource string `json:"revokedSource"`
		OwnerDevice   string `json:"ownerDevice"`
		RevokedDevice string `json:"revokedDevice"`
		GuestDevice   string `json:"guestDevice"`
		Link          string `json:"link"`
		RevokedLink   string `json:"revokedLink"`
	} `json:"ids"`
	Secrets struct {
		OwnerRefreshToken string `json:"ownerRefreshToken"`
	} `json:"secrets"`
}

func loadExpected(t testing.TB) expected {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(compatDir(t), "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out expected
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func stateSnapshot(t testing.TB) store.TenantSnapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(compatDir(t), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := statefile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// canonical 把快照换成可比较的通用值（与 state.json 导入的快照同形）。
func canonical(t testing.TB, snapshot store.TenantSnapshot) any {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"tenant":        snapshot.Tenant,
		"accounts":      snapshot.Accounts,
		"signingKeys":   snapshot.SigningKeys,
		"sources":       snapshot.Sources,
		"registrations": snapshot.Registrations,
		"devices":       snapshot.Devices,
		"links":         snapshot.Links,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t testing.TB, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("%s 不一致：\n得到 %s\n应为 %s", what, g, w)
	}
}

// sqliteLedger 读 SQLite 库的迁移账本（不经 Go 的存储，免得打开时补跑迁移）；夹具要先拷出来，WAL 会在库旁边建文件。
func sqliteLedger(t *testing.T, path string) []store.Applied {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT id, name, sha384 FROM relay_migrations ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Applied
	for rows.Next() {
		var row store.Applied
		if err := rows.Scan(&row.ID, &row.Name, &row.SHA384); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// postgresDumpLedger 从 postgres.sql 里取账本的 INSERT 行。
func postgresDumpLedger(t *testing.T) []store.Applied {
	t.Helper()
	dump, err := os.ReadFile(filepath.Join(compatDir(t), "postgres.sql"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`INSERT INTO cloud_migrations \(id, name, sha384, applied_at_ms\) VALUES \((\d+), '([^']+)', '([0-9a-f]{96})'`)
	var out []store.Applied
	for _, match := range pattern.FindAllStringSubmatch(string(dump), -1) {
		id, _ := strconv.Atoi(match[1])
		out = append(out, store.Applied{ID: id, Name: match[2], SHA384: match[3]})
	}
	return out
}

// embedded 把 Go 内嵌的迁移换成账本行。
func embedded(t *testing.T, list func() ([]store.Migration, error)) []store.Applied {
	t.Helper()
	migrations, err := list()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]store.Applied, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, store.Applied{ID: m.ID, Name: m.Name, SHA384: m.SHA384})
	}
	return out
}

// TestFixtureLedgersMatchEmbeddedMigrations：已部署的库里记的 SHA-384 与 Go 内嵌的迁移逐条相同。
func TestFixtureLedgersMatchEmbeddedMigrations(t *testing.T) {
	sqliteWant := embedded(t, sqlite.Migrations)
	if got := sqliteLedger(t, copyFixtureDB(t)); !reflect.DeepEqual(got, sqliteWant) {
		t.Fatalf("relay.db 的账本与内嵌迁移不同：\n夹具 %+v\n内嵌 %+v", got, sqliteWant)
	}
	postgresWant := embedded(t, postgres.Migrations)
	got := postgresDumpLedger(t)
	if len(got) != 3 || len(postgresWant) < 3 || !reflect.DeepEqual(got, postgresWant[:3]) {
		t.Fatalf("postgres.sql 的账本与内嵌迁移的前三条不同：\n夹具 %+v\n内嵌 %+v", got, postgresWant)
	}
}

// ---------------------------------------------------------------- 变更：让 Go 写一遍

// mutate 让 Go 通过端口写各种东西，并答期望的修订号增量下限。
func mutate(t *testing.T, db store.Backend, keyRotate func(ctx context.Context) error, want expected) {
	t.Helper()
	ctx := context.Background()
	ports := db.Ports()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := want.NowMs + 60_000

	// 旋转刷新令牌（会话）。
	must(ports.Devices.Update(ctx, tenant, want.IDs.OwnerDevice, func(d *store.DeviceRecord) error {
		previous := d.RefreshHash
		d.PreviousRefreshHash = &previous
		d.RefreshHash = strings.Repeat("ab", 32)
		d.Rotation++
		d.LastSeenAtMs = &now
		return nil
	}))
	must(ports.Devices.Insert(ctx, tenant, store.DeviceRecord{
		DeviceID: strings.Repeat("e5", 16), Subject: "acct:owner", Platform: "ios", Name: "phone",
		RefreshHash: strings.Repeat("cd", 32), Rotation: 1, CreatedAtMs: now, ExpiresAtMs: now + 1_000_000_000,
	}, now))
	// 源：替换（排到最后）、改最近在线。
	_, err := ports.Sources.Put(ctx, tenant, want.IDs.Source, func(existing *store.SourceRecord) (store.SourceRecord, error) {
		next := *existing
		next.CoreVersion = "0.10.0"
		next.Capabilities = append(next.Capabilities, "媒体")
		return next, nil
	})
	must(err)
	// 链接：计数、撤销（连同访客设备）。
	must(ports.Links.Update(ctx, tenant, want.IDs.Link, func(d *store.LinkRecord) error { d.Uses++; return nil }))
	_, ok, err := ports.Links.Revoke(ctx, tenant, want.IDs.Link, now)
	must(err)
	if !ok {
		t.Fatal("夹具里的链接应能撤销")
	}
	// 账号锁定计数、注册令牌、流量。
	must(ports.Accounts.Update(ctx, tenant, "acct:owner", func(d *store.AccountRecord) error {
		d.Failures = 5
		d.LockedUntilMs = now + 60_000
		d.LastFailureAtMs = now
		return nil
	}))
	_, err = ports.Registrations.Add(ctx, tenant, store.RegistrationToken{
		TokenHash: strings.Repeat("0f", 32), ExpiresAtMs: now + 600_000, AccountID: "acct:owner",
	}, now, 20)
	must(err)
	must(ports.Traffic.Upsert(ctx, tenant, []store.TrafficRow{
		{BucketStartMs: now / store.HourMs * store.HourMs, Node: "relay-2", SourceID: want.IDs.Source, Subject: "acct:owner",
			Kind: "e2e", Connections: 2, BytesToSource: 1 << 33, BytesToClient: 7},
	}))
	must(keyRotate(ctx))
}

// ---------------------------------------------------------------- state.json

func TestStateJSONFixtureImports(t *testing.T) {
	snapshot := stateSnapshot(t)
	want := canonical(t, snapshot)
	if got := snapshot.Tenant.Revision; got != 7 {
		t.Fatalf("修订号应照文件：%d", got)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AccountID != "acct:owner" {
		t.Fatalf("账号 id 应是 acct:<账号名>：%+v", snapshot.Accounts)
	}
	for _, token := range snapshot.Registrations {
		if token.AccountID != "acct:owner" {
			t.Fatalf("注册令牌的签发人应是账号：%+v", token)
		}
	}

	t.Run("写进 SQLite 再读出来逐字段相同", func(t *testing.T) {
		db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if err := db.ImportTenant(context.Background(), tenant, snapshot); err != nil {
			t.Fatal(err)
		}
		got, err := db.ExportTenant(context.Background(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, canonical(t, got), want, "导入再导出")
		// 租户已存在就拒绝，不覆盖。
		if err := db.ImportTenant(context.Background(), tenant, snapshot); store.KindOf(err) != store.KindExists {
			t.Fatalf("重复导入应为 exists：%v", err)
		}
	})

	t.Run("写进 PostgreSQL 再读出来逐字段相同", func(t *testing.T) {
		url, schema := pgtest.Schema(t)
		db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if err := db.ImportTenant(context.Background(), tenant, snapshot); err != nil {
			t.Fatal(err)
		}
		got, err := db.ExportTenant(context.Background(), tenant)
		if err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, canonical(t, got), want, "导入再导出")
	})
}

func TestStateFileRejections(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(compatDir(t), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	withChange := func(change func(m map[string]any)) []byte {
		var copied map[string]any
		_ = json.Unmarshal(raw, &copied)
		change(copied)
		data, _ := json.Marshal(copied)
		return data
	}
	cases := map[string][]byte{
		"不是 JSON":    []byte("{坏的"),
		"版本未知":       withChange(func(m map[string]any) { m["version"] = 2 }),
		"缺少 account": withChange(func(m map[string]any) { delete(m, "account") }),
		"没有签名钥":      withChange(func(m map[string]any) { m["signingKeys"] = []any{} }),
		"sourceId 不是 hex32": withChange(func(m map[string]any) {
			m["sources"].([]any)[0].(map[string]any)["sourceId"] = "短"
		}),
		"口令记录不是 scrypt": withChange(func(m map[string]any) {
			m["account"].(map[string]any)["password"].(map[string]any)["kdf"] = "argon2"
		}),
		"displayName 全是空白": withChange(func(m map[string]any) {
			m["settings"].(map[string]any)["displayName"] = "   "
		}),
		"限流额度越界": withChange(func(m map[string]any) {
			m["settings"].(map[string]any)["rateLimits"] = map[string]any{"token": 0}
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := statefile.Parse(data)
			var problem *statefile.Error
			if err == nil {
				t.Fatal("应该被拒绝")
			}
			if e, ok := err.(*statefile.Error); !ok || e.Code != statefile.Invalid {
				t.Fatalf("应为 invalid：%v %v", err, problem)
			}
			if strings.Contains(err.Error(), "scrypt") || strings.Contains(err.Error(), "owner") {
				t.Fatalf("错误信息不该带文件内容：%v", err)
			}
		})
	}

	t.Run("未知键被丢掉，displayName 去空白", func(t *testing.T) {
		data := withChange(func(m map[string]any) {
			m["多出来的"] = 1
			m["settings"].(map[string]any)["displayName"] = "  名字  "
		})
		state, err := statefile.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		if state.Settings.DisplayName == nil || *state.Settings.DisplayName != "名字" {
			t.Fatalf("displayName = %v", state.Settings.DisplayName)
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		_, err := statefile.Read(t.TempDir())
		if e, ok := err.(*statefile.Error); !ok || e.Code != statefile.NotInitialized {
			t.Fatalf("应为 not_initialized：%v", err)
		}
	})
}

// ---------------------------------------------------------------- SQLite

func copyFixtureDB(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(compatDir(t), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "relay.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSQLiteFixtureRoundTrip(t *testing.T) {
	ctx := context.Background()
	want := loadExpected(t)
	path := copyFixtureDB(t)

	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("Go 打开夹具库：%v", err)
	}
	defer func() { _ = db.Close() }()

	// Go 读到的与 state.json 导入的快照逐字段相同。
	exported, err := db.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, exported), canonical(t, stateSnapshot(t)), "Go 读夹具库")

	// 流量统计（不在 state.json 里）。
	rows, err := db.Ports().Traffic.Query(ctx, tenant, store.TrafficQuery{ToMs: want.NowMs * 2, Granularity: "hour", Limit: 10})
	if err != nil || len(rows) != 2 || rows[1].BytesToClient != 5_000_000_000 {
		t.Fatalf("夹具里的流量：%+v %v", rows, err)
	}

	before, _, _ := db.Revision(ctx, tenant)
	mutate(t, db, func(ctx context.Context) error { _, err := db.RotateKey(ctx, tenant, want.NowMs+60_000); return err }, want)
	after, _, _ := db.Revision(ctx, tenant)
	if after <= before {
		t.Fatalf("Go 写之后修订号应往前走：%d → %d", before, after)
	}

	goView, err := db.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("重开 Go 写过的夹具库：%v", err)
	}
	defer func() { _ = reopened.Close() }()
	again, err := reopened.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, again), canonical(t, goView), "重开 Go 写过的库")
	if got := sqliteLedger(t, path); !reflect.DeepEqual(got, embedded(t, sqlite.Migrations)) {
		t.Fatalf("Go 写过之后账本变了：%+v", got)
	}
}

func TestGoCreatedSQLiteMatchesFixture(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sqlite.Open(ctx, path, sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := stateSnapshot(t)
	if err := db.ImportTenant(ctx, tenant, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, got), canonical(t, snapshot), "重开 Go 新建的库")

	// Go 新建的库与夹具库的账本逐条相同（名字与 SHA-384），applied_at_ms 不比。
	if created, fixture := sqliteLedger(t, path), sqliteLedger(t, copyFixtureDB(t)); !reflect.DeepEqual(created, fixture) {
		t.Fatalf("Go 新建的库与夹具账本不同：\n新建 %+v\n夹具 %+v", created, fixture)
	}
}

// ---------------------------------------------------------------- PostgreSQL

// restoreDump 把 postgres.sql 还原进一个随机名字的 schema。
func restoreDump(t *testing.T) (url, schema string) {
	t.Helper()
	url, schema = pgtest.Schema(t)
	dump, err := os.ReadFile(filepath.Join(compatDir(t), "postgres.sql"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("测试库连不上：%v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// 没有参数时 pgx 走简单协议，转储里的多条语句一次执行。
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+";\n"+string(dump)); err != nil {
		t.Fatalf("还原转储：%v", err)
	}
	return url, schema
}

func TestPostgresFixtureRoundTrip(t *testing.T) {
	ctx := context.Background()
	want := loadExpected(t)
	url, schema := restoreDump(t)

	// Go 打开夹具 schema（停在 0003）：前三条的 SHA-384 一致，不拒绝；
	// 0004–0011（SaaS）补跑，老行取缺省。
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, Now: func() int64 { return 1 }})
	if err != nil {
		t.Fatalf("Go 打开夹具 schema：%v", err)
	}
	defer func() { _ = db.Close() }()
	var applied int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*)::int FROM cloud_migrations WHERE applied_at_ms = 1").Scan(&applied); err != nil || applied != 8 {
		t.Fatalf("应只补跑 0004–0011 八条：%d %v", applied, err)
	}

	exported, err := db.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, exported), canonical(t, stateSnapshot(t)), "Go 读夹具 schema")
	rows, err := db.Ports().Traffic.Query(ctx, tenant, store.TrafficQuery{ToMs: want.NowMs * 2, Granularity: "hour", Limit: 10})
	if err != nil || len(rows) != 2 || rows[1].BytesToClient != 5_000_000_000 {
		t.Fatalf("夹具里的流量：%+v %v", rows, err)
	}

	if got, want := postgresLedger(t, db), embedded(t, postgres.Migrations); !reflect.DeepEqual(got, want) {
		t.Fatalf("补跑之后账本与内嵌迁移不同：\n库里 %+v\n内嵌 %+v", got, want)
	}

	before, _, _ := db.Revision(ctx, tenant)
	mutate(t, db, func(ctx context.Context) error { _, err := db.RotateKey(ctx, tenant, want.NowMs+60_000); return err }, want)
	after, _, _ := db.Revision(ctx, tenant)
	if after <= before {
		t.Fatalf("Go 写之后修订号应往前走：%d → %d", before, after)
	}
	goView, err := db.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatalf("重开 Go 写过的 schema：%v", err)
	}
	defer func() { _ = reopened.Close() }()
	again, err := reopened.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, again), canonical(t, goView), "重开 Go 写过的 schema")
}

// postgresLedger 读 schema 里的 cloud_migrations。
func postgresLedger(t *testing.T, db *postgres.DB) []store.Applied {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), "SELECT id, name, sha384 FROM cloud_migrations ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.Applied
	for rows.Next() {
		var row store.Applied
		if err := rows.Scan(&row.ID, &row.Name, &row.SHA384); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGoCreatedPostgresMatchesFixture(t *testing.T) {
	ctx := context.Background()
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	snapshot := stateSnapshot(t)
	if err := db.ImportTenant(ctx, tenant, snapshot); err != nil {
		t.Fatal(err)
	}
	// 走一遍 Go 的写（含批量流量 upsert），重开后读回逐字段相同。
	mutate(t, db, func(ctx context.Context) error { _, err := db.RotateKey(ctx, tenant, 1_760_000_060_000); return err }, loadExpected(t))
	goView, err := db.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	again, err := reopened.ExportTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, canonical(t, again), canonical(t, goView), "重开 Go 新建的 schema")
	// 新建 schema 的账本前三条与夹具转储（已部署的库）逐条相同。
	if got := postgresLedger(t, reopened); len(got) < 3 || !reflect.DeepEqual(got[:3], postgresDumpLedger(t)) {
		t.Fatalf("Go 新建的 schema 与夹具账本不同：%+v", got)
	}
}
