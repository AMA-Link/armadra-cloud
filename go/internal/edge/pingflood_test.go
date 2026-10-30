package edge

import (
	"testing"
	"time"
)

// 客户端 PING 刷帧（#172）：一个 ping 轮内超过 maxPingsPerRound 条就按 1008 关闭；PING 不刷新活性。
func TestPingFlood(t *testing.T) {
	t.Run("上限之内每条都答 PONG，超限关 1008 ping_flood", func(t *testing.T) {
		p := newPipe(t, nil)
		ping := clientFrameBytes(opPing, true, []byte("p"))
		for i := 0; i < maxPingsPerRound; i++ {
			p.read(ping)
		}
		if n := len(p.conn.sent()); n != maxPingsPerRound {
			t.Fatalf("写出 %d 帧，应是 %d 条 PONG", n, maxPingsPerRound)
		}
		p.read(ping)
		cf := p.conn.closeFrames()
		if len(cf) != 1 || cf[0].code != closePolicyViolation || cf[0].reason != "ping_flood" {
			t.Fatalf("关闭帧 %+v", cf)
		}
		if n := len(p.conn.sent()); n != maxPingsPerRound+1 {
			t.Fatalf("超限的 PING 不该再答 PONG：写出 %d 帧", n)
		}
		p.read(ping)
		if n := len(p.conn.sent()); n != maxPingsPerRound+1 {
			t.Fatalf("关闭后还在写：%d 帧", n)
		}
	})

	t.Run("ping 轮清零计数", func(t *testing.T) {
		p := newPipe(t, nil)
		ping := clientFrameBytes(opPing, true, nil)
		for i := 0; i < maxPingsPerRound; i++ {
			p.read(ping)
		}
		p.ec.tick(time.Now().UnixMilli(), time.Minute)
		p.read(ping)
		if cf := p.conn.closeFrames(); len(cf) != 0 {
			t.Fatalf("新一轮的 PING 不该被关：%+v", cf)
		}
	})

	t.Run("PING 不刷新活性，PONG 与数据刷新", func(t *testing.T) {
		p := newPipe(t, nil)
		p.ec.lastSeen.Store(1)
		p.read(clientFrameBytes(opPing, true, nil))
		if p.ec.lastSeen.Load() != 1 {
			t.Fatal("PING 刷新了活性")
		}
		p.read(clientFrameBytes(opPong, true, nil))
		if p.ec.lastSeen.Load() == 1 {
			t.Fatal("PONG 应刷新活性")
		}
		p.ec.lastSeen.Store(1)
		p.read(clientFrameBytes(opBinary, true, []byte("x")))
		if p.ec.lastSeen.Load() == 1 {
			t.Fatal("数据应刷新活性")
		}
	})
}
