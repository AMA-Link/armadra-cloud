package saas

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
)

// 节点间链路的控制帧刷帧（#172）：待发 PONG 只留最新一条；控制帧队列到上限整条丢掉、断开。
func TestInternodeCtlFlood(t *testing.T) {
	t.Run("PING 刷帧只留最新一条 PONG", func(t *testing.T) {
		c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1), pongAt: -1}
		c.mu.Lock()
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: 1}), nil)
		c.mu.Unlock()
		const n = 20000
		for i := 1; i <= n; i++ {
			if !c.receive(encode(protocol.Frame{Type: protocol.FramePing, Ts: float64(i)})) {
				t.Fatalf("第 %d 条 PING 被当成断开", i)
			}
			if len(c.ctl) > 2 {
				t.Fatalf("第 %d 条 PING 之后队列 %d 帧，PONG 没合并", i, len(c.ctl))
			}
		}
		f, err := protocol.DecodeView(c.ctl[1].frame)
		if err != nil || f.Type != protocol.FramePong || f.Ts != n {
			t.Fatalf("队列里应是最新的 PONG：%+v（%v）", f, err)
		}
		// 写协程取走一批之后，下一条 PING 重新排队。
		c.mu.Lock()
		_, _, _ = c.collectLocked(false)
		c.mu.Unlock()
		c.receive(encode(protocol.Frame{Type: protocol.FramePing, Ts: 7}))
		if len(c.ctl) != 1 {
			t.Fatalf("取走之后新 PONG 应排队：%d", len(c.ctl))
		}
	})

	t.Run("控制帧队列到上限整条丢掉并断开，after 照常回调", func(t *testing.T) {
		c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1), pongAt: -1}
		rst := encode(protocol.Frame{Type: protocol.FrameRst, StreamID: 1, Code: protocol.RstProtocol})
		afters := 0
		c.mu.Lock()
		for i := 0; i < maxInternodeCtl; i++ {
			c.sendLocked(rst, func() { afters++ })
		}
		if len(c.ctl) != maxInternodeCtl || c.overflow {
			t.Fatalf("上限之内不该溢出：%d %v", len(c.ctl), c.overflow)
		}
		c.sendLocked(rst, func() { afters++ })
		c.mu.Unlock()
		if !c.overflow || len(c.ctl) != 0 || afters != maxInternodeCtl+1 {
			t.Fatalf("溢出后 overflow=%v ctl=%d afters=%d", c.overflow, len(c.ctl), afters)
		}
		if c.receive(encode(protocol.Frame{Type: protocol.FramePing, Ts: 1})) {
			t.Fatal("溢出后 receive 应答断开")
		}
		if len(c.ctl) != 0 {
			t.Fatal("溢出后还在排控制帧")
		}
	})
}
