package tunnel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 对应 packages/relay-kernel/src/tunnel/hub.test.ts 与 packages/relay-runtime-node 的隧道端到端用例。

type fakeMsg struct {
	binary bool
	data   []byte
}

type fakeCloseErr struct{ code int }

func (e fakeCloseErr) Error() string  { return "closed" }
func (e fakeCloseErr) CloseCode() int { return e.code }

// fakeSocket 是内存里的 Socket：测试往 in 里放消息，读出 hub 写回来的文本与二进制。
type fakeSocket struct {
	fakeTransport
	in      chan fakeMsg
	tmu     sync.Mutex
	texts   [][]byte
	done    chan struct{}
	once    sync.Once
	closeCh chan struct{}
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{in: make(chan fakeMsg, 16), done: make(chan struct{}), closeCh: make(chan struct{})}
}

func (s *fakeSocket) ReadMessage() (bool, []byte, error) {
	select {
	case m := <-s.in:
		return m.binary, m.data, nil
	case <-s.done:
		return false, nil, fakeCloseErr{code: 1001}
	}
}

func (s *fakeSocket) SendText(b []byte) error {
	s.tmu.Lock()
	s.texts = append(s.texts, bytes.Clone(b))
	s.tmu.Unlock()
	return nil
}

func (s *fakeSocket) Close(code protocol.CloseCode, reason string) {
	s.fakeTransport.Close(code, reason)
	s.hangup()
}

func (s *fakeSocket) Terminate() {
	s.fakeTransport.Terminate()
	s.hangup()
}

func (s *fakeSocket) hangup() { s.once.Do(func() { close(s.done) }) }

func (s *fakeSocket) textCount() int {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	return len(s.texts)
}

func (s *fakeSocket) text(i int) map[string]any {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(s.texts[i], &m)
	return m
}

type hubRig struct {
	env     *relayenv.Env
	hub     *tunnel.Hub
	metrics *obs.Metrics
	events  chan control.PresenceEvent
	fx      handshakeFixture
}

func newHubRig(t *testing.T, over func(*tunnel.HubOptions)) *hubRig {
	t.Helper()
	var fx handshakeFixture
	testutil.FixtureJSON(t, "tunnel/handshake.json", &fx)
	env := relayenv.New(t, relayenv.Options{FixedNowMs: testutil.FixedNow})
	r := &hubRig{env: env, events: make(chan control.PresenceEvent, 16), fx: fx}
	env.Control.Presence().Subscribe(func(e control.PresenceEvent) { r.events <- e })
	var hub *tunnel.Hub
	r.metrics = obs.NewMetrics(func() (int, int) { return hub.Counts() })
	o := tunnel.HubOptions{
		Control: env.Control, Auth: env.Auth, NodeID: fx.Challenge.RelayNode,
		Config: testConfig(nil), HandshakeTimeout: 10 * time.Second,
		Log: obs.Nop(), Metrics: r.metrics, Nonce: func() string { return fx.Challenge.Nonce2 },
	}
	if over != nil {
		over(&o)
	}
	hub = tunnel.NewHub(o)
	r.hub = hub
	return r
}

// serve 在协程里跑 Serve，返回结束信号。
func (r *hubRig) serve(s *fakeSocket) chan struct{} {
	done := make(chan struct{})
	go func() { r.hub.Serve(s); close(done) }()
	return done
}

func (s *fakeSocket) handshake(t *testing.T, fx handshakeFixture) {
	t.Helper()
	s.in <- fakeMsg{false, fx.Hello}
	eventually(t, "challenge", func() bool { return s.textCount() >= 1 })
	s.in <- fakeMsg{false, fx.Auth}
	eventually(t, "ready", func() bool { return s.textCount() >= 2 })
}

func (r *hubRig) event(t *testing.T) control.PresenceEvent {
	t.Helper()
	select {
	case e := <-r.events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("等不到在线事件")
		return control.PresenceEvent{}
	}
}

func TestHub(t *testing.T) {
	t.Run("握手 → ready：写路由、发 sourceOnline；二进制帧交给隧道；断开删路由、发 sourceOffline", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		done := r.serve(sock)
		sock.handshake(t, r.fx)
		var challenge gen.Challenge
		_ = json.Unmarshal(sock.texts[0], &challenge)
		if challenge != r.fx.Challenge {
			t.Fatalf("%+v", challenge)
		}
		ready := sock.text(1)
		if ready["type"] != "ready" || ready["heartbeatMs"] != float64(time.Hour.Milliseconds()) {
			t.Fatalf("%+v", ready)
		}
		if n, _ := r.hub.Counts(); n != 1 {
			t.Fatalf("隧道数 %d", n)
		}
		ctx := context.Background()
		route, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID)
		if route == nil || route.Node != r.fx.Challenge.RelayNode {
			t.Fatalf("%+v", route)
		}
		tun, err := r.hub.Lookup(ctx, r.env.SourceID)
		if err != nil || tun == nil || tun.ID() != route.TunnelID {
			t.Fatalf("Lookup %v %v", tun, err)
		}
		if e := r.event(t); e.Type != control.PresenceSourceOnline {
			t.Fatalf("%+v", e)
		}

		ping, _ := protocol.Encode(protocol.Frame{Type: protocol.FramePing, Ts: 7})
		sock.in <- fakeMsg{true, ping}
		eventually(t, "pong", func() bool { return sock.count() >= 1 })
		if got := sock.take(); got[0].Type != protocol.FramePong || got[0].Ts != 7 {
			t.Fatalf("%+v", got)
		}

		sock.hangup()
		<-done
		eventually(t, "路由已删", func() bool {
			rt, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID)
			return rt == nil
		})
		if e := r.event(t); e.Type != control.PresenceSourceOffline {
			t.Fatalf("%+v", e)
		}
		if n, _ := r.hub.Counts(); n != 0 {
			t.Fatalf("隧道数 %d", n)
		}
		snap := r.metrics.Snapshot()
		if snap.Handshakes["ok"] != 1 {
			t.Fatalf("%+v", snap.Handshakes)
		}
	})

	t.Run("ready 之前发二进制：关 4400，不发 reject", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		done := r.serve(sock)
		sock.in <- fakeMsg{true, []byte{1, 2, 3}}
		<-done
		if c := sock.closedWith(); c == nil || c.Code != protocol.CloseBadFrame || c.Reason != "binary_before_ready" {
			t.Fatalf("%+v", c)
		}
		if sock.textCount() != 0 {
			t.Fatal("不该发任何文本")
		}
		if r.metrics.Snapshot().Handshakes["binary_before_ready"] != 1 {
			t.Fatal("应记一次 binary_before_ready")
		}
	})

	t.Run("关闭后不再收新隧道", func(t *testing.T) {
		r := newHubRig(t, nil)
		if !r.hub.Accepting() {
			t.Fatal("应在收")
		}
		r.hub.Shutdown(0)
		if r.hub.Accepting() {
			t.Fatal("关闭后不该再收")
		}
	})

	t.Run("握手超时：关 4400 handshake_timeout", func(t *testing.T) {
		r := newHubRig(t, func(o *tunnel.HubOptions) { o.HandshakeTimeout = 50 * time.Millisecond })
		sock := newFakeSocket()
		done := r.serve(sock)
		<-done
		if c := sock.closedWith(); c == nil || c.Code != protocol.CloseBadFrame || c.Reason != "handshake_timeout" {
			t.Fatalf("%+v", c)
		}
		if r.metrics.Snapshot().Handshakes["timeout"] != 1 {
			t.Fatal("应记一次 timeout")
		}
	})

	t.Run("握手被拒：发 reject 再按码关闭，并计入指标", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		done := r.serve(sock)
		sock.in <- fakeMsg{false, []byte(`{"type":"hello","protocol":{"major":2,"minor":0},"sourceId":"0123456789abcdef0123456789abcdef","coreVersion":"1","capabilities":[],"tunnelToken":"x","nonce":"oaGhoaGhoaGhoaGhoaGhoQ"}`)}
		<-done
		if sock.textCount() != 1 || sock.text(0)["code"] != "protocol_unsupported" {
			t.Fatalf("%v", sock.texts)
		}
		if c := sock.closedWith(); c == nil || c.Code != protocol.CloseProtocolUnsupported {
			t.Fatalf("%+v", c)
		}
		if r.metrics.Snapshot().Handshakes["protocol_unsupported"] != 1 {
			t.Fatal("指标")
		}
	})

	t.Run("同源替换：新隧道顶掉旧的，旧的收 GOAWAY replaced 并以 4491 关闭，路由指向新的", func(t *testing.T) {
		r := newHubRig(t, nil)
		first, second := newFakeSocket(), newFakeSocket()
		r.serve(first)
		first.handshake(t, r.fx)
		ctx := context.Background()
		old, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID)
		r.serve(second)
		second.handshake(t, r.fx)
		eventually(t, "旧隧道关闭", func() bool { return first.closedWith() != nil })
		if c := first.closedWith(); c.Code != protocol.CloseTunnelReplaced {
			t.Fatalf("%+v", c)
		}
		got := first.take()
		if len(got) == 0 || got[0].Type != protocol.FrameGoaway || got[0].Goaway.Reason != "replaced" {
			t.Fatalf("%+v", got)
		}
		cur, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID)
		if cur == nil || cur.TunnelID == old.TunnelID {
			t.Fatalf("路由应指向新隧道：%+v %+v", old, cur)
		}
		tun, _ := r.hub.Lookup(ctx, r.env.SourceID)
		if tun == nil || tun.ID() != cur.TunnelID {
			t.Fatal("Lookup 应是新隧道")
		}
		// 旧隧道关闭不得带走新路由，也不发下线事件。
		time.Sleep(50 * time.Millisecond)
		if rt, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID); rt == nil {
			t.Fatal("新路由被旧隧道删掉了")
		}
		if n, _ := r.hub.Counts(); n != 1 {
			t.Fatalf("隧道数 %d", n)
		}
	})

	t.Run("节点命令：kick 发 GOAWAY replaced，closeTunnel 以 4492 关闭", func(t *testing.T) {
		r := newHubRig(t, nil)
		a := newFakeSocket()
		r.serve(a)
		a.handshake(t, r.fx)
		r.env.Control.Command(control.NodeCommand{Type: control.CommandKick, SourceID: r.env.SourceID, Reason: "test"})
		eventually(t, "kick 关闭", func() bool { return a.closedWith() != nil })
		if a.closedWith().Code != protocol.CloseTunnelReplaced {
			t.Fatalf("%+v", a.closedWith())
		}

		r2 := newHubRig(t, nil)
		b := newFakeSocket()
		r2.serve(b)
		b.handshake(t, r2.fx)
		r2.env.Control.Command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: r2.env.SourceID})
		eventually(t, "closeTunnel 关闭", func() bool { return b.closedWith() != nil })
		if c := b.closedWith(); c.Code != protocol.CloseTunnelGoaway || c.Reason != "closed_by_control" {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("ready 之后收到文本：关 4400 text_after_ready", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		r.serve(sock)
		sock.handshake(t, r.fx)
		sock.in <- fakeMsg{false, []byte("hi")}
		eventually(t, "关闭", func() bool { return sock.closedWith() != nil })
		if c := sock.closedWith(); c.Code != protocol.CloseBadFrame || c.Reason != "text_after_ready" {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("Lookup：路由指向别的节点时答空", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		r.serve(sock)
		sock.handshake(t, r.fx)
		ctx := context.Background()
		route, _ := r.env.Control.Routes().Get(ctx, r.env.SourceID)
		route.Node = "other-node"
		_ = r.env.Control.Routes().Set(ctx, r.env.SourceID, *route, time.Minute)
		if tun, _ := r.hub.Lookup(ctx, r.env.SourceID); tun != nil {
			t.Fatal("路由不在本节点，不该返回隧道")
		}
	})

	t.Run("Shutdown：给所有隧道发 GOAWAY shutdown，排空后以 4492 关闭", func(t *testing.T) {
		r := newHubRig(t, nil)
		sock := newFakeSocket()
		r.serve(sock)
		sock.handshake(t, r.fx)
		r.hub.Shutdown(time.Second)
		if c := sock.closedWith(); c == nil || c.Code != protocol.CloseTunnelGoaway {
			t.Fatalf("%+v", c)
		}
		got := sock.take()
		if len(got) == 0 || got[0].Type != protocol.FrameGoaway || got[0].Goaway.Reason != "shutdown" {
			t.Fatalf("%+v", got)
		}
	})
}

// ------------------------------------------------------------ 走真连接

func realServer(t *testing.T, r *hubRig) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(gen.PathTunnel, r.hub.HandleUpgrade)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHubRealConnection(t *testing.T) {
	t.Run("真连接：四步握手后开流，帧原样往返", func(t *testing.T) {
		r := newHubRig(t, func(o *tunnel.HubOptions) { o.Nonce = nil })
		r.env.SetNow(time.Now())
		base := realServer(t, r)
		core := r.env.DialCore(t, base, relayenv.HelloOptions{Capabilities: []string{"e2e"}})
		if core.Ready.Limits.StreamWindow != protocol.StreamWindow || core.Ready.Protocol.Major != gen.ProtocolMajor {
			t.Fatalf("%+v", core.Ready)
		}
		ctx := context.Background()
		tun, err := r.hub.Lookup(ctx, r.env.SourceID)
		if err != nil || tun == nil || !tun.HasCapability("e2e") {
			t.Fatalf("Lookup %v %v", tun, err)
		}
		sink := &recSink{autoConsume: true}
		s, err := tun.OpenStream(openPayload(), sink, nil)
		if err != nil {
			t.Fatal(err)
		}
		open := core.Read()
		if open.Type != protocol.FrameOpen || open.StreamID != s.ID() || open.Open.Kind != "e2e" {
			t.Fatalf("%+v", open)
		}
		s.Send([]byte("hello"))
		s.End()
		if d := core.Read(); d.Type != protocol.FrameData || string(d.Data) != "hello" {
			t.Fatalf("%+v", d)
		}
		if e := core.Read(); e.Type != protocol.FrameEnd {
			t.Fatalf("%+v", e)
		}
		core.Write(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: []byte("world")})
		core.Write(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()})
		eventually(t, "收到回显", func() bool {
			d, ended, _, _ := sink.snapshot()
			return ended && string(d) == "world"
		})
		if r.tunCount() != 1 {
			t.Fatal("隧道应仍在")
		}
	})

	t.Run("真连接：夹具序列 sequence-http-roundtrip.bin 逐帧一致", func(t *testing.T) {
		r := newHubRig(t, func(o *tunnel.HubOptions) { o.Nonce = nil })
		r.env.SetNow(time.Now())
		core := r.env.DialCore(t, realServer(t, r), relayenv.HelloOptions{})
		seq := splitSequence(t, fixtureBytes(t, "tunnel/sequence-http-roundtrip.bin"))
		first, _ := protocol.Decode(seq[0])
		tun, _ := r.hub.Lookup(context.Background(), r.env.SourceID)
		sink := &recSink{autoConsume: true}
		s, err := tun.OpenStream(first.Open, sink, nil)
		if err != nil || s.ID() != first.StreamID {
			t.Fatalf("%v %v", s, err)
		}
		for _, f := range []protocol.Frame{mustDecode(t, seq[1]), mustDecode(t, seq[2])} {
			s.Send(f.Data)
		}
		s.End()
		for i := 0; i < 4; i++ {
			_ = core.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, msg, err := core.Conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if p, _ := protocol.Decode(msg); p.Type == protocol.FramePing {
				i--
				continue
			}
			if !bytes.Equal(msg, seq[i]) {
				t.Fatalf("第 %d 帧与夹具不同", i+1)
			}
		}
		for _, b := range seq[4:] {
			if err := core.Conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, "响应", func() bool {
			d, ended, _, _ := sink.snapshot()
			return ended && len(d) > 0
		})
	})

	t.Run("真连接：令牌过期 → reject 后关 4490", func(t *testing.T) {
		r := newHubRig(t, func(o *tunnel.HubOptions) { o.Nonce = nil })
		r.env.SetNow(time.Now())
		base := realServer(t, r)
		conn, _, err := websocket.DefaultDialer.Dial(relayenv.WSURL(base, gen.PathTunnel), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		hello := gen.Hello{Type: "hello", Protocol: gen.HelloProtocol{Major: 1, Minor: 3}, SourceID: r.env.SourceID,
			CoreVersion: "0.2.0", Capabilities: []string{}, Nonce: "oaGhoaGhoaGhoaGhoaGhoQ",
			TunnelToken: r.env.TunnelTokenAt(t, nil, time.Now().Add(-3*time.Hour))}
		b, _ := json.Marshal(hello)
		_ = conn.WriteMessage(websocket.TextMessage, b)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var rej gen.Reject
		_ = json.Unmarshal(msg, &rej)
		if rej.Code != "tunnel_token_expired" {
			t.Fatalf("%+v", rej)
		}
		_, _, err = conn.ReadMessage()
		if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != 4490 {
			t.Fatalf("应以 4490 关闭：%v", err)
		}
	})
}

func (r *hubRig) tunCount() int { n, _ := r.hub.Counts(); return n }

func mustDecode(t *testing.T, b []byte) protocol.Frame {
	t.Helper()
	f, err := protocol.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
