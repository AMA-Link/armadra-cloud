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
	bulk []*tunnel.Stream
}

func newFairRig(t *testing.T, bulk int) *fairRig {
	t.Helper()
	r := newRig(t, func(c *tunnel.Config) {
		c.StreamWindow = 4 << 20
		c.TunnelWindow = 1 << 30
		c.MaxStreams = 4096
	})
	r.tr.delay = 50 * time.Microsecond // 模拟有限的链路速率
	f := &fairRig{rig: r}
	for i := 0; i < bulk; i++ {
		s := r.open(&recSink{})
		f.bulk = append(f.bulk, s)
	}
	r.tr.take()
	for _, s := range f.bulk {
		s.Send(make([]byte, 4<<20))
	}
	// 等写端进入稳态（已经写出一批帧，积压还在）。
	eventually(t, "大流量流开始写出", func() bool { return len(dataFrames(r.tr.takeKeep())) >= 3*bulk })
	return f
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
	for _, bulk := range []int{4, 16, 64} {
		f := newFairRig(t, bulk)
		probe := f.open(&recSink{})
		var worst int
		for i := 0; i < 5; i++ {
			from := len(f.tr.takeKeep())
			probe.Send(bytes.Repeat([]byte{byte(i + 1)}, 1024))
			var ahead int
			eventually(t, "探测帧写出", func() bool {
				var ok bool
				ahead, ok = bytesBetween(f.tr.takeKeep(), from, func(fr protocol.Frame) bool { return fr.StreamID == probe.ID() })
				return ok
			})
			worst = max(worst, ahead)
			time.Sleep(15 * time.Millisecond) // 停一停再发：每次都是「刚醒来的稀疏流」
		}
		if worst > bound {
			t.Fatalf("%d 条大流量流：探测帧前写出了 %d 字节（%.1f 块），应不超过 %d 字节", bulk, worst, float64(worst)/chunk, bound)
		}
		t.Logf("%d 条大流量流：探测帧前最多写出 %d 字节（%.1f 块）", bulk, worst, float64(worst)/chunk)
	}
}

// 控制帧（WINDOW、PONG）不被 DATA 挡住：PING 的回复排在当前批之后的下一批最前面。
func TestControlFramesAreNotBlockedByBulk(t *testing.T) {
	f := newFairRig(t, 32)
	bound := tunnel.WriteBudget + 3*chunk
	worst := 0
	for i := 0; i < 5; i++ {
		from := len(f.tr.takeKeep())
		f.feed(protocol.Frame{Type: protocol.FramePing, Ts: float64(i)})
		eventually(t, "PONG 写出", func() bool {
			frames := f.tr.takeKeep()
			n := 0
			for j := from; j < len(frames); j++ {
				if frames[j].Type == protocol.FramePong {
					worst = max(worst, n)
					return true
				}
				n += len(frames[j].Data)
			}
			return false
		})
		time.Sleep(7 * time.Millisecond)
	}
	if worst > bound {
		t.Fatalf("PONG 前写出了 %d 字节，应不超过 %d", worst, bound)
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
	time.Sleep(300 * time.Millisecond)
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
	sort.Ints(bulkCounts)
	if bulkCounts[0] == 0 {
		t.Fatalf("有大流量流被饿死：%v", bulkCounts)
	}
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
