package saas

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

const download = 4 << 20

// stalled 经节点 n 开一条通道但不读（慢客户端 / 中途断开的客户端）。
func (r *rig) stalled(n *running) *websocket.Conn {
	r.t.Helper()
	return r.stalledTo(n, r.env)
}

func (r *rig) stalledTo(n *running, src *relayenv.Env) *websocket.Conn {
	r.t.Helper()
	header := http.Header{"Host": {src.SourceID + ".src." + domain}}
	d := websocket.Dialer{Subprotocols: []string{gen.E2ESubprotocol, gen.SubprotocolRelay + src.RelayToken(r.t, nil)}, HandshakeTimeout: 5 * time.Second}
	c, _, err := d.Dial(strings.Replace(n.URL, "http://", "ws://", 1)+gen.E2EPath, header)
	if err != nil {
		r.t.Fatalf("拨号失败：%v", err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

// fullDownload 经节点 n 开一条到 src 的通道，读完源写的 total 字节（确定序列）。
func (r *rig) fullDownload(n *running, src *relayenv.Env, total int) {
	r.t.Helper()
	c := r.channelTo(n, src.SourceID, src.RelayToken(r.t, nil))
	got := 0
	deadline := time.After(20 * time.Second)
	for got < total {
		select {
		case b := <-c.msgs:
			for i, v := range b {
				if v != relayenv.BigByte(got+i) {
					r.t.Fatalf("第 %d 字节不对", got+i)
				}
			}
			got += len(b)
		case code := <-c.closed:
			if got < total {
				r.t.Fatalf("下载途中通道关闭：%d，收到 %d", code, got)
			}
		case <-deadline:
			r.t.Fatalf("下载卡住：收到 %d / %d", got, total)
		}
	}
}

// 回归（审查 P0）：客户端在下载途中断开很多次之后，源仍能给别的客户端出数据——持有节点丢掉挂着的
// 下行字节时要把源的隧道级信用还回去，否则 8 MiB 窗口漏光、源对所有客户端停发。
func TestMidDownloadDisconnectsDoNotLeakCredit(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	// 每条下载远大于内核缓冲：客户端不读时字节一路堆到持有节点。
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}, E2ESend: 64 << 20})
	r.online(a)
	for i := 0; i < 64; i++ {
		c := r.stalled(b)
		// 等下行字节堆满内核缓冲与入口节点的流窗，挂到持有节点上，再掉线（入口节点 RST 这条流）。
		eventually(t, "持有节点上有挂着的字节", func() bool { return a.internode.pendingBytes() >= 192<<10 })
		_ = c.NetConn().Close()
		eventually(t, "那条流收掉", func() bool { return a.internode.pendingBytes() == 0 })
	}
	// 新的客户端读一段：源还在出数据。
	c := r.channel(b, r.relayToken(nil))
	got := 0
	deadline := time.After(10 * time.Second)
	for got < download {
		select {
		case m := <-c.msgs:
			got += len(m)
		case <-deadline:
			t.Fatalf("源停发了：收到 %d", got)
		}
	}
}

// 回归（审查 P0）：节点间连接在下载途中断开很多次（持有节点那一侧丢掉全部挂着的下行字节），
// 源仍能出数据。去掉「丢 pending 时还源的信用」的修复，这个测试在第 40 次左右就会卡住。
func TestLinkDropsMidDownloadDoNotLeakCredit(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}, E2ESend: 64 << 20})
	r.online(a)
	for i := 0; i < 64; i++ {
		c := r.stalled(b)
		eventually(t, "持有节点上有挂着的字节", func() bool { return a.internode.pendingBytes() >= 192<<10 })
		a.internode.dropConns()
		eventually(t, "那条流收掉", func() bool { return a.internode.pendingBytes() == 0 })
		_ = c.Close()
	}
	c := r.channel(b, r.relayToken(nil))
	got := 0
	deadline := time.After(10 * time.Second)
	for got < download {
		select {
		case m := <-c.msgs:
			got += len(m)
		case <-deadline:
			t.Fatalf("源停发了：收到 %d", got)
		}
	}
}

// 回归（审查 P1-2）：同一条节点间连接上几十个不读的慢客户端（分在两个源上，合计远超 8 MiB）不拖住
// 经这条连接的别的源（链路级窗口不是瓶颈，跨租户没有队头阻塞）。同一个源自己的隧道窗口被它的慢客户端
// 占满是协议本身的限制，与转发无关，这里不测。
func TestSlowClientsDoNotBlockTheLink(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	slow1, slow2, fast := r.source(), r.source(), r.source()
	for _, src := range []*relayenv.Env{slow1, slow2, fast} {
		src.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}, E2ESend: 64 << 20})
		eventually(t, "源上线", func() bool {
			route, _ := a.Plane.Auth.Control.Routes().Get(context.Background(), src.SourceID)
			return route != nil && route.Node == "a"
		})
	}
	for i := 0; i < 24; i++ {
		r.stalledTo(b, slow1)
		r.stalledTo(b, slow2)
	}
	eventually(t, "慢客户端把字节堆在两端", func() bool { return a.internode.pendingBytes() >= 8<<20 })
	r.fullDownload(b, fast, download)
}

// 审查 P1-4：节点令牌只在升级时验不够——撤销之后已建立的连接要断，到期也要断。
func TestLinkClosesWhenNodeTokenRevokedOrExpired(t *testing.T) {
	old := internodeCheckEvery
	internodeCheckEvery = 50 * time.Millisecond
	t.Cleanup(func() { internodeCheckEvery = old })
	r := newRig(t)
	a := r.start("a", nil)
	jti := "n.b." + randomHex(6)
	b := r.start("b", func(c *Config) { c.NodeToken = r.nodeToken("b", relayenv.Claims{"jti": jti}) })
	now := time.Now().Unix()
	short := r.start("c", func(c *Config) {
		c.NodeToken = r.nodeToken("c", relayenv.Claims{"iat": now, "exp": now + 2})
	})
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)

	viaB := r.channel(b, r.relayToken(nil))
	viaC := r.channel(short, r.relayToken(nil))
	if viaB.outcome() != 0 || viaC.outcome() != 0 {
		t.Fatal("转发的通道应开成")
	}
	// 撤销 b 的节点令牌：a 上已建立的那条连接断开，经它转发的通道 4404；b 再拨号被拒，新通道也 4404。
	if err := a.Plane.Auth.Control.Tokens().Revoke(context.Background(), jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code := viaB.wait(t); code != 4404 {
		t.Fatalf("撤销之后：%d", code)
	}
	time.Sleep(linkRetry + 100*time.Millisecond)
	if code := r.channel(b, r.relayToken(nil)).outcome(); code != 4404 {
		t.Fatalf("撤销之后重新拨号应被拒：%d", code)
	}
	// c 的令牌 2 秒后到期：连接被断开。
	if code := viaC.wait(t); code != 4404 {
		t.Fatalf("到期之后：%d", code)
	}
	if !strings.Contains(a.logs.String(), "节点令牌已撤销") || !strings.Contains(a.logs.String(), "节点令牌到期") {
		t.Fatal(a.logs.String())
	}
}

// 审查 P2-6：远端的源没报 e2e 时，与本地一样以 4404 e2e_unsupported 结束。
func TestForwardedSourceWithoutE2E(t *testing.T) {
	r := newRig(t)
	a := r.start("a", nil)
	b := r.start("b", nil)
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{})
	r.online(a)
	reasons := map[string]string{}
	for name, n := range map[string]*running{"local": a, "forwarded": b} {
		header := http.Header{"Host": {r.env.SourceID + ".src." + domain}}
		d := websocket.Dialer{Subprotocols: []string{gen.E2ESubprotocol, gen.SubprotocolRelay + r.relayToken(nil)}}
		c, _, err := d.Dial(strings.Replace(n.URL, "http://", "ws://", 1)+gen.E2EPath, header)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = c.ReadMessage()
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != 4404 {
			t.Fatalf("%s：%v", name, err)
		}
		reasons[name] = ce.Text
		_ = c.Close()
	}
	if reasons["local"] != "e2e_unsupported" || reasons["forwarded"] != "e2e_unsupported" {
		t.Fatalf("%v", reasons)
	}
}

// 审查 P2-5：节点表里对端的 /x/v1 地址不是 wss（又没开 RELAY_ALLOW_INSECURE_TUNNEL）就不连、不送节点令牌。
func TestLinkRefusesInsecurePeerURL(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.admin.HSet(ctx, r.prefix+"nodes", "peer", `{"url":"wss://peer.example.test/t/v1","weight":1,"version":"x","draining":false,"connections":0,"tunnels":0,"internodeUrl":"ws://127.0.0.1:1/x/v1"}`).Err(); err != nil {
		t.Fatal(err)
	}
	l := newLinks("self", "token", false, r.admin, r.prefix, obs.Nop(), &internodeMetrics{})
	if _, err := l.connect("peer"); err == nil || !strings.Contains(err.Error(), "wss") {
		t.Fatalf("%v", err)
	}
}
