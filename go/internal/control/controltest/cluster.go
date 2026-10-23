// Package controltest 是多节点控制面（redis / postgres）共用的合同测试：两个节点共用一个存储，
// 对应 已删除的 TypeScript 版 packages/relay-runtime-node/test/external/cluster.ts。只在 _test.go 里用。
package controltest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

// Node 是合同要用的节点面：控制面本身，加测试用的「等后台写做完」。
type Node struct {
	control.Cluster
	Settle func()
}

// Pair 是共用一个存储的两个节点。
type Pair struct{ A, B Node }

// Limited 是合同用的限流规则：login.ip 每分钟 5 次，并发每个账号 1 个。
var Limited = map[control.LimitKind]control.LimitRule{
	control.LimitLoginIP:            {Max: 5, Window: time.Minute},
	control.LimitEdgeConcurrentAcct: {Concurrent: true, Max: 1},
}

// Source 是合同用的源 id。
const Source = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"

func route(node, tunnelID string) control.Route {
	return control.Route{Node: node, TunnelID: tunnelID, SinceMs: 1, CoreVersion: "0.3.0"}
}

// Eventually 等条件成立（最长 5 秒）。
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// Contract 跑两个节点的合同：路由互见、在线事件与节点命令广播、撤销同步、限流共用一个桶。
// pair 每次返回一对新的、互相隔离的节点。
func Contract(t *testing.T, pair func(t *testing.T) Pair) {
	ctx := context.Background()

	t.Run("路由互见；Touch 续期；Delete 只删同一条隧道的", func(t *testing.T) {
		p := pair(t)
		must(t, p.A.Routes().Set(ctx, Source, route("node-a", "t1"), 30*time.Second))
		got, err := p.B.Routes().Get(ctx, Source)
		if err != nil || got == nil || *got != route("node-a", "t1") {
			t.Fatalf("B 应看到 A 写的路由：%+v %v", got, err)
		}
		must(t, p.B.Routes().Delete(ctx, Source, "t-other"))
		p.B.Settle()
		if got, _ := p.A.Routes().Get(ctx, Source); got == nil || got.TunnelID != "t1" {
			t.Fatalf("别的隧道的删除不该生效：%+v", got)
		}
		must(t, p.B.Routes().Set(ctx, Source, route("node-b", "t2"), 30*time.Second))
		must(t, p.A.Routes().Delete(ctx, Source, "t1"))
		p.A.Settle()
		if got, _ := p.A.Routes().Get(ctx, Source); got == nil || got.TunnelID != "t2" || got.Node != "node-b" {
			t.Fatalf("旧隧道不能删掉替换它的新路由：%+v", got)
		}
		must(t, p.B.Routes().Touch(ctx, Source, 30*time.Second))
		must(t, p.B.Routes().Delete(ctx, Source, "t2"))
		p.B.Settle()
		if got, _ := p.A.Routes().Get(ctx, Source); got != nil {
			t.Fatalf("同一条隧道的删除应生效：%+v", got)
		}
		must(t, p.A.Routes().Set(ctx, Source, route("node-a", "t3"), 50*time.Millisecond))
		time.Sleep(120 * time.Millisecond)
		if got, _ := p.B.Routes().Get(ctx, Source); got != nil {
			t.Fatalf("过期的路由应读不到：%+v", got)
		}
		// Touch 不存在的路由什么也不做。
		must(t, p.A.Routes().Touch(ctx, "ffffffffffffffffffffffffffffffff", time.Minute))
		if got, _ := p.A.Routes().Get(ctx, "ffffffffffffffffffffffffffffffff"); got != nil {
			t.Fatal("Touch 不该凭空造路由")
		}
	})

	t.Run("在线事件与节点命令：本节点收一次，另一节点经广播收到", func(t *testing.T) {
		p := pair(t)
		var mu sync.Mutex
		var seenA, seenB []control.PresenceEvent
		p.A.Presence().Subscribe(func(e control.PresenceEvent) { mu.Lock(); seenA = append(seenA, e); mu.Unlock() })
		p.B.Presence().Subscribe(func(e control.PresenceEvent) { mu.Lock(); seenB = append(seenB, e); mu.Unlock() })
		event := control.PresenceEvent{
			Type: control.PresenceSourceOnline, SourceID: Source, Node: "node-a", TunnelID: "t1", CoreVersion: "0.3.0", AtMs: 5,
		}
		must(t, p.A.Presence().Publish(ctx, event))
		Eventually(t, "B 收到在线事件", func() bool { mu.Lock(); defer mu.Unlock(); return len(seenB) == 1 })
		mu.Lock()
		if seenB[0] != event || len(seenA) != 1 || seenA[0] != event {
			t.Fatalf("事件：A=%+v B=%+v", seenA, seenB)
		}
		mu.Unlock()
		offline := control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: Source, Node: "node-a", TunnelID: "t1", AtMs: 9}
		must(t, p.A.Presence().Publish(ctx, offline))
		Eventually(t, "B 收到下线事件", func() bool { mu.Lock(); defer mu.Unlock(); return len(seenB) == 2 })
		mu.Lock()
		if seenB[1] != offline {
			t.Fatalf("下线事件：%+v", seenB[1])
		}
		mu.Unlock()

		var commandsA, commandsB []control.NodeCommand
		p.A.NodeCommands().Subscribe(func(c control.NodeCommand) { mu.Lock(); commandsA = append(commandsA, c); mu.Unlock() })
		p.B.NodeCommands().Subscribe(func(c control.NodeCommand) { mu.Lock(); commandsB = append(commandsB, c); mu.Unlock() })
		p.B.Command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: Source})
		p.B.Command(control.NodeCommand{Type: control.CommandKick, SourceID: Source, Reason: "replaced"})
		Eventually(t, "A 收到两条命令", func() bool { mu.Lock(); defer mu.Unlock(); return len(commandsA) == 2 })
		time.Sleep(50 * time.Millisecond) // 本节点不该因为自己的广播回流再收一次
		mu.Lock()
		defer mu.Unlock()
		if len(commandsA) != 2 || len(commandsB) != 2 ||
			commandsA[0] != (control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: Source}) ||
			commandsA[1] != (control.NodeCommand{Type: control.CommandKick, SourceID: Source, Reason: "replaced"}) {
			t.Fatalf("命令：A=%+v B=%+v", commandsA, commandsB)
		}
	})

	t.Run("撤销：一个节点记下，另一个节点经广播立即拒绝；过期的不记", func(t *testing.T) {
		p := pair(t)
		if revoked, _ := p.B.Tokens().IsRevoked(ctx, "jti-1"); revoked {
			t.Fatal("没撤销的不该拒绝")
		}
		must(t, p.A.Tokens().Revoke(ctx, "jti-1", time.Now().Add(time.Minute)))
		if revoked, _ := p.A.Tokens().IsRevoked(ctx, "jti-1"); !revoked {
			t.Fatal("本节点应立即拒绝")
		}
		Eventually(t, "B 拒绝 jti-1", func() bool { r, _ := p.B.Tokens().IsRevoked(ctx, "jti-1"); return r })
		must(t, p.A.Tokens().Revoke(ctx, "jti-old", time.Now().Add(-time.Second)))
		if revoked, _ := p.B.Tokens().IsRevoked(ctx, "jti-old"); revoked {
			t.Fatal("过期的撤销不记")
		}
	})

	t.Run("速率限流两个节点共用一个桶；并发限流按节点", func(t *testing.T) {
		p := pair(t)
		for _, n := range []Node{p.A, p.B, p.A, p.B, p.A} {
			r, err := n.Limits().Hit(ctx, control.LimitLoginIP, "198.51.100.7")
			if err != nil || !r.Allowed {
				t.Fatalf("前 5 次应放行：%+v %v", r, err)
			}
		}
		denied, err := p.B.Limits().Hit(ctx, control.LimitLoginIP, "198.51.100.7")
		if err != nil || denied.Allowed || denied.RetryAfter <= 0 || denied.RetryAfter > time.Minute {
			t.Fatalf("第 6 次应被拒并带 RetryAfter：%+v %v", denied, err)
		}
		if r, _ := p.A.Limits().Hit(ctx, control.LimitLoginIP, "198.51.100.8"); !r.Allowed {
			t.Fatal("别的键有自己的桶")
		}

		hit := func(n Node) bool { r, _ := n.Limits().Hit(ctx, control.LimitEdgeConcurrentAcct, "u"); return r.Allowed }
		if !hit(p.A) || hit(p.A) {
			t.Fatal("A 的并发名额应只有 1 个")
		}
		if !hit(p.B) {
			t.Fatal("并发名额按节点，B 有自己的")
		}
		must(t, p.A.Limits().Release(ctx, control.LimitEdgeConcurrentAcct, "u"))
		if !hit(p.A) {
			t.Fatal("归还之后应能再占")
		}
	})
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
