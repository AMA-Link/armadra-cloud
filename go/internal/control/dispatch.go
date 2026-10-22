package control

import (
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// Dispatcher 把控制面调用从 epoll 循环等不能阻塞的协程上摘下来：投递即返回，由固定数量的
// 工作协程按序执行。同一个 key 总落在同一条队列，所以同一个源（或账号）的调用保持先后顺序。
//
// 队列有界。满了不丢弃也不阻塞调用方：多出来的任务另起协程执行（顺序不再保证，只在
// 控制面慢到积压几千条时发生；Overflowed 计数供观测）。内存控制面的调用不做网络 I/O，
// 投递后几乎立即执行。
type Dispatcher struct {
	shards     []chan func()
	wg         sync.WaitGroup
	closeOnce  sync.Once
	closed     atomic.Bool
	overflowed atomic.Int64
}

// 缺省的队列条数与每条的深度。
const (
	DefaultDispatchShards = 4
	DefaultDispatchDepth  = 4096
)

// NewDispatcher 建 shards 条队列、每条深度 depth（非正数取缺省）。
func NewDispatcher(shards, depth int) *Dispatcher {
	if shards <= 0 {
		shards = DefaultDispatchShards
	}
	if depth <= 0 {
		depth = DefaultDispatchDepth
	}
	d := &Dispatcher{shards: make([]chan func(), shards)}
	for i := range d.shards {
		ch := make(chan func(), depth)
		d.shards[i] = ch
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for fn := range ch {
				run(fn)
			}
		}()
	}
	return d
}

func run(fn func()) {
	defer func() { _ = recover() }() // 一个任务出错不影响后面的。
	fn()
}

// Go 投递一个任务；永不阻塞。key 相同的任务按投递顺序执行。关闭之后直接在新协程里执行。
func (d *Dispatcher) Go(key string, fn func()) {
	if d.closed.Load() {
		go run(fn)
		return
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	ch := d.shards[int(h.Sum32())%len(d.shards)]
	defer func() {
		// 与 Close 竞争时 ch 可能已关：退回新协程。
		if recover() != nil {
			go run(fn)
		}
	}()
	select {
	case ch <- fn:
	default:
		d.overflowed.Add(1)
		go run(fn)
	}
}

// Overflowed 返回因队列满而另起协程执行的任务数。
func (d *Dispatcher) Overflowed() int64 { return d.overflowed.Load() }

// Flush 等到调用时已在队列里的任务都执行完，或 timeout 到；答是否排空。
func (d *Dispatcher) Flush(timeout time.Duration) bool {
	if d.closed.Load() {
		return true
	}
	var wg sync.WaitGroup
	for _, ch := range d.shards {
		wg.Add(1)
		barrier := func() { wg.Done() }
		select {
		case ch <- barrier:
		default:
			wg.Done() // 队列满：不等（溢出的任务本来就不保证顺序）
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Close 停止接收并等队列里的任务做完。重复调用无害。
func (d *Dispatcher) Close() {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		for _, ch := range d.shards {
			close(ch)
		}
		d.wg.Wait()
	})
}
