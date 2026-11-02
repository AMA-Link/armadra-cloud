package netpoll_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
)

// 对端不读 socket、控制帧（ack 为 0）却不停地排：队列不能无限长，超过 MaxCtlBytes 就以
// ErrQueueOverflow 断开（#191）。
func TestCtlQueueOverflow(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4096) // 对端收得慢，内核缓冲很快写满
		}
		frame := bytes.Repeat([]byte{9}, 1024)
		// 内核缓冲之外最多再排 MaxCtlBytes；循环总量远大于内核缓冲，没有上限就永远不会关。
		const budget = 512 << 20
		for sent := 0; sent < budget; sent += len(frame) {
			if !c.Write(frame, 0) {
				break // 已被断开
			}
		}
		select {
		case err := <-h.closed:
			if !errors.Is(err, netpoll.ErrQueueOverflow) {
				t.Fatalf("OnClose 的原因应是 ErrQueueOverflow：%v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("控制帧排了 512 MiB 连接还没断：写队列无上限")
		}
		if c.Write(frame, 0) {
			t.Fatal("断开后不该再接受写")
		}
	})
}

// 带信用的帧不计入控制帧上限（它们受调用方流控约束）；控制帧写出去之后额度归还。
func TestCtlQueueBudgetReleased(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		data := bytes.Repeat([]byte{1}, 256<<10)
		ctl := bytes.Repeat([]byte{2}, 40<<10)
		total := 0
		for round := 0; round < 3; round++ {
			// 带信用的数据先把内核缓冲写满，其后的控制帧只能排队。
			for i := 0; i < 32; i++ {
				if !c.Write(data, len(data)) {
					t.Fatalf("第 %d 轮：连接被断开", round)
				}
				total += len(data)
			}
			if !c.Write(ctl, 0) {
				t.Fatalf("第 %d 轮：连接被断开", round)
			}
			// 对端读走一轮：排队的字节写完，控制帧额度要还回来，下一轮才不会累计超限。
			want := 32*len(data) + len(ctl)
			if n, err := io.CopyN(io.Discard, client, int64(want)); err != nil || n != int64(want) {
				t.Fatalf("第 %d 轮：只读到 %d/%d：%v", round, n, want, err)
			}
			eventually(t, "信用全部报告", func() bool { return h.acked() == total })
		}
		select {
		case err := <-h.closed:
			t.Fatalf("连接不该被断开：%v", err)
		default:
		}
	})
}
