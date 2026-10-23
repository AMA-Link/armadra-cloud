package saas

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// ------------------------------------------------------------ 持有节点：/x/v1

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
	auth    *auth.Context
	local   func(sourceID string) *tunnel.Tunnel
	cfg     tunnel.Config
	log     *obs.Logger
	metrics *internodeMetrics

	mu       sync.Mutex
	conns    map[*internodeConn]struct{}
	stopping bool
}

func newInternodeServer(a *auth.Context, local func(string) *tunnel.Tunnel, cfg tunnel.Config, log *obs.Logger, m *internodeMetrics) *internodeServer {
	return &internodeServer{auth: a, local: local, cfg: cfg, log: log, metrics: m, conns: map[*internodeConn]struct{}{}}
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
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = ws.Close()
		return true
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	s.log.Info("对端节点已连上", obs.Node(c.peer))
	c.serve()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.log.Info("对端节点已断开", obs.Node(c.peer))
	return true
}

// close 不再收新连接并断开现有的（转来的流已随本地隧道结束）。
func (s *internodeServer) close() {
	s.mu.Lock()
	s.stopping = true
	list := make([]*internodeConn, 0, len(s.conns))
	for c := range s.conns {
		list = append(list, c)
	}
	s.mu.Unlock()
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

	mu          sync.Mutex
	streams     map[uint32]*fwdStream
	linkCredit  int // 还能往对端发多少（对端的链路接收窗口）
	recvUnacked int // 从对端收下、还没补给对端的链路级字节
	recvLeft    int // 对端还能发多少（本节点给的链路窗口）；超了是对端违反流控，断开
	queue       []outItem
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
	credit     int      // 还能往对端发多少（对端的每流接收窗口）
	pending    [][]byte // 源发来、还没信用发给对端的字节
	pendingEnd bool     // 发完 pending 之后发 END
	unacked    int      // 已交给本地隧道、还没补给对端的字节
	recvLeft   int      // 对端在这条流上还能发多少（本节点给的流窗口）
	held       int      // 本地隧道挂起（源没给信用）的字节：Resume 之后才补给对端
	peerEnded  bool
	localEnded bool
	gone       bool
}

func newInternodeConn(s *internodeServer, ws *websocket.Conn, peer string) *internodeConn {
	ws.SetReadLimit(protocol.MaxFrameBytes + 16)
	return &internodeConn{
		s: s, ws: ws, peer: peer, streams: map[uint32]*fwdStream{}, linkCredit: s.cfg.TunnelWindow, recvLeft: s.cfg.TunnelWindow,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

func (c *internodeConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}

// serve 读帧直到连接断开，然后重置全部转来的流。
func (c *internodeConn) serve() {
	go c.writer()
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
	c.mu.Lock()
	list := make([]*fwdStream, 0, len(c.streams))
	for _, f := range c.streams {
		f.gone = true
		list = append(list, f)
	}
	clear(c.streams)
	c.mu.Unlock()
	for _, f := range list {
		if f.s != nil {
			f.s.Abort()
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

// send 把一帧排进写队列（持锁调用）。
func (c *internodeConn) sendLocked(frame []byte, after func()) {
	if c.closed {
		if after != nil {
			after()
		}
		return
	}
	c.queue = append(c.queue, outItem{frame: frame, after: after})
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *internodeConn) writer() {
	for {
		select {
		case <-c.done:
			c.mu.Lock()
			rest := c.queue
			c.queue = nil
			c.mu.Unlock()
			for _, it := range rest {
				if it.after != nil {
					it.after()
				}
			}
			return
		case <-c.wake:
		}
		c.mu.Lock()
		batch := c.queue
		c.queue = nil
		c.mu.Unlock()
		for i, it := range batch {
			_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := c.ws.WriteMessage(websocket.BinaryMessage, it.frame); err != nil {
				for _, rest := range batch[i:] {
					if rest.after != nil {
						rest.after()
					}
				}
				c.closeTransport()
				return
			}
			c.s.metrics.addBytes(obs.ToClient, dataBytes(it.frame))
			if it.after != nil {
				it.after()
			}
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
		if fs != nil {
			fs.peerEnded = true
			c.releaseLocked(fs)
		}
		c.mu.Unlock()
		if fs != nil && fs.s != nil {
			fs.s.End()
		}
	case protocol.FrameRst:
		c.mu.Lock()
		fs := c.streams[f.StreamID]
		if fs != nil {
			fs.gone = true
			delete(c.streams, f.StreamID)
		}
		c.mu.Unlock()
		if fs != nil && fs.s != nil {
			fs.s.Abort()
		}
	case protocol.FrameWindow:
		c.window(f.StreamID, int(f.Credit))
	case protocol.FramePing:
		c.mu.Lock()
		c.sendLocked(encode(protocol.Frame{Type: protocol.FramePong, Ts: f.Ts}), nil)
		c.mu.Unlock()
	case protocol.FramePong, protocol.FrameGoaway:
	}
	return true
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
	fs := &fwdStream{c: c, id: id, credit: c.s.cfg.StreamWindow, recvLeft: c.s.cfg.StreamWindow}
	c.streams[id] = fs
	c.mu.Unlock()
	t := c.s.local(sourceID)
	var s *tunnel.Stream
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
			c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: id, Code: protocol.RstSourceGone}), nil)
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
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameData, StreamID: fs.id, Data: p[:n]}), func() { s.Consumed(n) })
		if n == len(p) {
			fs.pending[0] = nil
			fs.pending = fs.pending[1:]
		} else {
			fs.pending[0] = p[n:]
		}
	}
	if len(fs.pending) == 0 && fs.pendingEnd && !fs.localEnded {
		fs.localEnded = true
		c.sendLocked(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: fs.id}), nil)
		c.releaseLocked(fs)
	}
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

// Reset 是源 RST 或本地隧道没了：给对端发 RST，入口节点给客户端 4404。
func (fs *fwdStream) Reset(_ *tunnel.Stream, code protocol.RstCode) {
	c := fs.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if fs.gone {
		return
	}
	fs.gone = true
	fs.pending = nil
	if c.streams[fs.id] == fs {
		delete(c.streams, fs.id)
	}
	c.sendLocked(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: fs.id, Code: code}), nil)
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
