package saas

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

const download = 4 << 20

// stalledTo 经节点 n 开一条到 src 的通道但不读（慢客户端）。
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

// rawLink 以节点 b 的身份直连 a 的 /x/v1（扮演入口节点，自己掌握给不给信用）。
func (r *rig) rawLink(a *running) *websocket.Conn {
	r.t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(a.URL, "http://", "ws://", 1)+gen.PathInternode,
		http.Header{"Authorization": {"Bearer " + r.nodeToken("b", nil)}})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	// 读走持有节点发来的帧（丢弃）：不读的话内核缓冲塞满，持有节点的写协程卡住，测的就不是信用了。
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return conn
}

func writeFrame(t *testing.T, c *websocket.Conn, f protocol.Frame) {
	t.Helper()
	b, err := protocol.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
		t.Fatal(err)
	}
}

// 回归（审查 P0）：转来的流被丢掉时（对端 RST、链路断开），持有节点上挂着的下行字节要把信用还给源，
// 否则源的 8 MiB 隧道窗口被一点点漏光，源对所有客户端停发。用直连 /x/v1、不给信用的「入口节点」
// 让字节确定地挂在持有节点上（不受内核缓冲大小影响）；去掉修复，两种丢法各自在 32 次左右就会卡住。
func TestDroppedForwardedStreamsReturnCredit(t *testing.T) {
	for _, how := range []string{"rst", "link"} {
		t.Run(how, func(t *testing.T) {
			r := newRig(t)
			a := r.start("a", nil)
			b := r.start("b", nil)
			r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}, E2ESend: 64 << 20})
			r.online(a)
			link := r.rawLink(a)
			for i := 0; i < 64; i++ {
				id := uint32(2*i + 1)
				writeFrame(t, link, protocol.Frame{Type: protocol.FrameOpen, StreamID: id, Open: edge.InternodeOpen(r.env.SourceID, gen.OpenPayload{RelayNode: "b"})})
				eventually(t, "持有节点上挂满一个流窗", func() bool { return a.internode.pendingBytes() >= protocol.StreamWindow })
				if how == "rst" {
					writeFrame(t, link, protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: protocol.RstClientGone})
				} else {
					_ = link.Close()
				}
				eventually(t, "那条流收掉", func() bool { return a.internode.pendingBytes() == 0 })
				if how == "link" {
					link = r.rawLink(a)
				}
			}
			// 源照常出数据：经 b 的真客户端读一段。
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
		})
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
	// 要等内核缓冲都塞满、字节堆到持有节点上，链路才真正被压住（Linux 的回环缓冲比 macOS 大得多，
	// 可能要很久）；等不到就跳过——没压住链路的这一次证明不了什么。
	deadline := time.Now().Add(45 * time.Second)
	for a.internode.pendingBytes() < 8<<20 {
		if time.Now().After(deadline) {
			t.Skipf("内核缓冲吸收了慢客户端的字节，没压到链路（持有节点上挂着 %d 字节）", a.internode.pendingBytes())
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.fullDownload(b, fast, download)
}

// 链路的限制本身：链路级窗口不小于「流数上限 × 流窗」，链路级信用不会先于每流窗口耗尽（P1-2 的不变式，
// 不依赖内核缓冲）；流数上限大于单隧道的 256。
func TestLinkConfigInvariant(t *testing.T) {
	c := linkConfig()
	if c.TunnelWindow < c.MaxStreams*c.StreamWindow || c.MaxStreams != LinkMaxStreams || c.MaxStreams <= 256 {
		t.Fatalf("%+v", c)
	}
	// 一次补给对端的链路级信用（半个窗口）放得进 WINDOW 帧的 u32。
	if c.TunnelWindow/2 > 1<<32-1 {
		t.Fatalf("链路窗口太大：%d", c.TunnelWindow)
	}
}

// 审查 P1-4：节点令牌只在升级时验不够——撤销之后已建立的连接要断，到期也要断。
func TestLinkClosesWhenNodeTokenRevokedOrExpired(t *testing.T) {
	r := newRig(t)
	a := r.start("a", func(c *Config) { c.InternodeCheckEvery = 50 * time.Millisecond })
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

// 撤销复查间隔随节点配置走、创建后不再改：缺省 5 秒，Config 可缩短（原先是被测试改写的包级变量）。
func TestInternodeCheckEveryComesFromConfig(t *testing.T) {
	if s := newInternodeServer(nil, nil, obs.Nop(), &internodeMetrics{}, 0); s.checkEvery != defaultInternodeCheckEvery {
		t.Fatalf("缺省：%v", s.checkEvery)
	}
	if s := newInternodeServer(nil, nil, obs.Nop(), &internodeMetrics{}, 50*time.Millisecond); s.checkEvery != 50*time.Millisecond {
		t.Fatalf("配置：%v", s.checkEvery)
	}
}

// 审查 G5：源自己发来的 RST refused 不能原样转回入口节点（入口节点把链路上的 refused 当 e2e_unsupported），
// 改成 sourceGone；其余码原样。持有节点自己判的 refused（源没报 e2e）走 open，不经这里。
func TestForwardedResetMapsSourceRefused(t *testing.T) {
	cases := []struct{ in, want protocol.RstCode }{
		{protocol.RstRefused, protocol.RstSourceGone},
		{protocol.RstSourceGone, protocol.RstSourceGone},
		{protocol.RstInternal, protocol.RstInternal},
		{protocol.RstClientGone, protocol.RstClientGone},
	}
	for _, tc := range cases {
		c := &internodeConn{streams: map[uint32]*fwdStream{}}
		fs := &fwdStream{c: c, id: 7}
		c.streams[7] = fs
		fs.Reset(nil, tc.in)
		if len(c.ctl) != 1 {
			t.Fatalf("%v：写队列 %d 帧", tc.in, len(c.ctl))
		}
		f, err := protocol.DecodeView(c.ctl[0].frame)
		if err != nil || f.Type != protocol.FrameRst || f.StreamID != 7 || f.Code != tc.want {
			t.Fatalf("%v → 想要 %v，实际 %+v（%v）", tc.in, tc.want, f, err)
		}
		if len(c.streams) != 0 {
			t.Fatalf("流没摘掉")
		}
	}
}

// 节点令牌撤销即时生效：定时复查关到 1 小时，仍在撤销广播到达时立刻断开已建立的连接
// （本节点写的撤销、别的节点经 relay:revoked 广播来的撤销各一次）。
func TestLinkClosesOnRevokedBroadcastWithoutPolling(t *testing.T) {
	r := newRig(t)
	a := r.start("a", func(c *Config) { c.InternodeCheckEvery = time.Hour })
	jtiLocal, jtiRemote := "n.b."+randomHex(6), "n.c."+randomHex(6)
	b := r.start("b", func(c *Config) { c.NodeToken = r.nodeToken("b", relayenv.Claims{"jti": jtiLocal}) })
	c := r.start("c", func(c *Config) { c.NodeToken = r.nodeToken("c", relayenv.Claims{"jti": jtiRemote}) })
	other := r.start("d", nil)
	r.env.StartCore(t, a.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	r.online(a)
	viaB := r.channel(b, r.relayToken(nil))
	viaC := r.channel(c, r.relayToken(nil))
	if viaB.outcome() != 0 || viaC.outcome() != 0 {
		t.Fatal("转发的通道应开成")
	}
	start := time.Now()
	ctx := context.Background()
	if err := a.Plane.Auth.Control.Tokens().Revoke(ctx, jtiLocal, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := other.Plane.Auth.Control.Tokens().Revoke(ctx, jtiRemote, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code := viaB.wait(t); code != 4404 {
		t.Fatalf("本节点撤销之后：%d", code)
	}
	if code := viaC.wait(t); code != 4404 {
		t.Fatalf("别的节点广播撤销之后：%d", code)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("撤销生效用了 %v，应远小于复查间隔", d)
	}
}

// 持有节点往入口节点写的下行不是先进先出（#57）：一条流积压再多，别的流的小帧与控制帧也在
// 下一批最前面写出，等待的字节数有上界（预算 + 一块），与积压的流数无关；流内保持顺序；关闭时只执行 after。
func TestInternodeWriterIsFairAcrossStreams(t *testing.T) {
	c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1)}
	data := func(id uint32, n int, tag byte) []byte {
		p := make([]byte, n)
		p[0] = tag
		return encode(protocol.Frame{Type: protocol.FrameData, StreamID: id, Data: p})
	}
	var all []outItem
	step := func(everything bool) []outItem {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, _, _ := c.collectLocked(everything)
		all = append(all, b...)
		return b
	}
	afters := 0
	bulk := make([]*fwdStream, 24)
	c.mu.Lock()
	for i := range bulk {
		bulk[i] = &fwdStream{c: c, id: uint32(i + 1)}
		for k := 0; k < 8; k++ {
			c.sendStreamLocked(bulk[i], data(bulk[i].id, protocol.MaxDataChunk, byte(k)), func() { afters++ })
		}
	}
	c.mu.Unlock()
	// 先写几批，让大流量流进入稳态（都是老流）。
	for i := 0; i < 5; i++ {
		step(false)
	}
	probe := &fwdStream{c: c, id: 100}
	c.mu.Lock()
	c.sendStreamLocked(probe, data(100, 1024, 0), nil)
	c.sendLocked(encode(protocol.Frame{Type: protocol.FramePong, Ts: 1}), nil)
	c.mu.Unlock()
	batch := step(false)
	ahead, pong, seenProbe := 0, -1, false
	for i, it := range batch {
		f, err := protocol.DecodeView(it.frame)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == protocol.FramePong {
			pong = i
		}
		if f.StreamID == 100 {
			seenProbe = true
			break
		}
		ahead += dataBytes(it.frame)
	}
	if pong != 0 {
		t.Fatalf("控制帧应在批首：下标 %d", pong)
	}
	if !seenProbe || ahead > tunnel.WriteBudget+protocol.MaxDataChunk {
		t.Fatalf("探测帧应在下一批里、前面至多一个预算：seen=%v ahead=%d", seenProbe, ahead)
	}
	// 取尽：一帧不丢，流内顺序不变，after 全部可执行。
	step(true)
	next := map[uint32]byte{}
	for _, it := range all {
		f, err := protocol.DecodeView(it.frame)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type != protocol.FrameData || f.StreamID == 100 {
			continue
		}
		if f.Data[0] != next[f.StreamID] {
			t.Fatalf("流 %d 乱序：第 %d 帧的标记是 %d", f.StreamID, next[f.StreamID], f.Data[0])
		}
		next[f.StreamID]++
		if it.after != nil {
			it.after()
		}
	}
	for _, fs := range bulk {
		if next[fs.id] != 8 {
			t.Fatalf("流 %d 丢帧：%d / 8", fs.id, next[fs.id])
		}
	}
	if afters != 24*8 {
		t.Fatalf("after %d / %d", afters, 24*8)
	}
}

// 源发 RST（Reset）或对端发 RST 时，该流已排进出队列的 DATA / END 作废：RST 之后对端收不到它们，
// 它们占的信用（after）全部归还本地隧道。
func TestResetDropsQueuedOutAndReturnsCredit(t *testing.T) {
	for _, how := range []string{"source-reset", "peer-rst"} {
		c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1)}
		fs := &fwdStream{c: c, id: 9}
		other := &fwdStream{c: c, id: 10}
		c.streams[9], c.streams[10] = fs, other
		returned := 0
		c.mu.Lock()
		for i := 0; i < 5; i++ {
			c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameData, StreamID: 9, Data: make([]byte, 100)}), func() { returned += 100 })
		}
		c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: 9}), nil)
		c.sendStreamLocked(other, encode(protocol.Frame{Type: protocol.FrameData, StreamID: 10, Data: make([]byte, 7)}), nil)
		c.mu.Unlock()
		if how == "source-reset" {
			fs.Reset(nil, protocol.RstInternal)
		} else {
			c.receive(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: 9, Code: protocol.RstClientGone}))
		}
		if returned != 500 {
			t.Fatalf("%s：归还 %d，应为 500", how, returned)
		}
		c.mu.Lock()
		batch, _, _ := c.collectLocked(true)
		c.mu.Unlock()
		for _, it := range batch {
			f, err := protocol.DecodeView(it.frame)
			if err != nil {
				t.Fatal(err)
			}
			if f.StreamID == 9 && f.Type != protocol.FrameRst {
				t.Fatalf("%s：RST 之后又写出该流的 %v", how, f.Type)
			}
		}
		if fs.outN != 0 {
			t.Fatalf("%s：outN=%d", how, fs.outN)
		}
	}
}

// 大量 1 字节小帧排队时入队不再是平方量级（出队列字节数随入队 / 出队增减，不遍历）。
func TestEnqueueManyTinyFramesIsNotQuadratic(t *testing.T) {
	c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1)}
	fs := &fwdStream{c: c, id: 1}
	frame := encode(protocol.Frame{Type: protocol.FrameData, StreamID: 1, Data: []byte{1}})
	start := time.Now()
	c.mu.Lock()
	for i := 0; i < 300_000; i++ {
		c.sendStreamLocked(fs, frame, nil)
	}
	c.mu.Unlock()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("30 万个 1 字节帧入队用了 %v，疑似平方", d)
	}
}

// 大量稀疏流同时就绪：一批里稀疏流的帧受预算限制（至多预算 + 一块），老流仍每批至少一帧。
func TestInternodeManySparseStreamsRespectBudget(t *testing.T) {
	c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1)}
	old := &fwdStream{c: c, id: 1}
	c.mu.Lock()
	for k := 0; k < 50; k++ {
		c.sendStreamLocked(old, encode(protocol.Frame{Type: protocol.FrameData, StreamID: 1, Data: make([]byte, protocol.MaxDataChunk)}), nil)
	}
	c.mu.Unlock()
	for i := 0; i < 3; i++ { // 批次号前进，让后面的流算稀疏流
		c.mu.Lock()
		_, _, _ = c.collectLocked(false)
		c.mu.Unlock()
	}
	c.mu.Lock()
	for i := 0; i < 500; i++ {
		fs := &fwdStream{c: c, id: uint32(100 + i)}
		c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameData, StreamID: fs.id, Data: make([]byte, 8<<10)}), nil)
	}
	batch, _, _ := c.collectLocked(false)
	c.mu.Unlock()
	size, olds := 0, 0
	for _, it := range batch {
		size += dataBytes(it.frame)
		if f, _ := protocol.DecodeView(it.frame); f.StreamID == 1 {
			olds++
		}
	}
	if size > tunnel.WriteBudget+protocol.MaxDataChunk {
		t.Fatalf("一批 %d 字节，超过预算 + 一块", size)
	}
	if olds != 1 {
		t.Fatalf("老流这一批应恰好出一帧：%d", olds)
	}
}

// #134：转来的流被重置（源 RST 或对端 RST）时，出队列里已扣链路信用、还没写出的 DATA 要把链路信用还回来，
// 对端永远收不到这些字节、也不会为它们补 WINDOW(0)；还回之后因链路信用挂着的别的流接着发。
func TestResetReturnsQueuedLinkCredit(t *testing.T) {
	for _, how := range []string{"source-reset", "peer-rst"} {
		t.Run(how, func(t *testing.T) {
			const window = 3000
			tr := &nullTransport{}
			tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", tr, tunnel.DefaultConfig(), tunnel.Hooks{})
			t.Cleanup(func() { tun.Close(protocol.CloseNormal, "cleanup") })
			c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1), linkCredit: window}
			open := func(id uint32) *fwdStream {
				fs := &fwdStream{c: c, id: id, credit: protocol.StreamWindow, recvLeft: protocol.StreamWindow}
				s, err := tun.OpenStream(&gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}}, fs, nil)
				if err != nil {
					t.Fatal(err)
				}
				fs.s = s
				c.streams[id] = fs
				return fs
			}
			for round := 0; round < 4; round++ {
				a, b := open(uint32(4*round+1)), open(uint32(4*round+3))
				a.Data(a.s, make([]byte, window)) // 扣光链路信用，全部排进 a 的出队列
				b.Data(b.s, make([]byte, 500))    // 链路信用不够，挂着
				c.mu.Lock()
				credit, bPending := c.linkCredit, b.pendingLen()
				c.mu.Unlock()
				if credit != 0 || bPending != 500 {
					t.Fatalf("第 %d 轮：链路信用 %d、b 挂着 %d", round, credit, bPending)
				}
				if how == "source-reset" {
					a.Reset(a.s, protocol.RstInternal)
				} else {
					c.receive(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: a.id, Code: protocol.RstClientGone}))
				}
				c.mu.Lock()
				credit, bPending, bOut := c.linkCredit, b.pendingLen(), b.outN
				c.mu.Unlock()
				if bPending != 0 || bOut != 500 {
					t.Fatalf("第 %d 轮：a 还回链路信用后 b 应排进出队列：挂着 %d、出队列 %d", round, bPending, bOut)
				}
				if credit != window-500 {
					t.Fatalf("第 %d 轮：链路信用应为 %d，实际 %d", round, window-500, credit)
				}
				// b 的字节写出、对端补 WINDOW(0)；收掉 b，链路信用回到初始窗口。
				c.mu.Lock()
				written, _, _ := c.collectLocked(true)
				c.linkCredit += 500
				credit = c.linkCredit
				c.mu.Unlock()
				for _, it := range written {
					if it.after != nil {
						it.after()
					}
				}
				b.Reset(b.s, protocol.RstInternal)
				if credit != window {
					t.Fatalf("第 %d 轮：链路信用应回到 %d，实际 %d", round, window, credit)
				}
			}
		})
	}
}

// nullTransport 丢弃隧道写出的帧（只为拿到真实的 tunnel.Stream）。
type nullTransport struct{}

func (nullTransport) Send([]byte) error                { return nil }
func (nullTransport) Close(protocol.CloseCode, string) {}
func (nullTransport) Terminate()                       {}
