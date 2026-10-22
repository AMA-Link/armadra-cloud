package core_test

import (
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 锁定：连续 5 次失败锁 1 分钟，此后每多一次翻倍，15 分钟封顶；最后一次失败一小时后清零。
func TestLockoutSchedule(t *testing.T) {
	const minute = int64(60_000)
	var lock core.Lockout
	now := int64(1_000_000)
	wantLock := []int64{0, 0, 0, 0, 1 * minute, 2 * minute, 4 * minute, 8 * minute, 15 * minute, 15 * minute, 15 * minute}
	for i, want := range wantLock {
		lock = core.NextLockout(lock, now)
		if lock.Failures != int64(i+1) {
			t.Fatalf("第 %d 次失败后 failures = %d", i+1, lock.Failures)
		}
		got := int64(0)
		if lock.LockedUntilMs > 0 {
			got = lock.LockedUntilMs - now
		}
		if got != want {
			t.Fatalf("第 %d 次失败后锁 %d 毫秒，应为 %d", i+1, got, want)
		}
		now = max(now, lock.LockedUntilMs) + 1 // 锁过了再失败
	}
	// 最后一次失败一小时后清零（锁已过）。
	next := core.NextLockout(lock, lock.LastFailureAtMs+3_600_001+15*minute)
	if next.Failures != 1 || next.LockedUntilMs != 0 {
		t.Fatalf("一小时后应清零：%+v", next)
	}
	// 还锁着时不算过期，哪怕离上次失败超过一小时。
	locked := core.Lockout{Failures: 5, LockedUntilMs: 10_000_000, LastFailureAtMs: 1}
	if next := core.NextLockout(locked, 5_000_000); next.Failures != 6 {
		t.Fatalf("锁着时继续累计：%+v", next)
	}
}

// 账号名不对也计数（只在内存，随同一个 Accounts 实例），连错 5 次同样锁定，不泄露账号名是否存在。
func TestUnknownAccountNamesLockToo(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	accounts := core.NewAccounts(e.h.Context)
	attempt := func(name, secret string) error {
		_, err := accounts.Login(e.ctx, core.LoginInput{Account: name, Password: secret, Device: core.DeviceInput{Platform: "desktop", Name: "t"}}, "198.51.100.1")
		return err
	}
	for i := 0; i < 5; i++ {
		eq(t, code(attempt("ghost", "whatever-password")), "credentials_invalid", "未知账号")
	}
	err := attempt("ghost", "whatever-password")
	eq(t, code(err), "account_locked", "第 6 次")
	var failure *core.Error
	if !asError(err, &failure) || failure.RetryAfter != time.Minute {
		t.Fatalf("应带 1 分钟的重试等待：%+v", err)
	}
	// 真账号不受影响。
	eq(t, code(attempt("alice", password)), "ok", "真账号")
	// 过了锁定时间又能试。
	e.h.Clock.Set(e.h.Clock.Ms() + 60_001)
	eq(t, code(attempt("ghost", "whatever-password")), "credentials_invalid", "锁过了")
}

func TestLoginClearsFailuresAndHonoursLockWindow(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	for i := 0; i < 4; i++ {
		_, err := e.login("alice", "wrong-password-xx")
		eq(t, code(err), "credentials_invalid", "错口令")
	}
	_, err := e.login("alice", password)
	eq(t, code(err), "ok", "第 5 次是对的")
	account, _ := e.h.Context.Ports.Accounts.Get(e.ctx, "t1", "acct:alice")
	if account.Failures != 0 || account.LockedUntilMs != 0 {
		t.Fatalf("成功后应清零：%+v", account)
	}
	for i := 0; i < 5; i++ {
		_, _ = e.login("alice", "wrong-password-xx")
	}
	_, err = e.login("alice", password)
	eq(t, code(err), "account_locked", "锁着")
	e.h.Clock.Set(e.h.Clock.Ms() + 60_001)
	_, err = e.login("alice", password)
	eq(t, code(err), "ok", "锁过了")
}

func TestRefreshRejections(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	session, err := e.login("alice", password)
	e.must(err)

	_, err = core.Refresh(e.ctx, e.h.Context, "不是令牌")
	eq(t, code(err), "session_revoked", "格式不对")
	_, err = core.Refresh(e.ctx, e.h.Context, "ffffffffffffffffffffffffffffffff.xxxx")
	eq(t, code(err), "session_revoked", "设备不存在")
	_, err = core.Refresh(e.ctx, e.h.Context, session.Device.DeviceID+".错的")
	eq(t, code(err), "session_revoked", "令牌不对")
	// 设备过期。
	e.h.Clock.Set(session.ExpiresAtMs + 1)
	_, err = core.Refresh(e.ctx, e.h.Context, session.RefreshToken)
	eq(t, code(err), "session_expired", "设备过期")
	// 令牌明文不落库：库里只有哈希。
	device, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", session.Device.DeviceID)
	if device.RefreshHash == session.RefreshToken || len(device.RefreshHash) != 64 {
		t.Fatalf("库里只该有哈希：%q", device.RefreshHash)
	}
}

func TestAccessTokenAuthentication(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	session, err := e.login("alice", password)
	e.must(err)
	auth := core.NewAuthenticator(e.h.Context)

	for name, header := range map[string]string{
		"没有头":       "",
		"不是 Bearer": "Basic abc",
		"令牌乱码":      "Bearer abc.def.ghi",
		"改过签名":      "Bearer " + session.AccessToken[:len(session.AccessToken)-4] + "AAAA",
	} {
		_, err := auth.Authenticate(e.ctx, header)
		eq(t, code(err), "unauthenticated", name)
	}
	caller, err := auth.Authenticate(e.ctx, "bearer "+session.AccessToken) // 大小写不敏感
	e.must(err)
	eq(t, caller.Role, core.RoleOwner, "角色")

	// 过期：15 分钟 + 5 分钟的时钟偏差之后。
	e.h.Clock.Set(e.h.Clock.Ms() + 21*60_000)
	_, err = auth.Authenticate(e.ctx, "Bearer "+session.AccessToken)
	eq(t, code(err), "unauthenticated", "过期")

	// 登出即时生效。
	e.h.Clock.Set(coretest.DefaultNow)
	_, err = auth.Authenticate(e.ctx, "Bearer "+session.AccessToken)
	e.must(err)
	_, err = core.RevokeDevice(e.ctx, e.h.Context, session.Device.DeviceID)
	e.must(err)
	_, err = auth.Authenticate(e.ctx, "Bearer "+session.AccessToken)
	eq(t, code(err), "unauthenticated", "登出之后")
}

func TestLastSeenIsBatchedInMemoryUntilFlush(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	session, err := e.login("alice", password)
	e.must(err)
	auth := core.NewAuthenticator(e.h.Context)
	before, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", session.Device.DeviceID)

	e.h.Clock.Set(coretest.DefaultNow + 120_000)
	_, err = auth.Authenticate(e.ctx, "Bearer "+session.AccessToken)
	e.must(err)
	mid, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", session.Device.DeviceID)
	if *mid.LastSeenAtMs != *before.LastSeenAtMs {
		t.Fatal("认证只记在内存里，不立刻写库")
	}
	e.must(auth.Flush(e.ctx))
	after, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", session.Device.DeviceID)
	if *after.LastSeenAtMs != coretest.DefaultNow+120_000 {
		t.Fatalf("Flush 之后应落库：%d", *after.LastSeenAtMs)
	}
	e.must(auth.Flush(e.ctx)) // 空批量不报错
}

func TestChangePasswordByNameRevokesAllOwnDevicesButNotGuests(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	guest := store.DeviceRecord{DeviceID: "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", Subject: "guest:" + "1111111111111111" + ":ab", Platform: "browser", Name: "g", RefreshHash: "x", Rotation: 1, ExpiresAtMs: coretest.DefaultNow + 1_000_000_000}
	e.must(e.h.Context.Ports.Devices.Insert(e.ctx, "t1", guest, coretest.DefaultNow))
	e.must(core.ChangePasswordByName(e.ctx, e.h.Context, "alice", "saffron-glacier-meadow-77", coretest.DefaultNow))
	device, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", owner.DeviceID)
	if device.RevokedAtMs == nil {
		t.Fatal("本人设备应撤销")
	}
	g, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", guest.DeviceID)
	if g.RevokedAtMs != nil {
		t.Fatal("访客设备不受影响")
	}
	// 默认策略：含账号名的口令被拒。
	eq(t, code(core.ChangePasswordByName(e.ctx, e.h.Context, "alice", "alice-is-my-name-1", 1)), "password_contains_name", "含账号名")
	eq(t, code(core.ChangePasswordByName(e.ctx, e.h.Context, "alice", "password1234", 1)), "password_too_common", "常见口令")
}
