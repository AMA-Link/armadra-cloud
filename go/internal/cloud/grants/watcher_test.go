package grants

import (
	"context"
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
	w.Refresh(AccessChanged{SourceID: "a", Visible: false})                    // AccountID 空只允许移除，对所有连接生效
	w.Refresh(AccessChanged{SourceID: "f", Visible: true})                     // 放宽不点名账号：忽略
	w.Refresh(core.NewSourceRevoked("b"))
	w.Refresh(&AccessChanged{SourceID: "e", AccountID: "me", Visible: true})
	w.Refresh(nil)
	w.Refresh(core.NewSourceOnline("a", 1, "x")) // 不相关的事件
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": false, "e": true, "f": false} {
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

// AccessChanged 是内部事件：MeStream 不把它放进客户端队列（core.InternalEvent）。
func TestAccessChangedIsInternalOnly(t *testing.T) {
	var event core.Event = AccessChanged{SourceID: "a", AccountID: "me", Visible: true}
	if _, ok := event.(core.InternalEvent); !ok {
		t.Fatal("AccessChanged 应实现 core.InternalEvent")
	}
}

// 心跳 Resync 取快照期间到了更新的 AccessChanged(visible=false)：旧快照被丢弃重取，不盖掉撤销。
func TestWatcherResyncDropsStaleSnapshot(t *testing.T) {
	w := newTestWatcher("a")
	calls := 0
	w.snapshot = func(context.Context) (map[string]struct{}, error) {
		calls++
		if calls == 1 {
			// 第一次取快照时读到了撤销之前的库状态；返回之前撤销事件抵达。
			w.Refresh(AccessChanged{SourceID: "a", AccountID: "me", Visible: false})
			return map[string]struct{}{"a": {}}, nil
		}
		return map[string]struct{}{}, nil // 重取时库里已是撤销之后
	}
	if err := w.Resync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Sees("a") || calls != 2 {
		t.Fatalf("旧快照不该覆盖撤销：Sees=%v 取快照 %d 次", w.Sees("a"), calls)
	}
	// 一直撞上并发更新：不覆盖，保留事件已有的结果。
	w2 := newTestWatcher("a")
	w2.snapshot = func(context.Context) (map[string]struct{}, error) {
		w2.Refresh(AccessChanged{SourceID: "a", AccountID: "me", Visible: false})
		return map[string]struct{}{"a": {}}, nil
	}
	if err := w2.Resync(context.Background()); err != nil || w2.Sees("a") {
		t.Fatalf("反复撞上更新时不该应用旧快照：%v %v", err, w2.Sees("a"))
	}
}
