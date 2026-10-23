package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const tenant = "cloud"

// clock 是可控的时钟：作业的到期、租约与退避都读它。
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	db    *postgres.DB
	store saas.Store
	clock *clock
}

func open(t *testing.T) *env {
	t.Helper()
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &env{t: t, db: db, store: db.SaaS(), clock: &clock{at: time.Now().Truncate(time.Second)}}
}

func (e *env) runner(poll time.Duration) *jobs.Runner {
	return jobs.New(jobs.Options{Store: e.store, TenantID: tenant, Workers: 2, Now: e.clock.Now, Poll: poll})
}

func (e *env) enqueue(job saas.NewJob) (int64, bool) {
	e.t.Helper()
	var (
		id       int64
		inserted bool
	)
	err := e.store.Tx(context.Background(), func(tx saas.Tx) error {
		var err error
		id, inserted, err = tx.Jobs().Enqueue(context.Background(), tenant, job, e.clock.Now().UnixMilli())
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id, inserted
}

func (e *env) row(id int64) (attempts int, runAt int64, doneAt, failedAt *int64, lastError *string) {
	e.t.Helper()
	err := e.db.Pool().QueryRow(context.Background(),
		"SELECT attempts, run_at_ms, done_at_ms, failed_at_ms, last_error FROM jobs WHERE job_id = $1", id).
		Scan(&attempts, &runAt, &doneAt, &failedAt, &lastError)
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时没有等到：%s", what)
}

func TestNotifyWakesRunnerAndRollbackDoesNot(t *testing.T) {
	e := open(t)
	// 轮询间隔拉到 1 小时：只有 NOTIFY 能让它在测试时限内领到作业。
	runner := e.runner(time.Hour)
	var ran atomic.Int32
	runner.Handle("t.notify", func(context.Context, saas.Job) error { ran.Add(1); return nil })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	time.Sleep(300 * time.Millisecond) // 等 LISTEN 就绪

	rollback := errors.New("回滚")
	err := e.store.Tx(context.Background(), func(tx saas.Tx) error {
		if _, _, err := tx.Jobs().Enqueue(context.Background(), tenant, saas.NewJob{Kind: "t.notify", RunAtMs: e.clock.Now().UnixMilli()}, e.clock.Now().UnixMilli()); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("回滚的入队不该被执行")
	}

	id, _ := e.enqueue(saas.NewJob{Kind: "t.notify", RunAtMs: e.clock.Now().UnixMilli()})
	eventually(t, "NOTIFY 唤醒后执行并完成", func() bool {
		_, _, done, _, _ := e.row(id)
		return done != nil && ran.Load() == 1
	})
}

func TestDedupeBlocksOnlyUnfinishedAndSlotBlocksForever(t *testing.T) {
	e := open(t)
	now := e.clock.Now().UnixMilli()
	first, ok := e.enqueue(saas.NewJob{Kind: "t.dedupe", DedupeKey: "team-1", RunAtMs: now})
	if !ok {
		t.Fatal("第一次应入队")
	}
	if _, ok := e.enqueue(saas.NewJob{Kind: "t.dedupe", DedupeKey: "team-1", RunAtMs: now}); ok {
		t.Fatal("未完成的同 kind 同 dedupe_key 不该再入队")
	}
	if _, ok := e.enqueue(saas.NewJob{Kind: "t.other", DedupeKey: "team-1", RunAtMs: now}); !ok {
		t.Fatal("dedupe 只在同 kind 内")
	}
	if _, ok := e.enqueue(saas.NewJob{Kind: "t.slot", SlotKey: "t.slot:2026-10-23", RunAtMs: now}); !ok {
		t.Fatal("时间槽第一次应入队")
	}

	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.dedupe", func(context.Context, saas.Job) error { return nil })
	runner.Handle("t.other", func(context.Context, saas.Job) error { return nil })
	runner.Handle("t.slot", func(context.Context, saas.Job) error { return nil })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	eventually(t, "三个作业都完成", func() bool {
		var open int
		_ = e.db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE done_at_ms IS NULL").Scan(&open)
		return open == 0
	})
	if _, _, done, _, _ := e.row(first); done == nil {
		t.Fatal("作业应已完成")
	}
	if _, ok := e.enqueue(saas.NewJob{Kind: "t.dedupe", DedupeKey: "team-1", RunAtMs: now}); !ok {
		t.Fatal("完成后同 dedupe_key 可以再入队")
	}
	if _, ok := e.enqueue(saas.NewJob{Kind: "t.slot", SlotKey: "t.slot:2026-10-23", RunAtMs: now}); ok {
		t.Fatal("时间槽完成后也不能再插")
	}
}

func TestFailureBacksOffThenDies(t *testing.T) {
	e := open(t)
	now := e.clock.Now().UnixMilli()
	id, _ := e.enqueue(saas.NewJob{Kind: "t.fail", RunAtMs: now})
	if _, err := e.db.Pool().Exec(context.Background(), "UPDATE jobs SET max_attempts = 3 WHERE job_id = $1", id); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.fail", func(context.Context, saas.Job) error {
		calls.Add(1)
		return errors.New("含有收件人 a@b.test 的消息不该进库")
	})
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })

	eventually(t, "第一次失败", func() bool { return calls.Load() == 1 })
	eventually(t, "第一次失败记上并重排", func() bool {
		attempts, runAt, _, _, last := e.row(id)
		return attempts == 1 && last != nil && runAt == now+30_000
	})
	_, _, _, _, last := e.row(id)
	if *last != "*errors.errorString" {
		t.Fatalf("last_error 只记类名：%q", *last)
	}
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("退避期内不该再跑")
	}
	e.clock.Advance(30 * time.Second)
	eventually(t, "退避到期后第二次", func() bool { return calls.Load() == 2 })
	eventually(t, "第二次的退避翻倍", func() bool {
		attempts, runAt, _, _, _ := e.row(id)
		return attempts == 2 && runAt == e.clock.Now().UnixMilli()+60_000
	})
	e.clock.Advance(60 * time.Second)
	eventually(t, "到 max_attempts 置 failed_at_ms", func() bool {
		attempts, _, done, failed, _ := e.row(id)
		return attempts == 3 && done == nil && failed != nil
	})
	e.clock.Advance(time.Hour)
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 3 {
		t.Fatalf("失败的作业不再跑：%d", calls.Load())
	}
}

func TestPanicIsAFailureNotACrash(t *testing.T) {
	e := open(t)
	id, _ := e.enqueue(saas.NewJob{Kind: "t.panic", RunAtMs: e.clock.Now().UnixMilli()})
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.panic", func(context.Context, saas.Job) error { panic("boom") })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	eventually(t, "panic 记成一次失败，last_error 可辨", func() bool {
		attempts, _, _, _, last := e.row(id)
		return attempts == 1 && last != nil && *last == "panic"
	})
}

func TestOnlyClaimsRegisteredKinds(t *testing.T) {
	e := open(t)
	id, _ := e.enqueue(saas.NewJob{Kind: "t.nobody", RunAtMs: e.clock.Now().UnixMilli()})
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.other", func(context.Context, saas.Job) error { return nil })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	time.Sleep(400 * time.Millisecond)
	if attempts, _, done, failed, last := e.row(id); attempts != 0 || done != nil || failed != nil || last != nil {
		t.Fatalf("没登记处理器的种类不该被领走：%d %v %v %v", attempts, done, failed, last)
	}
}

func TestExhaustedJobWithExpiredLeaseIsSweptNotReclaimed(t *testing.T) {
	e := open(t)
	now := e.clock.Now().UnixMilli()
	id, _ := e.enqueue(saas.NewJob{Kind: "t.last", RunAtMs: now})
	// 最后一次尝试时实例崩了：attempts 已满、租约过期。
	if _, err := e.db.Pool().Exec(context.Background(),
		"UPDATE jobs SET attempts = max_attempts, locked_by = 'dead', locked_until_ms = $2 WHERE job_id = $1", id, now-1000); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.last", func(context.Context, saas.Job) error { calls.Add(1); return nil })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	eventually(t, "清扫置失败", func() bool {
		_, _, done, failed, last := e.row(id)
		return done == nil && failed != nil && last != nil && *last == "lease_expired"
	})
	if calls.Load() != 0 {
		t.Fatal("尝试次数已满的作业不能再被执行")
	}
}

func TestHandlerIsCancelledWhenLeaseRunsOutWithoutRenewal(t *testing.T) {
	e := open(t)
	// 租约 200 毫秒、每 20 毫秒检查；可控时钟跳过租约到期时刻，下一次检查就该停手。
	r := jobs.New(jobs.Options{Store: e.store, TenantID: tenant, Workers: 1, Now: e.clock.Now, Poll: 20 * time.Millisecond, Lease: 200 * time.Millisecond, Renew: 20 * time.Millisecond})
	started, cancelled := make(chan struct{}), make(chan struct{})
	r.Handle("t.expire", func(ctx context.Context, _ saas.Job) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	e.enqueue(saas.NewJob{Kind: "t.expire", RunAtMs: e.clock.Now().UnixMilli()})
	r.Start()
	t.Cleanup(func() { r.Stop(5 * time.Second) })
	<-started
	e.clock.Advance(time.Second)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("租约到期时刻已过，处理器应被取消")
	}
}

func TestShutdownInterruptionReleasesWithoutCountingAttempt(t *testing.T) {
	e := open(t)
	started := make(chan struct{}, 1)
	r := e.runner(20 * time.Millisecond)
	r.Handle("t.shut", func(ctx context.Context, _ saas.Job) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	id, _ := e.enqueue(saas.NewJob{Kind: "t.shut", RunAtMs: e.clock.Now().UnixMilli()})
	r.Start()
	<-started
	r.Stop(100 * time.Millisecond)
	attempts, runAt, done, failed, last := e.row(id)
	if attempts != 0 || done != nil || failed != nil || last != nil || runAt != e.clock.Now().UnixMilli() {
		t.Fatalf("关闭中被打断：释放租约、不计尝试、不退避：%d %d %v %v %v", attempts, runAt, done, failed, last)
	}
	var lockedBy *string
	_ = e.db.Pool().QueryRow(context.Background(), "SELECT locked_by FROM jobs WHERE job_id = $1", id).Scan(&lockedBy)
	if lockedBy != nil {
		t.Fatal("租约应已释放")
	}
}

func TestStopGivesUpOnHandlersThatIgnoreCancellation(t *testing.T) {
	e := open(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	started := make(chan struct{}, 1)
	r := jobs.New(jobs.Options{Store: e.store, TenantID: tenant, Workers: 1, Now: e.clock.Now, Poll: 20 * time.Millisecond, AbortWait: 200 * time.Millisecond})
	r.Handle("t.deaf", func(context.Context, saas.Job) error { started <- struct{}{}; <-block; return nil })
	e.enqueue(saas.NewJob{Kind: "t.deaf", RunAtMs: e.clock.Now().UnixMilli()})
	r.Start()
	<-started
	begin := time.Now()
	r.Stop(100 * time.Millisecond)
	if time.Since(begin) > 3*time.Second {
		t.Fatal("不理会取消的处理器不能拖死关闭")
	}
}

func TestPerHandlerTimeout(t *testing.T) {
	e := open(t)
	r := e.runner(20 * time.Millisecond)
	r.Handle("t.slowjob", func(ctx context.Context, _ saas.Job) error { <-ctx.Done(); return ctx.Err() }, jobs.WithTimeout(100*time.Millisecond))
	id, _ := e.enqueue(saas.NewJob{Kind: "t.slowjob", RunAtMs: e.clock.Now().UnixMilli()})
	r.Start()
	t.Cleanup(func() { r.Stop(5 * time.Second) })
	eventually(t, "超时按失败重排", func() bool {
		attempts, _, _, _, last := e.row(id)
		return attempts == 1 && last != nil && *last == "context.deadlineExceededError"
	})
}

func TestListenConnectionIsCleanedUpOnStop(t *testing.T) {
	e := open(t)
	r := e.runner(time.Hour)
	r.Handle("t.noop", func(context.Context, saas.Job) error { return nil })
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop(5 * time.Second)
	// 池里剩下的每条连接都不再监听。
	ctx := context.Background()
	var held []*pgxpool.Conn
	for range int(e.db.Pool().Stat().TotalConns()) {
		conn, err := e.db.Pool().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		var channels []string
		rows, err := conn.Query(ctx, "SELECT pg_listening_channels()")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			channels = append(channels, c)
		}
		rows.Close()
		if len(channels) != 0 {
			t.Fatalf("放回池里的连接还在监听：%v", channels)
		}
	}
	for _, c := range held {
		c.Release()
	}
}

func TestCrashedWorkersLeaseExpires(t *testing.T) {
	e := open(t)
	id, _ := e.enqueue(saas.NewJob{Kind: "t.crash", RunAtMs: e.clock.Now().UnixMilli()})
	// 另一个实例领走作业后崩了：租约 60 秒。
	claimed, err := e.store.Queue().Claim(context.Background(), tenant, "dead", []string{"t.crash"}, e.clock.Now().UnixMilli(), 60_000, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("领取：%v %v", claimed, err)
	}
	var calls atomic.Int32
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.crash", func(context.Context, saas.Job) error { calls.Add(1); return nil })
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("租约未到期，别的实例不能领")
	}
	e.clock.Advance(61 * time.Second)
	eventually(t, "租约到期后被接手并完成", func() bool {
		attempts, _, done, _, _ := e.row(id)
		return calls.Load() == 1 && done != nil && attempts == 2
	})
}

func TestLeaseIsRenewedWhileRunningAndLostLeaseStopsTheJob(t *testing.T) {
	e := open(t)
	// 租约 400 毫秒、每 100 毫秒续一次；时钟随真实时间走，租约才会真的过期。
	real := jobs.Options{Store: e.store, TenantID: tenant, Workers: 1, Poll: 20 * time.Millisecond, Lease: 400 * time.Millisecond, Renew: 100 * time.Millisecond}
	a, b := jobs.New(real), jobs.New(real)
	var runs atomic.Int32
	slow := func(ctx context.Context, _ saas.Job) error {
		runs.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.Handle("t.slow", slow)
	b.Handle("t.slow", slow)
	id, _ := func() (int64, bool) {
		var id int64
		err := e.store.Tx(context.Background(), func(tx saas.Tx) error {
			var err error
			id, _, err = tx.Jobs().Enqueue(context.Background(), tenant, saas.NewJob{Kind: "t.slow", RunAtMs: time.Now().UnixMilli()}, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, true
	}()
	a.Start()
	b.Start()
	t.Cleanup(func() { a.Stop(5 * time.Second); b.Stop(5 * time.Second) })
	eventually(t, "跑完", func() bool {
		_, _, done, _, _ := e.row(id)
		return done != nil
	})
	if runs.Load() != 1 {
		t.Fatalf("续租期间另一个实例不该再领：跑了 %d 次", runs.Load())
	}

	// 租约被别人拿走：续租推不动，处理器的 ctx 被取消。先停掉 a、b：它们没有 t.lost 的处理器，会把作业领去记失败。
	a.Stop(5 * time.Second)
	b.Stop(5 * time.Second)
	id2, _ := func() (int64, bool) {
		var id int64
		_ = e.store.Tx(context.Background(), func(tx saas.Tx) error {
			var err error
			id, _, err = tx.Jobs().Enqueue(context.Background(), tenant, saas.NewJob{Kind: "t.lost", RunAtMs: time.Now().UnixMilli()}, time.Now().UnixMilli())
			return err
		})
		return id, true
	}()
	started, cancelled := make(chan struct{}), make(chan struct{})
	// 处理器登记在 Start 之后不安全，所以用新的运行器。
	c := jobs.New(real)
	c.Handle("t.lost", func(ctx context.Context, _ saas.Job) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	c.Start()
	t.Cleanup(func() { c.Stop(5 * time.Second) })
	<-started
	if _, err := e.db.Pool().Exec(context.Background(), "UPDATE jobs SET locked_by = 'thief' WHERE job_id = $1", id2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("租约丢了应取消处理器")
	}
}

func TestEverySlotRunsOncePerSlotAcrossInstances(t *testing.T) {
	e := open(t)
	var calls atomic.Int32
	handler := func(context.Context, saas.Job) error { calls.Add(1); return nil }
	var all []*jobs.Runner
	for range 3 {
		r := e.runner(20 * time.Millisecond)
		r.Handle("t.daily", handler)
		r.Every("t.daily", jobs.Daily)
		all = append(all, r)
	}
	for _, r := range all {
		r.Start()
	}
	t.Cleanup(func() {
		for _, r := range all {
			r.Stop(5 * time.Second)
		}
	})
	eventually(t, "同一天的槽只跑一次", func() bool { return calls.Load() >= 1 })
	time.Sleep(1500 * time.Millisecond) // 多过一个调度周期
	if calls.Load() != 1 {
		t.Fatalf("三个实例同一个槽应只跑一次：%d", calls.Load())
	}
	var slots int
	if err := e.db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE kind = 't.daily'").Scan(&slots); err != nil || slots != 1 {
		t.Fatalf("库里只有一行：%d %v", slots, err)
	}
	e.clock.Advance(24 * time.Hour)
	eventually(t, "第二天另起一个槽", func() bool { return calls.Load() == 2 })
}

func TestStopWaitsForRunningJobThenAbortsAfterTimeout(t *testing.T) {
	e := open(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	runner := e.runner(20 * time.Millisecond)
	var finished atomic.Bool
	runner.Handle("t.stop", func(ctx context.Context, _ saas.Job) error {
		started <- struct{}{}
		select {
		case <-release:
			finished.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	id, _ := e.enqueue(saas.NewJob{Kind: "t.stop", RunAtMs: e.clock.Now().UnixMilli()})
	runner.Start()
	<-started
	go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	runner.Stop(5 * time.Second)
	if !finished.Load() {
		t.Fatal("Stop 应等当前作业跑完")
	}
	if _, _, done, _, _ := e.row(id); done == nil {
		t.Fatal("跑完的作业应记完成")
	}

	// 超时：打断处理器，作业释放租约回到队列（别的实例马上可以领，不计尝试）。
	runner2 := e.runner(20 * time.Millisecond)
	runner2.Handle("t.stuck", func(ctx context.Context, _ saas.Job) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	id2, _ := e.enqueue(saas.NewJob{Kind: "t.stuck", RunAtMs: e.clock.Now().UnixMilli()})
	runner2.Start()
	<-started
	begin := time.Now()
	runner2.Stop(200 * time.Millisecond)
	if time.Since(begin) > 5*time.Second {
		t.Fatal("超时后应打断作业并返回")
	}
	if attempts, _, done, _, last := e.row(id2); attempts != 0 || done != nil || last != nil {
		t.Fatalf("被打断的作业释放租约、不计尝试：%d %v %v", attempts, done, last)
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	e := open(t)
	got := make(chan json.RawMessage, 1)
	runner := e.runner(20 * time.Millisecond)
	runner.Handle("t.payload", func(_ context.Context, job saas.Job) error { got <- job.Payload; return nil })
	e.enqueue(saas.NewJob{Kind: "t.payload", Payload: json.RawMessage(`{"teamId":"t1"}`), RunAtMs: e.clock.Now().UnixMilli()})
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	select {
	case payload := <-got:
		var decoded map[string]string
		if err := json.Unmarshal(payload, &decoded); err != nil || decoded["teamId"] != "t1" {
			t.Fatalf("载荷：%s %v", payload, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("没有收到载荷")
	}
}

func TestDeferReleasesWithoutCountingAttemptOrBackoff(t *testing.T) {
	e := open(t)
	now := e.clock.Now().UnixMilli()
	var calls atomic.Int32
	r := e.runner(20 * time.Millisecond)
	r.Handle("t.defer", func(context.Context, saas.Job) error { calls.Add(1); return jobs.Defer(5 * time.Minute) })
	id, _ := e.enqueue(saas.NewJob{Kind: "t.defer", RunAtMs: now})
	r.Start()
	t.Cleanup(func() { r.Stop(5 * time.Second) })
	eventually(t, "延后", func() bool {
		attempts, runAt, done, failed, last := e.row(id)
		return calls.Load() == 1 && attempts == 0 && runAt == now+300_000 && done == nil && failed == nil && last == nil
	})
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("延后期间不再跑")
	}
	e.clock.Advance(5 * time.Minute)
	eventually(t, "到点再跑", func() bool { return calls.Load() == 2 })
}
