package dataplane_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/dataplane"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// marker 是测试正文里的标记，日志里不该出现。
const marker = "PAYLOAD-MARKER-7f3a"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t       *testing.T
	env     *relayenv.Env
	plane   *dataplane.Plane
	srv     *httptest.Server
	logs    *syncBuf
	traffic *traffic.Meter
	rows    *rowSink
	tokens  []string
}

type rowSink struct {
	mu   sync.Mutex
	rows []traffic.Row
}

type harnessOptions struct {
	plane   func(*dataplane.Options)
	control control.MemoryOptions
	traffic bool
}

func newHarness(t *testing.T, ho harnessOptions) *harness {
	t.Helper()
	h := &harness{t: t, logs: &syncBuf{}, rows: &rowSink{}}
	h.env = relayenv.New(t, relayenv.Options{Control: ho.control})
	opts := dataplane.Options{
		Mode: "personal", NodeID: "relay-test", Version: "0.0.0", Control: h.env.Control,
		Addressing: edge.PathAddressing(), Log: obs.NewLogger(h.logs, obs.LevelDebug),
	}
	if ho.traffic {
		h.traffic = traffic.NewMeter(traffic.MeterOptions{Node: "relay-test", Write: h.rows.writeRows})
		opts.Traffic = h.traffic
	}
	if ho.plane != nil {
		ho.plane(&opts)
	}
	plane, err := dataplane.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.plane = plane
	h.srv = httptest.NewServer(plane)
	t.Cleanup(func() {
		h.srv.CloseClientConnections()
		plane.Shutdown(0)
		h.srv.Close()
		// 零日志：正文、令牌、路径都不该出现在日志里。
		text := h.logs.String()
		for _, bad := range []string{marker, "/echo"} {
			if strings.Contains(text, bad) {
				t.Errorf("日志里出现了 %q", bad)
			}
		}
		for _, tok := range h.tokens {
			if strings.Contains(text, tok[len(tok)-20:]) {
				t.Errorf("日志里出现了令牌")
			}
		}
	})
	return h
}

func (r *rowSink) writeRows(_ context.Context, rows []traffic.Row) error {
	r.mu.Lock()
	r.rows = append(r.rows, rows...)
	r.mu.Unlock()
	return nil
}

func (r *rowSink) all() []traffic.Row {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]traffic.Row(nil), r.rows...)
}

func (h *harness) base() string { return h.srv.URL }

func (h *harness) sourcePath() string { return "/s/" + h.env.SourceID }

// core 连上中继的隧道并握手。
func (h *harness) core(o relayenv.CoreOptions) *relayenv.FakeCore {
	h.t.Helper()
	return h.env.StartCore(h.t, h.base(), o)
}

func (h *harness) relayToken(over relayenv.Claims) string {
	tok := h.env.RelayToken(h.t, over)
	h.tokens = append(h.tokens, tok)
	return tok
}

// client 是一条客户端通道：后台读，消息进 msgs，断开时 closed 收到关闭码与原因。
type client struct {
	c      *websocket.Conn
	msgs   chan []byte
	closed chan closeInfo
}

type closeInfo struct {
	code   int
	reason string
}

func startClient(t *testing.T, c *websocket.Conn) *client {
	cl := &client{c: c, msgs: make(chan []byte, 4096), closed: make(chan closeInfo, 1)}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		for {
			mt, b, err := c.ReadMessage()
			if err != nil {
				info := closeInfo{code: 1006}
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					info = closeInfo{code: ce.Code, reason: ce.Text}
				}
				cl.closed <- info
				return
			}
			if mt == websocket.BinaryMessage {
				cl.msgs <- b
			}
		}
	}()
	return cl
}

func dial(t *testing.T, url string, protocols []string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 5 * time.Second}
	return d.Dial(url, nil)
}

func (h *harness) e2eURL() string {
	return relayenv.WSURL(h.base(), h.sourcePath()+"/_relay/e2e")
}

// channel 开一条端到端通道（缺省带有效令牌与版本子协议）。token 为空串表示不带令牌。
func (h *harness) channel(token string) *client {
	h.t.Helper()
	if token == "" {
		return h.open("armadra-e2e.1")
	}
	return h.open("armadra-e2e.1", "armadra-relay."+token)
}

func (h *harness) open(protocols ...string) *client {
	h.t.Helper()
	c, _, err := dial(h.t, h.e2eURL(), protocols)
	if err != nil {
		h.t.Fatalf("拨号失败：%v", err)
	}
	return startClient(h.t, c)
}

// outcome：通道开成答 0（open=true），被拒答关闭码。中继拒绝时也先完成升级再以 44xx 关：
// 开了之后等一小会儿看它是不是马上被关。
func (c *client) outcome() (code int, reason string, open bool) {
	select {
	case info := <-c.closed:
		c.closed <- info
		return info.code, info.reason, false
	case <-time.After(300 * time.Millisecond):
		return 0, "", true
	}
}

// wait 等关闭，答关闭码与原因。
func (c *client) wait(t *testing.T) closeInfo {
	t.Helper()
	select {
	case info := <-c.closed:
		c.closed <- info
		return info
	case <-time.After(10 * time.Second):
		t.Fatal("等通道关闭超时")
		return closeInfo{}
	}
}

// echo 经通道回显一段字节：大的切开发。
func (c *client) echo(t *testing.T, payload []byte) []byte {
	t.Helper()
	written := make(chan struct{})
	defer func() { <-written }() // 写协程收尾后才返回，免得下一次写与它重叠
	go func() {
		defer close(written)
		for off := 0; off < len(payload); off += 60000 {
			end := min(off+60000, len(payload))
			if err := c.c.WriteMessage(websocket.BinaryMessage, payload[off:end]); err != nil {
				return
			}
		}
	}()
	var got []byte
	deadline := time.After(30 * time.Second)
	for len(got) < len(payload) {
		select {
		case b := <-c.msgs:
			got = append(got, b...)
		case info := <-c.closed:
			t.Fatalf("回显途中通道关闭：%+v", info)
		case <-deadline:
			t.Fatal("回显超时")
		}
	}
	return got
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func (h *harness) streams() int { _, s := h.plane.Hub.Counts(); return s }

func (h *harness) tunnels() int { n, _ := h.plane.Hub.Counts(); return n }

func limits(over func(*tunnel.Config)) func(*dataplane.Options) {
	return func(o *dataplane.Options) {
		o.Config = tunnel.DefaultConfig()
		over(&o.Config)
	}
}
