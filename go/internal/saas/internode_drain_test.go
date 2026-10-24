package saas

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// wsPair 建一对真实的 WebSocket：答服务端（入向连接用）与客户端（扮演对端节点）。
func wsPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	got := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		got <- ws
	}))
	t.Cleanup(srv.Close)
	client, _, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http://", "ws://", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return <-got, client
}

// #138 的节点间方向：下线时本地隧道都关了，转来的流两端都已 END、出队列里还有源最后发的下行尾巴与 END。
// in.close 要等它们写给对端节点（有上限）再断开，不能立刻断开把尾巴丢掉。
func TestInternodeCloseFlushesForwardedTail(t *testing.T) {
	tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", nullTransport{}, tunnel.DefaultConfig(), tunnel.Hooks{})
	tun.Start()
	t.Cleanup(func() { tun.Close(protocol.CloseNormal, "cleanup") })

	s := newInternodeServer(nil, nil, nil, &internodeMetrics{}, 0)
	ws, peer := wsPair(t)
	c := newInternodeConn(s, ws, "node-b")
	fs := &fwdStream{c: c, id: 7, credit: protocol.StreamWindow, recvLeft: protocol.StreamWindow}
	st, err := tun.OpenStream(&gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}}, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.s = st
	c.streams[fs.id] = fs
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()

	// 对端 END，源发来下行尾巴与 END：都排进转来的流的出队列，流两端都已 END、摘出流表。
	if !c.receive(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: fs.id})) {
		t.Fatal("对端 END")
	}
	tail := bytes.Repeat([]byte("down"), 5000)
	tun.Receive(encode(protocol.Frame{Type: protocol.FrameData, StreamID: st.ID(), Data: tail}))
	tun.Receive(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: st.ID()}))
	if n := c.count(); n != 0 {
		t.Fatalf("两端都已 END，转来的流应已摘出流表，实际 %d", n)
	}

	// 写协程还没起来时开始下线：close 应等着，不立刻断开。
	closed := make(chan struct{})
	go func() { s.close(); close(closed) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("出队列里还有尾巴，close 不应立刻断开")
	default:
	}
	go c.writer()

	var data []byte
	end := false
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !end {
		_, msg, err := peer.ReadMessage()
		if err != nil {
			t.Fatalf("对端节点在收全尾巴之前连接就断了：收到 %d / %d 字节，END=%v：%v", len(data), len(tail), end, err)
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			t.Fatal(err)
		}
		if f.StreamID != fs.id {
			continue
		}
		switch f.Type {
		case protocol.FrameData:
			data = append(data, f.Data...)
		case protocol.FrameEnd:
			end = true
		case protocol.FrameRst:
			t.Fatal("两端都已 END：不应发 RST")
		}
	}
	if !bytes.Equal(data, tail) {
		t.Fatalf("对端节点应收到全部 %d 字节：收到 %d", len(tail), len(data))
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("尾巴写完后 close 应断开连接")
	}
}

// 写不出去时 close 至多等 flushWait，不无限等。
func TestInternodeCloseFlushIsBounded(t *testing.T) {
	s := newInternodeServer(nil, nil, nil, &internodeMetrics{}, 0)
	s.flushWait = 100 * time.Millisecond
	ws, _ := wsPair(t)
	c := newInternodeConn(s, ws, "node-b")
	fs := &fwdStream{c: c, id: 7}
	c.mu.Lock()
	c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: fs.id}), nil)
	c.mu.Unlock()
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	start := time.Now()
	s.close() // 写协程没起来：出队列永远写不空
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("close 应等满 flushWait 后断开，实际 %v", d)
	}
	if !c.flushed() {
		t.Fatal("close 之后连接应已关闭")
	}
}

// 链路信用耗尽时下线：源最后的下行字节与 END 挂着等对端补信用（还没排进出队列），
// close 要等对端补信用、尾巴写出之后再断开，不能因出队列为空就立刻断开。
func TestInternodeCloseWaitsForPendingTail(t *testing.T) {
	tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", nullTransport{}, tunnel.DefaultConfig(), tunnel.Hooks{})
	tun.Start()
	t.Cleanup(func() { tun.Close(protocol.CloseNormal, "cleanup") })

	s := newInternodeServer(nil, nil, nil, &internodeMetrics{}, 0)
	ws, peer := wsPair(t)
	c := newInternodeConn(s, ws, "node-b")
	c.linkCredit = 0 // 对端的链路窗口已用光
	fs := &fwdStream{c: c, id: 7, credit: protocol.StreamWindow, recvLeft: protocol.StreamWindow}
	st, err := tun.OpenStream(&gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}}, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.s = st
	c.streams[fs.id] = fs
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	go c.writer()

	tail := bytes.Repeat([]byte("down"), 500)
	tun.Receive(encode(protocol.Frame{Type: protocol.FrameData, StreamID: st.ID(), Data: tail}))
	tun.Receive(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: st.ID()}))
	c.mu.Lock()
	pending := fs.pendingLen()
	c.mu.Unlock()
	if pending != len(tail) {
		t.Fatalf("链路信用为 0：下行字节应全部挂着，实际 %d", pending)
	}

	closed := make(chan struct{})
	go func() { s.close(); close(closed) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("下行尾巴还挂着等信用，close 不应立刻断开")
	default:
	}
	// 对端补链路信用：尾巴与 END 排进出队列并写出。
	if !c.receive(encode(protocol.Frame{Type: protocol.FrameWindow, StreamID: 0, Credit: uint32(len(tail))})) {
		t.Fatal("对端 WINDOW")
	}

	var data []byte
	end := false
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !end {
		_, msg, err := peer.ReadMessage()
		if err != nil {
			t.Fatalf("对端节点在收全尾巴之前连接就断了：收到 %d / %d 字节：%v", len(data), len(tail), err)
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			t.Fatal(err)
		}
		if f.StreamID != fs.id {
			continue
		}
		switch f.Type {
		case protocol.FrameData:
			data = append(data, f.Data...)
		case protocol.FrameEnd:
			end = true
		case protocol.FrameRst:
			t.Fatal("不应发 RST")
		}
	}
	if !bytes.Equal(data, tail) {
		t.Fatalf("对端节点应收到全部 %d 字节：收到 %d", len(tail), len(data))
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("尾巴写完后 close 应断开连接")
	}
}

// 对端一直不补信用时 close 至多等 flushWait。
func TestInternodeClosePendingTailIsBounded(t *testing.T) {
	s := newInternodeServer(nil, nil, nil, &internodeMetrics{}, 0)
	s.flushWait = 100 * time.Millisecond
	ws, _ := wsPair(t)
	c := newInternodeConn(s, ws, "node-b")
	c.streams[7] = &fwdStream{c: c, id: 7, pending: [][]byte{[]byte("down")}, pendingEnd: true}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	start := time.Now()
	s.close()
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("close 应等满 flushWait 后断开，实际 %v", d)
	}
}
