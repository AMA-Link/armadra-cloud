// Package edge 是客户端边缘：解析源、限流、校验中继令牌、找隧道，决定开通道还是拒绝；
// 再把已升级的连接和隧道流接成一条端到端通道的管道。
//
// 对应 packages/relay-kernel/src/edge/ 与 packages/relay-runtime-node/src/edge.ts、e2e-pipe.ts。
// 判定（本文件）与字节进出（conn.go、server.go）分开；判定对模式零分支：地址写法由
// Addressing 决定，控制面由 control.Plane 决定。
//
// 只转端到端通道（0.5.0 起，cloud-api §20）：源地址上唯一的入口是 `_relay/e2e` 的 WS 升级，
// 要带子协议 armadra-e2e.1（缺了 4409），源要在 hello.capabilities 报过 e2e（没报 4404，
// 原因 e2e_unsupported）；开的是 OPEN kind: "e2e"，之后字节两边透传，中继不解析。
// 明文的应用请求（HTTP、普通 WS 升级、CORS 预检）一律 404。
//
// 拒绝：地址不匹配或不是通道 → 404（WS 也答 404，不升级）；之后 WS 先完成升级再以关闭码
// 结束（浏览器读不到升级失败的状态码）：令牌 4401 / 4403、限流 4429、源离线 4404。
//
// 不解析正文、不缓存、不记路径与头。
package edge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// Request 是运行时交给边缘的一个请求（HTTP 或 WS 升级）的元数据；不含正文。
type Request struct {
	Method string
	// URL 是请求目标（路径 + query）。
	URL string
	// Host 是请求的 Host（saas 的子域寻址用）。
	Host   string
	Header http.Header
	// ClientIP 是客户端 IP（受信代理见 ClientIP）。
	ClientIP string
}

// PolicyOptions 配置 Policy。
type PolicyOptions struct {
	NodeID     string
	Addressing Addressing
	Auth       *auth.Context
	Control    control.Plane
	// Lookup 找本节点上持有这个源、路由指向它、还能开流的隧道；没有答 nil。
	Lookup  func(ctx context.Context, sourceID string) (*tunnel.Tunnel, error)
	Metrics *obs.Metrics
	// CORSOrigins 是错误答复回显 CORS 的来源（原生 App 的两个来源总是回显，不用列）。
	// 答案不得随源在不在线、登没登记而变。缺省空。
	CORSOrigins func(sourceID string) []string
	// Traffic 是流量统计（cloud-api §19）；不给就不统计。
	Traffic *traffic.Meter
	// Dispatch 承接 ReleaseUpgradeAsync；不给就自建一个。
	Dispatch *control.Dispatcher
	// Gate 是模式自己的准入（SaaS：事故开关、团队速率与跨节点并发）；personal 不给。
	Gate Gate
}

// Gate 是模式自己的准入（SaaS 实施设计 §2.4 的 N4）。
type Gate interface {
	// Refuse 在验令牌之前判断（事故开关开着时答 limit_reached）；答空串放行。只读本地状态，不做 I/O。
	Refuse() gen.ErrorCode
	// Admit 在通道开成之前占名额。放行时 release 非空，连接结束时恰好调一次（可能做网络 I/O，
	// 不在 epoll 循环里调）；被拒时答错误码；err 非空按 503 处理。
	Admit(ctx context.Context, in GateInput) (release func(), code gen.ErrorCode, err error)
}

// GateInput 是 Gate.Admit 的入参：已验过的中继令牌里的身份。
type GateInput struct {
	SourceID string
	Subject  string
	DeviceID string
	// Team 是中继令牌的 team；个人空间为空串。
	Team string
}

// TrafficIdentity 是一条通道记到谁名下（流量统计，类别 e2e）：中继令牌的 sub / dev / team。
type TrafficIdentity struct {
	Subject  string
	DeviceID string
	Team     string
}

// wsClose 是升级被拒时用来关 WS 的关闭码；没有对应码（如 not_found）的改答 HTTP。
var wsClose = map[gen.ErrorCode]protocol.CloseCode{
	gen.ErrRelayTokenInvalid:   protocol.CloseUnauthenticated,
	gen.ErrRelaySourceMismatch: protocol.CloseForbidden,
	gen.ErrForbidden:           protocol.CloseForbidden,
	gen.ErrRateLimited:         protocol.CloseLimitReached,
	gen.ErrLimitReached:        protocol.CloseLimitReached,
	gen.ErrSourceOffline:       protocol.CloseSourceOffline,
	gen.ErrBadRequest:          protocol.CloseBadFrame,
	gen.ErrProtocolUnsupported: protocol.CloseProtocolUnsupported,
}

// WSCloseCode 返回升级被拒时用的关闭码；没有对应码答 false，改答 HTTP。
func WSCloseCode(code gen.ErrorCode) (protocol.CloseCode, bool) {
	c, ok := wsClose[code]
	return c, ok
}

// InternodeOpen 是节点间连接上转一条端到端通道的 OPEN：e2e 的 OPEN 规定路径固定、不带头，没处放
// 目标源，所以节点之间用 kind ws、路径寻址 `/s/<sourceId>/_relay/e2e`（与 personal 的源地址同一种写法）。
// 持有节点用 ParseInternodeOpen 取回目标源，再以标准的 e2e OPEN 转给源。
func InternodeOpen(sourceID string, base gen.OpenPayload) *gen.OpenPayload {
	base.Kind, base.Method, base.Path, base.Headers = "ws", "GET", "/s/"+sourceID+gen.E2EPath, [][2]string{}
	return &base
}

// ParseInternodeOpen 从节点间的 OPEN 里取目标源；不是 InternodeOpen 的形状答 false。
func ParseInternodeOpen(open *gen.OpenPayload) (string, bool) {
	if open == nil || open.Kind != "ws" || open.Method != "GET" || len(open.Headers) != 0 {
		return "", false
	}
	target, ok := PathAddressing().Resolve(open.Path, "")
	if !ok || target.Path != gen.E2EPath {
		return "", false
	}
	return target.SourceID, true
}

// IsE2EPath 判断地址解析之后的路径是不是端到端通道（可以带 query）。
func IsE2EPath(path string) bool {
	return path == gen.E2EPath || strings.HasPrefix(path, gen.E2EPath+"?")
}

// Refusal 是 HTTP 请求的下一步：只有拒绝（中继不转明文的应用请求）。
type Refusal struct {
	Code       gen.ErrorCode
	RetryAfter time.Duration
	CORS       map[string]string
	// Drain 为真时先把请求体读掉丢弃。
	Drain bool
}

// Decision 是 WS 升级的下一步。
type Decision struct {
	// Accept 为真：端到端通道，中继自己终止 WS，开 OPEN kind e2e。
	Accept bool

	// 拒绝时：
	Code     gen.ErrorCode
	SourceID string
	// Reason 是关闭原因（缺省就是 Code）。
	Reason string

	// 接受时：
	Target       Target
	Tunnel       *tunnel.Tunnel
	Subject      string
	RemoteIP     string
	Subprotocols []string
	// Device 是中继令牌的 dev（1.3，可选；只计数）。
	Device string
	// Team 是中继令牌的 team（1.4，可选；只计数）。
	Team string
	// Release 是 Gate.Admit 占的名额；没有 Gate 时为空。用 Policy.Release 连同账号并发一起还。
	Release func()
}

// Handle 是一条已升级连接的句柄；撤销源或访客时被断开。
type Handle interface {
	Disconnect()
}

// ConnectionFilter 是 Take 的条件：给了的都要满足。
type ConnectionFilter struct {
	SourceID string
	// SubjectPrefix 匹配中继令牌 sub 的前缀（访客：guest:<linkId>:）。
	SubjectPrefix string
	// Subject 精确匹配中继令牌的 sub（SaaS 按人踢）。
	Subject string
	// DeviceID 精确匹配中继令牌的 dev（SaaS 按设备踢）。
	DeviceID string
}

type connection struct {
	sourceID, subject, device string
}

// Policy 是边缘的判定。
type Policy struct {
	o PolicyOptions

	mu       sync.Mutex
	conns    map[Handle]connection
	dispatch *control.Dispatcher
	ownDisp  bool
}

// NewPolicy 建 Policy。
func NewPolicy(o PolicyOptions) *Policy {
	p := &Policy{o: o, conns: map[Handle]connection{}, dispatch: o.Dispatch}
	if p.dispatch == nil {
		p.dispatch, p.ownDisp = control.NewDispatcher(0, 0), true
	}
	return p
}

func originOf(r Request) (string, bool) {
	v := r.Header.Values("Origin")
	if len(v) == 0 {
		return "", false
	}
	return v[0], true
}

func headerJoined(h http.Header, name string) string {
	return strings.Join(h.Values(name), ", ")
}

// Resolve 把请求映射到目标源。
func (p *Policy) Resolve(r Request) (Target, bool) {
	return p.o.Addressing.Resolve(r.URL, r.Host)
}

// DecideRequest 处理 HTTP 请求：0.5.0 起中继不转明文的应用请求（cloud-api §20），
// 源地址上的 HTTP 一律 404 not_found（预检同样），不验令牌、不找隧道。
func (p *Policy) DecideRequest(r Request) Refusal {
	addressed, ok := p.Resolve(r)
	p.o.Metrics.EdgeConnection("not_found")
	id := ""
	if ok {
		id = addressed.SourceID
	}
	return Refusal{Code: gen.ErrNotFound, CORS: p.ErrorCORS(r, id), Drain: true}
}

// ErrorCORS 是错误答复的 CORS 头：名单与预检同一份（地址不匹配时只有原生 App 来源）。
func (p *Policy) ErrorCORS(r Request, sourceID string) map[string]string {
	var trusted []string
	if sourceID != "" && p.o.CORSOrigins != nil {
		trusted = p.o.CORSOrigins(sourceID)
	}
	origin, has := originOf(r)
	return ErrorCORSHeaders(origin, has, trusted)
}

// DecideUpgrade 处理 WS 升级：只认 _relay/e2e（别的路径 404，不升级）。令牌（子协议）、
// 版本子协议与源的 e2e 能力（令牌先验，未授权的请求看不到源支不支持）、按账号的并发上限。
// err 非空表示控制面出错，调用方按 503 处理。
func (p *Policy) DecideUpgrade(ctx context.Context, r Request) (Decision, error) {
	addressed, ok := p.Resolve(r)
	if !ok || !IsE2EPath(addressed.Path) {
		p.o.Metrics.EdgeConnection("not_found")
		d := Decision{Code: gen.ErrNotFound}
		if ok {
			d.SourceID = addressed.SourceID
		}
		return d, nil
	}
	if p.o.Gate != nil {
		if code := p.o.Gate.Refuse(); code != "" {
			p.o.Metrics.EdgeConnection(string(code))
			return Decision{Code: code, SourceID: addressed.SourceID}, nil
		}
	}
	token, rest := auth.SplitRelaySubprotocol(headerJoined(r.Header, "Sec-WebSocket-Protocol"))
	adm, err := p.admit(ctx, r, addressed, token)
	if err != nil {
		return Decision{}, err
	}
	if !adm.ok {
		p.o.Metrics.EdgeConnection(string(adm.code))
		return Decision{Code: adm.code, SourceID: adm.sourceID}, nil
	}
	if !contains(rest, gen.E2ESubprotocol) {
		p.o.Metrics.EdgeConnection(string(gen.ErrProtocolUnsupported))
		return Decision{Code: gen.ErrProtocolUnsupported, SourceID: addressed.SourceID}, nil
	}
	if !adm.tunnel.HasCapability(gen.E2ECapability) {
		p.o.Metrics.EdgeConnection(string(gen.ErrSourceOffline))
		return Decision{Code: gen.ErrSourceOffline, SourceID: addressed.SourceID, Reason: "e2e_unsupported"}, nil
	}
	hit, err := p.o.Control.Limits().Hit(ctx, control.LimitEdgeConcurrentAcct, adm.subject)
	if err != nil {
		return Decision{}, err
	}
	if !hit.Allowed {
		p.o.Metrics.EdgeConnection(string(gen.ErrLimitReached))
		return Decision{Code: gen.ErrLimitReached, SourceID: addressed.SourceID}, nil
	}
	var release func()
	if p.o.Gate != nil {
		rel, code, err := p.o.Gate.Admit(ctx, GateInput{
			SourceID: addressed.SourceID, Subject: adm.subject, DeviceID: adm.device, Team: adm.team,
		})
		if err != nil || code != "" {
			p.ReleaseUpgrade(adm.subject)
			if err != nil {
				return Decision{}, err
			}
			p.o.Metrics.EdgeConnection(string(code))
			return Decision{Code: code, SourceID: addressed.SourceID}, nil
		}
		release = rel
	}
	return Decision{
		Accept: true, Target: addressed, Tunnel: adm.tunnel, Subject: adm.subject,
		RemoteIP: adm.remoteIP, Subprotocols: []string{gen.E2ESubprotocol}, Device: adm.device,
		Team: adm.team, Release: release,
	}, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ReleaseUpgrade 在已接受的升级结束时还回 edge.concurrent.acct。
func (p *Policy) ReleaseUpgrade(subject string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.o.Control.Limits().Release(ctx, control.LimitEdgeConcurrentAcct, subject)
}

// Release 还回一次已接受的升级占的全部名额（账号并发与 Gate 的名额）。
func (p *Policy) Release(d Decision) {
	p.ReleaseUpgrade(d.Subject)
	if d.Release != nil {
		d.Release()
	}
}

// ReleaseUpgradeAsync 同 ReleaseUpgrade（另还 extra，可为空），但投递给工作协程立即返回：epoll 循环的
// 回调用它，控制面（redis / postgres）的网络 I/O 不会拖住整条循环。同一个账号的调用保持顺序。
func (p *Policy) ReleaseUpgradeAsync(subject string, extra func()) {
	p.dispatch.Go(subject, func() {
		p.ReleaseUpgrade(subject)
		if extra != nil {
			extra()
		}
	})
}

// Close 排空并释放自建的工作协程（调用方给的 Dispatcher 归调用方）。关闭之后的投递仍会执行。
func (p *Policy) Close() {
	if p.ownDisp {
		p.dispatch.Close()
	} else {
		p.dispatch.Flush(5 * time.Second)
	}
}

// Flush 等已投递的控制面调用做完（关闭前、测试用）。
func (p *Policy) Flush(timeout time.Duration) bool { return p.dispatch.Flush(timeout) }

type admission struct {
	ok       bool
	code     gen.ErrorCode
	sourceID string
	tunnel   *tunnel.Tunnel
	subject  string
	remoteIP string
	device   string
	team     string
}

// admit：解析源 → 中继令牌（无效按 IP 限流；有效按令牌与账号限流）→ 找隧道。
func (p *Policy) admit(ctx context.Context, r Request, target Target, token string) (admission, error) {
	limits := p.o.Control.Limits()
	sourceID := target.SourceID
	verified := auth.VerifyRelayToken(ctx, p.o.Auth, token, sourceID)
	if !verified.OK {
		// 无令牌 / 令牌无效 / 源不符：按 IP 严格限流（令牌校验是本地验签，先验后计）。
		byIP, err := limits.Hit(ctx, control.LimitEdgeConnectIP, r.ClientIP)
		if err != nil {
			return admission{}, err
		}
		if !byIP.Allowed {
			return admission{code: gen.ErrRateLimited, sourceID: sourceID}, nil
		}
		return admission{code: verified.Error, sourceID: sourceID}, nil
	}
	subject := verified.Claims.Sub
	// 有效令牌：先按令牌、再按账号计数；与 IP、预检的桶互不相干。
	for _, h := range []struct {
		kind control.LimitKind
		key  string
	}{
		{control.LimitEdgeConnectToken, verified.Claims.Jti},
		{control.LimitEdgeConnectAcct, subject},
	} {
		hit, err := limits.Hit(ctx, h.kind, h.key)
		if err != nil {
			return admission{}, err
		}
		if !hit.Allowed {
			return admission{code: gen.ErrRateLimited, sourceID: sourceID}, nil
		}
	}
	t, err := p.o.Lookup(ctx, sourceID)
	if err != nil {
		return admission{}, err
	}
	if t == nil {
		return admission{code: gen.ErrSourceOffline, sourceID: sourceID}, nil
	}
	a := admission{ok: true, tunnel: t, subject: subject, remoteIP: r.ClientIP, sourceID: sourceID}
	if verified.Claims.Dev != nil {
		a.device = *verified.Claims.Dev
	}
	if verified.Claims.Team != nil {
		a.team = *verified.Claims.Team
	}
	return a, nil
}

// OpenE2E 开端到端通道的流：
// OPEN { kind: "e2e", method: "GET", path: "/_relay/e2e", headers: [] }
// （子协议、令牌、头一概不转；地址里的 query 也不转）。给了 id 就按 e2e 类别统计。
// 成功时 code 为空；失败时 code 是该关 WS 用的错误码。
//
// t 是节点间连接（t.Link() 非空，SaaS 的转发）时发 InternodeOpen（目标源在路径里），持有节点
// 接到本地隧道、以标准的 e2e OPEN 转给源；流量照样记在入口节点（本节点）。
func (p *Policy) OpenE2E(t *tunnel.Tunnel, r Request, remoteIP string, id *TrafficIdentity, sink tunnel.Sink) (*tunnel.Stream, gen.ErrorCode) {
	sourceID := t.SourceID()
	if t.Link() != "" {
		target, ok := p.Resolve(r)
		if !ok {
			return nil, gen.ErrSourceOffline
		}
		sourceID = target.SourceID
	}
	var counter *traffic.Counter
	if id != nil && p.o.Traffic != nil {
		counter = p.o.Traffic.Counter(traffic.Key{
			SourceID: sourceID, Subject: id.Subject, DeviceID: id.DeviceID, Kind: traffic.KindE2E, TeamID: id.Team,
		})
	}
	open := &gen.OpenPayload{
		Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{},
		RemoteIP: remoteIP, RelayNode: p.o.NodeID,
	}
	if origin, ok := originOf(r); ok {
		open.ClientOrigin = &origin
	}
	if t.Link() != "" {
		open = InternodeOpen(sourceID, *open)
	}
	s, err := t.OpenStream(open, sink, counter)
	if err != nil {
		var ue *tunnel.UnavailableError
		if errors.As(err, &ue) {
			if ue.Reason == tunnel.ReasonMaxStreams {
				return nil, gen.ErrLimitReached
			}
			return nil, gen.ErrSourceOffline
		}
		return nil, gen.ErrInternal
	}
	p.o.Metrics.EdgeConnection("ok")
	p.o.Metrics.EdgeStream("e2e")
	counter.Connection()
	return s, ""
}

// ------------------------------------------------------------ 已升级的连接

// Track 登记一条已升级的连接。
func (p *Policy) Track(h Handle, sourceID, subject string) { p.TrackDevice(h, sourceID, subject, "") }

// TrackDevice 同 Track，另记中继令牌的 dev（按设备踢用）。
func (p *Policy) TrackDevice(h Handle, sourceID, subject, device string) {
	p.mu.Lock()
	p.conns[h] = connection{sourceID: sourceID, subject: subject, device: device}
	p.mu.Unlock()
}

// Untrack 摘掉一条连接。
func (p *Policy) Untrack(h Handle) {
	p.mu.Lock()
	delete(p.conns, h)
	p.mu.Unlock()
}

// Tracked 返回已登记的升级连接数（观测与测试用）。
func (p *Policy) Tracked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// TakeAll 摘掉并交出全部已升级连接（关闭时断开）。
func (p *Policy) TakeAll() []Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := make([]Handle, 0, len(p.conns))
	for h := range p.conns {
		all = append(all, h)
	}
	clear(p.conns)
	return all
}

// Take 摘掉并交出匹配的已升级连接（撤销源 / 访客）。
func (p *Policy) Take(f ConnectionFilter) []Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	var taken []Handle
	for h, c := range p.conns {
		if f.SourceID != "" && c.sourceID != f.SourceID {
			continue
		}
		if f.SubjectPrefix != "" && !strings.HasPrefix(c.subject, f.SubjectPrefix) {
			continue
		}
		if f.Subject != "" && c.subject != f.Subject {
			continue
		}
		if f.DeviceID != "" && c.device != f.DeviceID {
			continue
		}
		delete(p.conns, h)
		taken = append(taken, h)
	}
	return taken
}
