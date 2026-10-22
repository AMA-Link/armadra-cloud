package saas

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

// 节点起来：/health 的形状、节点表那一项、Host 寻址的端到端通道回显、共享 JWKS；停下后从节点表里删掉。
func TestNodeServesHostAddressedChannels(t *testing.T) {
	r := newRig(t)
	n := r.start("a", nil)
	ctx := context.Background()

	status, body := getJSON(t, n.URL+"/health")
	if status != 200 || body["mode"] != "saas" || body["node"] != "a" || body["version"] != "9.9.9" {
		t.Fatalf("/health：%d %v", status, body)
	}
	status, body = getJSON(t, n.URL+"/ready")
	if status != 200 || body["ok"] != true {
		t.Fatalf("/ready：%d %v", status, body)
	}

	// 节点表：字段 = 节点 id，值的形状与控制面约定一致，带过期。
	raw, err := r.admin.HGet(ctx, r.prefix+"nodes", "a").Result()
	if err != nil {
		t.Fatal(err)
	}
	info, err := cluster.DecodeNode("a", []byte(raw))
	if err != nil || info.URL != "ws://a.edge.example.test/t/v1" || info.InternodeURL != "ws://a.edge.example.test/x/v1" ||
		info.Weight != 1 || info.Version != "9.9.9" || info.Draining {
		t.Fatalf("节点表：%s %v", raw, err)
	}
	ttl, err := r.admin.HPTTL(ctx, r.prefix+"nodes", "a").Result()
	if err != nil || len(ttl) != 1 || ttl[0] <= 0 || ttl[0] > nodeTTL.Milliseconds() {
		t.Fatalf("节点表的字段要带过期：%v %v", ttl, err)
	}

	core := r.env.StartCore(t, n.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由写进 Redis", func() bool {
		route, _ := n.Plane.Hub.Lookup(ctx, r.env.SourceID)
		return route != nil
	})
	c := r.channel(n, r.relayToken(nil))
	payload := make([]byte, 300_000)
	_, _ = rand.Read(payload)
	if got := c.echo(t, payload); !bytes.Equal(got, payload) {
		t.Fatal("回显不一致")
	}
	if opens := core.Opens(); len(opens) != 1 || opens[0].Kind != "e2e" || opens[0].RelayNode != "a" {
		t.Fatalf("%+v", opens)
	}
	// 共享 JWKS：验令牌时回源一次并写进 Redis。
	if v, err := r.admin.Get(ctx, JWKSKey(r.prefix, r.env.Issuer.Issuer)).Result(); err != nil || !strings.Contains(v, r.env.Issuer.Kid) {
		t.Fatalf("共享 JWKS：%q %v", v, err)
	}
	// 中继令牌验过的地址之外（明文 HTTP、路径寻址）一律 404。
	req, _ := http.NewRequest(http.MethodGet, n.URL+"/_relay/e2e", nil)
	req.Host = r.env.SourceID + ".src." + domain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("明文 HTTP：%d", resp.StatusCode)
	}

	n.stop()
	if exists, _ := r.admin.HExists(ctx, r.prefix+"nodes", "a").Result(); exists {
		t.Fatal("正常下线要从节点表里删掉")
	}
	if !strings.Contains(n.logs.String(), "中继已停止") {
		t.Fatal(n.logs.String())
	}
}

// 第二个节点读共享 JWKS，不再回源。
func TestSharedJWKSAcrossNodes(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	_ = b
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	first := r.jwksHits.Load()
	if first != 1 {
		t.Fatalf("第一个节点回源一次：%d", first)
	}
	// b 验一枚令牌（源不在 b 上：4404，但令牌已验过）。
	if code := r.channel(b, r.relayToken(nil)).outcome(); code != int(protocol.CloseSourceOffline) {
		t.Fatalf("b 上没有源的隧道：%d", code)
	}
	if r.jwksHits.Load() != first {
		t.Fatalf("b 应读共享副本：%d", r.jwksHits.Load())
	}
}

// kick 按人、按设备只踢客户端连接，隧道保留；closeTunnel 关隧道；令牌撤销跨节点生效。
func TestKickAndRevocation(t *testing.T) {
	r := newRig(t)
	n := r.start("a", nil)
	other := r.start("b", nil)
	ctx := context.Background()
	core := r.env.StartCore(t, n.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由", func() bool { t2, _ := n.Plane.Hub.Lookup(ctx, r.env.SourceID); return t2 != nil })

	alice := r.channel(n, r.relayToken(relayenv.Claims{"sub": "acct_alice", "dev": "dev_1"}))
	alice2 := r.channel(n, r.relayToken(relayenv.Claims{"sub": "acct_alice", "dev": "dev_2"}))
	bob := r.channel(n, r.relayToken(relayenv.Claims{"sub": "acct_bob"}))
	for _, c := range []*client{alice, alice2, bob} {
		if code := c.outcome(); code != 0 {
			t.Fatalf("应开成：%d", code)
		}
	}
	// 控制面发到持有节点的频道：按人 + 设备。
	kick := control.NodeCommand{Type: control.CommandKick, SourceID: r.env.SourceID, Subject: "acct_alice", DeviceID: "dev_1", Reason: "device_revoked"}
	if err := r.admin.Publish(ctx, r.prefix+"node:a", cluster.EncodeCommand("cloud/test", kick)).Err(); err != nil {
		t.Fatal(err)
	}
	if code := alice.wait(t); code == 0 {
		t.Fatal("alice 的 dev_1 应被踢")
	}
	if alice2.outcome() != 0 || bob.outcome() != 0 {
		t.Fatal("别的设备、别的人不受影响")
	}
	// 按人（广播）：踢掉 alice 剩下的连接。
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeCommand("cloud/test", control.NodeCommand{Type: control.CommandKick, SourceID: r.env.SourceID, Subject: "acct_alice"})).Err(); err != nil {
		t.Fatal(err)
	}
	alice2.wait(t)
	if bob.outcome() != 0 {
		t.Fatal("bob 不受影响")
	}
	select {
	case <-core.Closed():
		t.Fatal("kick 不该关隧道")
	default:
	}

	// 撤销跨节点：在 b 上撤销的令牌，a 上新开的通道 4401。
	jti := relayenv.NewJTI()
	token := r.relayToken(relayenv.Claims{"jti": jti})
	if err := other.Plane.Auth.Control.Tokens().Revoke(ctx, jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a 拒绝 b 撤销的令牌", func() bool { return r.channel(n, token).outcome() == int(protocol.CloseUnauthenticated) })

	// closeTunnel：隧道 4492。
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeCommand("cloud/test", control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: r.env.SourceID})).Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-core.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("closeTunnel 之后隧道应关闭")
	}
	if bob.wait(t) != int(protocol.CloseSourceOffline) {
		t.Fatal("隧道关了，通道 4404")
	}
}

// 团队额度：并发上限取 relay:quota:team:*（订阅就绪时全量读、之后按广播更新），跨节点计数；
// 事故开关拒绝新通道与新隧道，在线连接保留。
func TestTeamQuotaAndIncident(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// 节点启动前写好的额度：就绪时读到。
	if err := r.admin.Set(ctx, r.prefix+"quota:team:team_1", cluster.EncodeQuotaValue(control.TeamQuota{Concurrent: 1}), 0).Err(); err != nil {
		t.Fatal(err)
	}
	a := r.start("a", nil)
	b := r.start("b", nil)
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由", func() bool { t2, _ := a.Plane.Hub.Lookup(ctx, r.env.SourceID); return t2 != nil })

	first := r.channel(a, r.relayToken(relayenv.Claims{"team": "team_1"}))
	if code := first.outcome(); code != 0 {
		t.Fatalf("第一条开成：%d", code)
	}
	// 同一团队的第二条：计数在 Redis 里（各节点共用），满了 4429。跨节点经转发进来的在节点间转发的测试里。
	second := r.channel(a, r.relayToken(relayenv.Claims{"team": "team_1", "sub": "acct_other"}))
	if code := second.outcome(); code != int(protocol.CloseLimitReached) {
		t.Fatalf("团队并发满了应 4429：%d", code)
	}
	if n, _ := r.admin.ZCard(ctx, r.prefix+"conc:team:team_1").Result(); n != 1 {
		t.Fatalf("计数：%d", n)
	}
	// 不带 team（个人空间）不受团队额度影响。
	if code := r.channel(a, r.relayToken(nil)).outcome(); code != 0 {
		t.Fatalf("个人空间：%d", code)
	}
	// 额度广播：放到 2。
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeQuota("cloud/test", control.TeamQuota{TeamID: "team_1", Concurrent: 2})).Err(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "额度更新后能再开一条", func() bool {
		return r.channel(a, r.relayToken(relayenv.Claims{"team": "team_1", "sub": "acct_x"})).outcome() == 0
	})
	// 关掉第一条：名额还回。
	_ = first.c.Close()
	eventually(t, "名额还回", func() bool { n, _ := r.admin.ZCard(ctx, r.prefix+"conc:team:team_1").Result(); return n == 1 })

	// 事故开关：新通道与新隧道 4429 / 429，在线连接保留。
	online := r.channel(a, r.relayToken(nil))
	if online.outcome() != 0 {
		t.Fatal("开关打开前的通道")
	}
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeIncident("cloud/test", control.Incident{RejectNewConnections: true, AtMs: 1})).Err(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a 拒新通道", func() bool { return r.channel(a, r.relayToken(nil)).outcome() == int(protocol.CloseLimitReached) })
	eventually(t, "b 拒新隧道", func() bool {
		resp, err := http.Get(b.URL + "/t/v1")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		// 普通 GET 不是升级：照常 404；升级才拒。
		req, _ := http.NewRequest(http.MethodGet, b.URL+"/t/v1", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusTooManyRequests
	})
	if online.outcome() != 0 {
		t.Fatal("在线连接保留")
	}
	// 节点重启时从键里读到开关。
	if err := r.admin.Set(ctx, r.prefix+"incident", cluster.EncodeIncidentValue(control.Incident{RejectNewConnections: true}), 0).Err(); err != nil {
		t.Fatal(err)
	}
	c := r.start("c", nil)
	if code := r.channel(c, r.relayToken(nil)).outcome(); code != int(protocol.CloseLimitReached) {
		t.Fatalf("启动时读到事故开关：%d", code)
	}
}

// drain：只认发给本节点的；节点表标 draining、/ready 不就绪、隧道 GOAWAY rebalance、不再收新隧道。
func TestDrainRebalancesTunnels(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.start("a", nil)
	core := r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由", func() bool { t2, _ := a.Plane.Hub.Lookup(ctx, r.env.SourceID); return t2 != nil })

	// 发给别的节点的 drain 不理。
	_ = r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeCommand("cloud/test", control.NodeCommand{Type: control.CommandDrain, Node: "zzz"})).Err()
	time.Sleep(200 * time.Millisecond)
	if a.Plane.Hub.Draining() {
		t.Fatal("别的节点的 drain 不该排空本节点")
	}
	if err := r.admin.Publish(ctx, r.prefix+"node:a", cluster.EncodeCommand("cloud/test", control.NodeCommand{Type: control.CommandDrain, Node: "a"})).Err(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "GOAWAY rebalance", func() bool {
		for _, reason := range core.Goaways() {
			if reason == "rebalance" {
				return true
			}
		}
		return false
	})
	raw, _ := r.admin.HGet(ctx, r.prefix+"nodes", "a").Result()
	var wire map[string]any
	_ = json.Unmarshal([]byte(raw), &wire)
	if wire["draining"] != true {
		t.Fatalf("节点表：%s", raw)
	}
	if status, _ := getJSON(t, a.URL+"/ready"); status != 503 {
		t.Fatalf("排空中 /ready：%d", status)
	}
	select {
	case <-core.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("宽限到了隧道应关闭")
	}
	if code := core.CloseCode(); code != int(protocol.CloseTunnelGoaway) {
		t.Fatalf("隧道关闭码：%d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, a.URL+"/t/v1", nil)
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("排空中不收新隧道：%d", resp.StatusCode)
	}
}

// 下线（ctx 结束）先排空：隧道收到 GOAWAY rebalance。
func TestShutdownDrainsFirst(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.start("a", nil)
	core := r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由", func() bool { t2, _ := a.Plane.Hub.Lookup(ctx, r.env.SourceID); return t2 != nil })
	a.stop()
	goaways := core.Goaways()
	if len(goaways) == 0 || goaways[0] != "rebalance" {
		t.Fatalf("下线先发 GOAWAY rebalance：%v", goaways)
	}
}
