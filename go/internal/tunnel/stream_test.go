package tunnel_test

import (
	"bytes"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-runtime-node/src/stream.test.ts。

func TestStreamWrite(t *testing.T) {
	t.Run("写：切块与信用/按 64 KiB 切块", func(t *testing.T) {
		r := newRig(t, nil)
		s := r.open(&recSink{})
		r.tr.take()
		ok := s.Send(bytes.Repeat([]byte{1}, protocol.MaxDataChunk*2+10))
		got := sizes(r.tr.take())
		want := []int{65536, 65536, 10}
		if !ok || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("ok=%v 块 %v", ok, got)
		}
	})

	t.Run("写：流窗口耗尽时 DATA 停发，WINDOW 后继续", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1000 })
		sink := &recSink{}
		s := r.open(sink)
		r.tr.take()
		if s.Send(bytes.Repeat([]byte{2}, 2500)) {
			t.Fatal("信用不够时 Send 应答 false")
		}
		if got := sizes(r.tr.take()); len(got) != 1 || got[0] != 1000 {
			t.Fatalf("块 %v", got)
		}
		r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: s.ID(), Credit: 1000})
		if got := sizes(r.tr.take()); len(got) != 1 || got[0] != 1000 {
			t.Fatalf("块 %v", got)
		}
		if _, _, _, resumes := sink.snapshot(); resumes != 0 {
			t.Fatal("还有挂起的字节时不该 Resume")
		}
		r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: s.ID(), Credit: 1000})
		if got := sizes(r.tr.take()); len(got) != 1 || got[0] != 500 {
			t.Fatalf("块 %v", got)
		}
		if _, _, _, resumes := sink.snapshot(); resumes != 1 {
			t.Fatalf("发完应 Resume 一次，实际 %d", resumes)
		}
		if s.Parked() {
			t.Fatal("不应再挂起")
		}
	})

	t.Run("写：隧道级窗口由所有流共享，WINDOW(0) 叫醒等待的流", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 10000; c.TunnelWindow = 1500 })
		sa, sb := &recSink{}, &recSink{}
		a, b := r.open(sa), r.open(sb)
		r.tr.take()
		a.Send(make([]byte, 1000))
		b.Send(make([]byte, 1000))
		first := dataFrames(r.tr.take())
		if len(first) != 2 || first[0].StreamID != a.ID() || len(first[0].Data) != 1000 ||
			first[1].StreamID != b.ID() || len(first[1].Data) != 500 {
			t.Fatalf("%+v", first)
		}
		r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: 500})
		second := dataFrames(r.tr.take())
		if len(second) != 1 || second[0].StreamID != b.ID() || len(second[0].Data) != 500 {
			t.Fatalf("%+v", second)
		}
		if _, _, _, resumes := sb.snapshot(); resumes != 1 {
			t.Fatalf("b 应被叫醒，Resume %d 次", resumes)
		}
	})

	t.Run("写：每流每轮最多一个 DATA（轮转）", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1 << 20; c.TunnelWindow = 1 << 22 })
		a, b := r.open(&recSink{}), r.open(&recSink{})
		r.tr.take()
		// 让写端停在第一帧上，攒齐两条流的队列；放开后应交错出队，而不是先把 a 写完。
		gate := make(chan struct{})
		r.tr.mu.Lock()
		r.tr.gate = gate
		r.tr.mu.Unlock()
		a.Send(make([]byte, protocol.MaxDataChunk*3))
		b.Send(make([]byte, protocol.MaxDataChunk*3))
		close(gate)
		eventually(t, "6 个 DATA", func() bool { return len(dataFrames(r.tr.takeKeep())) == 6 })
		var order []uint32
		for _, f := range dataFrames(r.tr.take()) {
			order = append(order, f.StreamID)
		}
		if len(order) != 6 {
			t.Fatalf("顺序 %v", order)
		}
		// b 的第一帧必须排在 a 的最后一帧之前，说明没有队头阻塞。
		firstB, lastA := -1, -1
		for i, id := range order {
			if id == b.ID() && firstB < 0 {
				firstB = i
			}
			if id == a.ID() {
				lastA = i
			}
		}
		if firstB < 0 || firstB > lastA {
			t.Fatalf("没有轮转：%v", order)
		}
	})
}

func TestStreamRead(t *testing.T) {
	t.Run("读：交给消费者的字节累计到窗口一半时发 WINDOW，隧道级同理", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1000; c.TunnelWindow = 4000 })
		sink := &recSink{autoConsume: true}
		s := r.open(sink)
		r.tr.take()
		feed := func() {
			r.feed(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: make([]byte, 400)})
		}
		feed()
		if got := r.tr.take(); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
		feed()
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameWindow || got[0].StreamID != s.ID() || got[0].Credit != 800 {
			t.Fatalf("%+v", got)
		}
		feed()
		feed()
		feed()
		got = r.tr.take()
		// 第 4 次 400 字节后流级又补 800；第 5 次后隧道级累计 2000 = 4000/2，补 2000。
		if len(got) != 2 || got[0].StreamID != s.ID() || got[0].Credit != 800 || got[1].StreamID != 0 || got[1].Credit != 2000 {
			t.Fatalf("%+v", got)
		}
		if data, _, _, _ := sink.snapshot(); len(data) != 2000 {
			t.Fatalf("收到 %d 字节", len(data))
		}
	})

	t.Run("读：消费者没交付时不补信用（背压传到对端）", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1000 })
		sink := &recSink{} // 不调 Consumed
		s := r.open(sink)
		r.tr.take()
		r.feed(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: make([]byte, 1000)})
		for _, f := range r.tr.take() {
			if f.Type == protocol.FrameWindow {
				t.Fatalf("不该补信用：%+v", f)
			}
		}
	})

	t.Run("读：对端超出流信用 → RST 5 + 关隧道 4400", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 1000 })
		sink := &recSink{}
		s := r.open(sink)
		r.tr.take()
		r.feed(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: make([]byte, 1001)})
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameRst || got[0].StreamID != s.ID() || got[0].Code != protocol.RstFlowControl {
			t.Fatalf("%+v", got)
		}
		eventually(t, "隧道关闭", func() bool { return r.tr.closedWith() != nil })
		if c := r.tr.closedWith(); c.Code != 4400 || c.Reason != "flow_control" {
			t.Fatalf("%+v", c)
		}
		if r.tun.State() != tunnel.StateClosed {
			t.Fatal("隧道应已关闭")
		}
		if _, _, reset, _ := sink.snapshot(); reset == nil || *reset != protocol.RstSourceGone {
			t.Fatalf("流应以 sourceGone 结束：%v", reset)
		}
	})

	t.Run("读：对端超出隧道信用 → RST 5 + 4400", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 10000; c.TunnelWindow = 500 })
		s := r.open(&recSink{})
		r.feed(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: make([]byte, 501)})
		eventually(t, "隧道关闭", func() bool { return r.tr.closedWith() != nil })
		if c := r.tr.closedWith(); c.Code != 4400 {
			t.Fatalf("%+v", c)
		}
	})
}

func TestStreamEndRst(t *testing.T) {
	t.Run("END/RST 双向：收 END → 读端结束；本地 End → 发 END；两端都结束后摘出流表", func(t *testing.T) {
		r := newRig(t, nil)
		sink := &recSink{}
		s := r.open(sink)
		r.tr.take()
		r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
		if _, ended, _, _ := sink.snapshot(); !ended {
			t.Fatal("应收到 End")
		}
		if r.tun.StreamCount() != 1 {
			t.Fatal("本地没结束，流应还在")
		}
		s.End()
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameEnd || got[0].StreamID != s.ID() {
			t.Fatalf("%+v", got)
		}
		if r.tun.StreamCount() != 0 {
			t.Fatal("两端都结束后应摘出流表")
		}
	})

	t.Run("END/RST 双向：END 排在挂着的上行字节之后", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 100 })
		s := r.open(&recSink{})
		r.tr.take()
		s.Send(make([]byte, 250))
		s.End()
		first := r.tr.take()
		for _, f := range first {
			if f.Type == protocol.FrameEnd {
				t.Fatal("还有字节没发完，不该发 END")
			}
		}
		r.feed(protocol.Frame{Type: protocol.FrameWindow, StreamID: s.ID(), Credit: 500})
		got := r.tr.take()
		if len(got) != 2 || got[0].Type != protocol.FrameData || got[1].Type != protocol.FrameEnd {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("END/RST 双向：收 RST → Reset 带码；本地 Abort → 发 RST clientGone", func(t *testing.T) {
		r := newRig(t, nil)
		sa := &recSink{}
		a := r.open(sa)
		r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: a.ID(), Code: protocol.RstRefused})
		if _, _, reset, _ := sa.snapshot(); reset == nil || *reset != protocol.RstRefused {
			t.Fatalf("%v", reset)
		}
		b := r.open(&recSink{})
		r.tr.take()
		b.Abort()
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameRst || got[0].StreamID != b.ID() || got[0].Code != protocol.RstClientGone {
			t.Fatalf("%+v", got)
		}
		if r.tun.StreamCount() != 0 {
			t.Fatal("流应已摘掉")
		}
	})

	t.Run("END/RST 双向：对端已重置的流本地销毁不再发 RST", func(t *testing.T) {
		r := newRig(t, nil)
		s := r.open(&recSink{})
		r.feed(protocol.Frame{Type: protocol.FrameRst, StreamID: s.ID(), Code: protocol.RstRefused})
		r.tr.take()
		s.Abort()
		if got := r.tr.take(); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("销毁时丢掉的未交付字节照样还隧道级信用", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.StreamWindow = 10000; c.TunnelWindow = 1000 })
		sink := &recSink{}
		s := r.open(sink)
		r.feed(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: make([]byte, 600)})
		r.tr.take()
		s.Abort()
		s.Consumed(600) // 边缘关连接时把没写出去的字节记为已消费
		var found bool
		for _, f := range r.tr.take() {
			if f.Type == protocol.FrameWindow && f.StreamID == 0 && f.Credit == 600 {
				found = true
			}
		}
		if !found {
			t.Fatal("应补 WINDOW(0, 600)")
		}
	})
}

func TestStreamGolden(t *testing.T) {
	t.Run("黄金夹具/HTTP 往返序列：中继发出的帧与夹具逐字节相同，源的响应原样交出", func(t *testing.T) {
		seq := splitSequence(t, fixtureBytes(t, "tunnel/sequence-http-roundtrip.bin"))
		frames := make([]protocol.Frame, len(seq))
		for i, b := range seq {
			f, err := protocol.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			frames[i] = f
		}
		if frames[0].Type != protocol.FrameOpen {
			t.Fatal("序列第一帧应是 OPEN")
		}
		r := newRig(t, nil)
		sink := &recSink{autoConsume: true}
		s, err := r.tun.OpenStream(frames[0].Open, sink, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames[1:3] {
			s.Send(f.Data)
		}
		s.End()
		sent := r.tr.takeRaw()
		if len(sent) != 4 {
			t.Fatalf("发出 %d 帧", len(sent))
		}
		for i := range sent {
			if !bytes.Equal(sent[i], seq[i]) {
				t.Fatalf("第 %d 帧与夹具不同", i+1)
			}
		}
		for _, b := range seq[4:] {
			r.tun.Receive(b)
		}
		data, ended, _, _ := sink.snapshot()
		if !ended || !bytes.Equal(data, frames[4].Data) {
			t.Fatalf("响应不对：ended=%v %d 字节", ended, len(data))
		}
		if r.tun.StreamCount() != 0 {
			t.Fatal("流应已摘掉")
		}
	})

	t.Run("黄金夹具/OPEN 帧与 open-http.bin 相同", func(t *testing.T) {
		golden := fixtureBytes(t, "tunnel/open-http.bin")
		f, err := protocol.Decode(golden)
		if err != nil {
			t.Fatal(err)
		}
		r := newRig(t, nil)
		if _, err := r.tun.OpenStream(f.Open, &recSink{}, nil); err != nil {
			t.Fatal(err)
		}
		raw := r.tr.takeRaw()
		if len(raw) != 1 || !bytes.Equal(raw[0], golden) {
			t.Fatal("OPEN 与夹具不同")
		}
	})
}
