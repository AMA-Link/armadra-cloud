package relayenv

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// Core 是走真连接的假 core（源）：连 `/t/v1`、走完四步握手，之后收发隧道帧。
type Core struct {
	t     testing.TB
	Conn  *websocket.Conn
	Ready gen.Ready
}

// HelloOptions 调整 hello。
type HelloOptions struct {
	Capabilities []string
	CoreVersion  string
	// Token 非空时覆盖隧道令牌。
	Token string
}

// WSURL 把 http 地址换成 ws 地址。
func WSURL(base, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = path
	return u.String()
}

// DialCore 连 base（形如 http://127.0.0.1:port）的 `/t/v1` 并握手；失败时测试终止。
func (e *Env) DialCore(t testing.TB, base string, o HelloOptions) *Core {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(WSURL(base, gen.PathTunnel), nil)
	if err != nil {
		t.Fatalf("连隧道失败：%v", err)
	}
	c := &Core{t: t, Conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	hello := gen.Hello{
		Type:         "hello",
		Protocol:     gen.HelloProtocol{Major: gen.ProtocolMajor, Minor: gen.ProtocolMinor},
		SourceID:     e.SourceID,
		CoreVersion:  "0.2.0",
		Capabilities: o.Capabilities,
		TunnelToken:  e.TunnelToken(t, nil),
		Nonce:        "oaGhoaGhoaGhoaGhoaGhoQ",
	}
	if o.CoreVersion != "" {
		hello.CoreVersion = o.CoreVersion
	}
	if o.Token != "" {
		hello.TunnelToken = o.Token
	}
	if hello.Capabilities == nil {
		hello.Capabilities = []string{}
	}
	c.sendJSON(hello)
	var challenge gen.Challenge
	c.readJSON(&challenge)
	if challenge.Type != "challenge" {
		t.Fatalf("应收到 challenge：%+v", challenge)
	}
	sig := ed25519.Sign(e.Source.Private, protocol.SigningInput(hello.SourceID, hello.Nonce, challenge.Nonce2, challenge.RelayNode))
	c.sendJSON(gen.Auth{Type: "auth", Sig: base64.RawURLEncoding.EncodeToString(sig)})
	c.readJSON(&c.Ready)
	if c.Ready.Type != "ready" {
		t.Fatalf("应收到 ready：%+v", c.Ready)
	}
	return c
}

func (c *Core) sendJSON(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Conn.WriteMessage(websocket.TextMessage, b); err != nil {
		c.t.Fatalf("写握手消息失败：%v", err)
	}
}

func (c *Core) readJSON(v any) {
	c.t.Helper()
	_ = c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, b, err := c.Conn.ReadMessage()
	if err != nil {
		c.t.Fatalf("读握手消息失败：%v", err)
	}
	if mt != websocket.TextMessage {
		c.t.Fatalf("应是文本消息")
	}
	if err := json.Unmarshal(b, v); err != nil {
		c.t.Fatal(err)
	}
}

// Write 发一帧。
func (c *Core) Write(f protocol.Frame) {
	c.t.Helper()
	b, err := protocol.Encode(f)
	if err != nil {
		c.t.Fatalf("编码帧失败：%v", err)
	}
	if err := c.Conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		c.t.Fatalf("写帧失败：%v", err)
	}
}

// Read 读下一帧（自动答 PING，不返回 PING/PONG）。
func (c *Core) Read() protocol.Frame {
	c.t.Helper()
	f, err := c.TryRead(5 * time.Second)
	if err != nil {
		c.t.Fatalf("读帧失败：%v", err)
	}
	return f
}

// TryRead 读下一帧，超时或断开时返回错误。
func (c *Core) TryRead(timeout time.Duration) (protocol.Frame, error) {
	for {
		_ = c.Conn.SetReadDeadline(time.Now().Add(timeout))
		mt, b, err := c.Conn.ReadMessage()
		if err != nil {
			return protocol.Frame{}, err
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := protocol.Decode(b)
		if err != nil {
			return protocol.Frame{}, err
		}
		if f.Type == protocol.FramePing {
			c.Write(protocol.Frame{Type: protocol.FramePong, Ts: f.Ts})
			continue
		}
		return f, nil
	}
}

// ReadUntil 读帧直到 pred 为真，返回命中的帧。
func (c *Core) ReadUntil(pred func(protocol.Frame) bool) protocol.Frame {
	c.t.Helper()
	for {
		f := c.Read()
		if pred(f) {
			return f
		}
	}
}

// Headers 返回带中继子协议的 WS 拨号头。
func Subprotocols(parts ...string) http.Header {
	h := http.Header{}
	h.Set("Sec-WebSocket-Protocol", strings.Join(parts, ", "))
	return h
}
