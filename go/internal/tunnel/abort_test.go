package tunnel_test

import (
	"bytes"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// #136：两端都已 END（流已摘出流表）、出队列里还有没写出的 DATA 与 END 时再中止，
// 对端要么收到全部字节与 END，要么收到 RST；不能两样都收不到、流在对端一直挂着。

// streamFrames 是流 id 写出的帧：载荷按序拼起来，另答是否写出了 END、RST。
func streamFrames(frames []protocol.Frame, id uint32) (data []byte, end, rst bool) {
	for _, f := range frames {
		if f.StreamID != id {
			continue
		}
		switch f.Type {
		case protocol.FrameData:
			data = append(data, f.Data...)
		case protocol.FrameEnd:
			end = true
		case protocol.FrameRst:
			rst = true
		}
	}
	return data, end, rst
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// 写端卡住时本端 End、对端 END，再中止（各条中止路径）：尾巴与 END 照常写出，不发 RST；
// 写出的字节由对端补 WINDOW，隧道信用恰好回到初始窗口（不重复归还）。
func TestAbortAfterBothEndsKeepsTail(t *testing.T) {
	kills := []struct {
		name string
		kill func(r *rig, s *tunnel.Stream)
	}{
		{"本端 Abort", func(_ *rig, s *tunnel.Stream) { s.Abort() }},
		{"重复 Abort", func(_ *rig, s *tunnel.Stream) { s.Abort(); s.Abort() }},
		{"对端 RST", func(r *rig, s *tunnel.Stream) {
			// 流已摘出流表：迟到的 RST 不影响尾巴写出。
			r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: s.ID(), Code: protocol.RstInternal})
		}},
	}
	for _, k := range kills {
		t.Run(k.name, func(t *testing.T) {
			r := newRig(t, leakConfig)
			r.tr.take()
			gate := gateWriter(r)
			s := r.open(&recSink{})
			tail := payload(2*leakChunk + leakChunk/3)
			if !s.Send(tail) {
				t.Fatal("信用够时 Send 应全部排进出队列")
			}
			s.End()
			r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
			if n := r.tun.StreamCount(); n != 0 {
				t.Fatalf("两端都已 END，流应已摘出流表，实际 %d", n)
			}
			k.kill(r, s)
			r.tr.mu.Lock()
			r.tr.gate = nil
			r.tr.mu.Unlock()
			close(gate)
			eventually(t, "尾巴与 END 写出", func() bool {
				_, end, rst := streamFrames(r.tr.takeKeep(), s.ID())
				return end || rst
			})
			frames := r.tr.take()
			data, end, rst := streamFrames(frames, s.ID())
			if rst {
				t.Fatal("两端都已 END：不应发 RST")
			}
			if !end || !bytes.Equal(data, tail) {
				t.Fatalf("对端应收到全部 %d 字节与 END：收到 %d 字节，END=%v", len(tail), len(data), end)
			}
			r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: uint32(len(data))})
			if got := r.tun.SendCredit(); got != leakWindow {
				t.Fatalf("对端补完 WINDOW 后隧道信用应恰好回到 %d，实际 %d", leakWindow, got)
			}
		})
	}
}

// 写协程已写出一部分、取走一段之后再中止：剩下的段与 END 照常写出，字节不重不漏。
func TestAbortAfterBothEndsMidWrite(t *testing.T) {
	r := newRig(t, leakConfig)
	r.tr.take()
	gate := gateWriter(r)
	s := r.open(&recSink{})
	tail := payload(leakWindow)
	if !s.Send(tail) {
		t.Fatal("信用够时 Send 应全部排进出队列")
	}
	s.End()
	r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
	for i := 0; i < 4 && dataBytesOf(r.tr.takeKeep(), s.ID()) == 0; i++ {
		gate = regate(r, gate)
	}
	if n := dataBytesOf(r.tr.takeKeep(), s.ID()); n != leakChunk {
		t.Fatalf("中止前应已写出 %d 字节，实际 %d", leakChunk, n)
	}
	s.Abort()
	r.tr.mu.Lock()
	r.tr.gate = nil
	r.tr.mu.Unlock()
	close(gate)
	eventually(t, "END 写出", func() bool {
		_, end, rst := streamFrames(r.tr.takeKeep(), s.ID())
		return end || rst
	})
	data, end, rst := streamFrames(r.tr.take(), s.ID())
	if rst || !end || !bytes.Equal(data, tail) {
		t.Fatalf("应收到全部 %d 字节与 END、无 RST：收到 %d 字节，END=%v，RST=%v", len(tail), len(data), end, rst)
	}
	r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: uint32(len(data))})
	if got := r.tun.SendCredit(); got != leakWindow {
		t.Fatalf("隧道信用应恰好回到 %d，实际 %d", leakWindow, got)
	}
}

// 对照：本端还没真正 END（挂着等信用的字节没发完，END 排在它们后面）时对端 END 再中止，
// 仍按原样发 RST、丢掉出队列并还隧道信用（#134）。
func TestAbortBeforeLocalEndQueuedStillResets(t *testing.T) {
	r := newRig(t, leakConfig)
	r.tr.take()
	gate := gateWriter(r)
	s := r.open(&recSink{})
	if s.Send(make([]byte, leakWindow+leakChunk/2)) {
		t.Fatal("信用不够时 Send 应答 false")
	}
	s.End() // 挂着的字节发完之后才发 END
	r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
	if n := r.tun.StreamCount(); n != 1 {
		t.Fatalf("本端 END 还没排进出队列，流应还在流表里，实际 %d", n)
	}
	s.Abort()
	if got := r.tun.SendCredit(); got != leakWindow {
		t.Fatalf("中止应还回出队列的隧道信用，实际 %d", got)
	}
	r.tr.mu.Lock()
	r.tr.gate = nil
	r.tr.mu.Unlock()
	close(gate)
	eventually(t, "RST 写出", func() bool {
		_, _, rst := streamFrames(r.tr.takeKeep(), s.ID())
		return rst
	})
	data, end, _ := streamFrames(r.tr.take(), s.ID())
	if end || len(data) != 0 {
		t.Fatalf("RST 之后不应再写出该流的 DATA / END：%d 字节，END=%v", len(data), end)
	}
}
