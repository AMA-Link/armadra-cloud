package personal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// /v1/me/stream（契约 §6，协议包 §5.7）的 WebSocket 外壳：票据与事件扇出在控制核（core.MeStream）。
//
//   - 升级：子协议 armadra-ticket.<票>，选中同一个子协议回给浏览器；
//   - 心跳：客户端 {"type":"ping"} → pong；服务端每 30 秒发 ping，同时复核设备仍有效，
//     失效（登出、撤销）就以 4401 关闭；
//   - 发送不阻塞：扇出在发事件的协程里同步进行，每个连接一个有界队列和一个写协程，
//     队列满了（客户端读得太慢）就断开这条连接，让它重连后重新拉取。

const (
	// MeStreamPath 是 me-stream 的路径。
	MeStreamPath = "/v1/me/stream"
	// MeStreamPing 是服务端 ping 与会话复核的间隔。
	MeStreamPing = 30 * time.Second

	streamQueue        = 256
	streamMaxMessage   = 4096
	streamWriteTimeout = 10 * time.Second
	streamCloseGrace   = time.Second
)

// MeStream 是 /v1/me/stream 的 WebSocket 外壳。
type MeStream struct {
	core *core.MeStream
	now  func() time.Time
	ping time.Duration

	mu      sync.Mutex
	clients map[*streamClient]struct{}
	closed  bool
}

// NewMeStream 建外壳；ping 为 0 取 30 秒，now 为 nil 取 time.Now。
func NewMeStream(c *core.MeStream, now func() time.Time, ping time.Duration) *MeStream {
	if now == nil {
		now = time.Now
	}
	if ping <= 0 {
		ping = MeStreamPing
	}
	return &MeStream{core: c, now: now, ping: ping, clients: map[*streamClient]struct{}{}}
}

type streamClient struct {
	conn *websocket.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func (c *streamClient) stop() { c.once.Do(func() { close(c.done) }) }

// encode 把事件序列化成一行 JSON（不转义 HTML）。
func encode(v any) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(v) != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

type tsEvent struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

// ticketOffered 取客户端在 Sec-WebSocket-Protocol 里报的票据子协议。
func ticketOffered(r *http.Request) (protocol, ticket string) {
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, gen.SubprotocolTicket) {
				return part, strings.TrimPrefix(part, gen.SubprotocolTicket)
			}
		}
	}
	return "", ""
}

// HandleUpgrade 是 dataplane 的 UpgradeExtra：路径是 /v1/me/stream 就认领。
func (m *MeStream) HandleUpgrade(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != MeStreamPath {
		return false
	}
	protocol, ticket := ticketOffered(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	caller, ok, err := m.core.Redeem(ctx, ticket)
	cancel()
	if err != nil {
		httperr.Write(w, gen.ErrUnavailable, httperr.Options{})
		return true
	}
	if !ok {
		httperr.Write(w, gen.ErrUnauthenticated, httperr.Options{})
		return true
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: []string{protocol},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return true
	}
	m.attach(conn, caller)
	return true
}

func (m *MeStream) attach(conn *websocket.Conn, caller core.Caller) {
	c := &streamClient{conn: conn, out: make(chan []byte, streamQueue), done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	m.clients[c] = struct{}{}
	m.mu.Unlock()

	// send 不阻塞：队列满了断开这条连接。
	send := func(v any) {
		b := encode(v)
		if b == nil {
			return
		}
		select {
		case <-c.done:
		case c.out <- b:
		default:
			c.stop()
		}
	}
	detach, err := m.core.Attach(caller, func(event core.Event) { send(event) }, c.stop)
	if err != nil {
		// 访客的链接查不了（库出错）：关掉让客户端重连，别留一条永远收不到事件的连接。
		m.mu.Lock()
		delete(m.clients, c)
		m.mu.Unlock()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "unavailable"),
			time.Now().Add(streamWriteTimeout))
		_ = conn.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 写协程：唯一往连接写数据的地方。
		defer wg.Done()
		for {
			select {
			case <-c.done:
				return
			case b := <-c.out:
				_ = conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
				if conn.WriteMessage(websocket.TextMessage, b) != nil {
					c.stop()
					return
				}
			}
		}
	}()
	go func() { // 心跳与会话复核。
		defer wg.Done()
		ticker := time.NewTicker(m.ping)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				valid, err := m.core.StillValid(ctx, caller)
				cancel()
				switch {
				case err != nil:
				case !valid:
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(gen.CloseUnauthenticated, "session_revoked"),
						time.Now().Add(streamWriteTimeout))
					// 给对端一点时间回关闭帧，之后读循环自会结束。
					time.AfterFunc(streamCloseGrace, c.stop)
					return
				default:
					send(tsEvent{Type: "ping", TS: m.now().UnixMilli()})
				}
			}
		}
	}()

	conn.SetReadLimit(streamMaxMessage)
	go func() { // 读循环：只认文本的 {"type":"ping"}。
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if messageType != websocket.TextMessage {
				continue
			}
			var message struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &message) == nil && message.Type == "ping" {
				send(tsEvent{Type: "pong", TS: m.now().UnixMilli()})
			}
		}
		c.stop()
		wg.Wait()
		detach()
		_ = conn.Close()
		m.mu.Lock()
		delete(m.clients, c)
		m.mu.Unlock()
	}()
	// 写协程或心跳协程让 c 停下时，读循环也要醒来。
	go func() {
		<-c.done
		_ = conn.Close()
	}()
}

// Close 断开所有连接并清掉票据。
func (m *MeStream) Close() {
	m.mu.Lock()
	m.closed = true
	clients := make([]*streamClient, 0, len(m.clients))
	for c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.Unlock()
	for _, c := range clients {
		c.stop()
	}
	m.core.Clear()
}
