package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/controltest"
	cr "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
)

// 测试读 REDIS_URL（pnpm dev:up 的 Redis，或 CI 的服务容器）；没有就跳过，CI 里一律失败。
func redisURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL（pnpm dev:up 的 Redis），跳过")
	}
	return url
}

func randomHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type env struct {
	t      *testing.T
	url    string
	prefix string
	admin  *goredis.Client
}

// newEnv 给每个测试一个随机前缀；结束时只删这个前缀下的键。
func newEnv(t *testing.T) *env {
	t.Helper()
	url := redisURL(t)
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, url: url, prefix: "relay-test:" + randomHex() + ":", admin: goredis.NewClient(opts)}
	t.Cleanup(func() {
		ctx := context.Background()
		iter := e.admin.Scan(ctx, 0, e.prefix+"*", 200).Iterator()
		for iter.Next(ctx) {
			e.admin.Del(ctx, iter.Val())
		}
		_ = e.admin.Close()
	})
	return e
}

type node struct {
	*cr.Plane
	name   string
	errors *errorLog
}

type errorLog struct {
	mu   sync.Mutex
	list []string
}

func (l *errorLog) add(op string) { l.mu.Lock(); l.list = append(l.list, op); l.mu.Unlock() }
func (l *errorLog) has(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, op := range l.list {
		if strings.HasPrefix(op, prefix) {
			return true
		}
	}
	return false
}

func (e *env) node(name string, over func(*cr.Options)) *node {
	e.t.Helper()
	log := &errorLog{}
	opts, err := goredis.ParseURL(e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	opts.ClientName = "armadra-go-test-" + e.prefix + name
	o := cr.Options{
		Options: cluster.Options{
			Issuer: "https://relay.example.test", NodeID: name, Limits: controltest.Limited,
			OnError: func(op string, _ error) { log.add(op) },
		},
		Client: goredis.NewClient(opts), Prefix: e.prefix,
		ReconnectMin: 50 * time.Millisecond, ReconnectMax: 200 * time.Millisecond,
	}
	if over != nil {
		over(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plane, err := cr.New(ctx, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		_ = plane.Close()
		if c, ok := o.Client.(*goredis.Client); ok {
			_ = c.Close()
		}
	})
	return &node{Plane: plane, name: name, errors: log}
}

func (e *env) pair() controltest.Pair {
	a, b := e.node("node-a", nil), e.node("node-b", nil)
	return controltest.Pair{
		A: controltest.Node{Cluster: a.Plane, Settle: a.Settle},
		B: controltest.Node{Cluster: b.Plane, Settle: b.Settle},
	}
}

func TestClusterContract(t *testing.T) {
	controltest.Contract(t, func(t *testing.T) controltest.Pair { return newEnv(t).pair() })
}

// 键的线上形状与 TypeScript 一侧一致：Go 写的路由 TypeScript 读得到，TypeScript 写的 Go 读得到，
// Lua 按 tunnelId 的 CAS 对两边写的都有效。
func TestWireFormatMatchesTypeScript(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.node("node-a", nil)

	must(t, n.Routes().Set(ctx, controltest.Source, control.Route{Node: "node-a", TunnelID: "t1", SinceMs: 7, CoreVersion: "0.3.0"}, time.Minute))
	raw, err := e.admin.Get(ctx, e.prefix+"route:src:"+controltest.Source).Result()
	must(t, err)
	if raw != `{"node":"node-a","tunnelId":"t1","sinceMs":7,"coreVersion":"0.3.0"}` {
		t.Fatalf("路由 JSON：%s", raw)
	}
	ttl := e.admin.PTTL(ctx, e.prefix+"route:src:"+controltest.Source).Val()
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("路由应带 PX 过期：%v", ttl)
	}

	// TypeScript 写的（JSON.stringify 的字段顺序也是这个）。
	must(t, e.admin.Set(ctx, e.prefix+"route:src:"+"bb", `{"node":"ts-node","tunnelId":"tt","sinceMs":9,"coreVersion":"0.3.0"}`, time.Minute).Err())
	got, err := n.Routes().Get(ctx, "bb")
	if err != nil || got == nil || got.Node != "ts-node" || got.TunnelID != "tt" || got.SinceMs != 9 {
		t.Fatalf("读 TypeScript 写的路由：%+v %v", got, err)
	}
	must(t, n.Routes().Delete(ctx, "bb", "other"))
	if got, _ := n.Routes().Get(ctx, "bb"); got == nil {
		t.Fatal("CAS：别的隧道删不掉")
	}
	must(t, n.Routes().Delete(ctx, "bb", "tt"))
	if got, _ := n.Routes().Get(ctx, "bb"); got != nil {
		t.Fatal("CAS：同一隧道应删掉")
	}

	// 撤销键：值 "1"，PXAT 到 exp 的毫秒。
	exp := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	must(t, n.Tokens().Revoke(ctx, "jti-x", exp))
	at := e.admin.PExpireTime(ctx, e.prefix+"revoked:jti-x").Val()
	if at.Milliseconds() != exp.UnixMilli() && at != time.Duration(exp.UnixMilli())*time.Millisecond {
		t.Fatalf("撤销键应 PXAT 到 %d：%v", exp.UnixMilli(), at)
	}
	if v := e.admin.Get(ctx, e.prefix+"revoked:jti-x").Val(); v != "1" {
		t.Fatalf("撤销键的值：%q", v)
	}

	// 广播形状：TypeScript 节点订阅了 <前缀>presence / ctl / revoked，要认得 Go 发的。
	sub := e.admin.Subscribe(ctx, e.prefix+"presence", e.prefix+"ctl", e.prefix+"revoked")
	defer func() { _ = sub.Close() }()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Receive(ctx)
	_, _ = sub.Receive(ctx)
	must(t, n.Presence().Publish(ctx, control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: "s", Node: "n", TunnelID: "t", AtMs: 3}))
	n.Command(control.NodeCommand{Type: control.CommandKick, SourceID: "s", Reason: "r"})
	must(t, n.Tokens().Revoke(ctx, "jti-y", time.UnixMilli(4102444800000)))
	want := map[string]string{
		e.prefix + "presence": `"kind":"presence","event":{"type":"sourceOffline","sourceId":"s","node":"n","tunnelId":"t","atMs":3}`,
		e.prefix + "ctl":      `"kind":"command","command":{"type":"kick","sourceId":"s","reason":"r"}`,
		e.prefix + "revoked":  `"kind":"revoked","jti":"jti-y","expMs":4102444800000`,
	}
	for i := 0; i < 3; i++ {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		msg, err := sub.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg.Payload, want[msg.Channel]) || !strings.Contains(msg.Payload, `"origin":"`) {
			t.Fatalf("%s 的广播形状：%s", msg.Channel, msg.Payload)
		}
		delete(want, msg.Channel)
	}
}

// 启动时全量读一遍撤销；订阅连接被断开后重连并再读一遍，断线期间只写了键、没收到广播的撤销也不漏。
func TestRevocationReloadOnStartAndReconnect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	at := time.Now().Add(time.Minute).UnixMilli()
	must(t, e.admin.Do(ctx, "SET", e.prefix+"revoked:before-start", "1", "PXAT", at).Err())
	must(t, e.admin.Do(ctx, "SET", e.prefix+"revoked:already-gone", "1", "PXAT", time.Now().Add(150*time.Millisecond).UnixMilli()).Err())
	n := e.node("node-a", nil)
	if revoked, _ := n.Tokens().IsRevoked(ctx, "before-start"); !revoked {
		t.Fatal("启动时应全量读到已有的撤销")
	}

	killSubscriber(t, e, "node-a")
	// 断线窗口里写键（不广播）。
	must(t, e.admin.Do(ctx, "SET", e.prefix+"revoked:during-outage", "1", "PXAT", at).Err())
	controltest.Eventually(t, "重连后全量重读到断线期间的撤销", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "during-outage")
		return r
	})
	// 重连之后广播照常。
	other := e.node("node-b", nil)
	must(t, other.Tokens().Revoke(ctx, "after-reconnect", time.Now().Add(time.Minute)))
	controltest.Eventually(t, "重连后广播仍能收到", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "after-reconnect")
		return r
	})
	time.Sleep(200 * time.Millisecond)
	if n.RevokedCount() != 3 {
		t.Fatalf("过期的不算：%d", n.RevokedCount())
	}
}

// killSubscriber 断开某个节点的订阅连接（按测试给它的客户端名找，不碰库里别人的连接）。
func killSubscriber(t *testing.T, e *env, name string) {
	t.Helper()
	ctx := context.Background()
	list := e.admin.ClientList(ctx).Val()
	killed := 0
	for _, line := range strings.Split(list, "\n") {
		if !strings.Contains(line, "name="+"armadra-go-test-"+e.prefix+name+" ") || !strings.Contains(line, "cmd=subscribe") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if id, ok := strings.CutPrefix(field, "id="); ok {
				e.admin.Do(ctx, "CLIENT", "KILL", "ID", id)
				killed++
			}
		}
	}
	if killed == 0 {
		t.Fatalf("没找到订阅连接：\n%s", list)
	}
}

// Redis 出错时速率限流退回本节点内存，并报告；不拖垮调用方。
func TestLimitFallsBackToLocalWhenRedisFails(t *testing.T) {
	e := newEnv(t)
	n := e.node("node-a", nil)
	ctx := context.Background()
	// 换掉服务端：把客户端关了，之后所有命令都出错。
	opts, _ := goredis.ParseURL(e.url)
	broken := goredis.NewClient(opts)
	m, err := cr.New(ctx, cr.Options{
		Options: cluster.Options{Issuer: "x", Limits: controltest.Limited, OnError: func(op string, _ error) { n.errors.add(op) }},
		Client:  broken, Prefix: e.prefix + "fb:", ReconnectMin: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
	})
	must(t, err)
	defer func() { _ = m.Close() }()
	_ = broken.Close()
	for i := 0; i < 5; i++ {
		r, err := m.Limits().Hit(ctx, control.LimitLoginIP, "203.0.113.1")
		if err != nil || !r.Allowed {
			t.Fatalf("退回本节点内存后前 5 次应放行：%+v %v", r, err)
		}
	}
	r, _ := m.Limits().Hit(ctx, control.LimitLoginIP, "203.0.113.1")
	if r.Allowed || r.RetryAfter <= 0 {
		t.Fatalf("本节点内存的桶第 6 次应拒：%+v", r)
	}
	if !n.errors.has("limit") {
		t.Fatal("退回本节点应报告")
	}
}

func TestNewFailsFastWhenRedisUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cr.New(ctx, cr.Options{Options: cluster.Options{Issuer: "x"}, URL: "redis://:s3cret-pass@127.0.0.1:1"})
	if err == nil {
		t.Fatal("连不上应报错")
	}
	if strings.Contains(err.Error(), "s3cret-pass") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("错误信息泄露了连接串片段：%v", err)
	}
	if _, err := cr.New(ctx, cr.Options{URL: "://坏的 s3cret-pass"}); err == nil || strings.Contains(err.Error(), "s3cret-pass") {
		t.Fatalf("坏连接串的错误：%v", err)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
