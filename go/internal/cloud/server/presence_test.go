package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// fakeSources 是只实现 Update 的源存储：记录按源 id 存，Update 对不存在的源给 nil（与真实存储一致）。
type fakeSources struct {
	store.SourceStore
	mu      sync.Mutex
	records map[string]*store.SourceRecord
	failFor map[string]error
}

func newFakeSources(ids ...string) *fakeSources {
	f := &fakeSources{records: map[string]*store.SourceRecord{}, failFor: map[string]error{}}
	for _, id := range ids {
		f.records[id] = &store.SourceRecord{SourceID: id, CoreVersion: "0.1.0"}
	}
	return f
}

func (f *fakeSources) Update(_ context.Context, _, id string, fn func(*store.SourceRecord) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failFor[id]; err != nil {
		return err
	}
	rec := f.records[id]
	if rec == nil {
		return fn(nil)
	}
	draft := *rec
	if err := fn(&draft); err != nil {
		return err
	}
	f.records[id] = &draft
	return nil
}

func (f *fakeSources) lastSeen(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec := f.records[id]; rec != nil && rec.LastSeenAtMs != nil {
		return *rec.LastSeenAtMs
	}
	return -1
}

func (f *fakeSources) version(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[id].CoreVersion
}

// fakeRoutes 是路由表：只有 Online 有内容，其余方法不会被用到。
type fakeRoutes struct {
	control.RouteTable
	mu     sync.Mutex
	online map[string]control.Route
	err    error
}

func (f *fakeRoutes) Online(context.Context) (map[string]control.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]control.Route{}
	for id, r := range f.online {
		out[id] = r
	}
	return out, nil
}

func (f *fakeRoutes) set(id string, r *control.Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r == nil {
		delete(f.online, id)
		return
	}
	f.online[id] = *r
}

type clock struct {
	mu sync.Mutex
	ms int64
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.ms += d.Milliseconds()
	c.mu.Unlock()
}

// 一直连着的主机在两次落盘之间 last_seen 前进；下线后停在下线时刻（#190）。
func TestPresenceFlushStampsConnectedSources(t *testing.T) {
	ctx := context.Background()
	const host, other = "h1", "h2"
	sources := newFakeSources(host, other)
	routes := &fakeRoutes{online: map[string]control.Route{}}
	clk := &clock{ms: 1_000_000}
	rec := newPresenceRecorder(sources, routes, clk.now, obs.Nop())

	// 上线事件落盘一次：此后主机一直连着，不再有任何事件。
	rec.Observe(control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: host, CoreVersion: "0.9.0", AtMs: 1_000_000})
	routes.set(host, &control.Route{Node: "n1", TunnelID: "t1", SinceMs: 1_000_000, CoreVersion: "0.9.0"})
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != 1_000_000 {
		t.Fatalf("第一次落盘 last_seen = %d", got)
	}

	for i := 1; i <= 3; i++ {
		clk.advance(5 * time.Minute)
		rec.Flush(ctx)
		want := int64(1_000_000) + int64(i)*5*60*1000
		if got := sources.lastSeen(host); got != want {
			t.Fatalf("第 %d 次落盘：在线主机的 last_seen = %d，应前进到 %d", i+1, got, want)
		}
	}
	if got := sources.lastSeen(other); got != -1 {
		t.Fatalf("没连过的主机不该有 last_seen：%d", got)
	}

	// 一直连着的主机换了核心版本（路由里是现役版本）：核心版本也跟着前进。
	routes.set(host, &control.Route{Node: "n1", TunnelID: "t1", SinceMs: 1_000_000, CoreVersion: "0.10.0"})
	clk.advance(5 * time.Minute)
	rec.Flush(ctx)
	if v := sources.version(host); v != "0.10.0" {
		t.Fatalf("core_version = %q", v)
	}

	// 下线：路由没了，下线事件的时刻就是最后一次在线；之后的落盘不再前进。
	offlineAt := clk.now().UnixMilli() - 90_000
	routes.set(host, nil)
	rec.Observe(control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: host, AtMs: offlineAt})
	clk.advance(5 * time.Minute)
	rec.Flush(ctx)
	// 下线事件早于上一轮的落盘戳时，只往后推：保持上一轮的值。
	prev := sources.lastSeen(host)
	if prev < offlineAt {
		t.Fatalf("下线后 last_seen 倒退了：%d < %d", prev, offlineAt)
	}
	clk.advance(5 * time.Minute)
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != prev {
		t.Fatalf("下线之后 last_seen 还在前进：%d → %d", prev, got)
	}
}

// 下线晚于落盘戳的时候，last_seen 停在下线时刻，不是「此刻」。
func TestPresenceOfflineStopsAtOfflineTime(t *testing.T) {
	ctx := context.Background()
	const host = "h1"
	sources := newFakeSources(host)
	routes := &fakeRoutes{online: map[string]control.Route{host: {Node: "n1", TunnelID: "t1", CoreVersion: "0.9.0"}}}
	clk := &clock{ms: 2_000_000}
	rec := newPresenceRecorder(sources, routes, clk.now, obs.Nop())
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != 2_000_000 {
		t.Fatalf("last_seen = %d", got)
	}
	clk.advance(2 * time.Minute)
	offlineAt := clk.now().UnixMilli()
	routes.set(host, nil)
	rec.Observe(control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: host, AtMs: offlineAt})
	clk.advance(3 * time.Minute)
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != offlineAt {
		t.Fatalf("下线后 last_seen = %d，应停在下线时刻 %d", got, offlineAt)
	}
	clk.advance(5 * time.Minute)
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != offlineAt {
		t.Fatalf("再落盘一次 last_seen = %d，应仍是 %d", got, offlineAt)
	}
}

// 只往后推：库里已有更晚的 last_seen（别的实例写的）不会被覆盖。
func TestPresenceFlushMovesForwardOnly(t *testing.T) {
	ctx := context.Background()
	const host = "h1"
	sources := newFakeSources(host)
	later := int64(9_000_000)
	sources.records[host].LastSeenAtMs = &later
	routes := &fakeRoutes{online: map[string]control.Route{host: {Node: "n1", TunnelID: "t1"}}}
	clk := &clock{ms: 1_000_000}
	rec := newPresenceRecorder(sources, routes, clk.now, obs.Nop())
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != later {
		t.Fatalf("last_seen 倒退了：%d", got)
	}
	if v := sources.version(host); v != "0.1.0" {
		t.Fatalf("路由里没有版本时不该清掉库里的核心版本：%q", v)
	}
}

// 读不到路由表：只落盘事件累计的部分，不丢也不 panic；写库失败的条目留到下一轮。
func TestPresenceFlushDegrades(t *testing.T) {
	ctx := context.Background()
	const host, bad = "h1", "h2"
	sources := newFakeSources(host, bad)
	routes := &fakeRoutes{online: map[string]control.Route{}, err: errors.New("redis 连不上")}
	clk := &clock{ms: 1_000_000}
	rec := newPresenceRecorder(sources, routes, clk.now, obs.Nop())
	rec.Observe(control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: host, CoreVersion: "0.9.0", AtMs: 5_000})
	rec.Flush(ctx)
	if got := sources.lastSeen(host); got != 5_000 {
		t.Fatalf("路由表读不到时仍应落盘事件：%d", got)
	}

	routes.err = nil
	routes.set(bad, &control.Route{Node: "n", TunnelID: "t"})
	sources.failFor[bad] = errors.New("写库失败")
	rec.Flush(ctx)
	if got := sources.lastSeen(bad); got != -1 {
		t.Fatalf("写库失败不该有记录：%d", got)
	}
	delete(sources.failFor, bad)
	routes.set(bad, nil) // 写失败期间已下线：上一轮攒下的戳仍要补写
	clk.advance(5 * time.Minute)
	rec.Flush(ctx)
	if got := sources.lastSeen(bad); got != 1_000_000 {
		t.Fatalf("失败的条目应留到下一轮：%d", got)
	}
}

// 实现不带枚举能力（或没有路由表）时退回只记事件。
func TestPresenceWithoutLister(t *testing.T) {
	ctx := context.Background()
	sources := newFakeSources("h1")
	clk := &clock{ms: 1_000_000}
	rec := newPresenceRecorder(sources, struct{ control.RouteTable }{}, clk.now, obs.Nop())
	rec.Observe(control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: "h1", AtMs: 7_000})
	rec.Flush(ctx)
	if got := sources.lastSeen("h1"); got != 7_000 {
		t.Fatalf("last_seen = %d", got)
	}
}
