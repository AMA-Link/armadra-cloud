package saas

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

// online 等源的路由写进 Redis 且指向 n。
func (r *rig) online(n *running) {
	r.t.Helper()
	eventually(r.t, "源在 "+n.name+" 上", func() bool {
		route, _ := n.Plane.Auth.Control.Routes().Get(context.Background(), r.env.SourceID)
		return route != nil && route.Node == n.name
	})
}

func hubStreams(n *running) int { _, s := n.Plane.Hub.Counts(); return s }

func metricLine(text, name string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimPrefix(line, name+" ")
		}
	}
	return ""
}

// 客户端连到 b、源的隧道在 a：b 经 /x/v1 把通道转到 a，字节双向原样（远超一个流窗，信用逐跳补）；
// 源看到的是标准的 e2e OPEN，relayNode 是入口节点 b；指标两边都记。
func TestForwardedChannel(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	core := r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)

	c := r.channel(b, r.relayToken(nil))
	payload := make([]byte, 3<<20)
	_, _ = rand.Read(payload)
	if got := c.echo(t, payload); !bytes.Equal(got, payload) {
		t.Fatal("回显不一致")
	}
	opens := core.Opens()
	if len(opens) != 1 || opens[0].Kind != "e2e" || opens[0].Path != gen.E2EPath || len(opens[0].Headers) != 0 || opens[0].RelayNode != "b" {
		t.Fatalf("源收到的 OPEN：%+v", opens)
	}
	if got := metricLine(b.Metrics(), "relay_routes_remote_total"); got != "1" {
		t.Fatalf("b 的 relay_routes_remote_total：%s", got)
	}
	if got := metricLine(b.Metrics(), "relay_routes_remote_failed_total"); got != "0" {
		t.Fatalf("b 的 relay_routes_remote_failed_total：%s", got)
	}
	if !strings.Contains(b.Metrics(), "relay_sched_latency_seconds_count ") {
		t.Fatal("节点指标要有调度延迟")
	}
	if got := metricLine(b.Metrics(), `relay_internode_bytes_total{direction="to_source"}`); got != "3145728" {
		t.Fatalf("b 的节点间上行字节：%s", got)
	}
	if got := metricLine(a.Metrics(), `relay_internode_bytes_total{direction="to_client"}`); got != "3145728" {
		t.Fatalf("a 的节点间下行字节：%s", got)
	}
	// 入口节点（b）有出向连接，指标在；全部传完后没有未写出的转发字节。持有节点没有出向连接，指标也在（0）。
	eventually(t, "入口节点没有未写出的转发字节", func() bool { return metricLine(b.Metrics(), "relay_internode_unwritten_bytes") == "0" })
	if metricLine(a.Metrics(), "relay_forward_account_refused_total") != "0" {
		t.Fatal(a.Metrics())
	}
	if metricLine(a.Metrics(), "relay_internode_streams") != "1" || metricLine(b.Metrics(), "relay_internode_streams") != "1" {
		t.Fatalf("两边各一条节点间流：%s / %s", metricLine(a.Metrics(), "relay_internode_streams"), metricLine(b.Metrics(), "relay_internode_streams"))
	}
	// 个人中转原有的指标照旧在前面。
	if !strings.HasPrefix(b.Metrics(), "# HELP relay_tunnels ") {
		t.Fatal("原有指标应在前")
	}
	// 客户端正常关闭：两边的流都收干净。
	_ = c.c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""))
	c.wait(t)
	eventually(t, "流收干净", func() bool {
		return metricLine(a.Metrics(), "relay_internode_streams") == "0" && metricLine(b.Metrics(), "relay_internode_streams") == "0" && hubStreams(a) == 0
	})
}

// /x/v1 要有效的节点令牌：没有、伪造、过期、错类型、已撤销一律 401，不升级。
func TestInternodeRequiresNodeToken(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	url := strings.Replace(a.URL, "http://", "ws://", 1) + gen.PathInternode
	try := func(token string) int {
		header := http.Header{}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(url, header)
		if err == nil {
			_ = conn.Close()
			return 101
		}
		if resp == nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	revoked := "n.b." + randomHex(6)
	if err := a.Plane.Auth.Control.Tokens().Revoke(context.Background(), revoked, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	good := r.nodeToken("b", nil)
	for name, token := range map[string]string{
		"没有令牌": "",
		"伪造":   good[:len(good)-4] + "AAAA",
		"过期":   r.nodeToken("b", relayenv.Claims{"iat": time.Now().Add(-3 * time.Hour).Unix(), "exp": time.Now().Add(-2 * time.Hour).Unix()}),
		"中继令牌": r.relayToken(nil),
		"已撤销":  r.nodeToken("b", relayenv.Claims{"jti": revoked}),
	} {
		if code := try(token); code != http.StatusUnauthorized {
			t.Errorf("%s：%d", name, code)
		}
	}
	if code := try(good); code != 101 {
		t.Fatalf("有效令牌应能升级：%d", code)
	}
	if !strings.Contains(a.logs.String(), "节点令牌被拒") {
		t.Fatal("被拒要记日志")
	}
	for _, secret := range []string{good, r.relayToken(nil)} {
		if strings.Contains(a.logs.String(), secret) {
			t.Fatal("日志里有令牌")
		}
	}
}

// 鉴权、并发、踢人都在入口节点：经 b 转发的通道照样按 b 上的 kick 断开；团队并发跨节点计数；
// 持有节点关隧道 → 转发的通道 4404；没有节点令牌的节点不转发（4404）。
func TestForwardingPolicyOnEntryNode(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.admin.Set(ctx, r.prefix+"quota:team:team_1", cluster.EncodeQuotaValue(control.TeamQuota{Concurrent: 1}), 0).Err(); err != nil {
		t.Fatal(err)
	}
	a := r.start("a", nil)
	b := r.start("b", nil)
	c := r.start("c", func(cfg *Config) { cfg.NodeToken = "" })
	core := r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)

	// 团队并发：a 上本地一条，b 上转发的第二条被拒（同一个 Redis 计数）。
	local := r.channel(a, r.relayToken(relayenv.Claims{"team": "team_1"}))
	if local.outcome() != 0 {
		t.Fatal("本地的第一条")
	}
	if code := r.channel(b, r.relayToken(relayenv.Claims{"team": "team_1", "sub": "acct_2"})).outcome(); code != int(protocol.CloseLimitReached) {
		t.Fatalf("跨节点的团队并发：%d", code)
	}

	// kick 广播：入口节点 b 断开经它转发的通道。
	forwarded := r.channel(b, r.relayToken(relayenv.Claims{"sub": "acct_kicked"}))
	other := r.channel(b, r.relayToken(relayenv.Claims{"sub": "acct_stays"}))
	if forwarded.outcome() != 0 || other.outcome() != 0 {
		t.Fatal("转发的通道应开成")
	}
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeCommand("cloud/test",
		control.NodeCommand{Type: control.CommandKick, SourceID: r.env.SourceID, Subject: "acct_kicked"})).Err(); err != nil {
		t.Fatal(err)
	}
	forwarded.wait(t)
	if other.outcome() != 0 || local.outcome() != 0 {
		t.Fatal("别人的通道不受影响")
	}

	// 没有节点令牌的节点不转发。
	if code := r.channel(c, r.relayToken(nil)).outcome(); code != int(protocol.CloseSourceOffline) {
		t.Fatalf("c 不转发：%d", code)
	}

	// 持有节点关隧道：转发的通道 4404。
	if err := r.admin.Publish(ctx, r.prefix+"ctl", cluster.EncodeCommand("cloud/test",
		control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: r.env.SourceID})).Err(); err != nil {
		t.Fatal(err)
	}
	<-core.Closed()
	if code := other.wait(t); code != int(protocol.CloseSourceOffline) {
		t.Fatalf("隧道关了，转发的通道 4404：%d", code)
	}
}

// 再平衡：持有节点 a 排空 → 隧道 GOAWAY rebalance、宽限后关 → 经 b 转发的通道 4404；
// 源重连到 b 之后，客户端重连落到 b（本地，不再转发）。
func TestRebalanceMovesSourceToOtherNode(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	core := r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)
	ch := r.channel(b, r.relayToken(nil))
	if got := ch.echo(t, []byte("before")); string(got) != "before" {
		t.Fatal(string(got))
	}
	a.Drain()
	eventually(t, "GOAWAY rebalance", func() bool {
		g := core.Goaways()
		return len(g) > 0 && g[0] == "rebalance"
	})
	// 节点表里 a 是 draining：控制面的 hint 不再给它。
	nodes, err := ReadNodes(context.Background(), r.admin, r.prefix)
	if err != nil {
		t.Fatal(err)
	}
	ranked := control.RankNodes(r.env.SourceID, nodes)
	if len(ranked) != 1 || ranked[0].ID != "b" {
		t.Fatalf("hint 只剩 b：%+v", ranked)
	}
	<-core.Closed()
	if code := ch.wait(t); code != int(protocol.CloseSourceOffline) {
		t.Fatalf("旧隧道结束，客户端通道 4404：%d", code)
	}
	r.env.StartCore(t, strings.TrimSuffix(ranked[0].URL, gen.PathTunnel), relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(b)
	again := r.channel(b, r.relayToken(nil))
	if got := again.echo(t, []byte("after")); string(got) != "after" {
		t.Fatal(string(got))
	}
}

// 持有节点这一侧的帧处理：形状不对的 OPEN 答 RST protocol，源不在本节点答 RST sourceGone，
// PING 答 PONG，转来的流原样回显，对端断开时转来的流被重置。
func TestInternodeServerFrames(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(a.URL, "http://", "ws://", 1)+gen.PathInternode,
		http.Header{"Authorization": {"Bearer " + r.nodeToken("b", nil)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	write := func(f protocol.Frame) {
		t.Helper()
		b, err := protocol.Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func() protocol.Frame {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		f, err := protocol.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	write(protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: &gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}}})
	if f := read(); f.Type != protocol.FrameRst || f.StreamID != 1 || f.Code != protocol.RstProtocol {
		t.Fatalf("e2e 形状的 OPEN：%+v", f)
	}
	write(protocol.Frame{Type: protocol.FrameOpen, StreamID: 3, Open: edge.InternodeOpen("ffffffffffffffffffffffffffffffff", gen.OpenPayload{RelayNode: "b"})})
	if f := read(); f.Type != protocol.FrameRst || f.StreamID != 3 || f.Code != protocol.RstSourceGone {
		t.Fatalf("源不在本节点：%+v", f)
	}
	write(protocol.Frame{Type: protocol.FramePing, Ts: 42})
	if f := read(); f.Type != protocol.FramePong || f.Ts != 42 {
		t.Fatalf("PING：%+v", f)
	}
	// 开一条真的流：字节转给源、回显原路回来；对端断开时转来的流被重置。
	write(protocol.Frame{Type: protocol.FrameOpen, StreamID: 5, Open: edge.InternodeOpen(r.env.SourceID, gen.OpenPayload{RelayNode: "b"})})
	write(protocol.Frame{Type: protocol.FrameData, StreamID: 5, Data: []byte("ping")})
	if f := read(); f.Type != protocol.FrameData || f.StreamID != 5 || string(f.Data) != "ping" {
		t.Fatalf("回显：%+v", f)
	}
	if hubStreams(a) != 1 {
		t.Fatal("本地隧道上有这条流")
	}
	_ = conn.Close()
	eventually(t, "转来的流随连接断开被重置", func() bool { return hubStreams(a) == 0 })
}
