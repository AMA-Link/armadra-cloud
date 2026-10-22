package edge

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 规定的握手哈希，不用于安全目的
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// Options 配置 Edge。
type Options struct {
	NodeID     string
	Addressing Addressing
	Auth       *auth.Context
	Control    control.Plane
	Lookup     func(ctx context.Context, sourceID string) (*tunnel.Tunnel, error)
	Log        *obs.Logger
	Metrics    *obs.Metrics
	// CORSOrigins 见 PolicyOptions。
	CORSOrigins func(sourceID string) []string
	// WSPing 与 WSIdle 是对客户端 WS 的 ping 间隔与无响应断开时长（缺省 30 秒 / 60 秒）。
	WSPing time.Duration
	WSIdle time.Duration
	// ClientIP 是客户端 IP 的取法（受信代理见 NewClientIP）；缺省用直连对端地址。
	ClientIP ClientIP
	// Traffic 是流量统计（cloud-api §19）；不给就不统计。
	Traffic *traffic.Meter
	// Poller 是连接的事件循环；不给就在 New 里建一个（Close 时关）。
	Poller *netpoll.Poller
}

// Edge 是客户端边缘的 HTTP 外壳：判定在 Policy，这里管字节进出。
type Edge struct {
	o      Options
	policy *Policy
	wheel  *wheel
	own    bool
	once   sync.Once
	// stopping 为真后不再接受新的升级（关闭服务的第一步）。
	stopping atomic.Bool
}

// StopAccepting 停止接受新的通道升级；已升级的连接不动。
func (e *Edge) StopAccepting() { e.stopping.Store(true) }

// New 建 Edge，并启动 ping 时间轮。
func New(o Options) (*Edge, error) {
	if o.WSPing <= 0 {
		o.WSPing = 30 * time.Second
	}
	if o.WSIdle <= 0 {
		o.WSIdle = 60 * time.Second
	}
	if o.Log == nil {
		o.Log = obs.Nop()
	}
	if o.ClientIP == nil {
		ip, _ := NewClientIP(nil)
		o.ClientIP = ip
	}
	e := &Edge{o: o, wheel: newWheel()}
	if o.Poller == nil {
		p, err := netpoll.New(0)
		if err != nil {
			return nil, err
		}
		e.o.Poller, e.own = p, true
	}
	e.policy = NewPolicy(PolicyOptions{
		NodeID: o.NodeID, Addressing: o.Addressing, Auth: o.Auth, Control: o.Control,
		Lookup: o.Lookup, Metrics: o.Metrics, CORSOrigins: o.CORSOrigins, Traffic: o.Traffic,
	})
	go e.wheel.run(o.WSPing, o.WSIdle)
	return e, nil
}

// Policy 返回判定（测试与撤销用）。
func (e *Edge) Policy() *Policy { return e.policy }

// Close 停掉 ping 轮；自己建的 Poller 一并关。已升级的连接用 CloseAll 断开。
func (e *Edge) Close() {
	e.once.Do(func() {
		close(e.wheel.stop)
		if e.own {
			e.o.Poller.Close()
		}
	})
}

func (e *Edge) request(r *http.Request) Request {
	return Request{
		Method:   r.Method,
		URL:      r.RequestURI,
		Host:     r.Host,
		Header:   r.Header,
		ClientIP: e.o.ClientIP(r.RemoteAddr, r.Header),
	}
}

// Addressed 判断请求是否指向某个源（交给边缘）。
func (e *Edge) Addressed(r *http.Request) bool {
	_, ok := e.policy.Resolve(e.request(r))
	return ok
}

// ServeRequest 处理源地址上的 HTTP：一律 404 not_found（中继不转明文的应用请求）。
func (e *Edge) ServeRequest(w http.ResponseWriter, r *http.Request) {
	refusal := e.policy.DecideRequest(e.request(r))
	httperr.Write(w, refusal.Code, httperr.Options{RetryAfter: refusal.RetryAfter, CORS: refusal.CORS})
}

// CloseAll 断开所有已升级的客户端连接（关闭服务时）。
func (e *Edge) CloseAll() {
	for _, h := range e.policy.TakeAll() {
		h.Disconnect()
	}
}

// Disconnect 断开匹配的已升级连接（撤销源 / 访客），答断开了几条。
func (e *Edge) Disconnect(f ConnectionFilter) int {
	hs := e.policy.Take(f)
	for _, h := range hs {
		h.Disconnect()
	}
	return len(hs)
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // 同上
	return base64.StdEncoding.EncodeToString(sum[:])
}

// validHandshake 检查升级请求：GET、Upgrade: websocket、版本 13（或 8）、key 是 16 字节的 base64。
func validHandshake(r *http.Request) (key string, ok bool) {
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return "", false
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return "", false
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" && v != "8" {
		return "", false
	}
	key = r.Header.Get("Sec-WebSocket-Key")
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 16 {
		return "", false
	}
	return key, true
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func upgradeResponse(key, subprotocol string) []byte {
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	b.WriteString(acceptKey(key))
	b.WriteString("\r\n")
	if subprotocol != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// ServeUpgrade 处理源地址上的 WS 升级：判定 → 完成升级 → 开 OPEN kind e2e → 两边透传。
// 被拒时先完成升级再以映射后的关闭码关闭（浏览器读不到升级失败的状态码）。
func (e *Edge) ServeUpgrade(w http.ResponseWriter, r *http.Request) {
	if e.stopping.Load() {
		httperr.Write(w, gen.ErrUnavailable, httperr.Options{})
		return
	}
	req := e.request(r)
	d, err := e.policy.DecideUpgrade(r.Context(), req)
	if err != nil {
		e.o.Log.Error("边缘判定出错", obs.Error(err))
		httperr.Write(w, gen.ErrUnavailable, httperr.Options{})
		return
	}
	if !d.Accept {
		e.refuseUpgrade(w, r, req, d.Code, d.SourceID, d.Reason)
		return
	}
	key, ok := validHandshake(r)
	if !ok {
		e.policy.ReleaseUpgrade(d.Subject)
		httperr.Write(w, gen.ErrBadRequest, httperr.Options{CORS: e.policy.ErrorCORS(req, d.Target.SourceID)})
		return
	}
	nc, brw, err := hijack(w)
	if err != nil {
		e.policy.ReleaseUpgrade(d.Subject)
		return
	}
	// 客户端在收到 101 之前不得发帧（RFC 6455 §4.1）；缓冲里有字节就当协议错误。
	if brw.Reader.Buffered() > 0 {
		_ = nc.Close()
		e.policy.ReleaseUpgrade(d.Subject)
		return
	}
	_ = nc.SetDeadline(time.Time{})
	ec := &edgeConn{e: e, sourceID: d.Target.SourceID, subject: d.Subject, started: time.Now()}
	ec.lastSeen.Store(time.Now().UnixMilli())
	// 先登记再接管：接管后任何时刻连接都可能关掉，OnClose 要能把登记摘干净。
	e.wheel.add(ec)
	e.policy.Track(ec, d.Target.SourceID, d.Subject)
	c, err := e.o.Poller.Adopt(nc, ec)
	if err != nil {
		e.wheel.remove(ec)
		e.policy.Untrack(ec)
		_ = nc.Close()
		e.policy.ReleaseUpgrade(d.Subject)
		return
	}
	ec.bind(c)
	c.Write(upgradeResponse(key, gen.E2ESubprotocol), 0)

	if ec.isFinished() {
		return // 101 都没写出去，连接已经没了：不开流
	}
	s, code := e.policy.OpenE2E(d.Tunnel, req, d.RemoteIP, &TrafficIdentity{Subject: d.Subject, DeviceID: d.Device}, ec)
	if code != "" {
		closeCode, ok := WSCloseCode(code)
		if !ok {
			closeCode = 1011 // 没有对应关闭码的内部错误
		}
		ec.closeGracefully(int(closeCode), string(code))
		return
	}
	ec.attach(s)
}

func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w).Hijack()
}

// refuseUpgrade：能映射到关闭码的先完成升级再以关闭码结束；否则（如 not_found）答 HTTP。
func (e *Edge) refuseUpgrade(w http.ResponseWriter, r *http.Request, req Request, code gen.ErrorCode, sourceID, reason string) {
	cors := e.policy.ErrorCORS(req, sourceID)
	closeCode, mapped := WSCloseCode(code)
	key, ok := validHandshake(r)
	if !mapped || !ok {
		httperr.Write(w, code, httperr.Options{CORS: cors})
		return
	}
	nc, _, err := hijack(w)
	if err != nil {
		return
	}
	if reason == "" {
		reason = string(code)
	}
	// 只为发关闭码而完成升级：回选客户端给的第一个子协议，浏览器才会接受这次握手。
	first := ""
	if offered := headerJoined(r.Header, "Sec-WebSocket-Protocol"); offered != "" {
		first = strings.TrimSpace(strings.Split(offered, ",")[0])
	}
	out := append(upgradeResponse(key, first), serverFrame(opClose, closePayload(int(closeCode), reason))...)
	_ = nc.SetDeadline(time.Now().Add(closeGrace))
	if _, err := nc.Write(out); err != nil {
		_ = nc.Close()
		return
	}
	// 等客户端回关闭帧（或超时）再断，免得 RST 把关闭帧冲掉。
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := nc.Read(buf); err != nil {
				break
			}
		}
		_ = nc.Close()
	}()
}
