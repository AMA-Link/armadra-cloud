package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 绑定模式不锁 tenants 行，读-改-写靠行锁：并发的读-改-写按行排队，不丢更新。

// 并发 Refresh 同一枚刷新令牌只成功一次（后来的读到旋转后的行，当作重放）。
func TestBoundRefreshSameTokenSucceedsOnce(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	h := coretest.NewHarness(t, e.db, coretest.Options{TenantID: "t2", Issuer: "https://c2.test"})
	base := h.Context
	nowMs := h.Clock.Ms()
	device, refresh := core.NewDevice("acct:a", core.DeviceInput{Platform: "browser", Name: "n"}, core.TransportBearer, core.SessionScopeSession, nowMs, 0)
	if err := base.Ports.Devices.Insert(ctx, base.TenantID, device, nowMs); err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.db.SaaS().Tx(ctx, func(tx saas.Tx) error {
				bound := base.Bound(tx.Ports(), nil)
				if _, err := core.Refresh(ctx, bound, refresh); err != nil {
					return err
				}
				ok.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("同一枚刷新令牌只能成功一次：%d", ok.Load())
	}
}

// 并发 Consume 同一枚注册令牌只成功一次。
func TestBoundConsumeSucceedsOnce(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	ports := e.db.Ports()
	added, err := ports.Registrations.Add(ctx, saasTenant, store.RegistrationToken{TokenHash: core.SecretHash(core.SecretRegistration, "tok"), ExpiresAtMs: 1 << 60, AccountID: "a1"}, 1, 10)
	if err != nil || !added {
		t.Fatal(added, err)
	}
	hash := core.SecretHash(core.SecretRegistration, "tok")
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.db.SaaS().Tx(ctx, func(tx saas.Tx) error {
				if _, got, err := tx.Ports().Registrations.Consume(ctx, saasTenant, hash, 2); err != nil {
					return err
				} else if got {
					ok.Add(1)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("同一枚注册令牌只能消耗一次：%d", ok.Load())
	}
}

// 并发的失败计数不丢。
func TestBoundFailureCountsAreNotLost(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	e.account("a1")
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.db.SaaS().Tx(ctx, func(tx saas.Tx) error {
				return tx.Ports().Accounts.Update(ctx, saasTenant, "a1", func(a *store.AccountRecord) error {
					a.Failures++
					return nil
				})
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	account, err := e.db.Ports().Accounts.Get(ctx, saasTenant, "a1")
	if err != nil || account.Failures != n {
		t.Fatalf("失败计数应是 %d：%v %v", n, account, err)
	}
}

// 同类多行按 id 升序加锁；批量接口自己排序；失败的锁不留状态。
func TestLockOrderWithinKind(t *testing.T) {
	e := openSaaS(t)
	ctx := e.ctx
	for _, id := range []string{"a1", "a2", "a3"} {
		e.account(id)
	}
	s := e.db.SaaS()
	err := s.Tx(ctx, func(tx saas.Tx) error {
		if _, err := tx.Accounts().LockAccount(ctx, saasTenant, "a2"); err != nil {
			return err
		}
		_, err := tx.Accounts().LockAccount(ctx, saasTenant, "a1")
		return err
	})
	if !errors.Is(err, saas.ErrLockOrder) {
		t.Fatalf("账号 id 反序应报错：%v", err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Accounts().LockAccounts(ctx, saasTenant, "a3", "a1", "a2", "a1")
	}); err != nil {
		t.Fatalf("批量接口自己排序：%v", err)
	}
	e.team("t1", "a1")
	e.team("t2", "a1")
	err = s.Tx(ctx, func(tx saas.Tx) error {
		if _, err := tx.Teams().LockTeam(ctx, saasTenant, "t2"); err != nil {
			return err
		}
		_, err := tx.Teams().LockTeam(ctx, saasTenant, "t1")
		return err
	})
	if !errors.Is(err, saas.ErrLockOrder) {
		t.Fatalf("团队 id 反序应报错：%v", err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error { return tx.Teams().LockTeams(ctx, saasTenant, "t2", "t1") }); err != nil {
		t.Fatal(err)
	}
	// 锁失败（上下文已取消）不留 locked.account：之后还能锁团队。
	err = s.Tx(ctx, func(tx saas.Tx) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := tx.Accounts().LockAccount(cancelled, saasTenant, "a1"); err == nil {
			t.Error("取消的上下文应让加锁失败")
		}
		_, err := tx.Teams().LockTeam(ctx, saasTenant, "t1")
		return err
	})
	if err != nil {
		t.Fatalf("失败的账号锁不该挡住后面的团队锁：%v", err)
	}
}
