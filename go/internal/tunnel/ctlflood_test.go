package tunnel_test

import (
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 控制帧刷帧（#172）：对端读得慢（写端卡在第一帧上）又不停发控制帧，队列不能无限长。
// 不靠时钟：写端卡在 gate 上，队列长度在每次收帧之后直接断言。

// gatedRig 建一条写端会卡在 gate 上的隧道。
func gatedRig(t *testing.T, over func(*tunnel.Config)) (*tunnel.Tunnel, *fakeTransport, func() []tunnel.CloseInfo) {
	t.Helper()
	tr := &fakeTransport{gate: make(chan struct{})}
	var infos []tunnel.CloseInfo
	tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", tr, testConfig(over), tunnel.Hooks{
		OnClose: func(i tunnel.CloseInfo) { tr.mu.Lock(); infos = append(infos, i); tr.mu.Unlock() },
	})
	tun.Start()
	t.Cleanup(func() {
		tun.Close(protocol.CloseNormal, "cleanup")
		tr.release()
	})
	closes := func() []tunnel.CloseInfo {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return append([]tunnel.CloseInfo(nil), infos...)
	}
	return tun, tr, closes
}

// release 放开卡住的写端（只放一次）。
func (f *fakeTransport) release() {
	f.mu.Lock()
	gate := f.gate
	f.gate = nil
	f.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (f *fakeTransport) gatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gated
}

func TestCtlFlood(t *testing.T) {
	t.Run("PING 刷帧/待发 PONG 只留最新一条，隧道不关", func(t *testing.T) {
		tun, tr, closes := gatedRig(t, nil)
		ping := func(ts float64) { tun.Receive(mustEncodeFrame(t, protocol.Frame{Type: protocol.FramePing, Ts: ts})) }
		// 第一条 PONG 被写端取走、卡在 Send 上。
		ping(1)
		eventually(t, "写端卡住", func() bool { return tr.gatedCount() == 1 })
		const n = 20000
		for i := 2; i <= n+1; i++ {
			ping(float64(i))
			if q := tun.CtlQueued(); q > 1 {
				t.Fatalf("第 %d 条 PING 之后队列里有 %d 帧，PONG 没合并", i, q)
			}
		}
		if tun.State() != tunnel.StateOpen || len(closes()) != 0 {
			t.Fatalf("隧道不该关：state=%v closes=%+v", tun.State(), closes())
		}
		tr.release()
		frames := tr.take()
		if len(frames) != 2 || frames[0].Type != protocol.FramePong || frames[0].Ts != 1 || frames[1].Type != protocol.FramePong || frames[1].Ts != n+1 {
			t.Fatalf("应只写出第一条与最新一条 PONG：%+v", frames)
		}
	})

	t.Run("控制帧刷帧/队列到上限整条丢掉、以 4429 关隧道", func(t *testing.T) {
		const limit = 8
		tun, tr, closes := gatedRig(t, func(c *tunnel.Config) { c.MaxCtlQueue = limit })
		// 对端发 OPEN 答 RST protocol：每条占队列一格，不合并。第一条被写端取走并卡住。
		open := func(id uint32) {
			tun.Receive(mustEncodeFrame(t, protocol.Frame{Type: protocol.FrameOpen, StreamID: id, Open: openPayload()}))
		}
		open(2)
		eventually(t, "写端卡住", func() bool { return tr.gatedCount() == 1 })
		for i := uint32(4); i <= 2+2*limit; i += 2 {
			open(i)
			if q := tun.CtlQueued(); q > limit {
				t.Fatalf("队列 %d 帧超过上限 %d", q, limit)
			}
		}
		if tun.State() != tunnel.StateOpen || tun.CtlQueued() != limit {
			t.Fatalf("上限之内不该关：state=%v queued=%d", tun.State(), tun.CtlQueued())
		}
		open(2 + 2*limit + 2) // 第 limit+1 条：溢出
		if tun.State() != tunnel.StateClosed || tun.CtlQueued() != 0 {
			t.Fatalf("溢出应立刻关隧道并清空队列：state=%v queued=%d", tun.State(), tun.CtlQueued())
		}
		infos := closes()
		if len(infos) != 1 || infos[0].Code != protocol.CloseLimitReached || infos[0].Reason != tunnel.ReasonCtlQueueOverflow {
			t.Fatalf("关闭信息 %+v", infos)
		}
		// 之后再来 PING 不再排队。
		tun.Receive(mustEncodeFrame(t, protocol.Frame{Type: protocol.FramePing, Ts: 9}))
		if tun.CtlQueued() != 0 {
			t.Fatal("关闭后还在排控制帧")
		}
		tr.release()
		eventually(t, "发关闭帧", func() bool { return tr.closedWith() != nil })
		if c := tr.closedWith(); c.Code != protocol.CloseLimitReached || c.Reason != tunnel.ReasonCtlQueueOverflow {
			t.Fatalf("传输层关闭码 %+v", c)
		}
		select {
		case <-tun.Closed():
		case <-time.After(3 * time.Second):
			t.Fatal("写协程没收尾")
		}
	})
}

func mustEncodeFrame(t *testing.T, f protocol.Frame) []byte {
	t.Helper()
	b, err := protocol.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
