package saas

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// linkDialTimeout 是连对端 /x/v1 的超时；linkRetry 是连不上之后多久内不再试（客户端直接 4404）。
const (
	linkDialTimeout = 5 * time.Second
	linkRetry       = 2 * time.Second
)

// internodeMetrics 是节点间转发的指标（在个人中转的指标之后输出，原有指标逐字节不变）。
type internodeMetrics struct {
	toSource, toClient atomic.Int64
	remote             atomic.Int64
	streams            func() int
}

func (m *internodeMetrics) addBytes(d obs.Direction, n int) {
	if d == obs.ToSource {
		m.toSource.Add(int64(n))
	} else {
		m.toClient.Add(int64(n))
	}
}

// render 输出 relay_internode_streams、relay_internode_bytes_total{direction}、relay_routes_remote_total。
func (m *internodeMetrics) render() string {
	var b strings.Builder
	b.WriteString("# HELP relay_internode_streams Streams on inter-node links of this node.\n")
	b.WriteString("# TYPE relay_internode_streams gauge\n")
	b.WriteString("relay_internode_streams " + itoa(int64(m.streams())) + "\n")
	b.WriteString("# HELP relay_internode_bytes_total Payload bytes over inter-node links.\n")
	b.WriteString("# TYPE relay_internode_bytes_total counter\n")
	b.WriteString(`relay_internode_bytes_total{direction="to_source"} ` + itoa(m.toSource.Load()) + "\n")
	b.WriteString(`relay_internode_bytes_total{direction="to_client"} ` + itoa(m.toClient.Load()) + "\n")
	b.WriteString("# HELP relay_routes_remote_total Client channels forwarded to the node holding the tunnel.\n")
	b.WriteString("# TYPE relay_routes_remote_total counter\n")
	b.WriteString("relay_routes_remote_total " + itoa(m.remote.Load()) + "\n")
	return b.String()
}

// ------------------------------------------------------------ 入口节点：到对端的常驻连接

type dialCall struct {
	done chan struct{}
	t    *tunnel.Tunnel
}

// links 是本节点到各对端的常驻连接（每个对端一条，断了下次用到时重连）。
type links struct {
	self    string
	token   string
	client  goredis.UniversalClient
	prefix  string
	cfg     tunnel.Config
	log     *obs.Logger
	metrics *internodeMetrics
	dialer  *websocket.Dialer

	mu      sync.Mutex
	byNode  map[string]*tunnel.Tunnel
	dialing map[string]*dialCall
	failed  map[string]time.Time
	closed  bool
}

func newLinks(self, token string, client goredis.UniversalClient, prefix string, cfg tunnel.Config, log *obs.Logger, m *internodeMetrics) *links {
	return &links{
		self: self, token: token, client: client, prefix: prefix, cfg: cfg, log: log, metrics: m,
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
			l.log.Warn("连不上对端节点，通道不转发", obs.Node(node), obs.Error(err))
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

// connect 读节点表找对端的 /x/v1，出示节点令牌升级，建一条隧道（本节点是开流方）。
func (l *links) connect(node string) (*tunnel.Tunnel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), linkDialTimeout)
	defer cancel()
	info, err := ReadNode(ctx, l.client, l.prefix, node)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, errors.New("对端不在节点表里")
	}
	target := info.InternodeURL
	if target == "" {
		u, err := url.Parse(info.URL)
		if err != nil {
			return nil, errors.New("对端的隧道地址不合格式")
		}
		u.Path, u.RawQuery = gen.PathInternode, ""
		target = u.String()
	}
	conn, resp, err := l.dialer.DialContext(ctx, target, http.Header{"Authorization": {"Bearer " + l.token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, errors.New("对端拒绝升级：" + resp.Status)
		}
		return nil, errors.New("连不上对端")
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
	t.SetCapabilities([]string{gen.E2ECapability})
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
// （有节点令牌时）；都没有答 nil（4404）。
func lookup(routes control.RouteTable, self string, local func(string) *tunnel.Tunnel, l *links) func(context.Context, string) (*tunnel.Tunnel, error) {
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
		if l == nil {
			return nil, nil
		}
		t := l.get(ctx, route.Node)
		if t != nil {
			l.metrics.remote.Add(1)
		}
		return t, nil
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
