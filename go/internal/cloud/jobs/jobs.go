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
	// AbortWait 是 Stop 超时打断作业之后最多再等多久（处理器不理 ctx 时不能拖死关闭）；缺省 5 秒。
	AbortWait time.Duration
}

// 记进 last_error 的固定名字（不含错误消息）。
var (
	ErrUnknownKind = NamedError("unknown_kind")
	ErrPanic       = NamedError("panic")
)

// NamedError 是 last_error 里记的固定名字：处理器想让失败原因可辨时返回它。
type NamedError string

func (e NamedError) Error() string { return string(e) }

// DeferError 让运行器把作业放回队列、过 After 之后再跑，不计尝试、不退避（例如发信方还没配置）。
type DeferError struct{ After time.Duration }

func (e DeferError) Error() string { return "deferred" }

// Defer 造一个延后错误。
func Defer(after time.Duration) error { return DeferError{After: after} }

// HandleOption 调整一种作业的处理方式。
type HandleOption func(*handlerSpec)

// WithTimeout 给这种作业的每次执行加上限（超时取消 ctx，按失败重试）。
func WithTimeout(d time.Duration) HandleOption { return func(h *handlerSpec) { h.timeout = d } }

type handlerSpec struct {
	run     Handler
	timeout time.Duration
}

// Runner 是作业运行器。
type Runner struct {
	o        Options
	handlers map[string]handlerSpec
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
	if options.AbortWait <= 0 {
		options.AbortWait = 5 * time.Second
	}
	return &Runner{o: options, handlers: map[string]handlerSpec{}}
}

// Handle 登记一种作业的处理器；必须在 Start 之前。
func (r *Runner) Handle(kind string, handler Handler, options ...HandleOption) {
	spec := handlerSpec{run: handler}
	for _, option := range options {
		option(&spec)
	}
	r.handlers[kind] = spec
}

// kinds 是登记过处理器的种类：只领这些，别的版本的实例登记的种类不被误领。
func (r *Runner) kinds() []string {
	kinds := make([]string, 0, len(r.handlers))
	for kind := range r.handlers {
		kinds = append(kinds, kind)
	}
	return kinds
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
		select {
		case <-done:
		case <-time.After(r.o.AbortWait):
			r.o.Log.Error("作业不理会取消，放弃等待", obs.Operation("jobs.stop"))
		}
	}
	abort()
}

func (r *Runner) work(intake, run context.Context, wake chan struct{}) {
	ticker := time.NewTicker(r.o.Poll)
	defer ticker.Stop()
	for {
		for intake.Err() == nil {
			claimedAt := r.nowMs()
			jobs, err := r.o.Store.Queue().Claim(intake, r.o.TenantID, r.o.WorkerID, r.kinds(), claimedAt, r.o.Lease.Milliseconds(), 1)
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
			r.execute(run, jobs[0], claimedAt)
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
func (r *Runner) execute(parent context.Context, job saas.Job, claimedAt int64) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		r.renew(ctx, cancel, job, claimedAt)
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
	var deferred DeferError
	if errors.As(err, &deferred) {
		if rerr := r.o.Store.Queue().Release(final, r.o.TenantID, job.ID, r.o.WorkerID, r.nowMs()+deferred.After.Milliseconds()); rerr != nil {
			r.o.Log.Warn("作业延后没有记上", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Error(rerr))
		}
		return
	}
	if parent.Err() != nil && errors.Is(err, context.Canceled) {
		// 关闭时被打断：只释放租约，不计这次尝试、不退避，别的实例马上可以接手。
		if rerr := r.o.Store.Queue().Release(final, r.o.TenantID, job.ID, r.o.WorkerID, 0); rerr != nil {
			r.o.Log.Warn("作业释放租约失败", obs.JobID(job.ID), obs.Kind(job.Kind), obs.Error(rerr))
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
	spec, ok := r.handlers[job.Kind]
	if !ok {
		return ErrUnknownKind
	}
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = ErrPanic
		}
	}()
	return spec.run(ctx, job)
}

// renew 每隔一段时间把租约往后推；推不动（租约已丢）或到了上次续租记下的到期时刻还没续上，
// 立即取消作业——别的实例随时可能已经接手，不能与它同时执行。
func (r *Runner) renew(ctx context.Context, cancelJob context.CancelFunc, job saas.Job, claimedAt int64) {
	leaseUntil := claimedAt + r.o.Lease.Milliseconds()
	ticker := time.NewTicker(r.o.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := r.nowMs()
		if now >= leaseUntil {
			r.o.Log.Warn("作业租约已到期没能续上，停手", obs.JobID(job.ID), obs.Kind(job.Kind))
			cancelJob()
			return
		}
		ok, err := r.o.Store.Queue().Extend(ctx, r.o.TenantID, job.ID, r.o.WorkerID, now, r.o.Lease.Milliseconds())
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
		leaseUntil = now + r.o.Lease.Milliseconds()
	}
}

// schedule 为定时作业插入当前时间槽（槽没变就不再问库）。
func (r *Runner) schedule(ctx context.Context) {
	last := map[string]string{}
	tick := func() {
		r.sweep(ctx)
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
	var named NamedError
	if errors.As(err, &named) {
		return string(named)
	}
	if code, ok := core.CodeOf(err); ok {
		return string(code)
	}
	return reflect.TypeOf(err).String()
}

// sweep 把尝试次数已满、租约又过期的作业（最后一次执行时实例崩了）置失败并记错误日志。
func (r *Runner) sweep(ctx context.Context) {
	n, err := r.o.Store.Queue().SweepExhausted(ctx, r.o.TenantID, r.nowMs())
	switch {
	case err != nil && ctx.Err() == nil:
		r.o.Log.Warn("清扫用尽重试的作业失败", obs.Operation("jobs.sweep"), obs.Error(err))
	case n > 0:
		r.o.Log.Error("作业在最后一次尝试时租约过期，记为失败", obs.Operation("jobs.sweep"), obs.Count(n))
	}
}
