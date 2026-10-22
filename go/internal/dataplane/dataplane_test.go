package dataplane_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-runtime-node/test/integration/roundtrip.test.ts：
// 假 core 经中继完成端到端通道往返、背压、限额、断线重连、GOAWAY。

func TestPlainPathsRemoved(t *testing.T) {
	t.Run("源地址上的 HTTP（带令牌、预检、媒体票）一律 404，不进隧道；/health 仍是中继自己的", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) {
			o.CORSOrigins = func(string) []string { return []string{"https://app.example"} }
		}})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		base := h.base() + h.sourcePath()
		do := func(method, path string, hdr map[string]string, body string) *http.Response {
			req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { res.Body.Close() })
			return res
		}
		get := do("GET", "/echo-request", map[string]string{"Origin": "https://app.example"}, "")
		var body struct{ Code string }
		_ = json.NewDecoder(get.Body).Decode(&body)
		if get.StatusCode != 404 || body.Code != "not_found" {
			t.Fatalf("%d %+v", get.StatusCode, body)
		}
		if get.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
			t.Fatalf("名单内来源应回显 CORS：%v", get.Header)
		}
		if do("POST", "/echo", nil, marker).StatusCode != 404 {
			t.Fatal("POST 应 404")
		}
		if do("OPTIONS", "/api/x", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST"}, "").StatusCode != 404 {
			t.Fatal("预检应 404")
		}
		if do("POST", "/_relay/media-tickets", nil, "{}").StatusCode != 404 {
			t.Fatal("媒体票应 404")
		}
		if do("GET", "/_relay/m/ticket", nil, "").StatusCode != 404 {
			t.Fatal("媒体票兑取应 404")
		}
		res, _ := http.Get(h.base() + "/s/abc/echo")
		if res.StatusCode != 404 {
			t.Fatal("坏源 ID 应 404")
		}
		res, _ = http.Get(h.base() + "/health")
		var health map[string]any
		_ = json.NewDecoder(res.Body).Decode(&health)
		if res.StatusCode != 200 || health["ok"] != true || health["node"] != "relay-test" {
			t.Fatalf("/health：%d %v", res.StatusCode, health)
		}
		if len(core.Opens()) != 0 || h.streams() != 0 {
			t.Fatal("不该进隧道")
		}
	})

	t.Run("别的路径上的 WS 升级（明文 WS）：404，不升级、不进隧道", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		d := websocket.Dialer{Subprotocols: []string{"armadra-ticket.t1", "armadra-relay." + h.relayToken(nil)}}
		_, res, err := d.Dial(relayenv.WSURL(h.base(), h.sourcePath()+"/api/events"), nil)
		if err == nil || res == nil || res.StatusCode != 404 {
			t.Fatalf("应收到 404：%v %v", err, res)
		}
		if len(core.Opens()) != 0 {
			t.Fatal("不该进隧道")
		}
	})
}

func TestChannel(t *testing.T) {
	t.Run("1000 条消息与 300 KB 二进制往返完好；源下线以 4404 结束", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		c := h.channel(h.relayToken(nil))
		if _, _, open := c.outcome(); !open {
			t.Fatal("通道应开成")
		}
		for i := 0; i < 1000; i++ {
			payload := []byte(fmt.Sprintf("%s-%d", marker, i))
			if got := c.echo(t, payload); string(got) != string(payload) {
				t.Fatalf("第 %d 条回显不一致", i)
			}
		}
		big := make([]byte, 300000)
		for i := range big {
			big[i] = 9
		}
		if got := c.echo(t, big); len(got) != len(big) {
			t.Fatal("300 KB 回显长度不对")
		}
		core.Terminate()
		if info := c.wait(t); info.code != int(protocol.CloseSourceOffline) {
			t.Fatalf("%+v", info)
		}
	})

	t.Run("中继对客户端定时 ping；客户端不说话超过 idle 就断", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) {
			o.WSPing, o.WSIdle = 60*time.Millisecond, 400*time.Millisecond
		}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		// gorilla 客户端读到 ping 会自动回 pong，所以这条一直活着。
		pings := make(chan struct{}, 100)
		conn, _, err := dial(t, h.e2eURL(), []string{"armadra-e2e.1", "armadra-relay." + h.relayToken(nil)})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetPingHandler(func(s string) error {
			pings <- struct{}{}
			return conn.WriteControl(websocket.PongMessage, []byte(s), time.Now().Add(time.Second))
		})
		cl := startClient(t, conn)
		for i := 0; i < 3; i++ {
			select {
			case <-pings:
			case <-time.After(3 * time.Second):
				t.Fatal("没收到 ping")
			}
		}
		// 换一个只握手不说话的裸连接。
		raw, err := net.Dial("tcp", h.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		fmt.Fprintf(raw, "GET %s/_relay/e2e HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
			"Sec-WebSocket-Protocol: armadra-e2e.1, armadra-relay.%s\r\n\r\n", h.sourcePath(), h.relayToken(nil))
		started := time.Now()
		got, _ := io.ReadAll(raw)
		if !strings.HasPrefix(string(got), "HTTP/1.1 101") {
			t.Fatalf("应先升级成功：%q", got)
		}
		if time.Since(started) < 350*time.Millisecond {
			t.Fatalf("idle 之前不该断：%v", time.Since(started))
		}
		if _, _, open := cl.outcome(); !open {
			t.Fatal("会回 pong 的连接应一直活着")
		}
	})
}

func TestLimits(t *testing.T) {
	t.Run("隧道流数满 → 4429 limit_reached；释放后恢复", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: limits(func(c *tunnel.Config) { c.MaxStreams = 1 })})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		held := h.channel(h.relayToken(nil))
		if _, _, open := held.outcome(); !open {
			t.Fatal("第一条应开成")
		}
		if code, _, open := h.channel(h.relayToken(nil)).outcome(); open || code != int(protocol.CloseLimitReached) {
			t.Fatalf("第二条应 4429：%d %v", code, open)
		}
		_ = held.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		held.wait(t)
		eventually(t, "流释放", func() bool { return h.streams() == 0 })
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("释放后应恢复")
		}
	})

	t.Run("同一账号并发通道超限 → 4429；关掉一条后又能连", func(t *testing.T) {
		h := newHarness(t, harnessOptions{control: control.MemoryOptions{Limits: map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConcurrentAcct: {Concurrent: true, Max: 1},
		}}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		first := h.channel(h.relayToken(nil))
		if _, _, open := first.outcome(); !open {
			t.Fatal("第一条应开成")
		}
		if code, _, open := h.channel(h.relayToken(nil)).outcome(); open || code != int(protocol.CloseLimitReached) {
			t.Fatalf("第二条应 4429：%d %v", code, open)
		}
		_ = first.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		first.wait(t)
		eventually(t, "并发名额归还", func() bool {
			probe := h.channel(h.relayToken(nil))
			_, _, open := probe.outcome()
			_ = probe.c.Close()
			return open
		})
	})

	t.Run("有效令牌按令牌限流 → 4429；别的令牌各自独立", func(t *testing.T) {
		h := newHarness(t, harnessOptions{control: control.MemoryOptions{Limits: map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConnectToken: {Max: 1, Window: time.Minute},
		}}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		fixed := h.relayToken(nil)
		if _, _, open := h.channel(fixed).outcome(); !open {
			t.Fatal("第一次应开成")
		}
		if code, _, open := h.channel(fixed).outcome(); open || code != int(protocol.CloseLimitReached) {
			t.Fatalf("同一令牌第二次应 4429：%d %v", code, open)
		}
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("别的令牌不受影响")
		}
	})

	t.Run("按账号限流：同一账号的多个令牌共用", func(t *testing.T) {
		h := newHarness(t, harnessOptions{control: control.MemoryOptions{Limits: map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConnectAcct: {Max: 2, Window: time.Minute},
		}}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		for i := 0; i < 2; i++ {
			if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
				t.Fatalf("第 %d 次应开成", i+1)
			}
		}
		if code, _, open := h.channel(h.relayToken(nil)).outcome(); open || code != int(protocol.CloseLimitReached) {
			t.Fatalf("第三次应 4429：%d %v", code, open)
		}
		if _, _, open := h.channel(h.relayToken(relayenv.Claims{"sub": "acct_other"})).outcome(); !open {
			t.Fatal("别的账号不受影响")
		}
	})

	t.Run("无令牌与无效令牌按 IP 严格限流，不挤占有效令牌", func(t *testing.T) {
		h := newHarness(t, harnessOptions{control: control.MemoryOptions{Limits: map[control.LimitKind]control.LimitRule{
			control.LimitEdgeConnectIP: {Max: 2, Window: time.Minute},
		}}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		if code, _, _ := h.channel("").outcome(); code != int(protocol.CloseUnauthenticated) {
			t.Fatalf("无令牌应 4401：%d", code)
		}
		if code, _, _ := h.channel("garbage").outcome(); code != int(protocol.CloseUnauthenticated) {
			t.Fatalf("坏令牌应 4401：%d", code)
		}
		if code, _, _ := h.channel("").outcome(); code != int(protocol.CloseLimitReached) {
			t.Fatalf("第三次应 4429：%d", code)
		}
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("带有效令牌的照常通过")
		}
	})
}

func TestBackpressure(t *testing.T) {
	t.Run("客户端不读时源被窗口挡住；恢复后整段字节完好", func(t *testing.T) {
		const total = 32 << 20
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}, E2ESend: total})
		conn, _, err := dial(t, h.e2eURL(), []string{"armadra-e2e.1", "armadra-relay." + h.relayToken(nil)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		// 不读：等源停下来（连续一段时间 BytesSent 不再增长）。
		last := int64(-1)
		eventually(t, "源被挡住", func() bool {
			time.Sleep(200 * time.Millisecond)
			cur := core.BytesSent.Load()
			stalled := cur == last && cur > 0
			last = cur
			return stalled
		})
		// 停下时源只送出了窗口 + 两端套接字缓冲那么多，远小于全量。
		if sent := core.BytesSent.Load(); sent >= 16<<20 {
			t.Fatalf("背压没起作用：源已送出 %d 字节", sent)
		}
		// 恢复读，整段字节完好。
		received, bad := 0, -1
		for received < total {
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, b, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("读失败：%v（已收 %d）", err, received)
			}
			for i, v := range b {
				if bad < 0 && v != relayenv.BigByte(received+i) {
					bad = received + i
				}
			}
			received += len(b)
		}
		if bad >= 0 {
			t.Fatalf("字节在 %d 处不一致", bad)
		}
	})

	t.Run("小窗口下 1 MB 也能完整往返（两侧信用都回补）", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: limits(func(c *tunnel.Config) { c.StreamWindow, c.TunnelWindow = 4096, 16384 })})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		c := h.channel(h.relayToken(nil))
		body := make([]byte, 1_000_000)
		for i := range body {
			body[i] = relayenv.BigByte(i)
		}
		got := c.echo(t, body)
		if sha256.Sum256(got) != sha256.Sum256(body) {
			t.Fatal("回显字节不一致")
		}
	})
}

func TestReconnect(t *testing.T) {
	t.Run("源断开 → 路由删除、4404；重连后路由更新、通道恢复", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		// 先订阅再连：sourceOnline 是隧道就绪之后异步发的。
		var emu sync.Mutex
		var events []control.PresenceType
		h.env.Control.Presence().Subscribe(func(e control.PresenceEvent) {
			emu.Lock()
			events = append(events, e.Type)
			emu.Unlock()
		})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		first := h.channel(h.relayToken(nil))
		if _, _, open := first.outcome(); !open {
			t.Fatal("通道应开成")
		}
		ctx := t.Context()
		firstRoute, _ := h.env.Control.Routes().Get(ctx, h.env.SourceID)
		core.Terminate()
		eventually(t, "路由已删", func() bool {
			r, _ := h.env.Control.Routes().Get(ctx, h.env.SourceID)
			return r == nil
		})
		if code, _, _ := h.channel(h.relayToken(nil)).outcome(); code != int(protocol.CloseSourceOffline) {
			t.Fatalf("源离线应 4404：%d", code)
		}
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		secondRoute, _ := h.env.Control.Routes().Get(ctx, h.env.SourceID)
		if secondRoute == nil || secondRoute.TunnelID == firstRoute.TunnelID {
			t.Fatalf("路由应更新：%+v %+v", firstRoute, secondRoute)
		}
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("重连后通道应恢复")
		}
		eventually(t, "在线事件", func() bool {
			emu.Lock()
			defer emu.Unlock()
			return len(events) == 3
		})
		emu.Lock()
		defer emu.Unlock()
		want := []control.PresenceType{control.PresenceSourceOnline, control.PresenceSourceOffline, control.PresenceSourceOnline}
		for i := range want {
			if events[i] != want[i] {
				t.Fatalf("在线事件 %v", events)
			}
		}
	})

	t.Run("同源新隧道替换旧的：旧的收 GOAWAY replaced 后以 4491 关闭，路由指向新的", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: limits(func(c *tunnel.Config) { c.GoawayGrace = 200 * time.Millisecond })})
		first := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		<-first.Closed()
		if first.CloseCode() != int(protocol.CloseTunnelReplaced) {
			t.Fatalf("旧隧道应以 4491 关闭：%d", first.CloseCode())
		}
		if g := first.Goaways(); len(g) != 1 || g[0] != "replaced" {
			t.Fatalf("%v", g)
		}
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("新隧道上通道应开成")
		}
		if h.tunnels() != 1 {
			t.Fatalf("隧道数 %d", h.tunnels())
		}
	})

	t.Run("心跳两次未答：中继断开隧道、删路由、计数", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: limits(func(c *tunnel.Config) { c.Heartbeat = 50 * time.Millisecond })})
		core := h.core(relayenv.CoreOptions{NoPong: true})
		<-core.Closed()
		eventually(t, "路由已删", func() bool {
			r, _ := h.env.Control.Routes().Get(t.Context(), h.env.SourceID)
			return r == nil
		})
		if n := h.plane.Metrics.Snapshot().HeartbeatMisses; n != 1 {
			t.Fatalf("心跳超时计数 %d", n)
		}
	})

	t.Run("握手：坏令牌被拒（4490）、握手超时不发 hello 关 4400", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) { o.HandshakeTimeout = 100 * time.Millisecond }})
		bad := h.env.TunnelToken(t, relayenv.Claims{"sub": "fedcba9876543210fedcba9876543210"})
		conn, _, err := websocket.DefaultDialer.Dial(relayenv.WSURL(h.base(), "/t/v1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		hello := fmt.Sprintf(`{"type":"hello","protocol":{"major":1,"minor":3},"sourceId":%q,"coreVersion":"0.2.0","capabilities":[],"tunnelToken":%q,"nonce":"oaGhoaGhoaGhoaGhoaGhoQ"}`, h.env.SourceID, bad)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(hello))
		_, msg, err := conn.ReadMessage()
		if err != nil || !strings.Contains(string(msg), "tunnel_token_invalid") {
			t.Fatalf("应收到 reject：%s %v", msg, err)
		}
		_, _, err = conn.ReadMessage()
		if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != int(protocol.CloseTunnelAuthFailed) {
			t.Fatalf("应以 4490 关闭：%v", err)
		}
		idle, _, err := websocket.DefaultDialer.Dial(relayenv.WSURL(h.base(), "/t/v1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer idle.Close()
		_, _, err = idle.ReadMessage()
		if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != int(protocol.CloseBadFrame) {
			t.Fatalf("握手超时应以 4400 关闭：%v", err)
		}
		snap := h.plane.Metrics.Snapshot()
		if snap.Handshakes["tunnel_token_invalid"] != 1 || snap.Handshakes["timeout"] != 1 {
			t.Fatalf("%+v", snap.Handshakes)
		}
	})
}

func TestGoaway(t *testing.T) {
	t.Run("中继关闭：发 GOAWAY shutdown，新通道 4404，旧通道自然结束后 4492", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		held := h.channel(h.relayToken(nil))
		if _, _, open := held.outcome(); !open {
			t.Fatal("通道应开成")
		}
		done := make(chan struct{})
		go func() { h.plane.Hub.Shutdown(5 * time.Second); close(done) }()
		eventually(t, "收到 GOAWAY", func() bool { return len(core.Goaways()) == 1 })
		if g := core.Goaways(); g[0] != "shutdown" {
			t.Fatalf("%v", g)
		}
		// 新通道：隧道在排空，不再开新流（4404）。
		if code, _, open := h.channel(h.relayToken(nil)).outcome(); open || code != int(protocol.CloseSourceOffline) {
			t.Fatalf("新通道应 4404：%d %v", code, open)
		}
		payload := []byte(marker)
		if got := held.echo(t, payload); string(got) != marker {
			t.Fatal("旧通道应还能用")
		}
		_ = held.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		<-core.Closed()
		if core.CloseCode() != int(protocol.CloseTunnelGoaway) {
			t.Fatalf("旧通道结束后应 4492：%d", core.CloseCode())
		}
		<-done
	})

	t.Run("源发 GOAWAY：中继不再给它开流，删路由", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		core.SendGoaway()
		eventually(t, "路由已删", func() bool {
			r, _ := h.env.Control.Routes().Get(t.Context(), h.env.SourceID)
			return r == nil
		})
		if code, _, _ := h.channel(h.relayToken(nil)).outcome(); code != int(protocol.CloseSourceOffline) {
			t.Fatalf("应 4404：%d", code)
		}
	})
}

func TestMetrics(t *testing.T) {
	t.Run("/metrics 只在单独的监听上，可要求令牌", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("通道应开成")
		}
		if res, _ := http.Get(h.base() + "/metrics"); res.StatusCode != 404 {
			t.Fatalf("主监听上不该有 /metrics：%d", res.StatusCode)
		}
		ms := newMetricsServer(t, h.plane.Metrics.Render, "metrics-secret")
		if res, _ := http.Get(ms + "/metrics"); res.StatusCode != 401 {
			t.Fatalf("无令牌应 401：%d", res.StatusCode)
		}
		req, _ := http.NewRequest("GET", ms+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer metrics-secret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := io.ReadAll(res.Body)
		for _, want := range []string{"relay_tunnels 1", `relay_edge_connections_total{result="ok"} 1`, `relay_handshake_total{result="ok"} 1`} {
			if !strings.Contains(string(text), want) {
				t.Errorf("指标里缺 %q", want)
			}
		}
	})
}

func TestE2EChannel(t *testing.T) {
	t.Run("源报 e2e：OPEN kind e2e 不带头；随机字节双向 10 MB 原样回来；指标 kind=e2e；日志没有内容", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		tok := h.relayToken(relayenv.Claims{"dev": "dev_1"})
		conn, _, err := dial(t, h.e2eURL(), []string{"armadra-e2e.1", "armadra-relay." + tok})
		if err != nil {
			t.Fatal(err)
		}
		if conn.Subprotocol() != "armadra-e2e.1" {
			t.Fatalf("子协议 %q", conn.Subprotocol())
		}
		eventually(t, "源收到 OPEN", func() bool { return len(core.Opens()) == 1 })
		open := core.Opens()[0]
		if open.Kind != "e2e" || open.Method != "GET" || open.Path != "/_relay/e2e" || len(open.Headers) != 0 ||
			open.ClientOrigin != nil || open.RemoteIP != "127.0.0.1" || open.RelayNode != "relay-test" {
			t.Fatalf("%+v", open)
		}
		const total = 10 << 20
		sent, got := sha256.New(), sha256.New()
		done := make(chan int, 1)
		go func() {
			n := 0
			for n < total {
				_, b, err := conn.ReadMessage()
				if err != nil {
					break
				}
				got.Write(b)
				n += len(b)
			}
			done <- n
		}()
		// 中继可以合并或切分消息：只比字节流。
		seed := uint32(1)
		for off := 0; off < total; {
			size := min(total-off, 1+int(seed%70000))
			chunk := make([]byte, size)
			for i := range chunk {
				seed = (seed*1103515245 + 12345) & 0x7fffffff
				chunk[i] = byte(seed)
			}
			sent.Write(chunk)
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				t.Fatal(err)
			}
			off += size
		}
		select {
		case n := <-done:
			if n != total || fmt.Sprintf("%x", sent.Sum(nil)) != fmt.Sprintf("%x", got.Sum(nil)) {
				t.Fatalf("字节流不一致：收到 %d", n)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("10 MB 往返超时")
		}
		if !strings.Contains(h.plane.Metrics.Render(), `relay_edge_streams_total{kind="e2e"} 1`) {
			t.Fatal("指标里缺 kind=e2e")
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		_, _, _ = conn.ReadMessage()
		eventually(t, "流收尾", func() bool { return h.streams() == 0 })
		eventually(t, "结束日志", func() bool { return strings.Contains(h.logs.String(), "端到端通道结束") })
		var line string
		for _, l := range strings.Split(h.logs.String(), "\n") {
			if strings.Contains(l, "端到端通道结束") {
				line = l
			}
		}
		if !strings.Contains(line, `"kind":"e2e"`) || strings.Contains(line, "dev_1") || strings.Contains(line, "acct_test") {
			t.Fatalf("结束日志不对：%s", line)
		}
	})

	t.Run("拒绝：令牌缺 4401、src 不符 4403、缺版本子协议 4409、源没报 e2e 4404 e2e_unsupported、源离线 4404；HTTP 404", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		check := func(c *client, wantCode int, wantReason string) {
			t.Helper()
			info := c.wait(t)
			if info.code != wantCode || info.reason != wantReason {
				t.Fatalf("应 %d %q，实际 %+v", wantCode, wantReason, info)
			}
		}
		check(h.open("armadra-e2e.1", "armadra-relay."+h.relayToken(nil)), 4404, "source_offline")
		core := h.core(relayenv.CoreOptions{})
		check(h.open("armadra-e2e.1"), 4401, "relay_token_invalid")
		wrong := h.relayToken(relayenv.Claims{"src": "fedcba9876543210fedcba9876543210"})
		check(h.open("armadra-e2e.1", "armadra-relay."+wrong), 4403, "relay_source_mismatch")
		check(h.open("armadra-relay."+h.relayToken(nil), "armadra-ticket.t"), 4409, "protocol_unsupported")
		check(h.open("armadra-e2e.1", "armadra-relay."+h.relayToken(nil)), 4404, "e2e_unsupported")
		if len(core.Opens()) != 0 {
			t.Fatal("不该进隧道")
		}
		req, _ := http.NewRequest("GET", h.base()+h.sourcePath()+"/_relay/e2e", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 404 {
			t.Fatalf("普通 HTTP 应 404：%v %v", res, err)
		}
	})

	t.Run("文本消息以 4400 关闭；源下线以 4404 结束；客户端关闭后流收尾", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		core := h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		text := h.channel(h.relayToken(nil))
		if _, _, open := text.outcome(); !open {
			t.Fatal("通道应开成")
		}
		_ = text.c.WriteMessage(websocket.TextMessage, []byte("plain"))
		if info := text.wait(t); info.code != int(protocol.CloseBadFrame) {
			t.Fatalf("文本消息应 4400：%+v", info)
		}
		eventually(t, "流收尾", func() bool { return h.streams() == 0 })
		ws := h.channel(h.relayToken(nil))
		_ = ws.c.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3})
		select {
		case b := <-ws.msgs:
			if string(b) != "\x01\x02\x03" {
				t.Fatalf("%v", b)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没收到回显")
		}
		core.Terminate()
		if info := ws.wait(t); info.code != int(protocol.CloseSourceOffline) || info.reason != "source_offline" {
			t.Fatalf("%+v", info)
		}
	})

	t.Run("撤销源时 e2e 连接一起断开", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		c := h.channel(h.relayToken(nil))
		if _, _, open := c.outcome(); !open {
			t.Fatal("通道应开成")
		}
		if n := h.plane.Edge.Disconnect(edgeFilter(h.env.SourceID)); n != 1 {
			t.Fatalf("断开了 %d 条", n)
		}
		c.wait(t)
	})
}

func TestTraffic(t *testing.T) {
	t.Run("端到端通道记为 e2e：字节是边缘 WS 载荷之和，设备取令牌的 dev", func(t *testing.T) {
		h := newHarness(t, harnessOptions{traffic: true})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		c := h.channel(h.relayToken(relayenv.Claims{"sub": "acct:alice", "dev": "dev_1"}))
		got := 0
		for i := 0; i < 10; i++ {
			got += len(c.echo(t, make([]byte, 1000)))
		}
		_ = c.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
		c.wait(t)
		eventually(t, "流收尾", func() bool { return h.streams() == 0 })
		if err := h.traffic.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		rows := h.rows.all()
		if len(rows) != 1 || rows[0].Subject != "acct:alice" || rows[0].DeviceID != "dev_1" || rows[0].Kind != "e2e" ||
			rows[0].Connections != 1 || rows[0].BytesToSource != 10000 || rows[0].BytesToClient != 10000 {
			t.Fatalf("%+v", rows)
		}
	})

	t.Run("没开统计：通道照常、不记账", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		h.core(relayenv.CoreOptions{Capabilities: []string{"e2e"}})
		if _, _, open := h.channel(h.relayToken(nil)).outcome(); !open {
			t.Fatal("通道应开成")
		}
		if h.traffic != nil {
			t.Fatal("不该有计量")
		}
	})
}

func TestRouting(t *testing.T) {
	t.Run("/ready 逐项探测，失败答 503", func(t *testing.T) {
		ok := true
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) {
			o.Probes = map[string]obs.Probe{"store": func(context.Context) error {
				if !ok {
					return fmt.Errorf("down")
				}
				return nil
			}}
		}})
		res, _ := http.Get(h.base() + "/ready")
		if res.StatusCode != 200 {
			t.Fatalf("%d", res.StatusCode)
		}
		ok = false
		res, _ = http.Get(h.base() + "/ready")
		if res.StatusCode != 503 {
			t.Fatalf("%d", res.StatusCode)
		}
	})

	t.Run("其余路径 404；Fallback 认领的交给它", func(t *testing.T) {
		h := newHarness(t, harnessOptions{plane: func(o *dataplane.Options) {
			o.Fallback = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v1/x" {
					_, _ = w.Write([]byte("fallback"))
					return true
				}
				return false
			}
		}})
		if res, _ := http.Get(h.base() + "/nothing"); res.StatusCode != 404 {
			t.Fatal("应 404")
		}
		res, _ := http.Get(h.base() + "/v1/x")
		b, _ := io.ReadAll(res.Body)
		if string(b) != "fallback" {
			t.Fatalf("%q", b)
		}
	})

	t.Run("非源地址的 WS 升级（未知路径）答 404", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		_, res, err := websocket.DefaultDialer.Dial(relayenv.WSURL(h.base(), "/nope"), nil)
		if err == nil || res.StatusCode != 404 {
			t.Fatalf("%v %v", res, err)
		}
	})
}
