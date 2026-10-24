package tunnel_test

import (
	"bytes"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 同一条隧道上的公平性（#57）：大流量流在跑时，交互流（稀疏、小块）和控制帧的出帧延迟有上界，
// 上界以「它入队之后、它的帧之前又写出了多少字节」度量，与就绪流的数量无关，也不依赖时钟快慢。

const chunk = protocol.MaxDataChunk

// ahead 度量：在隧道里先让 bulk 条大流量流各排满积压并写了一阵，再入队 probe，返回 probe 的帧之前
// 又写出的字节（含少量测量误差：读计数与入队之间写端可能多写一两块）。
type fairRig struct {
	*rig
	bulk    []*tunnel.Stream
	perBulk int // 每条大流量流一共发的字节
}

func newFairRig(t *testing.T, bulk int) *fairRig {
	t.Helper()
	r := newRig(t, func(c *tunnel.Config) {
		c.StreamWindow = 32 << 20
		c.TunnelWindow = 1 << 30
		c.MaxStreams = 4096
	})
	r.tr.delay = 50 * time.Microsecond // 模拟有限的链路速率
	// 总积压至少 64 MiB（每流至少 4 MiB）：整个测试期间大流量流都不能写空，否则之后的探测没有竞争，结果失真。
	f := &fairRig{rig: r, perBulk: max(4<<20, (64<<20)/bulk)}
	for i := 0; i < bulk; i++ {
		s := r.open(&recSink{})
		f.bulk = append(f.bulk, s)
	}
	r.tr.take()
	for _, s := range f.bulk {
		s.Send(make([]byte, f.perBulk))
	}
	// 等写端进入稳态（已经写出一批帧，积压还在）。
	eventually(t, "大流量流开始写出", func() bool { return len(dataFrames(r.tr.takeKeep())) >= 3*bulk })
	return f
}

// requireBacklog 在有大流量流已经写空时立即失败：没有积压的测量不算数。
func (f *fairRig) requireBacklog(t *testing.T, frames []protocol.Frame) {
	t.Helper()
	sent := map[uint32]int{}
	for _, fr := range dataFrames(frames) {
		sent[fr.StreamID] += len(fr.Data)
	}
	for _, s := range f.bulk {
		if sent[s.ID()] >= f.perBulk {
			t.Fatalf("大流量流 %d 的 %d 字节已全部写出，积压耗尽，测量失真", s.ID(), f.perBulk)
		}
	}
}

// bytesBefore 返回 id 的第一个 DATA 之前（含更早的）所有 DATA 载荷字节中，从下标 from 起的部分。
func bytesBetween(frames []protocol.Frame, from int, match func(protocol.Frame) bool) (int, bool) {
	n := 0
	for i := from; i < len(frames); i++ {
		if frames[i].Type != protocol.FrameData {
			continue
		}
		if match(frames[i]) {
			return n, true
		}
		n += len(frames[i].Data)
	}
	return 0, false
}

func TestInteractiveStreamLatencyIsBoundedWhileBulkRuns(t *testing.T) {
	// 上界：当前批剩下的（至多 WriteBudget 加一块）+ 测量误差（读计数到入队之间多写的一两块）。
	bound := tunnel.WriteBudget + 3*chunk
	// 不测 4 条：改前的 2N 块（8 块 = 128 KiB）恰好落在上界内，关掉稀疏优先也能通过，区分不了。
	for _, bulk := range []int{8, 16, 64} {
		f := newFairRig(t, bulk)
		probe := f.open(&recSink{})
		var aheads []int
		for i := 0; i < 7; i++ {
			from := len(f.tr.takeKeep())
			probe.Send(bytes.Repeat([]byte{byte(i + 1)}, 1024))
			var ahead int
			var frames []protocol.Frame
			eventually(t, "探测帧写出", func() bool {
				var ok bool
				frames = f.tr.takeKeep()
				ahead, ok = bytesBetween(frames, from, func(fr protocol.Frame) bool { return fr.StreamID == probe.ID() })
				return ok
			})
			aheads = append(aheads, ahead)
			// 不用固定停顿：等批次号前进（探测帧之后又写出超过一整批的字节），下一次探测才是「刚醒来的稀疏流」。
			eventually(t, "批次前进", func() bool {
				frames = f.tr.takeKeep()
				f.requireBacklog(t, frames)
				n := 0
				seen := false
				for j := from; j < len(frames); j++ {
					if frames[j].Type != protocol.FrameData {
						continue
					}
					if seen {
						n += len(frames[j].Data)
					}
					seen = seen || frames[j].StreamID == probe.ID()
				}
				return n > tunnel.WriteBudget+chunk
			})
		}
		// 读计数与入队之间，测试协程被抢占时写端会多写若干块（高负载下偶发）：看中位数，不看单次最大值。
		// 真有回归时每一次都是 2N 块量级，中位数照样超界。
		sort.Ints(aheads)
		median := aheads[len(aheads)/2]
		if median > bound {
			t.Fatalf("%d 条大流量流：探测帧前写出了 %d 字节（%.1f 块，各次 %v），应不超过 %d 字节", bulk, median, float64(median)/chunk, aheads, bound)
		}
		t.Logf("%d 条大流量流：探测帧前写出的字节中位数 %d（%.1f 块），最多 %d", bulk, median, float64(median)/chunk, aheads[len(aheads)-1])
	}
}

// 控制帧（WINDOW、PONG）不被 DATA 挡住：PING 的回复排在当前批之后的下一批最前面。
func TestControlFramesAreNotBlockedByBulk(t *testing.T) {
	f := newFairRig(t, 32)
	bound := tunnel.WriteBudget + 3*chunk
	var aheads []int
	for i := 0; i < 7; i++ {
		from := len(f.tr.takeKeep())
		f.feed(protocol.Frame{Type: protocol.FramePing, Ts: float64(i)})
		var ahead int
		eventually(t, "PONG 写出", func() bool {
			frames := f.tr.takeKeep()
			n := 0
			for j := from; j < len(frames); j++ {
				if frames[j].Type == protocol.FramePong {
					ahead = n
					return true
				}
				n += len(frames[j].Data)
			}
			return false
		})
		aheads = append(aheads, ahead)
		// 等批次前进（PONG 之后又写出超过一整批的字节），不用固定停顿。
		eventually(t, "批次前进", func() bool {
			frames := f.tr.takeKeep()
			f.requireBacklog(t, frames)
			n := 0
			seen := false
			for j := from; j < len(frames); j++ {
				if frames[j].Type == protocol.FramePong {
					seen = true
				} else if seen {
					n += len(frames[j].Data)
				}
			}
			return n > tunnel.WriteBudget+chunk
		})
	}
	// 同上：测试协程被抢占时写端会多写，看中位数。
	sort.Ints(aheads)
	if median := aheads[len(aheads)/2]; median > bound {
		t.Fatalf("PONG 前写出了 %d 字节（各次 %v），应不超过 %d", median, aheads, bound)
	}
}

// 大流量流之间仍然轮转（每流份额相同，不因稀疏流优先而偏向）；稀疏流再多也饿不死老流。
func TestBulkStreamsKeepTakingTurnsUnderSparseLoad(t *testing.T) {
	f := newFairRig(t, 4)
	var sparse []*tunnel.Stream
	for i := 0; i < 32; i++ {
		sparse = append(sparse, f.open(&recSink{}))
	}
	from := len(f.tr.takeKeep())
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, s := range sparse {
				s.Send([]byte("k"))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	// 不按墙钟定时长：等大流量流各写出足够多的帧，高负载下写端慢也一样有样本可比。
	eventually(t, "大流量流各写出若干帧", func() bool {
		counts := map[uint32]int{}
		for _, fr := range dataFrames(f.tr.takeKeep()[from:]) {
			counts[fr.StreamID]++
		}
		for _, s := range f.bulk {
			if counts[s.ID()] < 8 {
				return false
			}
		}
		return true
	})
	close(stop)
	wg.Wait()
	counts := map[uint32]int{}
	for _, fr := range dataFrames(f.tr.takeKeep()[from:]) {
		counts[fr.StreamID]++
	}
	var bulkCounts []int
	for _, s := range f.bulk {
		bulkCounts = append(bulkCounts, counts[s.ID()])
	}
	// 饿死在上面的等待里就会以超时失败（每条都要写出 8 帧），这里只比份额。
	sort.Ints(bulkCounts)
	if bulkCounts[len(bulkCounts)-1] > 2*bulkCounts[0]+4 {
		t.Fatalf("大流量流之间不公平：%v", bulkCounts)
	}
}

// 同一条流内 DATA 有序，END 在最后一块之后（调度不改变流内顺序）。
func TestSchedulerKeepsPerStreamOrder(t *testing.T) {
	f := newFairRig(t, 8)
	s := f.open(&recSink{})
	var want []byte
	for i := 0; i < 50; i++ {
		p := bytes.Repeat([]byte{byte(i)}, 700)
		want = append(want, p...)
		s.Send(p)
		if i%7 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	s.End()
	eventually(t, "END 写出", func() bool {
		for _, fr := range f.tr.takeKeep() {
			if fr.Type == protocol.FrameEnd && fr.StreamID == s.ID() {
				return true
			}
		}
		return false
	})
	var got []byte
	ended := false
	for _, fr := range f.tr.takeKeep() {
		if fr.StreamID != s.ID() {
			continue
		}
		switch fr.Type {
		case protocol.FrameData:
			if ended {
				t.Fatal("END 之后又有 DATA")
			}
			got = append(got, fr.Data...)
		case protocol.FrameEnd:
			ended = true
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("流内字节乱序或丢失：%d / %d", len(got), len(want))
	}
}

// 小帧加背压会在出队列里堆出很多段：入队不能是平方量级（出队列字节数随入队 / 出队增减）。
func TestEnqueueManyTinyFramesIsNotQuadratic(t *testing.T) {
	r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1 << 20; c.TunnelWindow = 1 << 30 })
	r.tr.gate = make(chan struct{}) // 写端卡住，段全部堆在出队列里
	s := r.open(&recSink{})
	one := []byte{1}
	start := time.Now()
	for i := 0; i < 300_000; i++ {
		s.Send(one)
	}
	d := time.Since(start)
	close(r.tr.gate)
	if d > 5*time.Second {
		t.Fatalf("30 万个 1 字节写入用了 %v，疑似平方", d)
	}
}
