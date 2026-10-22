package control_test

import (
	"context"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

var _ control.Plane = (*control.Memory)(nil)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func plane(limits map[control.LimitKind]control.LimitRule) (*control.Memory, *clock) {
	clk := &clock{t: time.UnixMilli(1_000_000)}
	return control.NewMemory(control.MemoryOptions{
		Issuer: "https://relay.test",
		Now:    clk.Now,
		Limits: limits,
	}), clk
}

func route(tunnelID string) control.Route {
	return control.Route{Node: "n1", TunnelID: tunnelID, CoreVersion: "0.2.0"}
}

var ctx = context.Background()

func mustGet(t *testing.T, m *control.Memory, id string) *control.Route {
	t.Helper()
	r, err := m.Routes().Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hit(t *testing.T, m *control.Memory, kind control.LimitKind, key string) control.LimitResult {
	t.Helper()
	res, err := m.Limits().Hit(ctx, kind, key)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMemory(t *testing.T) {
	t.Run("路由按 TTL 过期，touch 续期", func(t *testing.T) {
		m, clk := plane(nil)
		_ = m.Routes().Set(ctx, "s", route("t1"), time.Second)
		if r := mustGet(t, m, "s"); r == nil || r.TunnelID != "t1" {
			t.Fatalf("应有 t1：%+v", r)
		}
		clk.Advance(900 * time.Millisecond)
		_ = m.Routes().Touch(ctx, "s", time.Second)
		clk.Advance(900 * time.Millisecond)
		if mustGet(t, m, "s") == nil {
			t.Fatal("续期后应还在")
		}
		clk.Advance(101 * time.Millisecond)
		if mustGet(t, m, "s") != nil {
			t.Fatal("应已过期")
		}
		if n := m.RouteCount(); n != 0 {
			t.Fatalf("RouteCount = %d", n)
		}
	})

	t.Run("路由删除是 CAS：只删 tunnelId 相同的那条", func(t *testing.T) {
		m, _ := plane(nil)
		_ = m.Routes().Set(ctx, "s", route("new"), time.Second)
		_ = m.Routes().Delete(ctx, "s", "old")
		if r := mustGet(t, m, "s"); r == nil || r.TunnelID != "new" {
			t.Fatalf("旧隧道不应删掉新路由：%+v", r)
		}
		_ = m.Routes().Delete(ctx, "s", "new")
		if mustGet(t, m, "s") != nil {
			t.Fatal("同 tunnelId 应被删掉")
		}
	})

	t.Run("touch 不会复活已过期的路由", func(t *testing.T) {
		m, clk := plane(nil)
		_ = m.Routes().Set(ctx, "s", route("t"), 10*time.Millisecond)
		clk.Advance(11 * time.Millisecond)
		_ = m.Routes().Touch(ctx, "s", time.Second)
		if mustGet(t, m, "s") != nil {
			t.Fatal("过期路由不应被复活")
		}
	})

	t.Run("取出的路由是副本", func(t *testing.T) {
		m, _ := plane(nil)
		_ = m.Routes().Set(ctx, "s", route("t"), time.Second)
		mustGet(t, m, "s").TunnelID = "changed"
		if r := mustGet(t, m, "s"); r.TunnelID != "t" {
			t.Fatalf("改副本不应影响存储：%+v", r)
		}
	})

	t.Run("速率限流是固定窗口，答 retryAfter", func(t *testing.T) {
		m, clk := plane(map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConnectIP: {Max: 2, Window: time.Second},
		})
		k := control.LimitEdgeConnectIP
		if !hit(t, m, k, "ip").Allowed {
			t.Fatal("第 1 次应允许")
		}
		clk.Advance(100 * time.Millisecond)
		if !hit(t, m, k, "ip").Allowed {
			t.Fatal("第 2 次应允许")
		}
		clk.Advance(100 * time.Millisecond)
		if got := hit(t, m, k, "ip"); got != (control.LimitResult{Allowed: false, RetryAfter: 800 * time.Millisecond}) {
			t.Fatalf("第 3 次应被拒并答 800ms：%+v", got)
		}
		if !hit(t, m, k, "other").Allowed {
			t.Fatal("别的键不受影响")
		}
		clk.Advance(800 * time.Millisecond)
		if !hit(t, m, k, "ip").Allowed {
			t.Fatal("窗口过后应允许")
		}
	})

	t.Run("并发限流占名额，release 归还", func(t *testing.T) {
		m, _ := plane(map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConcurrentAcct: {Concurrent: true, Max: 2},
		})
		k := control.LimitEdgeConcurrentAcct
		want := []bool{true, true, false}
		for i, w := range want {
			if got := hit(t, m, k, "a").Allowed; got != w {
				t.Fatalf("第 %d 次：%v", i+1, got)
			}
		}
		_ = m.Limits().Release(ctx, k, "a")
		if !hit(t, m, k, "a").Allowed {
			t.Fatal("归还后应允许")
		}
		// 多放几次不会出现负数名额。
		for range 5 {
			_ = m.Limits().Release(ctx, k, "a")
		}
		for i := range 2 {
			if !hit(t, m, k, "a").Allowed {
				t.Fatalf("清空后第 %d 次应允许", i+1)
			}
		}
		if hit(t, m, k, "a").Allowed {
			t.Fatal("满额后应被拒")
		}
	})

	t.Run("缺省限流：隧道每源每分钟 10 次", func(t *testing.T) {
		m, _ := plane(nil)
		for i := range 10 {
			if !hit(t, m, control.LimitTunnelConnectSrc, "s").Allowed {
				t.Fatalf("第 %d 次应允许", i+1)
			}
		}
		if hit(t, m, control.LimitTunnelConnectSrc, "s").Allowed {
			t.Fatal("第 11 次应被拒")
		}
	})

	t.Run("撤销记到 exp，过期后自动忘掉", func(t *testing.T) {
		m, clk := plane(nil)
		now := clk.Now()
		_ = m.Tokens().Revoke(ctx, "j1", now.Add(time.Second))
		_ = m.Tokens().Revoke(ctx, "already-expired", now.Add(-time.Millisecond))
		check := func(jti string, want bool) {
			t.Helper()
			got, err := m.Tokens().IsRevoked(ctx, jti)
			if err != nil || got != want {
				t.Fatalf("%s：got %v err %v，want %v", jti, got, err, want)
			}
		}
		check("j1", true)
		check("already-expired", false)
		clk.Advance(time.Second)
		check("j1", false)
	})

	t.Run("在线事件发给所有订阅者，退订后不再收", func(t *testing.T) {
		m, _ := plane(nil)
		var seen []control.PresenceEvent
		off := m.Presence().Subscribe(func(e control.PresenceEvent) { seen = append(seen, e) })
		m.Presence().Subscribe(func(control.PresenceEvent) { panic("坏订阅者") })
		ev := control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: "s", Node: "n1", TunnelID: "t", AtMs: 1}
		_ = m.Presence().Publish(ctx, ev)
		off()
		_ = m.Presence().Publish(ctx, ev)
		if len(seen) != 1 || seen[0] != ev {
			t.Fatalf("seen = %+v", seen)
		}
	})

	t.Run("memory.go 不 import Redis / PostgreSQL（personal 模式无外部依赖）", func(t *testing.T) {
		f, err := parser.ParseFile(token.NewFileSet(), "memory.go", nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, ".") {
				t.Errorf("memory.go 不应依赖第三方包：%s", path)
			}
		}
	})

	t.Run("撤销判断：撤销表或 revoked 钩子任一为真即撤销", func(t *testing.T) {
		now := time.UnixMilli(1_000)
		m := control.NewMemory(control.MemoryOptions{
			Issuer: "https://relay.test",
			Now:    func() time.Time { return now },
			Revoked: func(_ context.Context, jti string) (bool, error) {
				return strings.HasPrefix(jti, "dead."), nil
			},
		})
		check := func(jti string, want bool) {
			t.Helper()
			if got, _ := m.Tokens().IsRevoked(ctx, jti); got != want {
				t.Fatalf("%s：got %v want %v", jti, got, want)
			}
		}
		check("dead.1", true)
		check("live.1", false)
		_ = m.Tokens().Revoke(ctx, "live.1", time.UnixMilli(2_000))
		check("live.1", true)
	})

	t.Run("节点命令：进程内发给订阅者（personal 撤销源时关隧道）", func(t *testing.T) {
		m, _ := plane(nil)
		var seen []control.NodeCommand
		off := m.NodeCommands().Subscribe(func(c control.NodeCommand) { seen = append(seen, c) })
		cmd := control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: "s"}
		m.Command(cmd)
		off()
		m.Command(cmd)
		if len(seen) != 1 || seen[0] != cmd {
			t.Fatalf("seen = %+v", seen)
		}
	})
}

func TestMemoryDefaults(t *testing.T) {
	m := control.NewMemory(control.MemoryOptions{Issuer: "i"})
	if m.Mode() != "personal" || m.Issuer() != "i" {
		t.Fatalf("mode=%s issuer=%s", m.Mode(), m.Issuer())
	}
	doc, err := m.JWKS(ctx)
	if err != nil || string(doc) != `{"keys":[]}` {
		t.Fatalf("缺省 JWKS：%s %v", doc, err)
	}
	if _, err := m.Limits().Hit(ctx, "no.such", "k"); err == nil {
		t.Fatal("未知限流种类应报错")
	}
	if len(control.DefaultLimitRules) != 9 {
		t.Fatalf("缺省规则数 = %d", len(control.DefaultLimitRules))
	}
}

func TestMemorySweep(t *testing.T) {
	m, clk := plane(nil)
	for i := range 1100 {
		_ = m.Routes().Set(ctx, "s"+strconv.Itoa(i), route("t"), time.Second)
	}
	clk.Advance(2 * time.Second)
	_ = m.Routes().Set(ctx, "fresh", route("t"), time.Second)
	if n := m.RouteCount(); n != 1 {
		t.Fatalf("RouteCount = %d", n)
	}
}

func TestMemoryConcurrent(t *testing.T) {
	m, _ := plane(map[control.LimitKind]control.LimitRule{
		control.LimitEdgeConcurrentAcct: {Concurrent: true, Max: 50},
		control.LimitEdgeConnectIP:      {Max: 100, Window: time.Hour},
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	conc, rate := 0, 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _ := m.Limits().Hit(ctx, control.LimitEdgeConcurrentAcct, "a")
			b, _ := m.Limits().Hit(ctx, control.LimitEdgeConnectIP, "ip")
			_ = m.Routes().Set(ctx, "s", route("t"), time.Second)
			_, _ = m.Routes().Get(ctx, "s")
			m.Presence().Subscribe(func(control.PresenceEvent) {})()
			mu.Lock()
			defer mu.Unlock()
			if a.Allowed {
				conc++
			}
			if b.Allowed {
				rate++
			}
		}()
	}
	wg.Wait()
	if conc != 50 || rate != 100 {
		t.Fatalf("并发名额 %d、速率 %d", conc, rate)
	}
}
