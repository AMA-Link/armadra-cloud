package tunnel_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// #134：流被重置时，出队列里已扣隧道信用、还没写出的段要把信用还给隧道；
// 否则对端永远收不到这些字节、也不会为它们补 WINDOW，带积压的流反复重置后隧道停发。

const (
	leakChunk  = 1000
	leakWindow = 3 * leakChunk
)

func leakConfig(c *tunnel.Config) {
	c.MaxDataChunk = leakChunk
	c.StreamWindow = 10 * leakWindow
	c.TunnelWindow = leakWindow
}

// gateWriter 让写端卡在一个控制帧上，之后排进出队列的段都留在队列里。
func gateWriter(r *rig) chan struct{} {
	r.t.Helper()
	gate := make(chan struct{})
	r.tr.mu.Lock()
	r.tr.gate = gate
	r.tr.mu.Unlock()
	r.feed(protocol.Frame{Type: protocol.FramePing, Ts: 1})
	eventually(r.t, "写端卡在 PONG 上", func() bool {
		r.tr.mu.Lock()
		defer r.tr.mu.Unlock()
		return r.tr.gated > 0
	})
	return gate
}

// dataBytesOf 是流 id 已经写出的 DATA 载荷字节。
func dataBytesOf(frames []protocol.Frame, id uint32) int {
	n := 0
	for _, f := range dataFrames(frames) {
		if f.StreamID == id {
			n += len(f.Data)
		}
	}
	return n
}

func TestResetReturnsQueuedTunnelCredit(t *testing.T) {
	kills := []struct {
		name string
		kill func(r *rig, s *tunnel.Stream)
	}{
		{"对端 RST", func(r *rig, s *tunnel.Stream) {
			r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: s.ID(), Code: protocol.RstInternal})
		}},
		{"本端 Abort", func(_ *rig, s *tunnel.Stream) { s.Abort() }},
		{"End 之后对端 RST", func(r *rig, s *tunnel.Stream) {
			s.End()
			r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: s.ID(), Code: protocol.RstProtocol})
		}},
	}
	r := newRig(t, leakConfig)
	r.tr.take()
	for round := 0; round < 4; round++ {
		for _, k := range kills {
			gate := gateWriter(r)
			s := r.open(&recSink{})
			// 积压：一个窗口的段排进出队列（扣光隧道信用），再挂起一些等信用的字节。
			if s.Send(make([]byte, leakWindow+leakChunk/2)) {
				t.Fatalf("第 %d 轮 %s：信用不够时 Send 应答 false", round, k.name)
			}
			if got := r.tun.SendCredit(); got != 0 {
				t.Fatalf("第 %d 轮 %s：排队后隧道信用应为 0，实际 %d", round, k.name, got)
			}
			k.kill(r, s)
			close(gate)
			// 已经写出的字节由对端在收到后补 WINDOW(0)（对端对已重置流的 DATA 照样还隧道信用）。
			if sent := dataBytesOf(r.tr.take(), s.ID()); sent > 0 {
				r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: uint32(sent)})
			}
			r.tr.mu.Lock()
			r.tr.gate = nil
			r.tr.mu.Unlock()
			if got := r.tun.SendCredit(); got != leakWindow {
				t.Fatalf("第 %d 轮 %s：重置后隧道信用应回到 %d，实际 %d", round, k.name, leakWindow, got)
			}
		}
	}
	// 反复重置之后，新流仍能用满整个隧道窗口。
	other := &recSink{}
	b := r.open(other)
	if !b.Send(make([]byte, leakWindow)) {
		t.Fatal("新流应能用满隧道窗口")
	}
	eventually(t, "新流的字节全部写出", func() bool { return dataBytesOf(r.tr.takeKeep(), b.ID()) == leakWindow })
}

// 别的流因隧道信用不够挂起时，重置带积压的流还回信用要把它叫醒（排进出队列并 Resume）。
func TestResetWakesStreamParkedOnTunnelCredit(t *testing.T) {
	for _, peer := range []bool{false, true} {
		name := "本端 Abort"
		if peer {
			name = "对端 RST"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, leakConfig)
			r.tr.take()
			gate := gateWriter(r)
			sa, sb := &recSink{}, &recSink{}
			a, b := r.open(sa), r.open(sb)
			if !a.Send(make([]byte, leakWindow)) {
				t.Fatal("a 应全部排进出队列")
			}
			if b.Send(make([]byte, leakChunk)) || !b.Parked() {
				t.Fatal("隧道信用用光时 b 应挂起")
			}
			if peer {
				r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: a.ID(), Code: protocol.RstInternal})
			} else {
				a.Abort()
			}
			if b.Parked() {
				t.Fatal("a 还回信用后 b 不应再挂起")
			}
			if _, _, _, resumes := sb.snapshot(); resumes != 1 {
				t.Fatalf("b 应被 Resume 一次，实际 %d", resumes)
			}
			close(gate)
			eventually(t, "b 的字节写出", func() bool { return dataBytesOf(r.tr.takeKeep(), b.ID()) == leakChunk })
			if n := dataBytesOf(r.tr.takeKeep(), a.ID()); n != 0 {
				t.Fatalf("a 已重置，出队列里的 %d 字节不应写出", n)
			}
			if got := r.tun.SendCredit(); got != leakWindow-leakChunk {
				t.Fatalf("隧道信用应为 %d，实际 %d", leakWindow-leakChunk, got)
			}
		})
	}
}

// 隧道关闭时不还信用（隧道已不可用），也不应出错。
func TestCloseWithQueuedOutputKeepsCredit(t *testing.T) {
	r := newRig(t, leakConfig)
	r.tr.take()
	gate := gateWriter(r)
	s := r.open(&recSink{})
	s.Send(make([]byte, leakWindow))
	r.tun.Close(protocol.CloseNormal, "test")
	close(gate)
	if got := r.tun.SendCredit(); got != 0 {
		t.Fatalf("隧道关闭后不再还信用，实际 %d", got)
	}
}
