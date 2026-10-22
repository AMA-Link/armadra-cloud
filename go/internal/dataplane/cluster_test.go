package dataplane_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/factory"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

// 两个节点共用一个存储（redis 或 postgres 控制面）：节点 A 是真实的数据面（隧道 + 边缘），
// 节点 B 只带控制面。验证：A 写的路由 B 看得到，撤销经存储传到 A 的边缘，B 发的命令到达
// 持有隧道的 A。对应设计 §3.6 的多节点控制面。
func TestTwoNodeControlPlane(t *testing.T) {
	type opener func(t *testing.T) func(env *relayenv.Env, node string) control.Cluster
	backends := map[string]opener{
		"redis": func(t *testing.T) func(*relayenv.Env, string) control.Cluster {
			url := os.Getenv("REDIS_URL")
			if url == "" {
				if os.Getenv("CI") != "" {
					t.Fatal("CI 里没有 REDIS_URL")
				}
				t.Skip("没有 Redis：设置 REDIS_URL")
			}
			var rnd [6]byte
			_, _ = rand.Read(rnd[:])
			prefix := "relay-test:" + hex.EncodeToString(rnd[:]) + ":"
			return func(env *relayenv.Env, node string) control.Cluster {
				c, err := factory.Open(context.Background(), factory.Config{
					Kind: factory.Redis, RedisURL: url, RedisPrefix: prefix, Options: options(env, node),
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c
			}
		},
		"postgres": func(t *testing.T) func(*relayenv.Env, string) control.Cluster {
			url, schema := pgtest.Schema(t)
			db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return func(env *relayenv.Env, node string) control.Cluster {
				c, err := factory.Open(context.Background(), factory.Config{
					Kind: factory.Postgres, PostgresURL: url, PostgresSchema: schema, PostgresChannelPrefix: schema + "_", Options: options(env, node),
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c
			}
		},
	}
	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			openNode := open(t)
			var a control.Cluster
			h := newHarness(t, harnessOptions{cluster: func(env *relayenv.Env) control.Plane {
				a = openNode(env, "relay-test")
				return a
			}})
			b := openNode(h.env, "relay-b")
			ctx := context.Background()

			var mu sync.Mutex
			var seen []control.PresenceEvent
			b.Presence().Subscribe(func(ev control.PresenceEvent) { mu.Lock(); seen = append(seen, ev); mu.Unlock() })

			core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})

			// 1. A 写的路由，B 看得到；上线事件经存储传到 B。
			eventually(t, "B 看到 A 的路由", func() bool {
				r, _ := b.Routes().Get(ctx, h.env.SourceID)
				return r != nil && r.Node == "relay-test"
			})
			eventually(t, "B 收到上线事件", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(seen) == 1 && seen[0].Type == control.PresenceSourceOnline && seen[0].Node == "relay-test"
			})

			// 2. 撤销经存储传到 A 的边缘：B 撤销的令牌，A 上新的通道被拒（4401）。
			jti := relayenv.NewJTI()
			token := h.relayToken(relayenv.Claims{"jti": jti})
			first := h.channel(token)
			if _, _, open := first.outcome(); !open {
				t.Fatal("撤销前通道应开成")
			}
			_ = first.c.Close()
			if err := b.Tokens().Revoke(ctx, jti, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			eventually(t, "A 的边缘拒绝已撤销的令牌", func() bool {
				code, _, open := h.channel(token).outcome()
				return !open && code == 4401
			})

			// 3. B 发的命令到达持有隧道的 A：closeTunnel → 隧道关闭、路由删除、下线事件。
			b.Command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: h.env.SourceID})
			select {
			case <-core.Closed():
			case <-time.After(10 * time.Second):
				t.Fatal("closeTunnel 之后隧道应关闭")
			}
			eventually(t, "路由被删、B 收到下线事件", func() bool {
				r, _ := b.Routes().Get(ctx, h.env.SourceID)
				mu.Lock()
				defer mu.Unlock()
				return r == nil && len(seen) == 2 && seen[1].Type == control.PresenceSourceOffline
			})

			// kick → 持有隧道的 A 发 GOAWAY replaced（源收到后自己重连）。
			again := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
			b.Command(control.NodeCommand{Type: control.CommandKick, SourceID: h.env.SourceID, Reason: "test"})
			eventually(t, "A 的隧道收到 GOAWAY replaced", func() bool {
				for _, reason := range again.Goaways() {
					if reason == "replaced" {
						return true
					}
				}
				return false
			})
		})
	}
}

func options(env *relayenv.Env, node string) cluster.Options {
	return cluster.Options{
		Issuer: env.Control.Issuer(), NodeID: node, JWKS: env.Control.JWKS,
		Now: env.Now,
	}
}
