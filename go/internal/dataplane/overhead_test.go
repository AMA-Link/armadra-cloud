package dataplane_test

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

// TestTrafficMeterOverhead 量一下开流量统计的代价：同一条回显通道，统计关 / 开交替各跑若干轮，
// 比较吞吐。只在设了 ARMADRA_BENCH=1 时跑（耗时、且结果随机器变）：
//
//	ARMADRA_BENCH=1 GOMAXPROCS=1 go test ./internal/dataplane -run TrafficMeterOverhead -v
//
// 单核（GOMAXPROCS=1）最贴近生产的「钉一个核」；消息大小分大（16 KiB，吞吐）与小（256 B，每帧开销占比最大）两档。
func TestTrafficMeterOverhead(t *testing.T) {
	if os.Getenv("ARMADRA_BENCH") == "" {
		t.Skip("设 ARMADRA_BENCH=1 才跑")
	}
	for _, size := range []int{16 << 10, 256} {
		total := 96 << 20
		if size < 1024 {
			total = 16 << 20
		}
		const rounds = 7
		rate := map[bool][]float64{}
		for round := 0; round < rounds; round++ {
			for _, on := range []bool{false, true} {
				rate[on] = append(rate[on], echoRate(t, on, size, total))
			}
		}
		off, on := median(rate[false]), median(rate[true])
		t.Logf("消息 %d 字节：统计关 %.1f MB/s，统计开 %.1f MB/s，开销 %.2f%%（%d 轮中位数）", size, off, on, (off-on)/off*100, rounds)
	}
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	return s[len(s)/2]
}

func echoRate(t *testing.T, traffic bool, size, total int) float64 {
	h := newHarness(t, harnessOptions{traffic: traffic})
	h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	c := h.channel(h.relayToken(nil))
	if _, _, open := c.outcome(); !open {
		t.Fatal("通道没开成")
	}
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	count := total / size
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < count; i++ {
			if err := c.c.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	got := 0
	deadline := time.After(60 * time.Second)
	for got < count*size {
		select {
		case b := <-c.msgs:
			got += len(b)
		case info := <-c.closed:
			t.Fatalf("回显途中通道关闭：%+v", info)
		case <-deadline:
			t.Fatal("超时")
		}
	}
	<-done
	elapsed := time.Since(start)
	return float64(count*size) / elapsed.Seconds() / (1 << 20)
}
