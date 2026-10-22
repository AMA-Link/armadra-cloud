package edge

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 收尾的宽限（变量只为测试能缩短）。
var (
	// closeGrace 是发出关闭帧之后等对端回关闭帧的时长，超时强断。
	closeGrace = 5 * time.Second
	// endGrace 是客户端关闭后等源回 END 的时长；源迟迟不回就重置，别让流一直占着名额。
	endGrace = 5 * time.Second
)

// pingFrame 是 ping 帧（空载荷），所有连接共用，只读。
var pingFrame = serverFrame(opPing, nil)

// edgeConn 是一条已升级的客户端连接，同时是 netpoll 的 Handler 与隧道流的 Sink。
// 端到端通道的管道语义（cloud-api §20）：
//
//   - 上行：二进制消息写进流；没信用就停读客户端，Resume 后恢复。文本消息以 4400 关闭并重置流。
//     客户端正常关闭 → END；掉线 → RST clientGone。
//   - 下行：流的每块作为一条二进制消息发出，字节写进内核后才补信用（背压一路传回源）。
//     源 END → 1000；源 RST 或隧道没了 → 4404。
//   - 活性：每 pingMs 发一次 ping，idleMs 内客户端没有任何消息就断开。
type edgeConn struct {
	e        *Edge
	c        netpoll.Conn
	sourceID string
	subject  string
	started  time.Time
	lastSeen atomic.Int64
	bucket   int

	// 只在所属循环的 OnRead 里读写。
	partial []byte
	inFrag  bool
	broken  bool

	mu         sync.Mutex
	s          *tunnel.Stream
	closeSent  bool
	closeRecv  bool
	srcDone    bool
	finished   bool
	closeCode  int
	closeTimer *time.Timer
	endTimer   *time.Timer

	bytesUp   atomic.Int64
	bytesDown atomic.Int64
}

func (ec *edgeConn) stream() *tunnel.Stream {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return ec.s
}

// bind 记下连接：netpoll 的回调可能抢在 Adopt 返回之前到达。
func (ec *edgeConn) bind(c netpoll.Conn) {
	ec.mu.Lock()
	if ec.c == nil {
		ec.c = c
	}
	ec.mu.Unlock()
}

func (ec *edgeConn) setStream(s *tunnel.Stream) {
	ec.mu.Lock()
	ec.s = s
	ec.mu.Unlock()
}

// Disconnect 实现 Handle：立即断开（撤销源或访客、关闭服务时）。
func (ec *edgeConn) Disconnect() {
	if ec.c != nil {
		ec.c.Close()
	}
}

// ------------------------------------------------------------ netpoll.Handler

// OnRead 解析客户端帧。p 在返回后会被复用，所以跨读的半帧要拷贝。
func (ec *edgeConn) OnRead(c netpoll.Conn, p []byte) {
	ec.bind(c)
	if ec.broken {
		return
	}
	ec.lastSeen.Store(time.Now().UnixMilli())
	data := p
	if len(ec.partial) > 0 {
		ec.partial = append(ec.partial, p...)
		data = ec.partial
	}
	off := 0
	for off < len(data) {
		f, used, ferr := parseClientFrame(data[off:], protocol.MaxFrameBytes)
		if ferr != nil {
			ec.partial, ec.broken = nil, true
			ec.protocolError(ferr.code, ferr.reason)
			return
		}
		if used == 0 {
			break
		}
		off += used
		ec.frame(c, f)
		if ec.broken {
			ec.partial = nil
			return
		}
	}
	if rest := data[off:]; len(rest) > 0 {
		ec.partial = append(make([]byte, 0, len(rest)+4096), rest...)
	} else {
		ec.partial = nil
	}
}

func (ec *edgeConn) frame(c netpoll.Conn, f clientFrame) {
	switch f.Op {
	case opContinuation:
		if !ec.inFrag {
			ec.broken = true
			ec.protocolError(closeProtocolError, "没有可续的分片")
			return
		}
		ec.inFrag = !f.Fin
		ec.upstream(c, f.Payload)
	case opBinary:
		if ec.inFrag {
			ec.broken = true
			ec.protocolError(closeProtocolError, "分片没结束就开始新消息")
			return
		}
		ec.inFrag = !f.Fin
		ec.upstream(c, f.Payload)
	case opText:
		if ec.inFrag {
			ec.broken = true
			ec.protocolError(closeProtocolError, "分片没结束就开始新消息")
			return
		}
		ec.inFrag = !f.Fin
		// 只收二进制：文本消息以 4400 关闭并重置流。
		if s := ec.stream(); s != nil {
			s.Abort()
		}
		ec.closeGracefully(int(protocol.CloseBadFrame), "binary_only")
	case opClose:
		ec.onCloseFrame(c, f.Payload)
	case opPing:
		c.Write(serverFrame(opPong, f.Payload), 0)
	case opPong:
	}
}

// upstream 把客户端的载荷发给源；没有信用就停读，等 Resume。
func (ec *edgeConn) upstream(c netpoll.Conn, payload []byte) {
	if len(payload) == 0 {
		return
	}
	s := ec.stream()
	if s == nil {
		return
	}
	ec.bytesUp.Add(int64(len(payload)))
	if s.Send(payload) {
		return
	}
	c.Pause()
	// Resume 可能抢在 Pause 之前到达：停读之后再查一次。
	if !s.Parked() {
		c.Resume()
	}
}

// onCloseFrame：客户端正常关闭 → 挂着的上行发完后发 END；回关闭帧后等对端断开。
func (ec *edgeConn) onCloseFrame(c netpoll.Conn, payload []byte) {
	code, bad := parseClose(payload)
	if bad != nil {
		ec.broken = true
		ec.protocolError(bad.code, bad.reason)
		return
	}
	ec.mu.Lock()
	if ec.closeRecv {
		ec.mu.Unlock()
		return
	}
	ec.closeRecv = true
	if ec.closeCode == 0 {
		ec.closeCode = code
	}
	sent := ec.closeSent
	ec.closeSent = true
	srcDone := ec.srcDone
	s := ec.s
	ec.mu.Unlock()
	if s != nil {
		s.End()
		if !srcDone {
			ec.armEndGrace(s)
		}
	}
	if !sent {
		var reply []byte
		if code != 1005 {
			reply = closePayload(code, "")
		}
		c.Write(serverFrame(opClose, reply), 0)
	}
	c.CloseAfterFlush(closeGrace)
}

// armEndGrace：客户端已关，源 5 秒不回 END 就重置。
func (ec *edgeConn) armEndGrace(s *tunnel.Stream) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if ec.endTimer != nil || ec.finished {
		return
	}
	ec.endTimer = time.AfterFunc(endGrace, func() {
		ec.mu.Lock()
		done := ec.srcDone
		ec.mu.Unlock()
		if !done {
			s.Abort()
		}
	})
}

// protocolError：客户端帧不合法，重置流、发关闭帧。
func (ec *edgeConn) protocolError(code int, reason string) {
	if s := ec.stream(); s != nil {
		s.Abort()
	}
	ec.closeGracefully(code, reason)
}

// closeGracefully 发关闭帧，等对端回关闭帧；超时强断。
func (ec *edgeConn) closeGracefully(code int, reason string) {
	ec.mu.Lock()
	if ec.closeSent || ec.finished {
		ec.mu.Unlock()
		return
	}
	ec.closeSent = true
	if ec.closeCode == 0 {
		ec.closeCode = code
	}
	c := ec.c
	ec.closeTimer = time.AfterFunc(closeGrace, func() { c.Close() })
	ec.mu.Unlock()
	if !c.Write(serverFrame(opClose, closePayload(code, reason)), 0) {
		c.Close()
	}
}

// OnSent：字节真正交给内核之后才补信用。
func (ec *edgeConn) OnSent(c netpoll.Conn, ack int) {
	ec.bind(c)
	if s := ec.stream(); s != nil {
		s.Consumed(ack)
	}
}

// OnClose：连接已断。没收到关闭帧就断了 → 重置流；收尾记日志、还并发名额。
func (ec *edgeConn) OnClose(c netpoll.Conn, _ error) {
	ec.bind(c)
	ec.mu.Lock()
	if ec.finished {
		ec.mu.Unlock()
		return
	}
	ec.finished = true
	recv := ec.closeRecv
	s := ec.s
	code := ec.closeCode
	if ec.closeTimer != nil {
		ec.closeTimer.Stop()
	}
	ec.mu.Unlock()
	if !recv && s != nil {
		s.Abort() // 掉线：RST clientGone
	}
	if code == 0 {
		code = 1006
	}
	e := ec.e
	e.wheel.remove(ec)
	e.policy.Untrack(ec)
	e.policy.ReleaseUpgrade(ec.subject)
	var id uint32
	if s != nil {
		id = s.ID()
	}
	e.o.Log.Info("端到端通道结束",
		obs.SourceID(ec.sourceID), obs.StreamID(id), obs.Kind("e2e"),
		obs.BytesIn(ec.bytesUp.Load()), obs.BytesOut(ec.bytesDown.Load()),
		obs.DurationMs(time.Since(ec.started).Milliseconds()), obs.Code(code))
}

// ------------------------------------------------------------ tunnel.Sink

// Data：源 → 客户端，每块一条二进制消息。
func (ec *edgeConn) Data(s *tunnel.Stream, p []byte) {
	ec.mu.Lock()
	if ec.s == nil {
		ec.s = s
	}
	skip := ec.closeSent || ec.finished
	c := ec.c
	ec.mu.Unlock()
	if skip {
		s.Consumed(len(p))
		return
	}
	ec.bytesDown.Add(int64(len(p)))
	if !c.Write(serverFrame(opBinary, p), len(p)) {
		s.Consumed(len(p))
	}
}

// End：源 END → 1000。
func (ec *edgeConn) End(s *tunnel.Stream) {
	ec.mu.Lock()
	if ec.s == nil {
		ec.s = s
	}
	ec.srcDone = true
	ec.mu.Unlock()
	ec.closeGracefully(int(protocol.CloseNormal), "")
}

// Reset：源 RST 或隧道没了 → 4404。
func (ec *edgeConn) Reset(s *tunnel.Stream, _ protocol.RstCode) {
	ec.mu.Lock()
	if ec.s == nil {
		ec.s = s
	}
	ec.srcDone = true
	ec.mu.Unlock()
	ec.closeGracefully(int(protocol.CloseSourceOffline), "source_offline")
}

// Resume：上行挂起的字节发完了，恢复读客户端。
func (ec *edgeConn) Resume(*tunnel.Stream) { ec.c.Resume() }

// tick 是 ping 轮的一次访问：空闲超时就断开，否则发 ping。
func (ec *edgeConn) tick(nowMs int64, idle time.Duration) {
	if nowMs-ec.lastSeen.Load() >= idle.Milliseconds() {
		ec.c.Close()
		return
	}
	ec.c.Write(pingFrame, 0)
}
