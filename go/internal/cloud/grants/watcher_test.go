package grants

import (
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

func newTestWatcher(sources ...string) *watcher {
	w := &watcher{account: "me", visible: map[string]struct{}{}}
	for _, id := range sources {
		w.visible[id] = struct{}{}
	}
	return w
}

func TestWatcherRefreshUpdatesVisibility(t *testing.T) {
	w := newTestWatcher("a", "b")
	if !w.Sees("a") || w.Sees("c") {
		t.Fatal("初始可见集合")
	}
	w.Refresh(AccessChanged{SourceID: "c", AccountID: "me", Visible: true})
	w.Refresh(AccessChanged{SourceID: "d", AccountID: "other", Visible: true}) // 别人的变化不影响本连接
	w.Refresh(AccessChanged{SourceID: "a", Visible: false})                    // AccountID 空 = 对所有连接生效
	w.Refresh(core.NewSourceRevoked("b"))
	w.Refresh(&AccessChanged{SourceID: "e", Visible: true})
	w.Refresh(nil)
	w.Refresh(core.NewSourceOnline("a", 1, "x")) // 不相关的事件
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": false, "e": true} {
		if got := w.Sees(id); got != want {
			t.Errorf("Sees(%s) = %v，应为 %v", id, got, want)
		}
	}
}

// Close 之后仍可能被调用：Sees 答 false，Refresh 和再次 Close 都安全。
func TestWatcherSafeAfterClose(t *testing.T) {
	w := newTestWatcher("a")
	w.Close()
	w.Close()
	w.Refresh(AccessChanged{SourceID: "a", Visible: true})
	w.Refresh(core.NewSourceRevoked("a"))
	if w.Sees("a") {
		t.Fatal("关闭后不该再可见")
	}
}

// 并发调用（-race）：Sees / Refresh / Close 来自不同的 goroutine。
func TestWatcherConcurrent(t *testing.T) {
	w := newTestWatcher("a")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				w.Sees("a")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				w.Refresh(AccessChanged{SourceID: "a", Visible: j%2 == 0})
			}
		}()
		go func() {
			defer wg.Done()
			w.Close()
		}()
	}
	wg.Wait()
	if w.Sees("a") {
		t.Fatal("全部结束后已关闭")
	}
}
