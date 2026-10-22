package tunnel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("握手消息编码失败：" + err.Error())
	}
	return b
}

const (
	writeTimeout = 30 * time.Second
	closeTimeout = 3 * time.Second
)

// wsSocket 把 gorilla 的连接适配成 Socket。读只在 Hub.Serve 的协程里；写由互斥保护。
type wsSocket struct {
	c   *websocket.Conn
	wmu sync.Mutex
	buf []byte
}

// NewSocket 包装一条已升级的 WebSocket，读上限为一帧加 16 字节。
func NewSocket(c *websocket.Conn) Socket {
	c.SetReadLimit(protocol.MaxFrameBytes + 16)
	return &wsSocket{c: c}
}

func (s *wsSocket) ReadMessage() (bool, []byte, error) {
	mt, r, err := s.c.NextReader()
	if err != nil {
		return false, nil, wrapClose(err)
	}
	buf := s.buf[:0]
	for {
		if len(buf) == cap(buf) {
			// 首次按 512 字节起步，之后翻倍；空闲的隧道不占读缓冲。
			buf = append(make([]byte, 0, max(512, 2*cap(buf))), buf...)
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, nil, wrapClose(err)
		}
	}
	s.buf = buf
	return mt == websocket.BinaryMessage, buf, nil
}

func (s *wsSocket) write(mt int, b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return s.c.WriteMessage(mt, b)
}

func (s *wsSocket) Send(frame []byte) error { return s.write(websocket.BinaryMessage, frame) }

func (s *wsSocket) SendText(b []byte) error { return s.write(websocket.TextMessage, b) }

func (s *wsSocket) Close(code protocol.CloseCode, reason string) {
	_ = s.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(int(code), reason), time.Now().Add(time.Second))
	// 给对端一点时间回关闭帧；不回就强断。
	time.AfterFunc(closeTimeout, func() { _ = s.c.Close() })
}

func (s *wsSocket) Terminate() { _ = s.c.Close() }

var upgrader = websocket.Upgrader{
	ReadBufferSize:   4096,
	WriteBufferSize:  4096,
	WriteBufferPool:  &sync.Pool{},
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 10 * time.Second,
}

// HandleUpgrade 完成 `/t/v1` 的 WebSocket 升级并交给 Serve。正在关闭时答 503。
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	if !h.Accepting() {
		httperr.Write(w, gen.ErrUnavailable, httperr.Options{})
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.Serve(NewSocket(c))
}

// closeError 带对端的关闭码，Hub 据此通知隧道。
type closeError struct {
	code int
	err  error
}

func (e *closeError) Error() string  { return e.err.Error() }
func (e *closeError) Unwrap() error  { return e.err }
func (e *closeError) CloseCode() int { return e.code }

func wrapClose(err error) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return &closeError{code: ce.Code, err: err}
	}
	return err
}
