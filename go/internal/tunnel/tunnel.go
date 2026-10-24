// Package tunnel 是中继的隧道层：`/t/v1` 握手、隧道表、流与信用、心跳、GOAWAY。
//
// 与 packages/relay-kernel 的 tunnel/ 逐项对应：
//
//   - Tunnel 是 ready 之后的一条隧道（tunnel.ts + stream.ts 的 StreamCore）；
//   - Handshake 是四步握手状态机（handshake.ts）；
//   - Hub 是隧道表与握手装配（hub.ts）。
//
// 并发模型：每条隧道一个读协程（Hub.Serve）、一个写协程（Tunnel.writer）。写端按流轮转
// 出队，每流每轮最多一个 DATA（#57 方案 A）。信用在字节入队时扣、WINDOW 补回；
// 对客户端的字节真正交给内核之后，边缘才调 Stream.Consumed 补信用。
//
// 锁序：Tunnel.mu → 流量计量的锁。Sink 与 Hooks 的回调一律在释放 Tunnel.mu 之后调用。
package tunnel

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// Config 是一条隧道的限制，缺省取协议值；测试用小窗口与短心跳覆盖。
type Config struct {
	MaxStreams      int
	StreamWindow    int
	TunnelWindow    int
	MaxDataChunk    int
	Heartbeat       time.Duration
	HeartbeatMisses int
	GoawayGrace     time.Duration
}

// DefaultConfig 返回协议规定的限制。
func DefaultConfig() Config {
	return Config{
		MaxStreams:      protocol.MaxStreams,
		StreamWindow:    protocol.StreamWindow,
		TunnelWindow:    protocol.TunnelWindow,
		MaxDataChunk:    protocol.MaxDataChunk,
		Heartbeat:       time.Duration(protocol.HeartbeatMs) * time.Millisecond,
		HeartbeatMisses: protocol.HeartbeatMisses,
		GoawayGrace:     time.Duration(protocol.GoawayGraceMs) * time.Millisecond,
	}
}

// State 是隧道状态：open → draining → closed。
type State int

// 隧道状态。
const (
	StateOpen State = iota
	StateDraining
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateDraining:
		return "draining"
	}
	return "closed"
}

// Transport 是隧道的传输层（一条 WebSocket）。Send 只会被写协程串行调用。
type Transport interface {
	// Send 发一条二进制消息。返回后不得再引用 frame（隧道会复用块里的帧头位置）。
	Send(frame []byte) error
	// Close 正常关闭（发关闭帧）。
	Close(code protocol.CloseCode, reason string)
	// Terminate 立即断开（心跳超时）。
	Terminate()
}

// CloseInfo 描述隧道为什么关闭。
type CloseInfo struct {
	Code   protocol.CloseCode
	Reason string
	// Replaced 为真表示被同源的新隧道替换（不该再删路由、发下线事件）。
	Replaced bool
}

// Hooks 是隧道对宿主的通知；全部可选，在释放隧道锁之后调用。
type Hooks struct {
	// OnPong 收到 PONG（路由续期）。
	OnPong func()
	// OnPeerGoaway 对端发来 GOAWAY：不再给它开新流。
	OnPeerGoaway func(reason string)
	OnClose      func(CloseInfo)
	// OnHeartbeatMiss 心跳超时断开前调用。
	OnHeartbeatMiss func()
	// OnBytes 载荷字节计数：ToSource 是中继到源，ToClient 是源到中继。
	OnBytes func(d obs.Direction, n int)
}

// Sink 接住一条流的下行事件，由边缘实现。三个方法都在隧道的读协程里串行调用
// （隧道关闭时的 Reset 例外，可能来自别的协程），所以实现必须自己加锁，且不能阻塞。
type Sink interface {
	// Data 是源发来的一块载荷；p 只在调用期间有效，要留就拷贝。
	// 字节真正交给客户端之后，调用方要调 Stream.Consumed 补信用。
	Data(s *Stream, p []byte)
	// End 是源发了 END。
	End(s *Stream)
	// Reset 是源发了 RST，或隧道关闭了（RstSourceGone）。流已从流表摘掉。
	Reset(s *Stream, code protocol.RstCode)
	// Resume 表示之前 Send 返回 false 挂起的上行字节已全部发出，可以继续读客户端。
	Resume(s *Stream)
}

// UnavailableError 表示隧道不能再开流。
type UnavailableError struct {
	// Reason 取 "draining"、"closed"、"max_streams"。
	Reason string
}

func (e *UnavailableError) Error() string { return "隧道不可用：" + e.Reason }

// Reason 取值。
const (
	ReasonDraining   = "draining"
	ReasonClosed     = "closed"
	ReasonMaxStreams = "max_streams"
)

// Tunnel 是 ready 之后的一条隧道。只有中继发 OPEN；对端发来的 OPEN 答 RST protocol。
//
// 节点间转发（SaaS）的出向连接也是一条 Tunnel：本节点是开流方，对端是持有隧道的节点；
// 它没有 sourceID，Link 答对端节点名。
type Tunnel struct {
	id          string
	sourceID    string
	link        string
	coreVersion string
	capsMu      sync.RWMutex
	caps        map[string]struct{}

	cfg   Config
	tr    Transport
	hooks Hooks

	missed atomic.Int32
	wake   chan struct{}
	// done 在进入 closed 状态时关闭（写协程据此收尾）；finished 在关闭帧发出之后关闭。
	done     chan struct{}
	finished chan struct{}

	mu            sync.Mutex
	state         State
	replaced      bool
	started       bool
	closeReq      bool
	closeCode     protocol.CloseCode
	closeReason   string
	goawayCode    protocol.CloseCode
	graceTimer    *time.Timer
	streams       map[uint32]*Stream
	nextID        uint32
	sendCredit    int
	recvRemaining int
	recvUnacked   int
	ctl           [][]byte
	ready         []*Stream // 持续有数据的流（老流）
	readyNext     []*Stream
	fresh         []*Stream // 刚从空闲转为就绪的流（稀疏流），先于老流出帧
	freshNext     []*Stream
	round         uint64 // 写端批次号
	scratch       []*Stream
	parked        []*Stream
}

// New 建一条隧道。写协程与心跳到 Start 才开始。
func New(id, sourceID, coreVersion string, tr Transport, cfg Config, hooks Hooks) *Tunnel {
	return &Tunnel{
		id:            id,
		sourceID:      sourceID,
		coreVersion:   coreVersion,
		caps:          map[string]struct{}{},
		cfg:           cfg,
		tr:            tr,
		hooks:         hooks,
		wake:          make(chan struct{}, 1),
		done:          make(chan struct{}),
		finished:      make(chan struct{}),
		goawayCode:    protocol.CloseTunnelGoaway,
		streams:       map[uint32]*Stream{},
		nextID:        1,
		sendCredit:    cfg.TunnelWindow,
		recvRemaining: cfg.TunnelWindow,
	}
}

// ID 是隧道 ID。
func (t *Tunnel) ID() string { return t.id }

// SourceID 是隧道属于的源（节点间连接为空串）。
func (t *Tunnel) SourceID() string { return t.sourceID }

// SetLink 标明这是到节点 peer 的节点间连接（Start 之前调）。
func (t *Tunnel) SetLink(peer string) { t.link = peer }

// Link 是节点间连接的对端节点名；源的隧道答空串。
func (t *Tunnel) Link() string { return t.link }

// CoreVersion 是源在 hello 里报的版本。
func (t *Tunnel) CoreVersion() string { return t.coreVersion }

// SetCapabilities 记下源在 hello.capabilities 里报的项（握手就绪时由 Hub 填）。
func (t *Tunnel) SetCapabilities(list []string) {
	t.capsMu.Lock()
	defer t.capsMu.Unlock()
	t.caps = make(map[string]struct{}, len(list))
	for _, c := range list {
		t.caps[c] = struct{}{}
	}
}

// HasCapability 源是否报过这一项。
func (t *Tunnel) HasCapability(name string) bool {
	t.capsMu.RLock()
	defer t.capsMu.RUnlock()
	_, ok := t.caps[name]
	return ok
}

// State 是当前状态。
func (t *Tunnel) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Replaced 是否被同源的新隧道替换。
func (t *Tunnel) Replaced() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.replaced
}

// StreamCount 是流表里的流数。
func (t *Tunnel) StreamCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.streams)
}

// PendingBytes 是还没写进传输层的上行载荷字节：挂着等信用的，加已扣信用、排在出队列里的。
func (t *Tunnel) PendingBytes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, s := range t.streams {
		n += s.pendingBytes
		for _, sg := range s.out {
			n += sg.n
		}
	}
	return n
}

// Accepting 能否再开流（open 状态）。
func (t *Tunnel) Accepting() bool { return t.State() == StateOpen }

// Closed 在隧道关闭、关闭帧已交给传输层之后关闭。
func (t *Tunnel) Closed() <-chan struct{} { return t.finished }

// Start 开始写协程与心跳。
func (t *Tunnel) Start() {
	t.mu.Lock()
	if t.started || t.state == StateClosed {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()
	go t.writer()
}

// ------------------------------------------------------------ 开流与收帧

// OpenStream 为一条边缘连接开流：发 OPEN。不在 open 状态、满流时返回 *UnavailableError。
// counter 非 nil 时，这条流两个方向的 DATA 载荷记到它上面（流量统计）。
func (t *Tunnel) OpenStream(open *gen.OpenPayload, sink Sink, counter *traffic.Counter) (*Stream, error) {
	t.mu.Lock()
	if t.state != StateOpen {
		reason := ReasonClosed
		if t.state == StateDraining {
			reason = ReasonDraining
		}
		t.mu.Unlock()
		return nil, &UnavailableError{Reason: reason}
	}
	if len(t.streams) >= t.cfg.MaxStreams {
		t.mu.Unlock()
		return nil, &UnavailableError{Reason: ReasonMaxStreams}
	}
	id := t.nextID
	frame, err := protocol.Encode(protocol.Frame{Type: protocol.FrameOpen, StreamID: id, Open: open})
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	t.nextID++
	s := &Stream{
		t:             t,
		id:            id,
		sink:          sink,
		counter:       counter,
		sendCredit:    t.cfg.StreamWindow,
		recvRemaining: t.cfg.StreamWindow,
	}
	t.streams[id] = s
	t.ctl = append(t.ctl, frame)
	t.mu.Unlock()
	t.signal()
	return s, nil
}

// Receive 处理收到的一条二进制消息。msg 只在调用期间有效。
func (t *Tunnel) Receive(msg []byte) {
	f, err := protocol.DecodeView(msg)
	if err != nil {
		code := protocol.CloseBadFrame
		if protocol.CodeOf(err) == protocol.ErrFrameTooLarge {
			code = protocol.CloseFrameTooLarge
		}
		t.Close(code, "bad_frame")
		return
	}
	switch f.Type {
	case protocol.FrameOpen:
		t.queueCtl(rstFrame(f.StreamID, protocol.RstProtocol))
	case protocol.FrameData:
		t.receiveData(f.StreamID, f.Data)
	case protocol.FrameEnd:
		t.receiveEnd(f.StreamID)
	case protocol.FrameRst:
		t.receiveRst(f.StreamID, f.Code)
	case protocol.FrameWindow:
		t.receiveWindow(f.StreamID, int(f.Credit))
	case protocol.FramePing:
		t.queueCtl(mustEncode(protocol.Frame{Type: protocol.FramePong, Ts: f.Ts}))
	case protocol.FramePong:
		t.missed.Store(0)
		if t.hooks.OnPong != nil {
			t.hooks.OnPong()
		}
	case protocol.FrameGoaway:
		t.receiveGoaway(f.Goaway)
	}
}

func (t *Tunnel) receiveData(id uint32, p []byte) {
	n := len(p)
	t.mu.Lock()
	if t.state == StateClosed {
		t.mu.Unlock()
		return
	}
	if n > t.recvRemaining {
		t.flowViolationLocked(id)
		return
	}
	t.recvRemaining -= n
	s := t.streams[id]
	if s == nil {
		t.consumedLocked(n)
		t.mu.Unlock()
		t.countBytes(nil, obs.ToClient, n)
		return
	}
	if n > s.recvRemaining {
		t.flowViolationLocked(id)
		t.countBytes(s, obs.ToClient, n)
		return
	}
	s.recvRemaining -= n
	s.bytesIn += int64(n)
	if s.remoteEnded || s.destroyed {
		// 已经结束或本地已销毁：丢弃，但隧道级信用照样还回去。
		t.consumedLocked(n)
		t.mu.Unlock()
		t.countBytes(s, obs.ToClient, n)
		return
	}
	sink := s.sink
	t.mu.Unlock()
	t.countBytes(s, obs.ToClient, n)
	if n > 0 {
		sink.Data(s, p)
	}
}

func (t *Tunnel) countBytes(s *Stream, d obs.Direction, n int) {
	if t.hooks.OnBytes != nil {
		t.hooks.OnBytes(d, n)
	}
	if s != nil && s.counter != nil {
		s.counter.Bytes(trafficDirection(d), n)
	}
}

func trafficDirection(d obs.Direction) traffic.Direction {
	if d == obs.ToSource {
		return traffic.ToSource
	}
	return traffic.ToClient
}

func (t *Tunnel) receiveEnd(id uint32) {
	t.mu.Lock()
	s := t.streams[id]
	if s == nil || s.remoteEnded {
		t.mu.Unlock()
		return
	}
	s.remoteEnded = true
	sink := s.sink
	closeNow := t.maybeReleaseLocked(s)
	t.mu.Unlock()
	sink.End(s)
	if closeNow {
		t.closeDrained()
	}
}

func (t *Tunnel) receiveRst(id uint32, code protocol.RstCode) {
	t.mu.Lock()
	s := t.streams[id]
	if s == nil {
		t.mu.Unlock()
		return
	}
	s.peerReset = true
	s.destroyLocked()
	sink := s.sink
	closeNow := t.releaseLocked(s)
	t.mu.Unlock()
	sink.Reset(s, code)
	if closeNow {
		t.closeDrained()
	}
}

func (t *Tunnel) receiveWindow(id uint32, credit int) {
	var resume []*Stream
	t.mu.Lock()
	if t.state == StateClosed {
		t.mu.Unlock()
		return
	}
	if id == 0 {
		t.sendCredit += credit
		for _, s := range append([]*Stream(nil), t.parked...) {
			if t.sendCredit <= 0 {
				break
			}
			if t.flushPendingLocked(s) {
				resume = append(resume, s)
			}
		}
	} else if s := t.streams[id]; s != nil {
		s.sendCredit += credit
		if len(s.pending) > 0 && t.flushPendingLocked(s) {
			resume = append(resume, s)
		}
	}
	t.mu.Unlock()
	t.signal()
	t.closeDrained()
	for _, s := range resume {
		s.sink.Resume(s)
	}
}

func (t *Tunnel) receiveGoaway(g *gen.GoawayPayload) {
	t.mu.Lock()
	if t.state != StateOpen {
		t.mu.Unlock()
		return
	}
	t.state = StateDraining
	t.armGraceLocked(time.Duration(g.GraceMs) * time.Millisecond)
	drained := len(t.streams) == 0
	t.mu.Unlock()
	if t.hooks.OnPeerGoaway != nil {
		t.hooks.OnPeerGoaway(g.Reason)
	}
	if drained {
		t.closeDrained()
	}
}

// ------------------------------------------------------------ GOAWAY 与关闭

// DefaultGrace 让 Goaway 取配置的宽限期。
const DefaultGrace time.Duration = -1

// Goaway 发 GOAWAY，不再开新流；旧流自然结束后（或 grace 到了）关闭：
// replaced 用 4491，其余 4492。grace 为 DefaultGrace 时取配置的宽限期，为 0 立即强关。
func (t *Tunnel) Goaway(reason string, grace time.Duration) {
	if grace < 0 {
		grace = t.cfg.GoawayGrace
	}
	t.mu.Lock()
	if t.state == StateClosed {
		t.mu.Unlock()
		return
	}
	if reason == "replaced" {
		t.replaced = true
		t.goawayCode = protocol.CloseTunnelReplaced
	}
	if t.state == StateOpen {
		frame := mustEncode(protocol.Frame{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: reason, GraceMs: grace.Milliseconds()}})
		t.ctl = append(t.ctl, frame)
		t.state = StateDraining
	}
	t.armGraceLocked(grace)
	drained := len(t.streams) == 0
	t.mu.Unlock()
	t.signal()
	if drained {
		t.closeDrained()
	}
}

func (t *Tunnel) armGraceLocked(grace time.Duration) {
	if t.graceTimer != nil {
		return
	}
	t.graceTimer = time.AfterFunc(grace, func() {
		t.mu.Lock()
		code := t.goawayCode
		t.mu.Unlock()
		t.Close(code, "goaway_grace")
	})
}

func (t *Tunnel) closeDrained() {
	t.mu.Lock()
	ok := t.state == StateDraining && len(t.streams) == 0
	code := t.goawayCode
	t.mu.Unlock()
	if ok {
		t.Close(code, "goaway")
	}
}

// Close 关隧道：所有流以 RstSourceGone 结束，传输层发关闭码。
func (t *Tunnel) Close(code protocol.CloseCode, reason string) {
	if !t.finish(code, reason, true) {
		return
	}
}

// TransportClosed 通知传输层已经断了（对端关闭或网络错误）。
func (t *Tunnel) TransportClosed(code protocol.CloseCode, reason string) {
	if reason == "" {
		reason = "transport_closed"
	}
	t.finish(code, reason, false)
}

// finish 把隧道置为 closed，答是否是这一次调用关的。wantClose 为真时写协程发完队列里的控制帧后再发关闭帧。
func (t *Tunnel) finish(code protocol.CloseCode, reason string, wantClose bool) bool {
	t.mu.Lock()
	if t.state == StateClosed {
		t.mu.Unlock()
		return false
	}
	t.state = StateClosed
	if t.graceTimer != nil {
		t.graceTimer.Stop()
	}
	streams := make([]*Stream, 0, len(t.streams))
	for _, s := range t.streams {
		s.peerReset = true
		s.destroyLocked()
		streams = append(streams, s)
	}
	t.streams = map[uint32]*Stream{}
	t.ready, t.readyNext, t.fresh, t.freshNext, t.parked = nil, nil, nil, nil, nil
	replaced := t.replaced
	started := t.started
	// 写协程还没起来就被关（握手刚完成、Start 之前来了 kick / GOAWAY）：排队的控制帧要自己发出去，
	// 否则对端只看到关闭码，收不到 GOAWAY。
	var pending [][]byte
	if !started && wantClose {
		pending = append(pending, t.ctl...)
	}
	if wantClose {
		t.closeReq, t.closeCode, t.closeReason = true, code, reason
	}
	t.mu.Unlock()
	close(t.done)
	for _, s := range streams {
		s.sink.Reset(s, protocol.RstSourceGone)
	}
	if t.hooks.OnClose != nil {
		t.hooks.OnClose(CloseInfo{Code: code, Reason: reason, Replaced: replaced})
	}
	if !started {
		if wantClose {
			for _, frame := range pending {
				_ = t.tr.Send(frame)
			}
			t.tr.Close(code, reason)
		}
		close(t.finished)
	}
	t.signal()
	return true
}

func (t *Tunnel) flowViolationLocked(id uint32) {
	t.ctl = append(t.ctl, rstFrame(id, protocol.RstFlowControl))
	t.mu.Unlock()
	t.Close(protocol.CloseBadFrame, "flow_control")
}

// ------------------------------------------------------------ 信用

// consumedLocked 把交给消费者（或丢弃）的字节记到隧道级，累计到窗口一半时补 WINDOW(0)。
func (t *Tunnel) consumedLocked(n int) {
	if n <= 0 {
		return
	}
	t.recvUnacked += n
	if t.recvUnacked >= t.cfg.TunnelWindow/2 {
		credit := t.recvUnacked
		t.recvUnacked = 0
		t.recvRemaining += credit
		t.queueCtlLocked(windowFrame(0, credit))
	}
}

func (t *Tunnel) queueCtl(frame []byte) {
	t.mu.Lock()
	t.queueCtlLocked(frame)
	t.mu.Unlock()
	t.signal()
}

func (t *Tunnel) queueCtlLocked(frame []byte) {
	if t.state == StateClosed {
		return
	}
	t.ctl = append(t.ctl, frame)
	t.signal()
}

func (t *Tunnel) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// flushPendingLocked 按信用把挂着的上行字节切成 DATA 排进流的出队列，答是否全部排完。
func (t *Tunnel) flushPendingLocked(s *Stream) bool {
	for len(s.pending) > 0 && s.sendCredit > 0 && t.sendCredit > 0 {
		p := &s.pending[0]
		n := min(len(p.buf)-p.off, t.cfg.MaxDataChunk, s.sendCredit, t.sendCredit)
		s.sendCredit -= n
		t.sendCredit -= n
		s.bytesOut += int64(n)
		s.out = append(s.out, segment{buf: p.buf, off: p.off, n: n})
		s.pendingBytes -= n
		p.off += n
		if p.off == len(p.buf) {
			s.pending[0] = pendingChunk{}
			s.pending = s.pending[1:]
		}
		t.noteSent(s, n)
	}
	t.markReadyLocked(s)
	if len(s.pending) > 0 {
		t.parkLocked(s)
		return false
	}
	s.pending = nil
	s.unparkLocked()
	if s.endAfterPending && !s.localEnded {
		s.queueEndLocked()
		t.maybeReleaseLocked(s)
	}
	return true
}

func (t *Tunnel) noteSent(s *Stream, n int) {
	// 计数的锁序是 Tunnel.mu → 计量锁；OnBytes 只做原子加。
	if t.hooks.OnBytes != nil {
		t.hooks.OnBytes(obs.ToSource, n)
	}
	if s.counter != nil {
		s.counter.Bytes(traffic.ToSource, n)
	}
}

// markReadyLocked 把有数据的流排进写端的就绪队列。刚从空闲转为就绪、且排着的字节不多
// （sparseMax 以内）的流（上一批没出过帧）进 fresh，先于持续有数据的老流出帧：终端按键与回显这类稀疏流因此不必排在大流量流后面。
// 刚出过帧又立刻就绪的流进老流队列尾，所以反复「停一下再发」换不到额外份额。
func (t *Tunnel) markReadyLocked(s *Stream) {
	if len(s.out) == 0 || s.queued != queueNone || s.destroyed {
		return
	}
	if s.servedRound+1 < t.round && s.outBytes() <= sparseMax {
		s.queued = queueFresh
		t.fresh = append(t.fresh, s)
		return
	}
	s.queued = queueOld
	t.ready = append(t.ready, s)
}

func (t *Tunnel) parkLocked(s *Stream) {
	if !s.inParked {
		s.inParked = true
		t.parked = append(t.parked, s)
	}
}

// maybeReleaseLocked 两端都结束就摘出流表；答是否该检查排空。
func (t *Tunnel) maybeReleaseLocked(s *Stream) bool {
	if s.localEnded && s.remoteEnded {
		return t.releaseLocked(s)
	}
	return false
}

func (t *Tunnel) releaseLocked(s *Stream) bool {
	if t.streams[s.id] == s {
		delete(t.streams, s.id)
	}
	s.unparkLocked()
	return t.state == StateDraining && len(t.streams) == 0
}

// ------------------------------------------------------------ 写协程与心跳

// writer 把控制帧与各流的 DATA 按轮写给传输层：每轮先写全部控制帧，再让每个有数据的流各写一帧。
func (t *Tunnel) writer() {
	defer close(t.finished)
	ticker := time.NewTicker(t.cfg.Heartbeat)
	defer ticker.Stop()
	var batch [][]byte
	for {
		var closing bool
		batch, closing = t.collect(batch[:0])
		for _, frame := range batch {
			if err := t.tr.Send(frame); err != nil {
				t.finish(protocol.CloseGoingAway, "write_failed", false)
				t.tr.Terminate()
				return
			}
		}
		if closing {
			t.mu.Lock()
			req, code, reason := t.closeReq, t.closeCode, t.closeReason
			t.mu.Unlock()
			if req {
				t.tr.Close(code, reason)
			}
			return
		}
		// 一直有活干时也要到点心跳，否则 PING 发不出去、路由 TTL 过期。
		select {
		case <-ticker.C:
			if !t.tick() {
				return
			}
		default:
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-t.wake:
		case <-t.done:
		case <-ticker.C:
			if !t.tick() {
				return
			}
		}
	}
}

// writeBudget 是一批最多交给传输层的 DATA 字节（至少一帧）。批次写完才会取下一批，
// 所以后到的控制帧与稀疏流的帧最多等「当前批剩下的 + 一块」，与就绪流的数量无关。
const writeBudget = 2 * protocol.MaxDataChunk

// sparseMax 是稀疏流排着的字节上限：超过它的（文件传输、刷屏输出）按老流轮转。
const sparseMax = 16 << 10

type queueKind uint8

const (
	queueNone queueKind = iota
	queueFresh
	queueOld
)

// collect 取出这一批要写的帧：先写全部控制帧；再给稀疏流各一帧（fresh）；再按轮转给老流各一帧，
// 超过 writeBudget 就停，没轮到的留在队首、下一批先出。老流每批至少出一帧，稀疏流再多也饿不死它们。
// 隧道已关闭时只交出剩下的控制帧。
func (t *Tunnel) collect(batch [][]byte) ([][]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	batch = append(batch, t.ctl...)
	clear(t.ctl)
	t.ctl = t.ctl[:0]
	if t.state == StateClosed {
		return batch, true
	}
	t.round++
	size := 0
	pop := func(s *Stream) {
		f := s.popLocked()
		size += len(f) - headroom
		s.servedRound = t.round
		batch = append(batch, f)
	}
	fresh, old := t.fresh, t.ready
	t.fresh, t.ready = t.freshNext[:0], t.readyNext[:0]

	// 稀疏流：每条一帧；发完一帧后还有数据的转入老流。预算用完时剩下的留在 fresh 队首。
	i := 0
	for ; i < len(fresh); i++ {
		if size >= writeBudget {
			break
		}
		s := fresh[i]
		s.queued = queueNone
		if s.destroyed || len(s.out) == 0 {
			continue
		}
		pop(s)
		if len(s.out) > 0 {
			s.queued = queueOld
			t.ready = append(t.ready, s)
		} else {
			s.out = nil
		}
	}
	t.fresh = append(t.fresh, fresh[i:]...)

	// 老流：轮转；留在队首的（没轮到的）排在这一批新排的前面，保持先来先出。
	served := 0
	j := 0
	var again []*Stream
	for ; j < len(old); j++ {
		if served > 0 && size >= writeBudget {
			break
		}
		s := old[j]
		s.queued = queueNone
		if s.destroyed || len(s.out) == 0 {
			continue
		}
		pop(s)
		served++
		if len(s.out) > 0 {
			s.queued = queueOld
			again = append(again, s)
		} else {
			s.out = nil
		}
	}
	// 新排进 t.ready 的（稀疏流转入的）在最后；没轮到的老流在最前，其次是本批发过帧的。
	merged := append(t.scratch[:0], old[j:]...)
	merged = append(merged, again...)
	merged = append(merged, t.ready...)
	t.ready = append(t.ready[:0], merged...)
	clear(merged)
	t.scratch = merged[:0]
	clear(fresh)
	clear(old)
	t.freshNext, t.readyNext = fresh[:0], old[:0]
	return batch, false
}

// tick 是一次心跳：连续 HeartbeatMisses 次没回应就断开，答是否该退出写协程。
func (t *Tunnel) tick() bool {
	if t.State() == StateClosed {
		return false
	}
	if int(t.missed.Load()) >= t.cfg.HeartbeatMisses {
		if t.hooks.OnHeartbeatMiss != nil {
			t.hooks.OnHeartbeatMiss()
		}
		t.finish(protocol.CloseGoingAway, "heartbeat_timeout", false)
		t.tr.Terminate()
		return false
	}
	t.missed.Add(1)
	t.queueCtl(mustEncode(protocol.Frame{Type: protocol.FramePing, Ts: float64(time.Now().UnixMilli())}))
	return true
}

// ------------------------------------------------------------ 帧的构造

func mustEncode(f protocol.Frame) []byte {
	b, err := protocol.Encode(f)
	if err != nil {
		// 只有内部构造的合法帧会走到这里。
		panic("隧道帧编码失败：" + err.Error())
	}
	return b
}

func rstFrame(id uint32, code protocol.RstCode) []byte {
	return mustEncode(protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: code})
}

func windowFrame(id uint32, credit int) []byte {
	return mustEncode(protocol.Frame{Type: protocol.FrameWindow, StreamID: id, Credit: uint32(credit)})
}

// ErrClosed 表示流所在的隧道已关闭。
var ErrClosed = errors.New("隧道已关闭")
