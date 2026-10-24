package tunnel

import (
	"encoding/binary"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// headroom 是每块上行载荷前预留的帧头（type u8 + streamId u32）。
// 发出时把帧头写进「已经发走的字节」或预留位，整块只拷贝一次。
const headroom = protocol.FrameHeaderBytes

type pendingChunk struct {
	buf []byte
	off int
}

// segment 是已经扣过信用、等写协程出队的一段载荷。
type segment struct {
	buf    []byte
	off, n int
	end    bool
}

// Stream 是隧道里的一条流。方法可以从任意协程调用。
type Stream struct {
	t       *Tunnel
	id      uint32
	sink    Sink
	counter *traffic.Counter

	// 以下由 t.mu 守护。
	sendCredit      int
	recvRemaining   int
	unacked         int
	pending         []pendingChunk
	pendingBytes    int
	out             []segment
	outN            int // out 里的载荷字节（入队 / 出队时增减，调度器判断稀疏流不必遍历）
	endAfterPending bool
	flow            Flow // 写端调度器里的状态
	inParked        bool
	remoteEnded     bool
	localEnded      bool
	peerReset       bool
	destroyed       bool
	tail            bool // 已摘出流表、出队列还没写空（计在 Tunnel.tails 里）
	bytesIn         int64
	bytesOut        int64
}

// ID 是流号。
func (s *Stream) ID() uint32 { return s.id }

// Link 是流所在隧道的 Link（节点间连接的对端节点名；源的隧道为空串）。
func (s *Stream) Link() string { return s.t.link }

// Bytes 返回这条流两个方向已计的载荷字节（只计数，不留内容）。
func (s *Stream) Bytes() (in, out int64) {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	return s.bytesIn, s.bytesOut
}

// Send 把客户端发来的载荷发给源。p 会被拷贝。信用够就全部排进出队列，答 true；
// 不够就把剩下的挂起，答 false：调用方应停读客户端，等 Sink.Resume 再继续。
func (s *Stream) Send(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	t := s.t
	t.mu.Lock()
	if s.destroyed || s.localEnded || s.endAfterPending || t.state == StateClosed {
		t.mu.Unlock()
		return true
	}
	buf := make([]byte, headroom+len(p))
	copy(buf[headroom:], p)
	s.pending = append(s.pending, pendingChunk{buf: buf, off: headroom})
	s.pendingBytes += len(p)
	drained := t.flushPendingLocked(s)
	t.mu.Unlock()
	t.signal()
	t.closeDrained()
	return drained
}

// Parked 是否还有挂起、没发出去的上行字节。边缘在停读之后再查一次，
// 避免 Resume 抢在停读之前到达而永远停住。
func (s *Stream) Parked() bool {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	return len(s.pending) > 0 && !s.destroyed
}

// End 是客户端正常关闭：挂着的上行字节发完之后发 END。
func (s *Stream) End() {
	t := s.t
	t.mu.Lock()
	if s.destroyed || s.localEnded || s.endAfterPending {
		t.mu.Unlock()
		return
	}
	if len(s.pending) > 0 {
		s.endAfterPending = true
		t.mu.Unlock()
		return
	}
	s.queueEndLocked()
	closeNow := t.maybeReleaseLocked(s)
	t.mu.Unlock()
	t.signal()
	if closeNow {
		t.closeDrained()
	}
}

// Abort 是本地销毁（客户端掉线、协议错误）：发 RST clientGone，没发完的字节作废。
// 两端都已 END 的流不再中止：END 已排在出队列的尾巴后面，尾巴与 END 照常写出（#136）。
func (s *Stream) Abort() { s.reset(protocol.RstClientGone) }

func (s *Stream) reset(code protocol.RstCode) {
	t := s.t
	t.mu.Lock()
	if s.destroyed {
		t.mu.Unlock()
		return
	}
	if s.localEnded && s.remoteEnded {
		// 已摘出流表，两端都结束了，中止没有意义。localEnded 只表示 END 已排进出队列：
		// 这时丢掉出队列又不发 RST，对端既收不到尾巴也收不到 END，流一直挂着。
		// 不动出队列：写协程照常写完，对端为写出的字节补 WINDOW，隧道信用不用在这里还。
		t.mu.Unlock()
		return
	}
	if !s.peerReset {
		t.queueCtlLocked(rstFrame(s.id, code))
	}
	resume, refunded := t.destroyStreamLocked(s)
	closeNow := t.releaseLocked(s)
	t.mu.Unlock()
	t.signal()
	if closeNow || refunded {
		t.closeDrained()
	}
	for _, p := range resume {
		p.sink.Resume(p)
	}
}

// Consumed 记下 n 字节已经交给客户端（真正写进内核之后，或被丢弃时）：
// 累计到窗口一半就给源补 WINDOW，背压一路传回源。
func (s *Stream) Consumed(n int) {
	if n <= 0 {
		return
	}
	t := s.t
	t.mu.Lock()
	t.consumedLocked(n)
	if !s.remoteEnded && !s.destroyed && t.state != StateClosed {
		s.unacked += n
		if s.unacked >= t.cfg.StreamWindow/2 {
			credit := s.unacked
			s.unacked = 0
			s.recvRemaining += credit
			t.queueCtlLocked(windowFrame(s.id, credit))
		}
	}
	t.mu.Unlock()
	t.signal()
}

func (s *Stream) queueEndLocked() {
	s.localEnded = true
	s.out = append(s.out, segment{end: true})
	s.t.markReadyLocked(s)
}

// destroyLocked 丢掉没发出去的字节，流不再收发；答出队列里作废的载荷字节。
// 这些字节在入队时扣过隧道信用、对端永远收不到，调用方要还给隧道（见 Tunnel.destroyStreamLocked）。
// 写协程已经取走的帧不在 out 里，照常写出、由对端补 WINDOW，不会重复归还；
// pending 还没扣隧道信用，流级信用随流作废，都不用还。
func (s *Stream) destroyLocked() int {
	dropped := s.outN
	s.destroyed = true
	s.t.untailLocked(s)
	s.pending, s.pendingBytes, s.out, s.outN = nil, 0, nil, 0
	s.unparkLocked()
	return dropped
}

func (s *Stream) unparkLocked() {
	if !s.inParked {
		return
	}
	s.inParked = false
	parked := s.t.parked
	for i, p := range parked {
		if p == s {
			copy(parked[i:], parked[i+1:])
			parked[len(parked)-1] = nil
			s.t.parked = parked[:len(parked)-1]
			return
		}
	}
}

// popLocked 取出下一帧；帧头写进块前面的预留位（或已经发走的字节）。
func (s *Stream) popLocked() []byte {
	sg := s.out[0]
	s.out[0] = segment{}
	s.out = s.out[1:]
	s.outN -= sg.n
	if sg.end {
		b := make([]byte, headroom)
		b[0] = byte(protocol.FrameEnd)
		binary.BigEndian.PutUint32(b[1:], s.id)
		return b
	}
	frame := sg.buf[sg.off-headroom : sg.off+sg.n]
	frame[0] = byte(protocol.FrameData)
	binary.BigEndian.PutUint32(frame[1:headroom], s.id)
	return frame
}
