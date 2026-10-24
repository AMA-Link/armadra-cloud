package tunnel

import (
	"sync/atomic"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
)

// 写端的公平调度（#57）。同一条连接上的多条流共用一个写协程，调度器决定下一帧给谁：
//
//   - 轮转：每条就绪的流每批至多出一帧，大流量流不会把别的流挡在整段积压后面；
//   - 稀疏流优先：刚从空闲转为就绪、排着的字节不多的流进 fresh，先于持续有数据的老流出帧，
//     终端按键与回显这类交互流因此不必排在大流量流后面；上一帧还没交给传输层就又就绪的流进老流队列，
//     抢在帧与帧之间反复补数据换不到额外优先（写端用 NoteSent 逐帧报告已写出的进度，慢链路上一批要写很久，
//     按批判定会让按键间隔短于一批写出时间的交互流失去优先；不报告时退化为「上一批开始之后」）；
//   - 字节预算：一批至多 budget 字节（老流至少出一帧），后到的控制帧与稀疏流的帧最多等
//     「当前批剩下的 + 一块」，与就绪流的数量无关。
//
// 线上协议不变：调度只决定已经扣过信用的帧的出帧顺序。

// WriteBudget 是一批最多交给传输层的 DATA 字节（至少一帧）。
const WriteBudget = 2 * protocol.MaxDataChunk

// SparseMax 是稀疏流排着的字节上限：超过它的（文件传输、刷屏输出）按老流轮转。
const SparseMax = 16 << 10

type queueKind uint8

const (
	queueNone queueKind = iota
	queueFresh
	queueOld
)

// Flow 是流在调度器里的状态，嵌在流里，由持有调度器的那把锁守护。
type Flow struct {
	queued queueKind // 在哪条就绪队列里
	seq    uint64    // 最近一次出的帧的全局序号
}

type entry[T any] struct {
	v T
	f *Flow
}

// Sched 是一条连接的写调度器；不自带锁，调用方持锁使用。
type Sched[T any] struct {
	fresh, old         []entry[T]
	freshNext, oldNext []entry[T]
	scratch            []entry[T]
	seq                uint64        // 已出帧的全局序号（持锁）
	sent               atomic.Uint64 // 其中已交给传输层的序号：写端逐帧报告，每批开始时至少追到 seq
}

// Add 把有数据的流排进就绪队列；已经在队列里的忽略。bytes 是它排着的字节数。
func (q *Sched[T]) Add(v T, f *Flow, bytes int) {
	if f.queued != queueNone {
		return
	}
	if bytes <= SparseMax && f.seq <= q.sent.Load() {
		f.queued = queueFresh
		q.fresh = append(q.fresh, entry[T]{v, f})
		return
	}
	f.queued = queueOld
	q.old = append(q.old, entry[T]{v, f})
}

// Reset 清空（连接关闭时）。
func (q *Sched[T]) Reset() {
	q.fresh, q.old, q.freshNext, q.oldNext, q.scratch = nil, nil, nil, nil, nil
	q.seq = 0
	q.sent.Store(0)
}

// NoteSent 由写端（不必持锁）在帧交给传输层后调用：seq 是 Drain 返回的 base 加上这一批里这一帧
// 的序号（从 1 数）。这条流的上一帧写出之后再就绪，才算稀疏流。
func (q *Sched[T]) NoteSent(seq uint64) {
	q.sent.Store(seq)
}

// Drain 取出一批：先给稀疏流各一帧，再按轮转给老流各一帧，超过 budget 字节就停（老流每批至少一帧，
// 稀疏流再多也饿不死它们）；没轮到的留在队首，下一批先出。step 给流出一帧并答帧的载荷字节数与流里
// 是否还有数据；size 为负表示这条流没有可出的帧（已销毁、已空），直接摘掉。返回这一批之前的帧序号 base，
// 批里第 k 个数据帧（从 1 数，不含控制帧）的序号是 base+k。调用方是单个写协程：上一批已经写完才取下一批，
// 所以这里先把 sent 追到 seq。
func (q *Sched[T]) Drain(budget int, step func(v T) (size int, more bool)) (base uint64) {
	base = q.seq
	q.sent.Store(base)
	size := 0
	take := func(e entry[T]) (more, ok bool) {
		e.f.queued = queueNone
		n, more := step(e.v)
		if n < 0 {
			return false, false
		}
		q.seq++
		e.f.seq = q.seq
		size += n
		return more, true
	}
	fresh, old := q.fresh, q.old
	q.fresh, q.old = q.freshNext[:0], q.oldNext[:0]

	// 稀疏流：每条一帧；发完一帧后还有数据的转入老流。预算用完时剩下的留在 fresh 队首。
	i := 0
	for ; i < len(fresh); i++ {
		if size >= budget {
			break
		}
		if more, ok := take(fresh[i]); ok && more {
			fresh[i].f.queued = queueOld
			q.old = append(q.old, fresh[i])
		}
	}
	q.fresh = append(q.fresh, fresh[i:]...)

	// 老流：轮转；没轮到的排在最前，其次是本批发过帧的，稀疏流转入的在最后。
	served := 0
	j := 0
	again := q.scratch[:0]
	for ; j < len(old); j++ {
		if served > 0 && size >= budget {
			break
		}
		more, ok := take(old[j])
		if !ok {
			continue
		}
		served++
		if more {
			old[j].f.queued = queueOld
			again = append(again, old[j])
		}
	}
	// 拼成 rest + again + promoted。promoted 与 q.old 同底，先拷到 scratch 里再重写 q.old。
	buf := append(again, q.old...)
	q.old = append(append(q.old[:0], old[j:]...), buf...)
	clear(buf)
	q.scratch = buf[:0]
	clear(fresh)
	clear(old)
	q.freshNext, q.oldNext = fresh[:0], old[:0]
	return base
}
