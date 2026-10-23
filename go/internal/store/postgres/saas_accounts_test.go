package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func saasAccount(id, email string) saas.Account {
	return saas.Account{
		AccountRecord: store.AccountRecord{
			AccountID: id, Name: id, CreatedAtMs: 1, UpdatedAtMs: 1,
			Password: identity.PasswordRecord{KDF: "scrypt", Cost: 16384, Block: 8, Parallel: 1, Length: 32, Salt: "c2FsdA==", Hash: "aGFzaA=="},
		},
		Email: email, DisplayName: "Ann", Locale: "en",
	}
}

func TestSaaSAccountCreateAndRead(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Accounts().Create(ctx, saasTenant, saasAccount("a1", "Ann@Example.com"))
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read().AccountByEmail(ctx, saasTenant, "ANN@example.COM")
	if err != nil || got == nil || got.AccountID != "a1" || got.Email != "ann@example.com" || got.Locale != "en" || got.Status != saas.AccountActive || got.DisplayName != "Ann" {
		t.Fatalf("按邮箱读（大小写不敏感）：%+v %v", got, err)
	}
	if none, err := s.Read().AccountByEmail(ctx, "default", "ann@example.com"); err != nil || none != nil {
		t.Fatalf("别的租户读不到：%+v %v", none, err)
	}
	// 邮箱唯一：映射成 email_taken。
	err = s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Accounts().Create(ctx, saasTenant, saasAccount("a2", "ann@example.com"))
	})
	if !errors.Is(err, saas.ErrEmailTaken) {
		t.Fatalf("邮箱冲突：%v", err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Accounts().SetDisplayName(ctx, saasTenant, "a1", "Annie", 9)
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Read().Account(ctx, saasTenant, "a1")
	if got.DisplayName != "Annie" || got.UpdatedAtMs != 9 {
		t.Fatalf("改显示名：%+v", got)
	}
	// 核心端口也读得到这个账号（口令、计数），且 SaaS 列不被它改写。
	core, err := e.db.Ports().Accounts.Get(ctx, saasTenant, "a1")
	if err != nil || core == nil || core.Name != "a1" {
		t.Fatalf("核心端口读：%+v %v", core, err)
	}
}

func TestSaaSMembershipsAndIncident(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	e.account("a1")
	e.team("t1", "a1")
	e.team("t2", "a1")
	e.must("UPDATE teams SET deleted_at_ms = 5, purge_at_ms = 9 WHERE team_id = 't2'")
	teams, err := s.Read().Memberships(ctx, saasTenant, "a1")
	if err != nil || len(teams) != 1 || teams[0].TeamID != "t1" || teams[0].Role != saas.TeamRole("owner") {
		t.Fatalf("成员关系应排除已删除的团队：%+v %v", teams, err)
	}
	incident, err := s.Read().Incident(ctx, saasTenant)
	if err != nil || incident != (saas.Incident{}) {
		t.Fatalf("没写过事故开关应全关：%+v %v", incident, err)
	}
	e.must(`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms)
	  VALUES ($1, 'incident', '{"registrationsPaused": true}', 1)`, saasTenant)
	incident, err = s.Read().Incident(ctx, saasTenant)
	if err != nil || !incident.RegistrationsPaused || incident.AssertionsPaused {
		t.Fatalf("事故开关：%+v %v", incident, err)
	}
	e.must(`UPDATE platform_settings SET value = '[1]' WHERE key = 'incident'`)
	if _, err := s.Read().Incident(ctx, saasTenant); store.KindOf(err) != store.KindCorrupt {
		t.Fatalf("坏的事故开关应拒绝：%v", err)
	}
}

// TxPorts：核心端口与 SaaS 表在同一个事务里，回滚一起回滚；不改租户修订号、不锁 tenants 行。
func TestTxPortsShareTransaction(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	var revisionBefore int64
	if err := e.pool.QueryRow(ctx, "SELECT revision FROM tenants WHERE tenant_id = $1", saasTenant).Scan(&revisionBefore); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Accounts().Create(ctx, saasTenant, saasAccount("a1", "a1@x.test")); err != nil {
			return err
		}
		device := store.DeviceRecord{DeviceID: "d1", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99, Transport: "cookie"}
		if err := tx.Ports().Devices.Insert(ctx, saasTenant, device, 1); err != nil {
			return err
		}
		// 同事务里读得到自己刚写的。
		got, err := tx.Ports().Devices.Get(ctx, saasTenant, "d1")
		if err != nil || got == nil || got.Transport != "cookie" {
			return errors.New("本事务读不到自己的写")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if e.count("SELECT count(*)::int FROM accounts") != 0 || e.count("SELECT count(*)::int FROM devices") != 0 {
		t.Fatal("回滚应连核心表一起回滚")
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Accounts().Create(ctx, saasTenant, saasAccount("a1", "a1@x.test")); err != nil {
			return err
		}
		return tx.Ports().Devices.Insert(ctx, saasTenant, store.DeviceRecord{DeviceID: "d1", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	var revisionAfter int64
	if err := e.pool.QueryRow(ctx, "SELECT revision FROM tenants WHERE tenant_id = $1", saasTenant).Scan(&revisionAfter); err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("绑定模式不加租户修订号：%d → %d", revisionBefore, revisionAfter)
	}
}

// 绑定模式不锁 tenants 行：另一个事务对租户行持着 FOR SHARE（与独立模式的 FOR UPDATE 冲突、与外键检查的
// KEY SHARE 不冲突）时，SaaS 事务照样写设备；独立模式则被挡住。
func TestTxPortsDoNotLockTenantsRow(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM tenants WHERE tenant_id = $1 FOR SHARE", saasTenant); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	blockedCtx, cancelBlocked := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelBlocked()
	go func() {
		blocked <- e.db.Ports().Devices.Insert(blockedCtx, saasTenant, store.DeviceRecord{DeviceID: "d0", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99}, 1)
	}()
	done := make(chan error, 1)
	go func() {
		done <- s.Tx(ctx, func(tx saas.Tx) error {
			return tx.Ports().Devices.Insert(ctx, saasTenant, store.DeviceRecord{DeviceID: "d1", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99}, 1)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaaS 事务被 tenants 行锁挡住了")
	}
	if err := <-blocked; err == nil {
		t.Fatal("独立模式应被租户行锁挡住（对照）")
	}
}

// devices.Insert 的失效设备清理在绑定模式只看这个账号。
func TestTxPortsDeviceCleanupBySubject(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	day := int64(24 * 3600 * 1000)
	now := 100 * day
	stale := func(id, subject string) store.DeviceRecord {
		revoked := int64(1)
		return store.DeviceRecord{DeviceID: id, Subject: subject, Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 2, RevokedAtMs: &revoked}
	}
	for _, d := range []store.DeviceRecord{stale("old-a", "a1"), stale("old-b", "b1")} {
		if err := e.db.Ports().Devices.Insert(ctx, saasTenant, d, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Ports().Devices.Insert(ctx, saasTenant, store.DeviceRecord{DeviceID: "new-a", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: now, ExpiresAtMs: now + day}, now)
	}); err != nil {
		t.Fatal(err)
	}
	if e.count("SELECT count(*)::int FROM devices WHERE device_id = 'old-a'") != 0 {
		t.Fatal("同账号的失效设备应被清掉")
	}
	if e.count("SELECT count(*)::int FROM devices WHERE device_id = 'old-b'") != 1 {
		t.Fatal("别的账号的失效设备不该被扫到")
	}
	// 独立模式（个人中转）仍是按租户清理。
	if err := e.db.Ports().Devices.Insert(ctx, saasTenant, store.DeviceRecord{DeviceID: "new-c", Subject: "c1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: now, ExpiresAtMs: now + day}, now); err != nil {
		t.Fatal(err)
	}
	if e.count("SELECT count(*)::int FROM devices WHERE device_id = 'old-b'") != 0 {
		t.Fatal("独立模式仍清整个租户")
	}
}

// 统一加锁顺序：团队 → 账号。反序在事务里直接报错；正序的并发事务不死锁。
func TestLockOrderTeamBeforeAccount(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	s := e.db.SaaS()
	e.account("a1")
	e.account("a2")
	e.team("t1", "a1")
	err := s.Tx(ctx, func(tx saas.Tx) error {
		if _, err := tx.Accounts().LockAccount(ctx, saasTenant, "a1"); err != nil {
			return err
		}
		_, err := tx.Teams().LockTeam(ctx, saasTenant, "t1")
		return err
	})
	if !errors.Is(err, saas.ErrLockOrder) {
		t.Fatalf("先账号后团队应报加锁顺序错误：%v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		account := []string{"a1", "a2"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Tx(ctx, func(tx saas.Tx) error {
				if _, err := tx.Teams().LockTeam(ctx, saasTenant, "t1"); err != nil {
					return err
				}
				_, err := tx.Accounts().LockAccount(ctx, saasTenant, account)
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// SaaSPorts：每次写自己的事务，不锁 tenants 行、不加租户修订号；读-改-写靠行锁。
func TestSaaSPortsDoNotTouchTenantsRow(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM tenants WHERE tenant_id = $1 FOR SHARE", saasTenant); err != nil {
		t.Fatal(err)
	}
	var before int64
	_ = e.pool.QueryRow(ctx, "SELECT revision FROM tenants WHERE tenant_id = $1", saasTenant).Scan(&before)
	ports := e.db.SaaSPorts()
	done := make(chan error, 1)
	go func() {
		done <- ports.Devices.Insert(ctx, saasTenant, store.DeviceRecord{DeviceID: "d1", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99}, 1)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaaSPorts 被 tenants 行锁挡住了")
	}
	if err := ports.Devices.Update(ctx, saasTenant, "d1", func(d *store.DeviceRecord) error { d.Rotation = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	var after int64
	_ = e.pool.QueryRow(ctx, "SELECT revision FROM tenants WHERE tenant_id = $1", saasTenant).Scan(&after)
	if after != before {
		t.Fatalf("不加租户修订号：%d → %d", before, after)
	}
}
