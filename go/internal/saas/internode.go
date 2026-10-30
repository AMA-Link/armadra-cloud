package saas

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 节点间转发（Go 设计 §3.6，关掉 #51）：客户端连到入口节点 A，源的隧道在持有节点 B。A 经一条常驻的
// 节点间 WebSocket（B 的 /x/v1，A 出示节点令牌）把这条端到端通道转过去。
//
//   - 线上直接复用隧道帧：A 发 OPEN（edge.InternodeOpen：kind ws、路径 /s/<sourceId>/_relay/e2e，因为
//     e2e 的 OPEN 规定路径固定、不带头，没处放目标源），之后 DATA / WINDOW / END / RST，信用逐跳计；
//     A 定时 PING，B 答 PONG。B 以标准的 e2e OPEN 转给源。
//   - A 这一侧就是一条 tunnel.Tunnel（A 是开流方，与中继对源同一个角色），边缘照常在它上面开流；
//   - B 这一侧（internodeConn）把每条转来的流当作本地边缘流接进本节点的隧道（不再验中继令牌、不计流量）。
//   - 鉴权、限流、并发与流量只在入口节点：A 验过中继令牌，知道 sub、dev、team。

// LinkMaxStreams 是一条节点间连接上的流数上限（入口节点上经这个对端转发的全部通道，跨租户共用）。
const LinkMaxStreams = 4096

// linkConfig 是节点间连接的限制（两端同一份）：流数上限 LinkMaxStreams；链路级窗口取「流数上限 × 流窗」
// = 1 GiB，链路级信用永远不是瓶颈——慢客户端只占满自己那条流的 256 KiB 流窗，拖不住同一条链路上的别人。
// 内存上界：每条链路每一端最多「在途的流数 × 256 KiB」（4096 条流全卡住时 1 GiB；实际受边缘的
// 账号 / 团队并发上限约束，远小于此）。流窗、帧大小与心跳仍是协议值。
func linkConfig() tunnel.Config {
	c := tunnel.DefaultConfig()
	c.MaxStreams = LinkMaxStreams
	c.TunnelWindow = LinkMaxStreams * c.StreamWindow
	return c
}

// linkDialTimeout 是连对端 /x/v1 的超时；linkRetry 是连不上之后多久内不再试（客户端直接 4404）。
const (
	linkDialTimeout = 5 * time.Second
	linkRetry       = 2 * time.Second
)

// internodeMetrics 是节点间转发的指标（在个人中转的指标之后输出，原有指标逐字节不变）。
type internodeMetrics struct {
	toSource, toClient atomic.Int64
	remote             atomic.Int64
	// remoteFailed 是路由指向别的节点、却拿不到到那个节点的连接（连不上、没配节点令牌）而没转出去的通道。
	remoteFailed atomic.Int64
	streams      func() int
	// unwritten 是入口节点出向连接上还没写出的转发字节（挂着等信用的上行 + 已排队没写进内核的）。
	unwritten func() int64
}

func (m *internodeMetrics) addBytes(d obs.Direction, n int) {
	if d == obs.ToSource {
		m.toSource.Add(int64(n))
	} else {
		m.toClient.Add(int64(n))
	}
}

// render 输出 relay_internode_streams、relay_internode_bytes_total{direction}、relay_routes_remote_total、
// relay_routes_remote_failed_total。
func (m *internodeMetrics) render() string {
	var b strings.Builder
	b.WriteString("# HELP relay_internode_streams Streams on inter-node links of this node.\n")
	b.WriteString("# TYPE relay_internode_streams gauge\n")
	b.WriteString("relay_internode_streams " + itoa(int64(m.streams())) + "\n")
	b.WriteString("# HELP relay_internode_bytes_total Payload bytes over inter-node links.\n")
	b.WriteString("# TYPE relay_internode_bytes_total counter\n")
	b.WriteString(`relay_internode_bytes_total{direction="to_source"} ` + itoa(m.toSource.Load()) + "\n")
	b.WriteString(`relay_internode_bytes_total{direction="to_client"} ` + itoa(m.toClient.Load()) + "\n")
	if m.unwritten != nil {
		b.WriteString("# HELP relay_internode_unwritten_bytes Forwarded payload bytes queued on this node's outbound inter-node links but not yet written.\n")
		b.WriteString("# TYPE relay_internode_unwritten_bytes gauge\n")
		b.WriteString("relay_internode_unwritten_bytes " + itoa(m.unwritten()) + "\n")
	}
	b.WriteString("# HELP relay_routes_remote_total Client channels forwarded to the node holding the tunnel.\n")
	b.WriteString("# TYPE relay_routes_remote_total counter\n")
	b.WriteString("relay_routes_remote_total " + itoa(m.remote.Load()) + "\n")
	b.WriteString("# HELP relay_routes_remote_failed_total Client channels whose tunnel is on another node but could not be forwarded (no link to that node).\n")
	b.WriteString("# TYPE relay_routes_remote_failed_total counter\n")
	b.WriteString("relay_routes_remote_failed_total " + itoa(m.remoteFailed.Load()) + "\n")
	return b.String()
}

// ------------------------------------------------------------ 入口节点：到对端的常驻连接

type dialCall struct {
	done chan struct{}
	t    *tunnel.Tunnel
}

// links 是本节点到各对端的常驻连接（每个对端一条，断了下次用到时重连）。
type links struct {
	self     string
	token    string
	insecure bool
	client   goredis.UniversalClient
	prefix   string
	cfg      tunnel.Config
	log      *obs.Logger
	metrics  *internodeMetrics
	dialer   *websocket.Dialer

	mu      sync.Mutex
	byNode  map[string]*tunnel.Tunnel
	dialing map[string]*dialCall
	failed  map[string]time.Time
	closed  bool
}

func newLinks(self, token string, insecure bool, client goredis.UniversalClient, prefix string, log *obs.Logger, m *internodeMetrics) *links {
	return &links{
		self: self, token: token, insecure: insecure, client: client, prefix: prefix, cfg: linkConfig(), log: log, metrics: m,
		dialer: &websocket.Dialer{HandshakeTimeout: linkDialTimeout, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10},
		byNode: map[string]*tunnel.Tunnel{}, dialing: map[string]*dialCall{}, failed: map[string]time.Time{},
	}
}

// streams 是本节点各出向连接上的流数。
func (l *links) streams() int {
	l.mu.Lock()
	list := make([]*tunnel.Tunnel, 0, len(l.byNode))
	for _, t := range l.byNode {
		list = append(list, t)
	}
	l.mu.Unlock()
	n := 0
	for _, t := range list {
		n += t.StreamCount()
	}
	return n
}

// unwrittenBytes 是各出向连接上还没写出的转发字节。
func (l *links) unwrittenBytes() int64 {
	l.mu.Lock()
	list := make([]*tunnel.Tunnel, 0, len(l.byNode))
	for _, t := range l.byNode {
		list = append(list, t)
	}
	l.mu.Unlock()
	var n int64
	for _, t := range list {
		n += int64(t.PendingBytes())
	}
	return n
}

// get 答到对端 node 的连接（还能开流的）；连不上答 nil（调用方给客户端 4404）。同一对端同时只拨一次。
func (l *links) get(ctx context.Context, node string) *tunnel.Tunnel {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	if t := l.byNode[node]; t != nil && t.Accepting() {
		l.mu.Unlock()
		return t
	}
	if at, ok := l.failed[node]; ok && time.Since(at) < linkRetry {
		l.mu.Unlock()
		return nil
	}
	call := l.dialing[node]
	if call == nil {
		call = &dialCall{done: make(chan struct{})}
		l.dialing[node] = call
		go l.dial(node, call)
	}
	l.mu.Unlock()
	select {
	case <-call.done:
		return call.t
	case <-ctx.Done():
		return nil
	}
}

func (l *links) dial(node string, call *dialCall) {
	t, err := l.connect(node)
	l.mu.Lock()
	delete(l.dialing, node)
	if err != nil || l.closed {
		l.failed[node] = time.Now()
		l.mu.Unlock()
		if t != nil {
			t.Close(protocol.CloseGoingAway, "shutdown")
		}
		if err != nil {
			l.log.Warn("连不上对端节点，通道不转发", dialFields(node, err)...)
		}
		close(call.done)
		return
	}
	delete(l.failed, node)
	l.byNode[node] = t
	call.t = t
	l.mu.Unlock()
	l.log.Info("节点间连接已建立", obs.Node(node))
	close(call.done)
}

// connect 读节点表找对端的 /x/v1，出示节点令牌升级，建一条隧道（本节点是开流方）。失败答 *dialFailure（带分类）。
func (l *links) connect(node string) (*tunnel.Tunnel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), linkDialTimeout)
	defer cancel()
	info, err := ReadNode(ctx, l.client, l.prefix, node)
	if err != nil {
		return nil, &dialFailure{msg: "读不到节点表", reason: dialNodeLookup, cause: err}
	}
	if info == nil {
		return nil, &dialFailure{msg: "对端不在节点表里", reason: dialNodeUnknown}
	}
	target := info.InternodeURL
	if target == "" {
		u, err := url.Parse(info.URL)
		if err != nil {
			return nil, &dialFailure{msg: "对端的隧道地址不合格式", reason: dialBadURL}
		}
		u.Path, u.RawQuery = gen.PathInternode, ""
		target = u.String()
	}
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = u.Host
	}
	// 节点表里的地址同样要是 wss（开发时 RELAY_ALLOW_INSECURE_TUNNEL 才许 ws），不往明文或别的协议上送节点令牌。
	if !wsURL(target, l.insecure) {
		return nil, &dialFailure{msg: "对端的 /x/v1 地址不是 wss", reason: dialInsecureURL, host: host}
	}
	conn, resp, err := l.dialer.DialContext(ctx, target, http.Header{"Authorization": {"Bearer " + l.token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, &dialFailure{msg: "对端拒绝升级：" + resp.Status, reason: dialRejected, status: resp.StatusCode, host: host, cause: err}
		}
		return nil, &dialFailure{msg: "连不上对端", reason: classifyDial(err), host: host, cause: err}
	}
	sock := tunnel.NewSocket(conn)
	var t *tunnel.Tunnel
	t = tunnel.New("x."+randomHex(8), "", "", sock, l.cfg, tunnel.Hooks{
		OnBytes: l.metrics.addBytes,
		OnClose: func(info tunnel.CloseInfo) {
			l.mu.Lock()
			if l.byNode[node] == t {
				delete(l.byNode, node)
			}
			l.mu.Unlock()
			l.log.Info("节点间连接已关闭", obs.Node(node), obs.Code(int(info.Code)), obs.Reason(info.Reason))
		},
	})
	// 不替远端的源声明能力：源报没报 e2e 由持有节点判，没报时答 RST refused，入口节点照本地的规矩
	// 以 4404 e2e_unsupported 关客户端。
	t.SetLink(node)
	t.Start()
	go func() {
		for {
			binary, data, err := sock.ReadMessage()
			if err != nil {
				t.TransportClosed(closeCodeOf(err), "")
				return
			}
			if !binary {
				t.Close(protocol.CloseBadFrame, "text_on_link")
				continue
			}
			t.Receive(data)
		}
	}()
	return t, nil
}

// 连对端失败的分类（日志的 reason 字段，取值固定）：排障时据此区分名字解析、拒绝连接、证书、对端拒绝升级等，
// 不必记错误的 message（message 里可能带着对端给的内容）。
const (
	dialNodeLookup  = "node_lookup"  // 读节点表出错（Redis）
	dialNodeUnknown = "node_unknown" // 节点表里没有这个对端
	dialBadURL      = "bad_url"      // 节点表里的地址不合格式
	dialInsecureURL = "insecure_url" // 地址不是 wss（没开 RELAY_ALLOW_INSECURE_TUNNEL）
	dialDNS         = "dns"          // 名字解析失败
	dialRefused     = "refused"      // 对端拒绝连接
	dialReset       = "reset"        // 连接被对端重置
	dialUnreachable = "unreachable"  // 网络或主机不可达
	dialTimeout     = "timeout"      // 连接或握手超时
	dialTLS         = "tls"          // 证书或 TLS 握手失败
	dialRejected    = "rejected"     // 对端答了 HTTP 状态而不是升级（code 字段是状态码，401 是节点令牌被拒）
	dialNetwork     = "network"      // 其余网络错误
)

// dialFailure 是连对端 /x/v1 失败：reason 是上面的分类，status 是对端拒绝升级时的 HTTP 状态，host 是节点表里
// 对端地址的主机与端口（不含路径与 query），cause 是底层错误（日志只记它的类型）。
type dialFailure struct {
	msg    string
	reason string
	status int
	host   string
	cause  error
}

func (f *dialFailure) Error() string { return f.msg }
func (f *dialFailure) Unwrap() error { return f.cause }

// classifyDial 把拨号错误归到固定的分类；只看错误的类型与系统错误码，不看 message。
func classifyDial(err error) string {
	var (
		dnsErr     *net.DNSError
		verifyErr  *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
		alertErr   tls.AlertError
		recordErr  tls.RecordHeaderError
		netErr     net.Error
	)
	switch {
	case errors.As(err, &dnsErr):
		return dialDNS
	case errors.As(err, &verifyErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalidErr),
		errors.As(err, &alertErr), errors.As(err, &recordErr):
		return dialTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return dialRefused
	case errors.Is(err, syscall.ECONNRESET):
		return dialReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return dialUnreachable
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return dialTimeout
	}
	return dialNetwork
}

// dialFields 是「连不上对端节点」那一行的字段：节点、分类、拒绝升级时的状态码、对端的主机与端口、底层错误的类型。
func dialFields(node string, err error) []obs.Field {
	fields := []obs.Field{obs.Node(node)}
	var f *dialFailure
	if !errors.As(err, &f) {
		return append(fields, obs.Error(err))
	}
	fields = append(fields, obs.Reason(f.reason))
	if f.status != 0 {
		fields = append(fields, obs.Code(f.status))
	}
	if f.host != "" {
		fields = append(fields, obs.Host(f.host))
	}
	return append(fields, obs.Error(f.cause))
}

// close 关掉全部出向连接（下线时，边缘连接都断了之后）。
func (l *links) close() {
	l.mu.Lock()
	l.closed = true
	list := make([]*tunnel.Tunnel, 0, len(l.byNode))
	for _, t := range l.byNode {
		list = append(list, t)
	}
	l.mu.Unlock()
	for _, t := range list {
		t.Close(protocol.CloseGoingAway, "shutdown")
	}
}

func closeCodeOf(err error) protocol.CloseCode {
	var ce interface{ CloseCode() int }
	if errors.As(err, &ce) {
		return protocol.CloseCode(ce.CloseCode())
	}
	return 1006
}

// lookup 是 SaaS 节点给边缘的找隧道：路由指向本节点 → 本地隧道；指向别的节点 → 到那个节点的连接
// （有节点令牌时）；都没有答 nil（4404）。指向别的节点却没拿到连接的计进 relay_routes_remote_failed_total。
func lookup(routes control.RouteTable, self string, local func(string) *tunnel.Tunnel, l *links, m *internodeMetrics) func(context.Context, string) (*tunnel.Tunnel, error) {
	return func(ctx context.Context, sourceID string) (*tunnel.Tunnel, error) {
		route, err := routes.Get(ctx, sourceID)
		if err != nil || route == nil {
			return nil, err
		}
		if route.Node == self {
			t := local(sourceID)
			if t == nil || t.ID() != route.TunnelID {
				return nil, nil
			}
			return t, nil
		}
		var t *tunnel.Tunnel
		if l != nil {
			t = l.get(ctx, route.Node)
		}
		if t != nil {
			m.remote.Add(1)
		} else {
			m.remoteFailed.Add(1)
		}
		return t, nil
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
