package tunnel_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-runtime-node/src/tunnel.test.ts。

func TestHeartbeat(t *testing.T) {
	t.Run("心跳/每 heartbeatMs 发 PING；连续两次未答关闭隧道", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.Heartbeat = 40 * time.Millisecond })
		// 前两个周期各发一个 PING，隧道仍是 open。
		eventually(t, "两个 PING", func() bool { return r.tr.count() >= 2 })
		for _, f := range r.tr.take()[:2] {
			if f.Type != protocol.FramePing {
				t.Fatalf("应是 PING：%+v", f)
			}
		}
		if r.tun.State() != tunnel.StateOpen && r.tr.wasTerminated() == false {
			t.Fatal("两次未答之前不该关")
		}
		eventually(t, "心跳超时断开", r.tr.wasTerminated)
		if r.tun.State() != tunnel.StateClosed {
			t.Fatal("隧道应已关闭")
		}
		r.mu.Lock()
		miss := r.miss
		r.mu.Unlock()
		infos := r.closes()
		if miss != 1 || len(infos) != 1 || infos[0].Code != protocol.CloseGoingAway || infos[0].Reason != "heartbeat_timeout" || infos[0].Replaced {
			t.Fatalf("miss=%d infos=%+v", miss, infos)
		}
	})

	t.Run("心跳/PONG 清零未答计数并回调（路由续期）", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.Heartbeat = 30 * time.Millisecond })
		for i := 0; i < 5; i++ {
			before := r.tr.count()
			eventually(t, "PING", func() bool { return r.tr.count() > before })
			r.feed(protocol.Frame{Type: protocol.FramePong, Ts: 1})
		}
		r.mu.Lock()
		pongs := r.pong
		r.mu.Unlock()
		if pongs != 5 {
			t.Fatalf("PONG 回调 %d 次", pongs)
		}
		if r.tun.State() != tunnel.StateOpen || r.tr.wasTerminated() {
			t.Fatal("一直有回应，隧道应保持 open")
		}
	})

	t.Run("心跳/源发来 PING 立即答 PONG（原样回 ts）", func(t *testing.T) {
		r := newRig(t, nil)
		r.tun.Receive(fixtureBytes(t, "tunnel/ping.bin"))
		raw := r.tr.takeRaw()
		if len(raw) != 1 || !bytes.Equal(raw[0], fixtureBytes(t, "tunnel/pong.bin")) {
			t.Fatalf("应回 pong.bin：%x", raw)
		}
		f, _ := protocol.Decode(raw[0])
		if f.Type != protocol.FramePong || f.Ts != testutil.FixedNow {
			t.Fatalf("%+v", f)
		}
	})
}

// 上行一直有数据时写协程也要按时发 PING，否则源收不到心跳、路由 TTL 过期而隧道还活着。
func TestHeartbeatUnderLoad(t *testing.T) {
	r := newRig(t, func(c *tunnel.Config) {
		c.Heartbeat = 40 * time.Millisecond
		c.HeartbeatMisses = 1000 // 这里不测断开
		c.StreamWindow, c.TunnelWindow = 1<<30, 1<<30
	})
	r.tr.delay = 2 * time.Millisecond
	var streams []*tunnel.Stream
	for i := 0; i < 8; i++ {
		streams = append(streams, r.open(&recSink{}))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				select {
				case <-stop:
					return
				default:
					s.Send(buf)
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	time.Sleep(600 * time.Millisecond)
	close(stop)
	wg.Wait()
	pings := 0
	for _, f := range r.tr.takeKeep() {
		if f.Type == protocol.FramePing {
			pings++
		}
	}
	if pings < 5 {
		t.Fatalf("600 ms 内心跳周期 40 ms，应至少发出 5 个 PING，实际 %d", pings)
	}
}

func TestGoaway(t *testing.T) {
	t.Run("GOAWAY/发 GOAWAY 后拒绝新流，旧流自然结束后以 4492 关闭", func(t *testing.T) {
		r := newRig(t, nil)
		sink := &recSink{}
		s := r.open(sink)
		r.tr.take()
		r.tun.Goaway("shutdown", 30*time.Second)
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameGoaway || got[0].Goaway.Reason != "shutdown" || got[0].Goaway.GraceMs != 30000 {
			t.Fatalf("%+v", got)
		}
		_, err := r.tun.OpenStream(openPayload(), &recSink{}, nil)
		var ue *tunnel.UnavailableError
		if !errors.As(err, &ue) || ue.Reason != tunnel.ReasonDraining {
			t.Fatalf("应拒绝新流：%v", err)
		}
		if r.tun.State() != tunnel.StateDraining {
			t.Fatal("应是 draining")
		}
		s.End()
		r.feed(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
		eventually(t, "隧道关闭", func() bool { return r.tr.closedWith() != nil })
		if c := r.tr.closedWith(); c.Code != protocol.CloseTunnelGoaway || c.Reason != "goaway" {
			t.Fatalf("%+v", c)
		}
		if infos := r.closes(); len(infos) != 1 || infos[0].Replaced {
			t.Fatalf("%+v", infos)
		}
	})

	t.Run("GOAWAY/grace 到了强关，剩下的流以 sourceGone 结束", func(t *testing.T) {
		r := newRig(t, nil)
		sink := &recSink{}
		r.open(sink)
		r.tun.Goaway("rebalance", 150*time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		if r.tun.State() != tunnel.StateDraining {
			t.Fatal("宽限期内仍是 draining")
		}
		eventually(t, "宽限期到强关", func() bool { return r.tun.State() == tunnel.StateClosed })
		eventually(t, "关闭帧", func() bool { return r.tr.closedWith() != nil })
		if r.tr.closedWith().Code != protocol.CloseTunnelGoaway {
			t.Fatalf("%+v", r.tr.closedWith())
		}
		if _, _, reset, _ := sink.snapshot(); reset == nil || *reset != protocol.RstSourceGone {
			t.Fatalf("%v", reset)
		}
	})

	t.Run("GOAWAY/同源替换：GOAWAY replaced，关闭码 4491，标记 replaced", func(t *testing.T) {
		r := newRig(t, nil)
		r.tun.Goaway("replaced", tunnel.DefaultGrace)
		got := r.tr.take()
		if len(got) != 1 || got[0].Goaway.Reason != "replaced" {
			t.Fatalf("%+v", got)
		}
		eventually(t, "关闭帧", func() bool { return r.tr.closedWith() != nil })
		if r.tr.closedWith().Code != protocol.CloseTunnelReplaced {
			t.Fatalf("%+v", r.tr.closedWith())
		}
		if infos := r.closes(); len(infos) != 1 || !infos[0].Replaced {
			t.Fatalf("%+v", infos)
		}
	})

	t.Run("GOAWAY/源发来 GOAWAY：不再开新流并回调", func(t *testing.T) {
		r := newRig(t, nil)
		s := r.open(&recSink{})
		r.tun.Receive(fixtureBytes(t, "tunnel/goaway.bin"))
		r.mu.Lock()
		peer := append([]string(nil), r.peer...)
		r.mu.Unlock()
		if len(peer) != 1 || peer[0] != "shutdown" {
			t.Fatalf("%v", peer)
		}
		if r.tun.Accepting() || r.tun.State() != tunnel.StateDraining {
			t.Fatal("应不再接新流")
		}
		s.Abort()
		if r.tun.State() != tunnel.StateClosed {
			t.Fatal("最后一条流没了，隧道应关闭")
		}
	})
}

func TestFramesAndTable(t *testing.T) {
	t.Run("帧与流表/坏帧关 4400；超大帧关 4413", func(t *testing.T) {
		a := newRig(t, nil)
		a.tun.Receive([]byte{99, 0, 0, 0, 0})
		eventually(t, "关闭", func() bool { return a.tr.closedWith() != nil })
		if a.tr.closedWith().Code != protocol.CloseBadFrame {
			t.Fatalf("%+v", a.tr.closedWith())
		}
		b := newRig(t, nil)
		b.tun.Receive(make([]byte, protocol.MaxFrameBytes+1))
		eventually(t, "关闭", func() bool { return b.tr.closedWith() != nil })
		if b.tr.closedWith().Code != protocol.CloseFrameTooLarge {
			t.Fatalf("%+v", b.tr.closedWith())
		}
	})

	t.Run("帧与流表/源发 OPEN 是协议错误：答 RST protocol", func(t *testing.T) {
		r := newRig(t, nil)
		r.tun.Receive(fixtureBytes(t, "tunnel/open-http.bin"))
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameRst || got[0].StreamID != 1 || got[0].Code != protocol.RstProtocol {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("帧与流表/超过 maxStreams 拒绝开流", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.MaxStreams = 2 })
		r.open(&recSink{})
		r.open(&recSink{})
		_, err := r.tun.OpenStream(openPayload(), &recSink{}, nil)
		var ue *tunnel.UnavailableError
		if !errors.As(err, &ue) || ue.Reason != tunnel.ReasonMaxStreams {
			t.Fatalf("%v", err)
		}
	})

	t.Run("帧与流表/未知流的 DATA 丢弃但还隧道信用；关隧道时流以 sourceGone 结束", func(t *testing.T) {
		r := newRig(t, func(c *tunnel.Config) { c.TunnelWindow = 100000 })
		r.tun.Receive(fixtureBytes(t, "tunnel/data-64k.bin"))
		got := r.tr.take()
		if len(got) != 1 || got[0].Type != protocol.FrameWindow || got[0].StreamID != 0 || got[0].Credit != 65536 {
			t.Fatalf("%+v", got)
		}
		sink := &recSink{}
		r.open(sink)
		r.tun.TransportClosed(1006, "")
		if _, _, reset, _ := sink.snapshot(); reset == nil || *reset != protocol.RstSourceGone {
			t.Fatalf("%v", reset)
		}
	})

	t.Run("帧与流表/关闭时流表里每条流都 RST 3", func(t *testing.T) {
		r := newRig(t, nil)
		sinks := []*recSink{{}, {}, {}}
		for _, s := range sinks {
			r.open(s)
		}
		r.tun.Close(protocol.CloseNormal, "done")
		for i, s := range sinks {
			if _, _, reset, _ := s.snapshot(); reset == nil || *reset != protocol.RstSourceGone {
				t.Fatalf("第 %d 条流没收到 RST 3", i)
			}
		}
		if r.tun.StreamCount() != 0 {
			t.Fatal("流表应清空")
		}
	})

	t.Run("帧与流表/隧道关闭后 OpenStream 与 Send 都不再生效", func(t *testing.T) {
		r := newRig(t, nil)
		s := r.open(&recSink{})
		r.tun.Close(protocol.CloseNormal, "done")
		if _, err := r.tun.OpenStream(openPayload(), &recSink{}, nil); err == nil {
			t.Fatal("应拒绝")
		}
		if !s.Send([]byte("x")) {
			t.Fatal("关闭后的 Send 应当作已处理")
		}
	})
}

// 写协程还没起来（握手刚完成、Start 之前）就来了 GOAWAY：关闭前要把排队的 GOAWAY 帧发出去，
// 不能只发关闭码（对端靠帧里的 reason 知道是被替换了）。
func TestGoawayBeforeStartIsDelivered(t *testing.T) {
	tr := &fakeTransport{}
	tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", tr, testConfig(nil), tunnel.Hooks{})
	tun.Goaway("replaced", 30*time.Second)
	got := tr.take()
	if len(got) != 1 || got[0].Type != protocol.FrameGoaway || got[0].Goaway.Reason != "replaced" {
		t.Fatalf("关闭前应发出 GOAWAY：%+v", got)
	}
	if c := tr.closedWith(); c == nil || c.Code != protocol.CloseTunnelReplaced {
		t.Fatalf("随后以 replaced 的关闭码关闭：%+v", c)
	}
}
