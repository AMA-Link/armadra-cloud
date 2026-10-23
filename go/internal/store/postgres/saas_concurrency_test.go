package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 绑定模式不锁 tenants 行，读-改-写靠行锁：并发的读-改-写按行排队，不丢更新。

// held 是一个做完 fn 但还没提交的 SaaS 事务：release 提交它并答事务的结果。
type held struct {
	release chan struct{}
	done    chan error
	once    sync.Once
}

func hold(t *testing.T, e *saasEnv, fn func(tx saas.Tx) error) *held {
	t.Helper()
	h := &held{release: make(chan struct{}), done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		h.done <- e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
			if err := fn(tx); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			<-h.release
			return nil
		})
	}()
	// 测试提前失败时也要放开事务，否则清理 schema 会被它挡住。
	t.Cleanup(func() { h.once.Do(func() { close(h.release) }) })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *held) commit() error {
	h.once.Do(func() { close(h.release) })
	return <-h.done
}

// stillRunning 报告一个后台事务在 wait 内是否没做完（被锁挡住）。
func stillRunning(done <-chan error, wait time.Duration) bool {
	select {
	case <-done:
		return false
	case <-time.After(wait):
		return true
	}
}

// 读-改-写靠行锁：事务 1 旋转了刷新令牌还没提交，事务 2 的同一枚令牌 Refresh 必须等它——读到旋转后的行，
// 按重放拒绝。没有 FOR UPDATE 时事务 2 读到旧行、等到事务 1 提交后覆盖，两边都成功（丢更新）。
func TestBoundRefreshSameTokenSucceedsOnce(t *testing.T) {
	e := openSaaS(t)
	h := coretest.NewHarness(t, e.db, coretest.Options{TenantID: "t2", Issuer: "https://c2.test"})
	base := h.Context
	nowMs := h.Clock.Ms()
	device, refresh := core.NewDevice("acct:a", core.DeviceInput{Platform: "browser", Name: "n"}, core.TransportBearer, core.SessionScopeSession, nowMs, 0)
	if err := base.Ports.Devices.Insert(e.ctx, base.TenantID, device, nowMs); err != nil {
		t.Fatal(err)
	}
	first := hold(t, e, func(tx saas.Tx) error {
		_, err := core.Refresh(e.ctx, base.Bound(tx.Ports(), nil), refresh)
		return err
	})
	second := make(chan error, 1)
	go func() {
		second <- e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
			_, err := core.Refresh(e.ctx, base.Bound(tx.Ports(), nil), refresh)
			return err
		})
	}()
	if !stillRunning(second, 400*time.Millisecond) {
		t.Fatal("事务 2 应被事务 1 持有的行锁挡住")
	}
	if err := first.commit(); err != nil {
		t.Fatal(err)
	}
	err := <-second
	if code, _ := core.CodeOf(err); code != "session_revoked" {
		t.Fatalf("事务 2 读到旋转后的行，当作重放：%v", err)
	}
}

// 同一枚注册令牌只能消耗一次：事务 1 消耗了还没提交，事务 2 的 Consume 读到令牌（不加行锁）、DELETE 等
// 事务 1 提交，之后影响 0 行，必须答「没命中」。去掉 RowsAffected()==1 的核对时它会答命中（重复消耗）。
func TestBoundConsumeSucceedsOnce(t *testing.T) {
	e := openSaaS(t)
	hash := core.SecretHash(core.SecretRegistration, "tok")
	added, err := e.db.Ports().Registrations.Add(e.ctx, saasTenant, store.RegistrationToken{TokenHash: hash, ExpiresAtMs: 1 << 60, AccountID: "a1"}, 1, 10)
	if err != nil || !added {
		t.Fatal(added, err)
	}
	var firstOK bool
	first := hold(t, e, func(tx saas.Tx) error {
		_, ok, err := tx.Ports().Registrations.Consume(e.ctx, saasTenant, hash, 2)
		firstOK = ok
		return err
	})
	type result struct {
		ok  bool
		err error
	}
	second := make(chan result, 1)
	secondDone := make(chan error, 1)
	go func() {
		err := e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
			_, ok, err := tx.Ports().Registrations.Consume(e.ctx, saasTenant, hash, 2)
			second <- result{ok, err}
			return err
		})
		secondDone <- err
	}()
	if !stillRunning(secondDone, 400*time.Millisecond) {
		t.Fatal("事务 2 的 DELETE 应等事务 1 提交")
	}
	if err := first.commit(); err != nil {
		t.Fatal(err)
	}
	<-secondDone
	got := <-second
	if !firstOK || got.err != nil || got.ok {
		t.Fatalf("只有第一个消耗得到：first=%v second=%+v", firstOK, got)
	}
}

// Consume 不锁别的令牌：事务 1 消耗 A 还没提交，事务 2 消耗 B 照样马上完成（否则全部主机登记被串行化）。
func TestConsumeDoesNotSerializeOtherTokens(t *testing.T) {
	e := openSaaS(t)
	a, b := core.SecretHash(core.SecretRegistration, "a"), core.SecretHash(core.SecretRegistration, "b")
	for _, h := range []string{a, b} {
		if added, err := e.db.Ports().Registrations.Add(e.ctx, saasTenant, store.RegistrationToken{TokenHash: h, ExpiresAtMs: 1 << 60, AccountID: "a1"}, 1, 10); err != nil || !added {
			t.Fatal(added, err)
		}
	}
	first := hold(t, e, func(tx saas.Tx) error {
		_, _, err := tx.Ports().Registrations.Consume(e.ctx, saasTenant, a, 2)
		return err
	})
	done := make(chan error, 1)
	go func() {
		done <- e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
			_, ok, err := tx.Ports().Registrations.Consume(e.ctx, saasTenant, b, 2)
			if err == nil && !ok {
				err = errors.New("B 应命中")
			}
			return err
		})
	}()
	if stillRunning(done, 3*time.Second) {
		t.Fatal("消耗 B 被消耗 A 的事务挡住了")
	}
	if err := first.commit(); err != nil {
		t.Fatal(err)
	}
}

// Get / List 这类普通读不拿行锁：事务里读了一行，别的连接照样能改它；读也不影响 saas.Tx 的加锁顺序。
func TestPlainReadsInBoundTxTakeNoRowLocks(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	if err := e.db.Ports().Devices.Insert(e.ctx, saasTenant, store.DeviceRecord{DeviceID: "d1", Subject: "a1", Platform: "web", Name: "n", RefreshHash: "h", Rotation: 1, CreatedAtMs: 1, ExpiresAtMs: 99}, 1); err != nil {
		t.Fatal(err)
	}
	reading := hold(t, e, func(tx saas.Tx) error {
		ports := tx.Ports()
		if d, err := ports.Devices.Get(e.ctx, saasTenant, "d1"); err != nil || d == nil {
			return errors.New("读不到设备")
		}
		if _, err := ports.Devices.ListBySubject(e.ctx, saasTenant, "a1"); err != nil {
			return err
		}
		if a, err := ports.Accounts.Get(e.ctx, saasTenant, "a1"); err != nil || a == nil {
			return errors.New("读不到账号")
		}
		if _, err := ports.Sources.List(e.ctx, saasTenant); err != nil {
			return err
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := e.pool.Exec(e.ctx, "UPDATE devices SET name = 'x' WHERE device_id = 'd1'; UPDATE accounts SET updated_at_ms = 5 WHERE account_id = 'a1'")
		done <- err
	}()
	if stillRunning(done, 3*time.Second) {
		t.Fatal("普通读不该拿行锁，别人的写不该被挡")
	}
	if err := reading.commit(); err != nil {
		t.Fatal(err)
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
