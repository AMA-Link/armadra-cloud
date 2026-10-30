package saas

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// ------------------------------------------------------------ 持有节点：/x/v1

// internodeCheckEvery 是已建立的 /x/v1 连接多久复查一次对端的节点令牌有没有被撤销（查的是撤销表的内存副本）；
// 到 exp 时另有定时器直接断开。缺省值；测试经 Config.InternodeCheckEvery 缩短（不改包级变量，免得与别的节点的读并发）。
const defaultInternodeCheckEvery = 5 * time.Second

// internodeIdle 是 /x/v1 连接多久没收到任何帧就当断了（对端每个心跳周期发一次 PING）。
func internodeIdle(cfg tunnel.Config) time.Duration {
	return cfg.Heartbeat * time.Duration(cfg.HeartbeatMisses+1)
}

var internodeUpgrader = websocket.Upgrader{
	ReadBufferSize:   32 << 10,
	WriteBufferSize:  32 << 10,
	WriteBufferPool:  &sync.Pool{},
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 10 * time.Second,
}

// internodeServer 收别的节点转来的通道。
type internodeServer struct {
	auth  *auth.Context
	local func(sourceID string) *tunnel.Tunnel
	cfg   tunnel.Config
	// checkEvery 是撤销复查间隔，创建后不再改。
	checkEvery time.Duration
	log        *obs.Logger
	metrics    *internodeMetrics
	// flushWait 是 close 等各连接写完已排队帧的上限，创建后不再改。
	flushWait time.Duration

	mu       sync.Mutex
	conns    map[*internodeConn]struct{}
	stopping bool
	// unsubscribe 取消撤销订阅（控制面支持 RevokedNotifier 时）。
	unsubscribe func()
}

func newInternodeServer(a *auth.Context, local func(string) *tunnel.Tunnel, log *obs.Logger, m *internodeMetrics, checkEvery time.Duration) *internodeServer {
	if checkEvery <= 0 {
		checkEvery = defaultInternodeCheckEvery
	}
	s := &internodeServer{auth: a, local: local, cfg: linkConfig(), checkEvery: checkEvery, log: log, metrics: m, flushWait: internodeFlushWait, conns: map[*internodeConn]struct{}{}}
	// 撤销广播（relay:revoked）一到就断开持这枚令牌的连接；定时复查（watch）留作兜底（广播丢了、订阅重连之间）。
	if a != nil {
		if n, ok := a.Control.(control.RevokedNotifier); ok {
			s.unsubscribe = n.OnRevoked(s.revoked)
		}
	}
	return s
}

// revoked 是撤销通知：断开用这枚节点令牌建立的连接。
func (s *internodeServer) revoked(jti string) {
	s.mu.Lock()
	var hit []*internodeConn
	for c := range s.conns {
		if c.jti == jti {
			hit = append(hit, c)
		}
	}
	s.mu.Unlock()
	for _, c := range hit {
		s.log.Warn("对端的节点令牌已撤销，断开", obs.Node(c.peer))
		c.closeTransport()
	}
}

// streams 是各入向连接上的流数。
func (s *internodeServer) streams() int {
	s.mu.Lock()
	list := make([]*internodeConn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range list {
		n += c.count()
	}
	return n
}

// pendingBytes 是各入向连接上挂着、还没信用发给对端的下行字节（测试与观测用）。
func (s *internodeServer) pendingBytes() int {
	s.mu.Lock()
	list := make([]*internodeConn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range list {
		c.mu.Lock()
		for _, fs := range c.streams {
			n += fs.pendingLen()
		}
		c.mu.Unlock()
	}
	return n
}

// HandleUpgrade 认领 /x/v1：验节点令牌（Authorization: Bearer），过了才升级。
func (s *internodeServer) HandleUpgrade(w http.ResponseWriter, r *http.Request) bool {
	if pathOf(r) != gen.PathInternode {
		return false
	}
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		httperr.Write(w, gen.ErrUnavailable, httperr.Options{})
		return true
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	res := auth.VerifyNodeToken(r.Context(), s.auth, strings.TrimSpace(token))
	if !res.OK {
		s.log.Warn("节点令牌被拒", obs.Result(res.Failure))
		httperr.Write(w, gen.ErrUnauthenticated, httperr.Options{})
		return true
	}
	ws, err := internodeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return true
	}
	c := newInternodeConn(s, ws, res.Claims.Sub)
	c.jti, c.exp = res.Claims.Jti, time.Unix(res.Claims.Exp, 0)
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = ws.Close()
		return true
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	// 验令牌与登记之间到达的撤销通知没人收：登记之后补查一次。
	if revoked, err := s.auth.Control.Tokens().IsRevoked(r.Context(), c.jti); err == nil && revoked {
		s.log.Warn("对端的节点令牌已撤销，断开", obs.Node(c.peer))
		c.closeTransport()
	}
	s.log.Info("对端节点已连上", obs.Node(c.peer))
	c.serve()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.log.Info("对端节点已断开", obs.Node(c.peer))
	return true
}

// internodeWriteTimeout 是链路上写一帧的超时：写不动就断开。
const internodeWriteTimeout = 30 * time.Second

// maxInternodeCtl 是链路控制帧队列的硬上限，与隧道同值：对端慢读又不停发控制帧就断开（#172）。
const maxInternodeCtl = tunnel.DefaultMaxCtlQueue

// internodeFlushWait 是下线时等入向连接写完已排队帧（转来的流的下行尾巴与 END）的上限。
const internodeFlushWait = 5 * time.Second

// close 不再收新连接，等各连接把已排队的帧写完（至多 flushWait），再断开（#138）。
// 本地隧道都关了：转来的流要么已 RST，要么两端都已 END、只剩出队列里的下行尾巴与 END；
// 这时立刻断开会把尾巴丢掉，对端节点的客户端收不全源最后发的字节。
func (s *internodeServer) close() {
	s.mu.Lock()
	s.stopping = true
	unsubscribe := s.unsubscribe
	s.unsubscribe = nil
	list := make([]*internodeConn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	deadline := time.Now().Add(s.flushWait)
	for _, c := range list {
		for !c.flushed() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, c := range list {
		c.closeTransport()
	}
}

// outItem 是写协程要发的一帧；after 在帧写进内核之后调（转来的下行字节此时才补源的信用）。
type outItem struct {
	frame []byte
	after func()
}

// internodeConn 是一条入向的 /x/v1：对端是开流方，本节点把每条流接进本地隧道。
// 读在 serve 的协程里；写只在写协程里。信用两层都照协议值：对端给的窗口（链路 TunnelWindow、
// 每流 StreamWindow）约束本节点往回发，本节点按同样的窗口给对端补 WINDOW。
type internodeConn struct {
	s    *internodeServer
	ws   *websocket.Conn
	peer string
	// jti 与 exp 是对端出示的节点令牌：到期或被撤销就断开（watch）。
	jti string
	exp time.Time

	mu          sync.Mutex
	streams     map[uint32]*fwdStream
	linkCredit  int                      // 还能往对端发多少（对端的链路接收窗口）
	recvUnacked int                      // 从对端收下、还没补给对端的链路级字节
	recvLeft    int                      // 对端还能发多少（本节点给的链路窗口）；超了是对端违反流控，断开
	ctl         []outItem                // 控制帧（WINDOW、PONG、RST）：每批先写；超过 maxInternodeCtl 整条丢掉并断开
	pongAt      int                      // ctl 里待发 PONG 的下标（-1 没有）：再来 PING 只替换（RFC 6455 §5.5.3）
	overflow    bool                     // 控制帧队列超限：receive 据此断开
	sched       tunnel.Sched[*fwdStream] // 各流的 DATA / END：按流轮转、稀疏流优先、按字节预算分批（#57）
	queued      int                      // 各流出队列里还没取走的帧数（含已摘表的流）
	writing     bool                     // 写协程正在写一批
	wake        chan struct{}
	done        chan struct{}
	closed      bool
	once        sync.Once
}

// fwdStream 是一条转来的流：对端那边的流号 id，本节点隧道上的流 s。
type fwdStream struct {
	c  *internodeConn
	id uint32
	s  *tunnel.Stream

	// 以下由 c.mu 守护。
	out        []outItem   // 已扣过信用、等写协程出队的 DATA / END
	outN       int         // out 里的 DATA 载荷字节（随入队 / 出队增减，免得每次入队遍历）
	flow       tunnel.Flow // 在 c.sched 里的状态
	credit     int         // 还能往对端发多少（对端的每流接收窗口）
	pending    [][]byte    // 源发来、还没信用发给对端的字节
	pendingEnd bool        // 发完 pending 之后发 END
	unacked    int         // 已交给本地隧道、还没补给对端的字节
	recvLeft   int         // 对端在这条流上还能发多少（本节点给的流窗口）
	held       int         // 本地隧道挂起（源没给信用）的字节：Resume 之后才补给对端
	peerEnded  bool
	localEnded bool
	gone       bool
}

func newInternodeConn(s *internodeServer, ws *websocket.Conn, peer string) *internodeConn {
	ws.SetReadLimit(protocol.MaxFrameBytes + 16)
	return &internodeConn{
		s: s, ws: ws, peer: peer, streams: map[uint32]*fwdStream{}, linkCredit: s.cfg.TunnelWindow, recvLeft: s.cfg.TunnelWindow,
		wake: make(chan struct{}, 1), done: make(chan struct{}), pongAt: -1,
	}
}

// flushed 答已排队的帧是否都写出去了（或连接已关）。挂着等链路或流信用的下行字节、
// 还没排进出队列的 END 也算没写完：对端补了信用它们才会排队写出（close 的上限兜底）。
func (c *internodeConn) flushed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return true
	}
	if len(c.ctl) > 0 || c.queued > 0 || c.writing {
		return false
	}
	for _, fs := range c.streams {
		if len(fs.pending) > 0 || (fs.pendingEnd && !fs.localEnded) {
			return false
		}
	}
	return true
}

func (c *internodeConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}

// serve 读帧直到连接断开，然后重置全部转来的流。
func (c *internodeConn) serve() {
	go c.writer()
	go c.watch()
	idle := internodeIdle(c.s.cfg)
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(idle))
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.BinaryMessage {
			break
		}
		if !c.receive(data) {
			break
		}
	}
	c.closeTransport()
	c.resetAll()
}

// resetAll 连接断开后重置全部转来的流：挂着的下行字节还给本地隧道，再中止本地隧道上的流
// （两端都已 END 的流不中止，已排进出队列的上行尾巴与 END 照常写给源，#136）。
func (c *internodeConn) resetAll() {
	c.mu.Lock()
	type dropped struct {
		s *tunnel.Stream
		n int
	}
	list := make([]dropped, 0, len(c.streams))
	for _, f := range c.streams {
		list = append(list, dropped{f.s, f.dropLocked()})
	}
	clear(c.streams)
	c.mu.Unlock()
	for _, d := range list {
		if d.s != nil {
			d.s.Consumed(d.n)
			d.s.Abort()
		}
	}
}

// watch 在节点令牌到期时断开，并定期查撤销表（armadra-cloud nodes revoke 之后已建立的连接也断）。
func (c *internodeConn) watch() {
	if c.jti == "" {
		return
	}
	expire := time.NewTimer(time.Until(c.exp))
	defer expire.Stop()
	check := time.NewTicker(c.s.checkEvery)
	defer check.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-expire.C:
			c.s.log.Warn("对端的节点令牌到期，断开", obs.Node(c.peer))
			c.closeTransport()
			return
		case <-check.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			revoked, err := c.s.auth.Control.Tokens().IsRevoked(ctx, c.jti)
			cancel()
			if err == nil && revoked {
				c.s.log.Warn("对端的节点令牌已撤销，断开", obs.Node(c.peer))
				c.closeTransport()
				return
			}
		}
	}
}

func (c *internodeConn) closeTransport() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
		_ = c.ws.Close()
	})
}

// sendLocked 把一个控制帧排进写队列（持锁调用）；写协程每批先写全部控制帧。
func (c *internodeConn) sendLocked(frame []byte, after func()) {
	if c.closed {
		if after != nil {
			after()
		}
		return
	}
	if c.overflow {
		if after != nil {
			after()
		}
		return
	}
	if len(c.ctl) >= maxInternodeCtl {
		// 队列顶爆：整条丢掉（after 照常回调，还信用），标记溢出；读协程看到就断开。
		dropped := c.ctl
		c.ctl, c.pongAt, c.overflow = nil, -1, true
		for _, it := range dropped {
			if it.after != nil {
				it.after()
			}
		}
		if after != nil {
			after()
		}
		if c.s != nil && c.s.log != nil {
			c.s.log.Warn("节点间链路控制帧队列超限，断开", obs.Node(c.peer), obs.Reason(tunnel.ReasonCtlQueueOverflow))
		}
		return
	}
	c.ctl = append(c.ctl, outItem{frame: frame, after: after})
	c.signalLocked()
}

// pongLocked 排一条 PONG：队列里已有待发的 PONG 就只换成最新的，对端刷 PING 也至多占一个位置。
func (c *internodeConn) pongLocked(ts float64) {
	frame := encode(protocol.Frame{Type: protocol.FramePong, Ts: ts})
	if c.pongAt >= 0 && c.pongAt < len(c.ctl) {
		c.ctl[c.pongAt] = outItem{frame: frame}
		c.signalLocked()
		return
	}
	c.sendLocked(frame, nil)
	if !c.overflow {
		c.pongAt = len(c.ctl) - 1
	}
}

// sendStreamLocked 把一条流的 DATA / END 排进这条流自己的出队列（持锁调用）：流内保持顺序，
// 流之间由调度器轮转，一条流积压再多也不会把别的流挡在后面。
func (c *internodeConn) sendStreamLocked(fs *fwdStream, frame []byte, after func()) {
	if c.closed {
		if after != nil {
			after()
		}
		return
	}
	fs.out = append(fs.out, outItem{frame: frame, after: after})
	fs.outN += dataBytes(frame)
	c.queued++
	c.sched.Add(fs, &fs.flow, fs.outN)
	c.signalLocked()
}

func (c *internodeConn) signalLocked() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// collectLocked 取出这一批要写的帧：全部控制帧，再按调度器给各流出帧；all 为真时取尽（关闭时用）。
func (c *internodeConn) collectLocked(all bool) (batch []outItem, base uint64, ctlN int) {
	batch = c.ctl
	c.ctl = nil
	c.pongAt = -1
	ctlN = len(batch)
	budget := tunnel.WriteBudget
	if all {
		budget = int(^uint(0) >> 1)
	}
	base = c.sched.Drain(budget, func(fs *fwdStream) (int, bool) {
		if len(fs.out) == 0 {
			return -1, false
		}
		if all {
			size := 0
			for _, it := range fs.out {
				size += dataBytes(it.frame)
			}
			batch = append(batch, fs.out...)
			c.queued -= len(fs.out)
			fs.out, fs.outN = nil, 0
			return size, false
		}
		it := fs.out[0]
		fs.out[0] = outItem{}
		fs.out = fs.out[1:]
		c.queued--
		batch = append(batch, it)
		fs.outN -= dataBytes(it.frame)
		more := len(fs.out) > 0
		if !more {
			fs.out = nil
		}
		return dataBytes(it.frame), more
	})
	return batch, base, ctlN
}

// abandon 连接关了：还没写出的帧不写，只执行它们的 after（把字节的信用还给本地隧道）。
func (c *internodeConn) abandon() {
	c.mu.Lock()
	rest, _, _ := c.collectLocked(true)
	c.mu.Unlock()
	for _, it := range rest {
		if it.after != nil {
			it.after()
		}
	}
}

func (c *internodeConn) writer() {
	for {
		c.mu.Lock()
		batch, base, ctlN := c.collectLocked(false)
		c.writing = len(batch) > 0
		c.mu.Unlock()
		if len(batch) == 0 {
			select {
			case <-c.done:
				c.abandon()
				return
			case <-c.wake:
			}
			continue
		}
		for i, it := range batch {
			_ = c.ws.SetWriteDeadline(time.Now().Add(internodeWriteTimeout))
			if err := c.ws.WriteMessage(websocket.BinaryMessage, it.frame); err != nil {
				for _, rest := range batch[i:] {
					if rest.after != nil {
						rest.after()
					}
				}
				c.closeTransport()
				c.abandon()
				return
			}
			c.s.metrics.addBytes(obs.ToClient, dataBytes(it.frame))
			if i >= ctlN {
				c.sched.NoteSent(base + uint64(i-ctlN+1))
			}
			if it.after != nil {
				it.after()
			}
		}
		select {
		case <-c.done:
			c.abandon()
			return
		default:
		}
	}
}

// dataBytes 是一帧 DATA 的载荷字节数（别的帧答 0）。
func dataBytes(frame []byte) int {
	if len(frame) > protocol.FrameHeaderBytes && protocol.FrameType(frame[0]) == protocol.FrameData {
		return len(frame) - protocol.FrameHeaderBytes
	}
	return 0
}

func encode(f protocol.Frame) []byte {
	b, err := protocol.Encode(f)
	if err != nil {
		panic("节点间帧编码失败：" + err.Error())
	}
	return b
}

// receive 处理对端的一帧；答 false 表示协议错误，断开连接。
func (c *internodeConn) receive(msg []byte) bool {
	f, err := protocol.DecodeView(msg)
	if err != nil {
		return false
	}
	switch f.Type {
	case protocol.FrameOpen:
		c.open(f.StreamID, f.Open)
	case protocol.FrameData:
		return c.data(f.StreamID, f.Data)
	case protocol.FrameEnd:
		c.mu.Lock()
		fs := c.streams[f.StreamID]
		var s *tunnel.Stream
		if fs != nil {
			fs.peerEnded = true
			s = fs.s
			c.releaseLocked(fs)
		}
		c.mu.Unlock()
		if s != nil {
			s.End()
		}
	case protocol.FrameRst:
		c.mu.Lock()
		fs := c.streams[f.StreamID]
		dropped := 0
		var s *tunnel.Stream
		var afters []func()
		if fs != nil {
			dropped = fs.dropLocked()
			delete(c.streams, f.StreamID)
			afters = fs.takeOutLocked()
			s = fs.s
		}
		c.mu.Unlock()
		runAll(afters)
		if s != nil {
			s.Consumed(dropped)
			s.Abort()
		}
	case protocol.FrameWindow:
		c.window(f.StreamID, int(f.Credit))
	case protocol.FramePing:
		c.mu.Lock()
		c.pongLocked(f.Ts)
		c.mu.Unlock()
	case protocol.FramePong, protocol.FrameGoaway:
	}
	c.mu.Lock()
	overflow := c.overflow
	c.mu.Unlock()
	return !overflow
}

// open 接一条转来的流：目标源在 edge.InternodeOpen 的路径里，只认本节点手里的隧道；没有就 RST
// （入口节点给客户端 4404）。
func (c *internodeConn) open(id uint32, open *gen.OpenPayload) {
	sourceID, ok := edge.ParseInternodeOpen(open)
	c.mu.Lock()
	if _, dup := c.streams[id]; dup || !ok {
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: protocol.RstProtocol}), nil)
		c.mu.Unlock()
		return
	}
	if len(c.streams) >= c.s.cfg.MaxStreams {
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: protocol.RstInternal}), nil)
		c.mu.Unlock()
		return
	}
	fs := &fwdStream{c: c, id: id, credit: c.s.cfg.StreamWindow, recvLeft: c.s.cfg.StreamWindow}
	c.streams[id] = fs
	c.mu.Unlock()
	t := c.s.local(sourceID)
	var s *tunnel.Stream
	code := protocol.RstSourceGone
	if t != nil && !t.HasCapability(gen.E2ECapability) {
		// 源没报 e2e：refused，入口节点以 4404 e2e_unsupported 关客户端（与本地判定同一结果）。
		t, code = nil, protocol.RstRefused
	}
	if t != nil {
		// 转给源的是标准的 e2e OPEN；remoteIp、clientOrigin、relayNode 是入口节点填的。
		forward := gen.OpenPayload{
			Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{},
			ClientOrigin: open.ClientOrigin, RemoteIP: open.RemoteIP, RelayNode: open.RelayNode,
		}
		s, _ = t.OpenStream(&forward, fs, nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s == nil {
		if !fs.gone {
			delete(c.streams, id)
			fs.gone = true
			c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: code}), nil)
		}
		return
	}
	fs.s = s
	if fs.gone {
		// 开流期间对端已经 RST 或连接断了。
		go s.Abort()
	}
}

// data 是客户端 → 源：交给本地隧道；交出去了才给对端补信用（源没信用时等 Resume）。
// 对端超出给它的窗口（链路级或流级）答 false，断开连接。
func (c *internodeConn) data(id uint32, p []byte) bool {
	n := len(p)
	c.mu.Lock()
	if n > c.recvLeft {
		c.mu.Unlock()
		return false
	}
	c.recvLeft -= n
	c.recvUnacked += n
	if c.recvUnacked >= c.s.cfg.TunnelWindow/2 {
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameWindow, Credit: uint32(c.recvUnacked)}), nil)
		c.recvLeft += c.recvUnacked
		c.recvUnacked = 0
	}
	fs := c.streams[id]
	if fs == nil || fs.s == nil || fs.peerEnded {
		c.mu.Unlock()
		return true
	}
	if n > fs.recvLeft {
		c.mu.Unlock()
		return false
	}
	fs.recvLeft -= n
	s := fs.s
	c.mu.Unlock()
	c.s.metrics.addBytes(obs.ToSource, n)
	sent := s.Send(p)
	c.mu.Lock()
	if sent {
		fs.unacked += n
		c.grantLocked(fs, false)
	} else {
		fs.held += n
	}
	c.mu.Unlock()
	return true
}

// grantLocked 攒到半个窗口（或 force）就给对端补这条流的 WINDOW。
func (c *internodeConn) grantLocked(fs *fwdStream, force bool) {
	if fs.gone || fs.unacked == 0 {
		return
	}
	if force || fs.unacked >= c.s.cfg.StreamWindow/2 {
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameWindow, StreamID: fs.id, Credit: uint32(fs.unacked)}), nil)
		fs.recvLeft += fs.unacked
		fs.unacked = 0
	}
}

// window 是对端补的信用：链路级（id 0）或某条流。
func (c *internodeConn) window(id uint32, credit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 {
		c.linkCredit += credit
		for _, fs := range c.streams {
			c.flushLocked(fs)
		}
		return
	}
	if fs := c.streams[id]; fs != nil {
		fs.credit += credit
		c.flushLocked(fs)
	}
}

// flushLocked 按两层信用把挂着的下行字节发给对端；发完了且源已 END 就发 END。
func (c *internodeConn) flushLocked(fs *fwdStream) {
	for len(fs.pending) > 0 && fs.credit > 0 && c.linkCredit > 0 {
		p := fs.pending[0]
		n := min(len(p), fs.credit, c.linkCredit, protocol.MaxDataChunk)
		fs.credit -= n
		c.linkCredit -= n
		s := fs.s
		c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameData, StreamID: fs.id, Data: p[:n]}), func() { s.Consumed(n) })
		if n == len(p) {
			fs.pending[0] = nil
			fs.pending = fs.pending[1:]
		} else {
			fs.pending[0] = p[n:]
		}
	}
	if len(fs.pending) == 0 && fs.pendingEnd && !fs.localEnded {
		fs.localEnded = true
		c.sendStreamLocked(fs, encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: fs.id}), nil)
		c.releaseLocked(fs)
	}
}

// dropLocked 作废这条流：丢掉挂着的下行字节，答丢了多少（调用方在锁外 s.Consumed 还给本地隧道）。
func (fs *fwdStream) dropLocked() int {
	n := fs.pendingLen()
	fs.gone = true
	fs.pending = nil
	return n
}

// takeOutLocked 取走已排进出队列、还没写出的 DATA / END（RST 之后对端不该再收到它们），
// 返回这些帧的 after（把已扣的信用还给本地隧道），调用方在锁外执行。
// 这些 DATA 入队时扣过链路信用、对端永远收不到，也不会为它们补 WINDOW(0)：连接还开着就当场还回链路信用，
// 再让因链路信用挂着的流接着发（#134）。写协程已经取走的帧不在 out 里，照常写出、由对端补 WINDOW。
// 调用方先把 fs 摘出流表，免得它自己被重新排进出队列。
func (fs *fwdStream) takeOutLocked() []func() {
	c := fs.c
	var afters []func()
	for _, it := range fs.out {
		if it.after != nil {
			afters = append(afters, it.after)
		}
	}
	refund := fs.outN
	c.queued -= len(fs.out)
	fs.out = nil
	fs.outN = 0
	if refund > 0 && !c.closed {
		c.linkCredit += refund
		for _, other := range c.streams {
			if c.linkCredit <= 0 {
				break
			}
			c.flushLocked(other)
		}
	}
	return afters
}

func runAll(list []func()) {
	for _, f := range list {
		f()
	}
}

// pendingLen 是挂着的下行字节数（持 c.mu 调用）。
func (fs *fwdStream) pendingLen() int {
	n := 0
	for _, p := range fs.pending {
		n += len(p)
	}
	return n
}

// releaseLocked 两头都结束了就从流表摘掉。
func (c *internodeConn) releaseLocked(fs *fwdStream) {
	if fs.peerEnded && fs.localEnded && c.streams[fs.id] == fs {
		delete(c.streams, fs.id)
		fs.gone = true
	}
}

// ------------------------------------------------------------ tunnel.Sink（本地隧道的回调）

// Data 是源 → 客户端：按信用发给对端，字节写进内核之后才给源补信用。
func (fs *fwdStream) Data(s *tunnel.Stream, p []byte) {
	c := fs.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if fs.s == nil {
		fs.s = s
	}
	if fs.gone || c.closed {
		s.Consumed(len(p))
		return
	}
	fs.pending = append(fs.pending, append([]byte(nil), p...))
	c.flushLocked(fs)
}

// End 是源发了 END：挂着的字节发完之后给对端发 END。
func (fs *fwdStream) End(s *tunnel.Stream) {
	c := fs.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if fs.gone {
		return
	}
	fs.pendingEnd = true
	c.flushLocked(fs)
}

// Reset 是源 RST 或本地隧道没了：给对端发 RST，入口节点给客户端 4404。挂着的下行字节作废，
// 信用还给本地隧道（隧道还在、只是这条流被源重置时，不还会永久漏掉隧道级窗口）。
//
// 链路上的 refused 只表示「持有节点上的源没报 e2e」（open 里自己判的），入口节点据此答 e2e_unsupported；
// 源自己发来的 refused 若原样转回，客户端会看到 e2e_unsupported，本地却是 source_offline，
// 所以这里改成 sourceGone 再转出。
func (fs *fwdStream) Reset(s *tunnel.Stream, code protocol.RstCode) {
	if code == protocol.RstRefused {
		code = protocol.RstSourceGone
	}
	c := fs.c
	c.mu.Lock()
	if fs.gone {
		c.mu.Unlock()
		return
	}
	dropped := fs.dropLocked()
	if c.streams[fs.id] == fs {
		delete(c.streams, fs.id)
	}
	afters := fs.takeOutLocked()
	c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: fs.id, Code: code}), nil)
	c.mu.Unlock()
	runAll(afters)
	s.Consumed(dropped)
}

// Resume 是本地隧道挂起的上行字节发完了：把扣着的信用补给对端。
func (fs *fwdStream) Resume(*tunnel.Stream) {
	c := fs.c
	c.mu.Lock()
	defer c.mu.Unlock()
	fs.unacked += fs.held
	fs.held = 0
	c.grantLocked(fs, true)
}

var _ tunnel.Sink = (*fwdStream)(nil)
