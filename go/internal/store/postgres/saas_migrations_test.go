package postgres_test

// 迁移 0004–0009（SaaS）的测试，对应设计 §3.5。直接用 SQL 验证 DDL 的约束，端口相关的用端口。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

const saasTenant = "cloud"

type saasEnv struct {
	t    *testing.T
	ctx  context.Context
	db   *postgres.DB
	pool *pgxpool.Pool
}

func openSaaS(t *testing.T) *saasEnv {
	t.Helper()
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &saasEnv{t: t, ctx: ctx, db: db, pool: db.Pool()}
	if err := db.CreateTenant(ctx, saasTenant, store.Tenant{TenantSettings: store.TenantSettings{
		Issuer: "https://cloud.armadra.test", AllowGuests: true, TrustedOrigins: []string{},
	}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *saasEnv) exec(sql string, args ...any) error {
	_, err := e.pool.Exec(e.ctx, sql, args...)
	return err
}

func (e *saasEnv) must(sql string, args ...any) {
	e.t.Helper()
	if err := e.exec(sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *saasEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// reject 断言 err 是 PostgreSQL 的 code 错误，constraint 非空时约束 / 索引名也要对上。
func reject(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("应被数据库拒绝（%s %s）：%v", code, constraint, err)
	}
	if pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Fatalf("拒绝的原因不对：want %s %s, got %s %s (%s)", code, constraint, pgErr.Code, pgErr.ConstraintName, pgErr.Message)
	}
}

func (e *saasEnv) account(id string) {
	e.t.Helper()
	e.must(`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
	  last_failure_at_ms, created_at_ms, updated_at_ms, email) VALUES ($1, $2, $2, '{}', 0, 0, 0, 1, 1, $2 || '@x.test')`,
		saasTenant, id)
}

func (e *saasEnv) team(id string, owners ...string) {
	e.t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	slug := "t-" + id
	if _, err := tx.Exec(e.ctx, `INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
	  VALUES ($1, $2, $3, $2, '{}', 'x', 1, 1)`, saasTenant, id, slug); err != nil {
		e.t.Fatal(err)
	}
	for _, o := range owners {
		if _, err := tx.Exec(e.ctx, `INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms)
		  VALUES ($1, $2, $3, 'owner', 1)`, saasTenant, id, o); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *saasEnv) source(id string) {
	e.t.Helper()
	_, err := e.db.Ports().Sources.Put(e.ctx, saasTenant, id, func(*store.SourceRecord) (store.SourceRecord, error) {
		return store.SourceRecord{
			SourceID: id, Name: "s-" + id, Kind: "desktop",
			PublicJWK:      gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
			CoreVersion:    "0.3.0",
			Capabilities:   []string{},
			RegisteredAtMs: 1,
			TrustedOrigins: []string{},
		}, nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// 空库连续跑 0001–0009：账本连续，SaaS 的表都在。
func TestSaaSMigrationsFromEmpty(t *testing.T) {
	e := openSaaS(t)
	rows, err := e.pool.Query(e.ctx, "SELECT id, name FROM cloud_migrations ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{
		"0001_control_init", "0002_relay_control", "0003_traffic", "0004_saas_accounts", "0005_saas_teams",
		"0006_saas_hosts", "0007_saas_grants", "0008_saas_audit", "0009_saas_ops",
	}
	var got []string
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		if id != len(got)+1 {
			t.Fatalf("账本不连续：%d 排在第 %d 位", id, len(got)+1)
		}
		got = append(got, name)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("账本：%v", got)
	}
	for _, table := range []string{
		"account_credentials", "email_tokens", "device_codes", "teams", "team_members", "groups", "group_members",
		"source_spaces", "entitlements", "source_grants", "invitations", "trust_requests", "audit_log",
		"audit_log_default", "jobs", "outbox_emails", "platform_settings", "cloud_issued_tokens",
	} {
		if e.count("SELECT count(*)::int FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1", table) != 1 {
			t.Errorf("缺表 %s", table)
		}
	}
}

// 对停在 0003 的库补跑 0004–0009：老行取缺省，现有端口照常读写。
func TestSaaSMigrationsOnOldDatabase(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	all, err := postgres.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 9 {
		t.Fatalf("随包迁移应有 9 条：%d", len(all))
	}
	old, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema, Migrations: all[:3]})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(old.CreateTenant(ctx, store.DefaultTenant, store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://relay.armadra.test", TrustedOrigins: []string{}}}))
	must(old.AddAccount(ctx, store.DefaultTenant, store.AccountRecord{
		AccountID: "acct:alice", Name: "alice", CreatedAtMs: 1, UpdatedAtMs: 1,
		Password: identity.PasswordRecord{KDF: "scrypt", Cost: 16384, Block: 8, Parallel: 1, Length: 32, Salt: "c2FsdA==", Hash: "aGFzaA=="},
	}))
	must(old.Ports().Devices.Insert(ctx, store.DefaultTenant, store.DeviceRecord{
		DeviceID: "d1", Subject: "acct:alice", Platform: "desktop", Name: "d", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 9,
	}, 1))
	added, err := old.Ports().Registrations.Add(ctx, store.DefaultTenant, store.RegistrationToken{TokenHash: "r1", ExpiresAtMs: 99, AccountID: "acct:alice"}, 1, 10)
	if err != nil || !added {
		t.Fatalf("登记令牌：%v %v", added, err)
	}
	if _, err := old.Pool().Exec(ctx, `INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind)
	  VALUES ('default', 0, 'n', 's', 'acct:alice', 'e2e')`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()

	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*)::int FROM cloud_migrations").Scan(&n); err != nil || n != 9 {
		t.Fatalf("账本应有 9 条：%d %v", n, err)
	}
	var email, reason *string
	var verified *int64
	var display, locale, status string
	var scopes []string
	if err := db.Pool().QueryRow(ctx, `SELECT email, email_verified_at_ms, display_name, locale, status, status_reason, scopes
	  FROM accounts WHERE account_id = 'acct:alice'`).Scan(&email, &verified, &display, &locale, &status, &reason, &scopes); err != nil {
		t.Fatal(err)
	}
	if email != nil || verified != nil || display != "" || locale != "zh-CN" || status != "active" || reason != nil || len(scopes) != 0 {
		t.Fatalf("老账号的新列应取缺省：%v %v %q %q %q %v %v", email, verified, display, locale, status, reason, scopes)
	}
	var transport, scope string
	var elevated *int64
	if err := db.Pool().QueryRow(ctx, "SELECT transport, scope, elevated_until_ms FROM devices WHERE device_id = 'd1'").Scan(&transport, &scope, &elevated); err != nil {
		t.Fatal(err)
	}
	if transport != "bearer" || scope != "session" || elevated != nil {
		t.Fatalf("老设备的新列应取缺省：%q %q %v", transport, scope, elevated)
	}
	var teamID *string
	if err := db.Pool().QueryRow(ctx, "SELECT team_id FROM registration_tokens WHERE token_hash = 'r1'").Scan(&teamID); err != nil || teamID != nil {
		t.Fatalf("老注册令牌的 team_id 应为 NULL：%v %v", teamID, err)
	}
	var bucketTeam string
	if err := db.Pool().QueryRow(ctx, "SELECT team_id FROM relay_traffic_buckets WHERE node = 'n'").Scan(&bucketTeam); err != nil || bucketTeam != "" {
		t.Fatalf("老流量桶的 team_id 应为空串：%q %v", bucketTeam, err)
	}
	// 老行经现有端口仍可读写。
	account, err := db.Ports().Accounts.Get(ctx, store.DefaultTenant, "acct:alice")
	if err != nil || account == nil {
		t.Fatalf("端口读老账号：%v %v", account, err)
	}
}

// 现有端口的 UPDATE 不改写新列。
func TestSaaSColumnsSurvivePortUpdates(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	e.account("a1")
	e.must(`UPDATE accounts SET display_name = 'Ann', locale = 'en', status = 'disabled', status_reason = 'x',
	  email_verified_at_ms = 5, scopes = '{operator}' WHERE account_id = 'a1'`)
	if err := e.db.Ports().Accounts.Update(ctx, saasTenant, "a1", func(d *store.AccountRecord) error {
		d.Failures = 3
		d.LockedUntilMs = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found, err := e.db.Ports().Accounts.ResetPassword(ctx, saasTenant, "a1", identity.PasswordRecord{
		KDF: "scrypt", Cost: 16384, Block: 8, Parallel: 1, Length: 32, Salt: "c2FsdA==", Hash: "bmV3",
	}, 9, ""); err != nil || !found {
		t.Fatalf("重置口令：%v %v", found, err)
	}
	var email, display, locale, status, reason string
	var verified int64
	var scopes []string
	if err := e.pool.QueryRow(ctx, `SELECT email, display_name, locale, status, status_reason, email_verified_at_ms, scopes
	  FROM accounts WHERE account_id = 'a1'`).Scan(&email, &display, &locale, &status, &reason, &verified, &scopes); err != nil {
		t.Fatal(err)
	}
	if email != "a1@x.test" || display != "Ann" || locale != "en" || status != "disabled" || reason != "x" || verified != 5 || len(scopes) != 1 || scopes[0] != "operator" {
		t.Fatalf("账号新列被端口改写：%q %q %q %q %q %d %v", email, display, locale, status, reason, verified, scopes)
	}

	if err := e.db.Ports().Devices.Insert(ctx, saasTenant, store.DeviceRecord{
		DeviceID: "d1", Subject: "a1", Platform: "web", Name: "d", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99,
	}, 1); err != nil {
		t.Fatal(err)
	}
	e.must("UPDATE devices SET transport = 'cookie', scope = 'operator', elevated_until_ms = 77 WHERE device_id = 'd1'")
	if err := e.db.Ports().Devices.Update(ctx, saasTenant, "d1", func(d *store.DeviceRecord) error { d.Rotation = 2; d.RefreshHash = "h2"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Ports().Devices.Touch(ctx, saasTenant, map[string]int64{"d1": 50}); err != nil {
		t.Fatal(err)
	}
	var transport, scope string
	var elevated int64
	if err := e.pool.QueryRow(ctx, "SELECT transport, scope, elevated_until_ms FROM devices WHERE device_id = 'd1'").Scan(&transport, &scope, &elevated); err != nil {
		t.Fatal(err)
	}
	if transport != "cookie" || scope != "operator" || elevated != 77 {
		t.Fatalf("设备新列被端口改写：%q %q %d", transport, scope, elevated)
	}
}

func TestTeamOwnerGuard(t *testing.T) {
	t.Run("建团队不插 owner 在提交时失败", func(t *testing.T) {
		e := openSaaS(t)
		err := e.exec(`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
		  VALUES ($1, 'tx', 'tx', 'x', '{}', 'x', 1, 1)`, saasTenant)
		reject(t, err, "23514", "team_has_owner")
		if n := e.count("SELECT count(*)::int FROM teams"); n != 0 {
			t.Fatalf("失败后不该留下团队：%d", n)
		}
	})
	t.Run("删唯一 owner 失败", func(t *testing.T) {
		e := openSaaS(t)
		e.account("a1")
		e.team("t1", "a1")
		reject(t, e.exec("DELETE FROM team_members WHERE account_id = 'a1'"), "23514", "team_has_owner")
		if n := e.count("SELECT count(*)::int FROM team_members"); n != 1 {
			t.Fatalf("owner 应还在：%d", n)
		}
	})
	t.Run("降级唯一 owner 失败", func(t *testing.T) {
		e := openSaaS(t)
		e.account("a1")
		e.team("t1", "a1")
		reject(t, e.exec("UPDATE team_members SET role = 'admin' WHERE account_id = 'a1'"), "23514", "team_has_owner")
	})
	t.Run("删账号级联删掉唯一 owner 同样失败", func(t *testing.T) {
		e := openSaaS(t)
		e.account("a1")
		e.team("t1", "a1")
		reject(t, e.exec("DELETE FROM accounts WHERE account_id = 'a1'"), "23514", "team_has_owner")
	})
	t.Run("同一事务先升后降成功", func(t *testing.T) {
		e := openSaaS(t)
		e.account("a1")
		e.account("a2")
		e.team("t1", "a1")
		e.must(`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ($1, 't1', 'a2', 'member', 1)`, saasTenant)
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if _, err := tx.Exec(e.ctx, "UPDATE team_members SET role = 'owner' WHERE account_id = 'a2'"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.ctx, "UPDATE team_members SET role = 'admin' WHERE account_id = 'a1'"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(e.ctx); err != nil {
			t.Fatalf("转移所有权应成功：%v", err)
		}
		if n := e.count("SELECT count(*)::int FROM team_members WHERE role = 'owner' AND account_id = 'a2'"); n != 1 {
			t.Fatal("a2 应是 owner")
		}
	})
	t.Run("两个并发事务各降一个 owner 恰好一个失败", func(t *testing.T) {
		e := openSaaS(t)
		e.account("a1")
		e.account("a2")
		e.team("t1", "a1", "a2")
		var txs [2]pgx.Tx
		for i, id := range []string{"a1", "a2"} {
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(e.ctx) }()
			if _, err := tx.Exec(e.ctx, "UPDATE team_members SET role = 'admin' WHERE account_id = $1", id); err != nil {
				t.Fatal(err)
			}
			txs[i] = tx
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range txs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = txs[i].Commit(e.ctx) }()
		}
		wg.Wait()
		failed := 0
		for _, err := range errs {
			if err != nil {
				reject(t, err, "23514", "team_has_owner")
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("应恰好一个失败：%v", errs)
		}
		if n := e.count("SELECT count(*)::int FROM team_members WHERE role = 'owner'"); n != 1 {
			t.Fatalf("应剩一个 owner：%d", n)
		}
	})
}

// 引用 sources 的外键延迟到提交：Put（同事务删了再插）照常成功；真删源而子行还在则提交失败。
func TestDeferredSourceForeignKeys(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.source("s1")
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 's1', 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO source_grants (tenant_id, grant_id, source_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ($1, 'g1', 's1', 'member', 'a1', 'viewer', 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, source_id, host_role, state, invited_by, created_at_ms, expires_at_ms)
	  VALUES ($1, 'i1', 'host', 'x@x.test', 's1', 'viewer', 'pending', 'a1', 1, 9)`, saasTenant)
	e.must(`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, account_id, device_id, device_name, platform, state, requested_at_ms, expires_at_ms)
	  VALUES ($1, 'r1', 's1', 'p1', 'a1', 'd1', 'n', 'web', 'pending', 1, 9)`, saasTenant)

	for i := 0; i < 2; i++ {
		e.source("s1") // 删了再插
	}
	if n := e.count("SELECT count(*)::int FROM source_spaces WHERE source_id = 's1'"); n != 1 {
		t.Fatalf("重新登记后子行应还在：%d", n)
	}

	reject(t, e.exec("DELETE FROM sources WHERE source_id = 's1'"), "23503", "")
	if n := e.count("SELECT count(*)::int FROM sources WHERE source_id = 's1'"); n != 1 {
		t.Fatal("失败后源应还在")
	}
	// 不用级联：先删子行，再删源。
	for _, table := range []string{"source_spaces", "source_grants", "invitations", "trust_requests"} {
		e.must("DELETE FROM " + table)
	}
	e.must("DELETE FROM sources WHERE source_id = 's1'")
}

func TestSaaSChecksAndPartialIndexes(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.source("s1")

	t.Run("邮箱必须小写", func(t *testing.T) {
		reject(t, e.exec("UPDATE accounts SET email = 'A@X.test' WHERE account_id = 'a1'"), "23514", "")
		reject(t, e.exec(`INSERT INTO email_tokens (tenant_id, token_hash, account_id, purpose, email, created_at_ms, expires_at_ms)
		  VALUES ($1, 'h', 'a1', 'verify', 'A@X.test', 1, 2)`, saasTenant), "23514", "")
		reject(t, e.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, team_id, team_role, state, invited_by, created_at_ms, expires_at_ms)
		  VALUES ($1, 'ix', 'team', 'A@X.test', 't', 'member', 'pending', 'a1', 1, 2)`, saasTenant), "23514", "")
	})
	t.Run("账号邮箱唯一（同租户）", func(t *testing.T) {
		e.account("a2")
		reject(t, e.exec("UPDATE accounts SET email = 'a1@x.test' WHERE account_id = 'a2'"), "23505", "accounts_email")
	})
	t.Run("账号与设备的枚举列", func(t *testing.T) {
		reject(t, e.exec("UPDATE accounts SET locale = 'fr' WHERE account_id = 'a1'"), "23514", "")
		reject(t, e.exec("UPDATE accounts SET status = 'gone' WHERE account_id = 'a1'"), "23514", "")
	})
	t.Run("待处理的团队邀请按 (团队, 邮箱) 唯一，处理后可再邀", func(t *testing.T) {
		ins := func(id, state string) error {
			return e.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, team_id, team_role, state, invited_by, created_at_ms, expires_at_ms)
			  VALUES ($1, $2, 'team', 'n@x.test', 't1', 'member', $3, 'a1', 1, 2)`, saasTenant, id, state)
		}
		if err := ins("i1", "pending"); err != nil {
			t.Fatal(err)
		}
		reject(t, ins("i2", "pending"), "23505", "invitations_pending_team")
		e.must("UPDATE invitations SET state = 'revoked' WHERE invitation_id = 'i1'")
		if err := ins("i2", "pending"); err != nil {
			t.Fatalf("撤销后应可再邀：%v", err)
		}
	})
	t.Run("待处理的主机邀请按 (主机, 邮箱) 唯一", func(t *testing.T) {
		ins := func(id string) error {
			return e.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, source_id, host_role, state, invited_by, created_at_ms, expires_at_ms)
			  VALUES ($1, $2, 'host', 'h@x.test', 's1', 'viewer', 'pending', 'a1', 1, 2)`, saasTenant, id)
		}
		if err := ins("h1"); err != nil {
			t.Fatal(err)
		}
		reject(t, ins("h2"), "23505", "invitations_pending_host")
	})
	t.Run("邀请两类各自的必填列", func(t *testing.T) {
		reject(t, e.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, state, invited_by, created_at_ms, expires_at_ms)
		  VALUES ($1, 'bad1', 'team', 'z@x.test', 'accepted', 'a1', 1, 2)`, saasTenant), "23514", "")
		reject(t, e.exec(`INSERT INTO invitations (tenant_id, invitation_id, kind, email, source_id, host_role, team_role, state, invited_by, created_at_ms, expires_at_ms)
		  VALUES ($1, 'bad2', 'host', 'z@x.test', 's1', 'viewer', 'member', 'accepted', 'a1', 1, 2)`, saasTenant), "23514", "")
	})
	t.Run("同一主机对同一主体只有一条有效授予", func(t *testing.T) {
		ins := func(id string) error {
			return e.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
			  VALUES ($1, $2, 's1', 'member', 'a1', 'viewer', 'a1', 1, 1)`, saasTenant, id)
		}
		if err := ins("g1"); err != nil {
			t.Fatal(err)
		}
		reject(t, ins("g2"), "23505", "source_grants_active")
		e.must("UPDATE source_grants SET revoked_at_ms = 2, revoked_by = 'a1' WHERE grant_id = 'g1'")
		if err := ins("g2"); err != nil {
			t.Fatalf("撤销后应可再授：%v", err)
		}
		reject(t, e.exec(`INSERT INTO source_grants (tenant_id, grant_id, source_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
		  VALUES ($1, 'g3', 's1', 'robot', 'a1', 'viewer', 'a1', 1, 1)`, saasTenant), "23514", "")
	})
	t.Run("user_code 格式与唯一（待处理 / 已批准期间）", func(t *testing.T) {
		ins := func(hash, code, status string) error {
			return e.exec(`INSERT INTO device_codes (tenant_id, device_code_hash, user_code, status, platform, name, created_at_ms, expires_at_ms)
			  VALUES ($1, $2, $3, $4, 'cli', 'n', 1, 2)`, saasTenant, hash, code, status)
		}
		reject(t, ins("d0", "ABCD-EFGH", "pending"), "23514", "") // 含元音
		reject(t, ins("d0", "bcdf-ghjk", "pending"), "23514", "") // 小写
		if err := ins("d1", "BCDF-GHJK", "pending"); err != nil {
			t.Fatal(err)
		}
		reject(t, ins("d2", "BCDF-GHJK", "pending"), "23505", "device_codes_user_code")
		e.must("UPDATE device_codes SET status = 'consumed' WHERE device_code_hash = 'd1'")
		if err := ins("d2", "BCDF-GHJK", "pending"); err != nil {
			t.Fatalf("用完后码可复用：%v", err)
		}
	})
	t.Run("信任请求最长 24 小时", func(t *testing.T) {
		ins := func(id string, expires int64) error {
			return e.exec(`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, account_id, device_id, device_name, platform, state, requested_at_ms, expires_at_ms)
			  VALUES ($1, $2, 's1', $2, 'a1', $2, 'n', 'web', 'pending', 1000, $3)`, saasTenant, id, expires)
		}
		reject(t, ins("r1", 1000+86_400_001), "23514", "")
		if err := ins("r2", 1000+86_400_000); err != nil {
			t.Fatalf("恰好 24 小时应通过：%v", err)
		}
		// 同一主机同一待信任 id 幂等；同一设备同时只有一条 pending。
		reject(t, e.exec(`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, account_id, device_id, device_name, platform, state, requested_at_ms, expires_at_ms)
		  VALUES ($1, 'r3', 's1', 'r2', 'a1', 'other', 'n', 'web', 'pending', 1000, 2000)`, saasTenant), "23505", "trust_requests_idem")
		reject(t, e.exec(`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, account_id, device_id, device_name, platform, state, requested_at_ms, expires_at_ms)
		  VALUES ($1, 'r4', 's1', 'p4', 'a1', 'r2', 'n', 'web', 'pending', 1000, 2000)`, saasTenant), "23505", "trust_requests_pending")
	})
	t.Run("团队与主机的取值范围", func(t *testing.T) {
		reject(t, e.exec(`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
		  VALUES ($1, 'bad', 'Bad_Slug', 'n', '{}', 'x', 1, 1)`, saasTenant), "23514", "")
		reject(t, e.exec(`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms, deleted_at_ms)
		  VALUES ($1, 'bad', 'ok-slug', 'n', '{}', 'x', 1, 1, 5)`, saasTenant), "23514", "") // 删除时间与清除时间要成对
		reject(t, e.exec(`INSERT INTO source_spaces (tenant_id, source_id, owner_account_id, tags, created_at_ms, updated_at_ms)
		  VALUES ($1, 's1', 'a1', array_fill('x'::text, ARRAY[17]), 1, 1)`, saasTenant), "23514", "")
	})
	t.Run("团队 slug 唯一", func(t *testing.T) {
		e.account("o1")
		e.team("u1", "o1")
		reject(t, e.exec(`INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
		  VALUES ($1, 'u2', 't-u1', 'n', '{}', 'x', 1, 1)`, saasTenant), "23505", "teams_slug")
	})
}

// audit_ensure_partition：幂等；兜底分区里已有该月的行时搬进新分区，兜底分区随后为空且仍挂着。
func TestAuditEnsurePartition(t *testing.T) {
	e := openSaaS(t)
	month := func(y int, m time.Month) (int64, int64) {
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	}
	ensure := func(y int, m time.Month) error {
		from, to := month(y, m)
		return e.exec("SELECT audit_ensure_partition($1, $2, $3)", from, to, fmt.Sprintf("audit_log_%d_%02d", y, m))
	}
	insert := func(at int64, id string) {
		e.must(`INSERT INTO audit_log (at_ms, event_id, tenant_id, actor_kind, action) VALUES ($1, $2, $3, 'system', 'x.y')`, at, id, saasTenant)
	}
	children := func() map[string]bool {
		rows, err := e.pool.Query(e.ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent WHERE p.relname = 'audit_log' AND p.relnamespace = c.relnamespace`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out[name] = true
		}
		return out
	}

	if err := ensure(2026, time.November); err != nil {
		t.Fatal(err)
	}
	if err := ensure(2026, time.November); err != nil {
		t.Fatalf("重复建应幂等：%v", err)
	}
	if got := children(); !got["audit_log_2026_11"] || !got["audit_log_default"] || len(got) != 2 {
		t.Fatalf("分区：%v", got)
	}

	// 十二月还没有分区：行落进兜底分区。
	from, to := month(2026, time.December)
	insert(from+1, "e1")
	insert(to-1, "e2")
	insert(to, "e3") // 一月，不该被搬
	if n := e.count("SELECT count(*)::int FROM audit_log_default"); n != 3 {
		t.Fatalf("兜底分区应有 3 行：%d", n)
	}
	if err := ensure(2026, time.December); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*)::int FROM audit_log_2026_12"); n != 2 {
		t.Fatalf("新分区应搬来 2 行：%d", n)
	}
	if n := e.count("SELECT count(*)::int FROM audit_log_default"); n != 1 {
		t.Fatalf("兜底分区应只剩一月的 1 行：%d", n)
	}
	if got := children(); !got["audit_log_2026_12"] || !got["audit_log_default"] || len(got) != 3 {
		t.Fatalf("兜底分区应仍挂着：%v", got)
	}
	if n := e.count("SELECT count(*)::int FROM audit_log"); n != 3 {
		t.Fatalf("总行数不变：%d", n)
	}

	// 一月的兜底行被搬走后兜底分区为空。
	if err := ensure(2027, time.January); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*)::int FROM audit_log_default"); n != 0 {
		t.Fatalf("兜底分区应为空：%d", n)
	}
	// 有了分区后，该月的新行直接进分区。
	insert(from+5, "e4")
	if n := e.count("SELECT count(*)::int FROM audit_log_2026_12"); n != 3 {
		t.Fatalf("新行应进分区：%d", n)
	}
	reject(t, e.exec(`INSERT INTO audit_log (at_ms, event_id, tenant_id, actor_kind, action) VALUES (1, 'bad', 'cloud', 'robot', 'x')`), "23514", "")
	reject(t, e.exec(`INSERT INTO audit_log (at_ms, event_id, tenant_id, actor_kind, action, reason) VALUES (1, 'bad', 'cloud', 'system', 'x', repeat('r', 501))`), "23514", "")
}

func TestJobsSlotAndOutbox(t *testing.T) {
	e := openSaaS(t)
	enqueue := func(kind string, dedupe, slot *string) (int64, error) {
		var id int64
		err := e.pool.QueryRow(e.ctx, `INSERT INTO jobs (tenant_id, kind, payload, dedupe_key, slot_key, run_at_ms, created_at_ms)
		  VALUES ($1, $2, '{}', $3, $4, 1, 1) RETURNING job_id`, saasTenant, kind, dedupe, slot).Scan(&id)
		return id, err
	}
	s := func(v string) *string { return &v }

	slot := s("audit.retention:2026-10-23")
	id, err := enqueue("audit.retention", nil, slot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = enqueue("audit.retention", nil, slot)
	reject(t, err, "23505", "jobs_slot")
	e.must("UPDATE jobs SET done_at_ms = 5 WHERE job_id = $1", id)
	_, err = enqueue("audit.retention", nil, slot)
	reject(t, err, "23505", "jobs_slot") // 完成后同一槽仍冲突
	if _, err := enqueue("audit.retention", nil, s("audit.retention:2026-10-24")); err != nil {
		t.Fatalf("别的槽应可入队：%v", err)
	}

	dedupe := s("team-1")
	d1, err := enqueue("team.purge", dedupe, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = enqueue("team.purge", dedupe, nil)
	reject(t, err, "23505", "jobs_dedupe")
	if _, err := enqueue("other.kind", dedupe, nil); err != nil {
		t.Fatalf("不同 kind 不冲突：%v", err)
	}
	e.must("UPDATE jobs SET failed_at_ms = 5 WHERE job_id = $1", d1)
	if _, err := enqueue("team.purge", dedupe, nil); err != nil {
		t.Fatalf("上一个失败后应可再入队：%v", err)
	}

	e.must(`INSERT INTO outbox_emails (tenant_id, email_id, job_id, template, locale, to_email, params, message_id, created_at_ms)
	  VALUES ($1, 'm1', $2, 'verify', 'en', 'a@x.test', '{}', '<m1@x>', 1)`, saasTenant, d1)
	reject(t, e.exec(`INSERT INTO outbox_emails (tenant_id, email_id, template, locale, to_email, params, message_id, created_at_ms)
	  VALUES ($1, 'm2', 'verify', 'fr', 'a@x.test', '{}', '<m2@x>', 1)`, saasTenant), "23514", "")
	reject(t, e.exec(`INSERT INTO outbox_emails (tenant_id, email_id, template, locale, to_email, params, message_id, created_at_ms)
	  VALUES ($1, 'm3', 'verify', 'en', 'A@x.test', '{}', '<m3@x>', 1)`, saasTenant), "23514", "")
	e.must("DELETE FROM jobs WHERE job_id = $1", d1)
	var jobID *int64
	if err := e.pool.QueryRow(e.ctx, "SELECT job_id FROM outbox_emails WHERE email_id = 'm1'").Scan(&jobID); err != nil || jobID != nil {
		t.Fatalf("作业删除后 job_id 应置空：%v %v", jobID, err)
	}
}

func TestPlatformSettingsAndTrafficTeam(t *testing.T) {
	e := openSaaS(t)
	reject(t, e.exec("INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms) VALUES ('nope', 'k', '{}', 1)"), "23503", "")
	e.must("INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms) VALUES ($1, 'incident.readonly', 'true', 1)", saasTenant)
	e.must(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind, team_id)
	  VALUES ($1, 0, 'n', 's', 'a', 'e2e', 't1')`, saasTenant)
	if n := e.count("SELECT count(*)::int FROM relay_traffic_buckets WHERE team_id <> ''"); n != 1 {
		t.Fatalf("团队流量桶：%d", n)
	}
	e.must("INSERT INTO cloud_issued_tokens (jti, account_id, source_id, device_id, exp_ms) VALUES ('j1', 'a', 's', 'd', 9)")
	reject(t, e.exec("INSERT INTO cloud_issued_tokens (jti, account_id, source_id, device_id, exp_ms) VALUES ('j1', 'a', 's', 'd', 9)"), "23505", "")
}
