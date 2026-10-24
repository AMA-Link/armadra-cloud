package tunnel_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// #138：排空（GOAWAY）期间最后一条流两端都 END、已摘出流表，但出队列还没写完时，
// 隧道要等尾巴 DATA 与 END 写出之后再关，不能立刻发关闭帧把尾巴丢掉。

// openGate 放开写端。
func openGate(r *rig, gate chan struct{}) {
	r.tr.mu.Lock()
	r.tr.gate = nil
	r.tr.mu.Unlock()
	close(gate)
}

// 尾巴在关闭帧之前写完：源收到全部字节与 END，之后才是 4492。
func assertTailThenClose(t *testing.T, r *rig, s *tunnel.Stream, tail []byte) {
	t.Helper()
	eventually(t, "隧道关闭", func() bool { return r.tr.closedWith() != nil })
	data, end, rst := streamFrames(r.tr.take(), s.ID())
	if rst {
		t.Fatal("两端都已 END：不应发 RST")
	}
	if !end || !bytes.Equal(data, tail) {
		t.Fatalf("源应收到全部 %d 字节与 END 后再关隧道：收到 %d 字节，END=%v", len(tail), len(data), end)
	}
	if c := r.tr.closedWith(); c.Code != protocol.CloseTunnelGoaway || c.Reason != "goaway" {
		t.Fatalf("尾巴写完后应以 4492 goaway 关闭，实际 %+v", c)
	}
}

func TestDrainKeepsTailOfReleasedStream(t *testing.T) {
	cases := []struct {
		name string
		over func(*tunnel.Config)
		size int
		// goaway 让隧道进入排空：本端发 GOAWAY，或收到对端的 GOAWAY。
		goaway func(r *rig)
	}{
		{"本端 Goaway 单帧尾巴", nil, 2000, func(r *rig) { r.tun.Goaway("shutdown", 30*time.Second) }},
		{"本端 Goaway 多帧尾巴", leakConfig, 2*leakChunk + leakChunk/3, func(r *rig) { r.tun.Goaway("shutdown", 30*time.Second) }},
		{"对端 GOAWAY", nil, 2000, func(r *rig) {
			r.feed(protocol.Frame{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: "shutdown", GraceMs: 30000}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.over)
			s := r.open(&recSink{})
			r.tr.take()
			gate := gateWriter(r)
			c.goaway(r)
			tail := payload(c.size)
			if !s.Send(tail) {
				t.Fatal("信用够时 Send 应全部排进出队列")
			}
			s.End()
			r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
			if n := r.tun.StreamCount(); n != 0 {
				t.Fatalf("两端都已 END，流应已摘出流表，实际 %d", n)
			}
			if st := r.tun.State(); st != tunnel.StateDraining {
				t.Errorf("尾巴没写完，隧道应仍在排空，实际 %v", st)
			}
			openGate(r, gate)
			assertTailThenClose(t, r, s, tail)
		})
	}
}

// 两条流先后摘表：前一条写空时还有一条带尾巴的，隧道等最后一条写完才关。
func TestDrainWaitsForEveryReleasedTail(t *testing.T) {
	r := newRig(t, leakConfig)
	a, b := r.open(&recSink{}), r.open(&recSink{})
	r.tr.take()
	gate := gateWriter(r)
	r.tun.Goaway("shutdown", 30*time.Second)
	ta, tb := payload(leakChunk), payload(leakChunk+leakChunk/2)
	if !a.Send(ta) || !b.Send(tb) {
		t.Fatal("信用够时 Send 应全部排进出队列")
	}
	for _, s := range []*tunnel.Stream{a, b} {
		s.End()
		r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
	}
	openGate(r, gate)
	eventually(t, "隧道关闭", func() bool { return r.tr.closedWith() != nil })
	frames := r.tr.take()
	for _, x := range []struct {
		s    *tunnel.Stream
		tail []byte
	}{{a, ta}, {b, tb}} {
		data, end, rst := streamFrames(frames, x.s.ID())
		if rst || !end || !bytes.Equal(data, x.tail) {
			t.Fatalf("流 %d 应收到全部 %d 字节与 END：收到 %d 字节，END=%v，RST=%v", x.s.ID(), len(x.tail), len(data), end, rst)
		}
	}
	if c := r.tr.closedWith(); c.Code != protocol.CloseTunnelGoaway {
		t.Fatalf("应以 4492 关闭，实际 %+v", c)
	}
}

// 宽限到了仍按时强关：排空的上界不因尾巴没写完而延长。
func TestDrainTailBoundedByGrace(t *testing.T) {
	r := newRig(t, nil)
	s := r.open(&recSink{})
	r.tr.take()
	gate := gateWriter(r)
	defer openGate(r, gate)
	r.tun.Goaway("shutdown", 100*time.Millisecond)
	s.Send(payload(2000))
	s.End()
	r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
	eventually(t, "宽限到期关闭", func() bool { return r.tun.State() == tunnel.StateClosed })
	if info := r.closes(); len(info) != 1 || info[0].Reason != "goaway_grace" {
		t.Fatalf("写端卡住时应在宽限到期后关闭，实际 %+v", info)
	}
}

// 隧道关闭后 tails 清零；孤儿流（已摘表、尾巴没写完）此后再被中止也不会把计数减成负数。
func TestTailsNotNegativeAfterClose(t *testing.T) {
	r := newRig(t, nil)
	s := r.open(&recSink{})
	r.tr.take()
	gate := gateWriter(r)
	defer openGate(r, gate)
	s.Send(payload(2000))
	s.End()
	r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
	if n := r.tun.Tails(); n != 1 {
		t.Fatalf("摘表时出队列非空，tails 应为 1，实际 %d", n)
	}
	r.tun.Close(protocol.CloseNormal, "test")
	s.Abort()
	r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: s.ID(), Code: protocol.RstInternal})
	if n := r.tun.Tails(); n != 0 {
		t.Fatalf("隧道关闭后 tails 应为 0，实际 %d", n)
	}
}
