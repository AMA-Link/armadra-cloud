package relayenv

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// BigByte 是 E2ESend 模式下第 i 个字节的确定取值。
func BigByte(i int) byte { return byte((i*31 + 7) & 0xff) }

// CoreOptions 配置假 core 的行为。
type CoreOptions struct {
	Capabilities []string
	// E2ESend 非零时，端到端流不回显，改为写这么多字节的确定序列（BigByte）后结束（背压测试）。
	E2ESend int
	// NoPong 为真时不答 PING（心跳测试）。
	NoPong bool
	// Token 覆盖 hello 里的隧道令牌。
	Token string
}

// FakeCore 是会回显的假 core：每条 OPEN kind e2e 的流把收到的字节原样写回，
// 遵守中继给的信用，发出回显之后才补信用，背压一路传回客户端。
// 所有帧的收发都在一个协程里，状态不需要加锁（对外的计数用原子量与互斥）。
type FakeCore struct {
	*Core
	opts CoreOptions

	BytesSent atomic.Int64

	mu       sync.Mutex
	opens    []gen.OpenPayload
	goaways  []string
	closed   chan struct{}
	closeErr error

	// 以下只在 run 协程里读写。
	streams    map[uint32]*coreStream
	sendCredit int
	recvUnack  int
	window     gen.ReadyLimits
}

type coreStream struct {
	id        uint32
	credit    int
	pending   [][]byte
	unacked   int
	remoteEnd bool
	sent      int // E2ESend 模式已写出的字节数
	finished  bool
}

// StartCore 连上中继并握手，然后在后台处理帧。
func (e *Env) StartCore(t testing.TB, base string, o CoreOptions) *FakeCore {
	t.Helper()
	core := e.DialCore(t, base, HelloOptions{Capabilities: o.Capabilities, Token: o.Token})
	f := &FakeCore{
		Core: core, opts: o, closed: make(chan struct{}),
		streams: map[uint32]*coreStream{}, sendCredit: int(core.Ready.Limits.TunnelWindow),
		window: core.Ready.Limits,
	}
	go f.run()
	return f
}

// Opens 是收到的端到端通道 OPEN。
func (c *FakeCore) Opens() []gen.OpenPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gen.OpenPayload(nil), c.opens...)
}

// Goaways 是收到的 GOAWAY 原因。
func (c *FakeCore) Goaways() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.goaways...)
}

// Closed 在隧道连接断开时关闭。
func (c *FakeCore) Closed() <-chan struct{} { return c.closed }

// CloseCode 返回断开时的关闭码，没有关闭帧时是 1006。
func (c *FakeCore) CloseCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ce, ok := c.closeErr.(*websocket.CloseError); ok {
		return ce.Code
	}
	return 1006
}

// Terminate 立即断开。
func (c *FakeCore) Terminate() { _ = c.Conn.Close() }

// SendGoaway 主动发 GOAWAY shutdown。
func (c *FakeCore) SendGoaway() {
	c.writeLocked(protocol.Frame{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: "shutdown", GraceMs: 1000}})
}

var writeMu sync.Mutex

func (c *FakeCore) writeLocked(f protocol.Frame) {
	b, err := protocol.Encode(f)
	if err != nil {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.Conn.WriteMessage(websocket.BinaryMessage, b)
}

func (c *FakeCore) run() {
	defer close(c.closed)
	for {
		mt, msg, err := c.Conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.closeErr = err
			c.mu.Unlock()
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			continue
		}
		c.handle(f)
	}
}

func (c *FakeCore) handle(f protocol.Frame) {
	switch f.Type {
	case protocol.FrameOpen:
		s := &coreStream{id: f.StreamID, credit: int(c.window.StreamWindow)}
		c.streams[f.StreamID] = s
		c.mu.Lock()
		c.opens = append(c.opens, *f.Open)
		c.mu.Unlock()
		if c.opts.E2ESend > 0 {
			c.pump(s)
		}
	case protocol.FrameData:
		s := c.streams[f.StreamID]
		if s == nil {
			c.consumed(nil, len(f.Data))
			return
		}
		if c.opts.E2ESend > 0 {
			c.consumed(s, len(f.Data))
			return
		}
		s.pending = append(s.pending, f.Data)
		c.pump(s)
	case protocol.FrameEnd:
		if s := c.streams[f.StreamID]; s != nil {
			s.remoteEnd = true
			c.pump(s)
		}
	case protocol.FrameRst:
		delete(c.streams, f.StreamID)
	case protocol.FrameWindow:
		if f.StreamID == 0 {
			c.sendCredit += int(f.Credit)
			for _, s := range c.streams {
				c.pump(s)
			}
		} else if s := c.streams[f.StreamID]; s != nil {
			s.credit += int(f.Credit)
			c.pump(s)
		}
	case protocol.FramePing:
		if !c.opts.NoPong {
			c.writeLocked(protocol.Frame{Type: protocol.FramePong, Ts: f.Ts})
		}
	case protocol.FrameGoaway:
		c.mu.Lock()
		c.goaways = append(c.goaways, f.Goaway.Reason)
		c.mu.Unlock()
	}
}

// pump 按信用把能发的发出去（回显或确定序列），回显发出后才补收信用，发完且对端 END 后收尾。
func (c *FakeCore) pump(s *coreStream) {
	if s.finished {
		return
	}
	if c.opts.E2ESend > 0 {
		for s.sent < c.opts.E2ESend && s.credit > 0 && c.sendCredit > 0 {
			n := min(c.opts.E2ESend-s.sent, protocol.MaxDataChunk, s.credit, c.sendCredit)
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = BigByte(s.sent + i)
			}
			c.send(s, buf)
			s.sent += n
		}
		if s.sent >= c.opts.E2ESend {
			s.finished = true
			c.writeLocked(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.id})
		}
		return
	}
	for len(s.pending) > 0 && s.credit > 0 && c.sendCredit > 0 {
		b := s.pending[0]
		n := min(len(b), protocol.MaxDataChunk, s.credit, c.sendCredit)
		c.send(s, b[:n])
		c.consumed(s, n)
		if n == len(b) {
			s.pending = s.pending[1:]
		} else {
			s.pending[0] = b[n:]
		}
	}
	if len(s.pending) == 0 && s.remoteEnd {
		s.finished = true
		c.writeLocked(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.id})
		delete(c.streams, s.id)
	}
}

func (c *FakeCore) send(s *coreStream, b []byte) {
	s.credit -= len(b)
	c.sendCredit -= len(b)
	c.BytesSent.Add(int64(len(b)))
	c.writeLocked(protocol.Frame{Type: protocol.FrameData, StreamID: s.id, Data: b})
}

// consumed 记一次收到字节的消费，累计到窗口一半时补 WINDOW。
func (c *FakeCore) consumed(s *coreStream, n int) {
	c.recvUnack += n
	if half := int(c.window.TunnelWindow) / 2; c.recvUnack >= half {
		c.writeLocked(protocol.Frame{Type: protocol.FrameWindow, Credit: uint32(c.recvUnack)})
		c.recvUnack = 0
	}
	if s == nil || s.remoteEnd {
		return
	}
	s.unacked += n
	if s.unacked >= int(c.window.StreamWindow)/2 {
		c.writeLocked(protocol.Frame{Type: protocol.FrameWindow, StreamID: s.id, Credit: uint32(s.unacked)})
		s.unacked = 0
	}
}
