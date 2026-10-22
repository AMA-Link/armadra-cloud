package edge

import (
	"sync"
	"time"
)

const wheelBuckets = 30

// wheel 是全局的 ping 时间轮：连接按序号分到 30 个桶，每个节拍处理一个桶，
// 每条连接每个 pingMs 一次。一次全量遍历会在单核上造成一阵几百毫秒的停顿。
type wheel struct {
	mu      sync.Mutex
	buckets [wheelBuckets]map[*edgeConn]struct{}
	next    int
	stop    chan struct{}
}

func newWheel() *wheel {
	w := &wheel{stop: make(chan struct{})}
	for i := range w.buckets {
		w.buckets[i] = map[*edgeConn]struct{}{}
	}
	return w
}

func (w *wheel) add(ec *edgeConn) {
	w.mu.Lock()
	ec.bucket = w.next % wheelBuckets
	w.next++
	w.buckets[ec.bucket][ec] = struct{}{}
	w.mu.Unlock()
}

func (w *wheel) remove(ec *edgeConn) {
	w.mu.Lock()
	delete(w.buckets[ec.bucket], ec)
	w.mu.Unlock()
}

func (w *wheel) snapshot(i int) []*edgeConn {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.buckets[i%wheelBuckets]
	out := make([]*edgeConn, 0, len(b))
	for ec := range b {
		out = append(out, ec)
	}
	return out
}

// run 每 ping/30 走一格；停止时返回。
func (w *wheel) run(ping, idle time.Duration) {
	step := max(ping/wheelBuckets, time.Millisecond)
	ticker := time.NewTicker(step)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-ticker.C:
			i++
			now := time.Now().UnixMilli()
			for _, ec := range w.snapshot(i) {
				ec.tick(now, idle)
			}
		case <-w.stop:
			return
		}
	}
}
