// Package jobs 是作业队列的运行侧（设计 §6.1）：从 jobs 表领取到期作业、按种类分派给处理器，
// 跑得久的作业续租，失败按退避重排，定时作业按时间槽各实例各插一次只成功一个。
//
// 业务事务里用 tx.Jobs().Enqueue 入队（随提交 NOTIFY），这里 LISTEN cloud_jobs 收到就领，否则每秒一轮。
// 处理器必须可重放：先读状态再动作，租约丢了或进程崩了作业会被别的实例再领一次。
// 日志只记作业 id、种类、尝试次数与错误类名；作业载荷里没有令牌与内容。
package jobs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 租约与轮询的缺省值（设计 §6.1）。
const (
	DefaultLease   = 60 * time.Second
	DefaultRenew   = 20 * time.Second
	DefaultPoll    = time.Second
	DefaultWorkers = 2
	// scheduleEvery 是检查定时作业该不该插新时间槽的间隔。
	scheduleEvery = time.Second
)

// Handler 处理一个作业：返回 nil 即完成，返回错误按退避重试。ctx 在租约丢失或关闭超时时取消。
type Handler func(ctx context.Context, job saas.Job) error

// Slot 是定时作业的时间槽粒度。
type Slot int

// 时间槽：每小时、每天（UTC）。
const (
	Hourly Slot = iota
	Daily
)

// key 是 kind 在 t 所在槽里的 slot_key（audit.retention:2026-10-23）。
func (s Slot) key(kind string, t time.Time) string {
	t = t.UTC()
	if s == Hourly {
		return kind + ":" + t.Format("2006-01-02T15")
	}
	return kind + ":" + t.Format("2006-01-02")
}

// Options 是作业运行器的装配。
type Options struct {
	Store    saas.Store
	TenantID string
	// Workers 是并发数；缺省 2（JOB_WORKERS）。
	Workers int
	// Now 缺省 time.Now。
	Now func() time.Time
	Log *obs.Logger
	// WorkerID 写进 locked_by；缺省随机。
	WorkerID string
	// Lease、Renew、Poll 缺省 60 秒、20 秒、1 秒（测试可缩短）。
	Lease, Renew, Poll time.Duration
}

// Runner 是作业运行器。
type Runner struct {
	o        Options
	handlers map[string]Handler
	periodic []periodic

	mu      sync.Mutex
	started bool
	stop    context.CancelFunc // 停止领取
	abort   context.CancelFunc // 打断在跑的作业
	intake  sync.WaitGroup
	running sync.WaitGroup
	wakes   []chan struct{}
}

type periodic struct {
	kind string
	slot Slot
}

// New 建作业运行器。
func New(options Options) *Runner {
	if options.Workers <= 0 {
		options.Workers = DefaultWorkers
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Log == nil {
		options.Log = obs.Nop()
	}
	if options.WorkerID == "" {
		options.WorkerID = "w-" + core.RandomToken(8)
	}
	if options.Lease <= 0 {
		options.Lease = DefaultLease
	}
	if options.Renew <= 0 {
		options.Renew = DefaultRenew
	}
	if options.Poll <= 0 {
		options.Poll = DefaultPoll
	}
	return &Runner{o: options, handlers: map[string]Handler{}}
}

// Handle 登记一种作业的处理器；必须在 Start 之前。
func (r *Runner) Handle(kind string, handler Handler) {
	r.handlers[kind] = handler
}

// Every 登记一个定时作业：每个时间槽（小时或天）至多插一次（slot_key），多实例各插一次只成功一个。
// 处理器另用 Handle 登记；必须在 Start 之前。
func (r *Runner) Every(kind string, slot Slot) {
	r.periodic = append(r.periodic, periodic{kind: kind, slot: slot})
}

func (r *Runner) nowMs() int64 { return r.o.Now().UnixMilli() }

// Start 开始领取与调度（幂等）。
func (r *Runner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	intakeCtx, stop := context.WithCancel(context.Background())
	runCtx, abort := context.WithCancel(context.Background())
	r.stop, r.abort = stop, abort
	for i := 0; i < r.o.Workers; i++ {
		wake := make(chan struct{}, 1)
		r.wakes = append(r.wakes, wake)
		r.intake.Add(1)
		go func() {
			defer r.intake.Done()
			r.work(intakeCtx, runCtx, wake)
		}()
	}
	r.intake.Add(2)
	go func() {
		defer r.intake.Done()
		r.o.Store.Queue().Listen(intakeCtx, r.wake)
	}()
	go func() {
		defer r.intake.Done()
		r.schedule(intakeCtx)
	}()
}

// wake 叫醒全部工作协程去领一轮。
func (r *Runner) wake() {
	for _, wake := range r.wakes {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Stop 停止领取，等在跑的作业最多 timeout；超时则打断它们（租约到期后别的实例会再领）。
func (r *Runner) Stop(timeout time.Duration) {
	r.mu.Lock()
	stop, abort, started := r.stop, r.abort, r.started
	r.mu.Unlock()
	if !started {
		return
	}
	stop()
	done := make(chan struct{})
	go func() { r.intake.Wait(); r.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		abort()
		<-done
	}
	abort()
}

func (r *Runner) work(intake, run context.Context, wake chan struct{}) {
	ticker := time.NewTicker(r.o.Poll)
	defer ticker.Stop()
	for {
		for intake.Err() == nil {
			jobs, err := r.o.Store.Queue().Claim(intake, r.o.TenantID, r.o.WorkerID, r.nowMs(), r.o.Lease.Milliseconds(), 1)
			if err != nil {
				if intake.Err() == nil {
					r.o.Log.Warn("领取作业失败", obs.Operation("jobs.claim"), obs.Error(err))
				}
				break
			}
			if len(jobs) == 0 {
				break
			}
			r.running.Add(1)
			r.execute(run, jobs[0])
			r.running.Done()
		}
		select {
		case <-intake.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// execute 跑一个作业：续租、分派、记结果。
func (r *Runner) execute(parent context.Context, job saas.Job) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		r.renew(ctx, cancel, job)
	}()
	err := r.call(ctx, job)
	cancel()
	<-renewed

	// 收尾不受取消影响（关闭超时也要把结果记下来）。
	final, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer done()
	if err == nil {
		ok, cerr := r.o.Store.Queue().Complete(final, r.o.TenantID, job.ID, r.o.WorkerID, r.nowMs())
		switch {
		case cerr != nil:
			r.o.Log.Warn("作业完成没有记上", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Error(cerr))
		case !ok:
			r.o.Log.Warn("作业租约已丢，完成未记", obs.JobID(job.ID), obs.Kind(job.Kind))
		}
		return
	}
	dead, ferr := r.o.Store.Queue().Fail(final, r.o.TenantID, job, r.o.WorkerID, r.nowMs(), errorName(err))
	if ferr != nil {
		r.o.Log.Warn("作业失败没有记上", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Error(ferr))
		return
	}
	if dead {
		r.o.Log.Error("作业重试用尽", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Attempts(job.Attempts), obs.Error(err))
		return
	}
	r.o.Log.Warn("作业失败，稍后重试", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Attempts(job.Attempts), obs.Error(err))
}

// call 分派给处理器；panic 当作失败，不带走整个进程。
func (r *Runner) call(ctx context.Context, job saas.Job) (err error) {
	handler, ok := r.handlers[job.Kind]
	if !ok {
		return errors.New("unknown_kind")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic")
		}
	}()
	return handler(ctx, job)
}

// renew 每隔一段时间把租约往后推；推不动说明租约已丢，立即取消作业。
func (r *Runner) renew(ctx context.Context, cancelJob context.CancelFunc, job saas.Job) {
	ticker := time.NewTicker(r.o.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ok, err := r.o.Store.Queue().Extend(ctx, r.o.TenantID, job.ID, r.o.WorkerID, r.nowMs(), r.o.Lease.Milliseconds())
		if ctx.Err() != nil {
			return // 作业已经结束，续租被一并取消
		}
		if err != nil {
			r.o.Log.Warn("作业续租失败", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Error(err))
			continue
		}
		if !ok {
			r.o.Log.Warn("作业租约已丢，停手", obs.JobID(job.ID), obs.Kind(job.Kind))
			cancelJob()
			return
		}
	}
}

// schedule 为定时作业插入当前时间槽（槽没变就不再问库）。
func (r *Runner) schedule(ctx context.Context) {
	last := map[string]string{}
	tick := func() {
		now := r.o.Now()
		for _, p := range r.periodic {
			slot := p.slot.key(p.kind, now)
			if last[p.kind] == slot {
				continue
			}
			err := r.o.Store.Tx(ctx, func(tx saas.Tx) error {
				_, _, err := tx.Jobs().Enqueue(ctx, r.o.TenantID, saas.NewJob{Kind: p.kind, RunAtMs: now.UnixMilli(), SlotKey: slot}, now.UnixMilli())
				return err
			})
			if err != nil {
				if ctx.Err() == nil {
					r.o.Log.Warn("定时作业入队失败", obs.Kind(p.kind), obs.Error(err))
				}
				continue
			}
			last[p.kind] = slot
		}
	}
	tick()
	ticker := time.NewTicker(scheduleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// errorName 是写进 last_error 的类名或错误码：不含错误消息（消息里可能带着地址等内容）。
func errorName(err error) string {
	if code, ok := core.CodeOf(err); ok {
		return string(code)
	}
	return reflect.TypeOf(err).String()
}
