package control

import (
	"sync"
	"testing"
	"time"
)

func TestDispatcherKeepsOrderPerKey(t *testing.T) {
	d := NewDispatcher(4, 64)
	defer d.Close()
	var mu sync.Mutex
	got := map[string][]int{}
	for i := 0; i < 50; i++ {
		for _, k := range []string{"a", "b", "c"} {
			d.Go(k, func() {
				mu.Lock()
				got[k] = append(got[k], i)
				mu.Unlock()
			})
		}
	}
	if !d.Flush(5 * time.Second) {
		t.Fatal("没排空")
	}
	for k, seq := range got {
		if len(seq) != 50 {
			t.Fatalf("%s 只执行了 %d 个", k, len(seq))
		}
		for i, v := range seq {
			if v != i {
				t.Fatalf("%s 顺序乱了：%v", k, seq)
			}
		}
	}
}

func TestDispatcherNeverBlocksWhenFull(t *testing.T) {
	d := NewDispatcher(1, 2)
	gate := make(chan struct{})
	var done sync.WaitGroup
	const n = 20
	done.Add(n)
	finished := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			d.Go("k", func() { <-gate; done.Done() })
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("队列满时投递阻塞了")
	}
	if d.Overflowed() == 0 {
		t.Fatal("应当有溢出的任务")
	}
	close(gate)
	done.Wait()
	d.Close()
	// 关闭之后仍然执行，不丢。
	ran := make(chan struct{})
	d.Go("k", func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭后的任务没执行")
	}
}

func TestDispatcherSurvivesPanic(t *testing.T) {
	d := NewDispatcher(1, 8)
	defer d.Close()
	d.Go("k", func() { panic("boom") })
	ok := make(chan struct{})
	d.Go("k", func() { close(ok) })
	select {
	case <-ok:
	case <-time.After(3 * time.Second):
		t.Fatal("panic 之后队列停了")
	}
}

// 放不下栅栏的队列没等，Flush 不能谎称排空。
func TestDispatcherFlushReportsFalseWhenABarrierCouldNotBePlaced(t *testing.T) {
	d := NewDispatcher(1, 1)
	gate := make(chan struct{})
	started := make(chan struct{})
	d.Go("k", func() { close(started); <-gate })
	<-started
	d.Go("k", func() {}) // 队列（深度 1）满了
	if d.Flush(100 * time.Millisecond) {
		t.Fatal("没放上栅栏应答 false")
	}
	close(gate)
	deadline := time.Now().Add(3 * time.Second)
	for !d.Flush(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("排空之后应答 true")
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.Close()
}
