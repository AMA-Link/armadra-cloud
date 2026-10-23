// Package dataplane 把数据面装配起来：隧道表、客户端边缘、健康与指标，
// 并按请求分派（对应 packages/relay-runtime-node 的 dispatch.ts 与 start-relay.ts 里的装配）。
// 个人中转（G3）与 SaaS 节点（G5）在它外面加各自的控制面与 HTTP 面。
//
// 分派：
//
//   - 指向某个源的请求与升级（saas 的 `<id>.src.<域>`、personal 的 `/s/<id>/…`）先于一切
//     交给边缘——源自己也有 /health，不能被中继的同名端点截走。
//   - 升级 /t/v1 → 隧道终端；模式自己的升级（personal 的 /v1/me/stream）→ UpgradeExtra；
//     其余升级 → 边缘（答 404）。
//   - 其余请求：/health、/ready → Fallback → 404。
//   - /metrics 不在这里：只在单独的监听上（obs.MetricsHandler）。
package dataplane

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/auth"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/httperr"
	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// Options 配置数据面。
type Options struct {
	Mode    string
	NodeID  string
	Version string
	Control control.Plane
	// Addressing 是客户端地址写法：personal 用 edge.PathAddressing，saas 用 edge.HostAddressing。
	Addressing edge.Addressing
	Log        *obs.Logger
	// Config 是隧道限制；零值取协议的缺省。
	Config           tunnel.Config
	HandshakeTimeout time.Duration
	RouteTTL         time.Duration
	CORSOrigins      func(sourceID string) []string
	WSPing, WSIdle   time.Duration
	ClientIP         edge.ClientIP
	Traffic          *traffic.Meter
	Poller           *netpoll.Poller
	// Probes 是 /ready 的依赖探测。
	Probes map[string]obs.Probe
	// UpgradeExtra 处理模式自己的升级，认领了返回 true。
	UpgradeExtra func(w http.ResponseWriter, r *http.Request) bool
	// Fallback 处理精确路由都没命中的请求，认领了返回 true，否则 404。
	Fallback func(w http.ResponseWriter, r *http.Request) bool
	// Gate 是模式自己的准入（SaaS：事故开关、团队名额）；personal 不给。
	Gate edge.Gate
	// ClientKick 为真时 kick 命令只踢客户端连接、不碰隧道（SaaS），由装配方接到 Edge.Disconnect。
	ClientKick bool
	// Lookup 换掉边缘找隧道的办法（SaaS：路由在别的节点时答到那个节点的节点间连接）；缺省是隧道表的 Lookup。
	Lookup func(ctx context.Context, sourceID string) (*tunnel.Tunnel, error)
}

// Plane 是装配好的数据面。
type Plane struct {
	Hub     *tunnel.Hub
	Edge    *edge.Edge
	Auth    *auth.Context
	Metrics *obs.Metrics
	Log     *obs.Logger

	o      Options
	health http.Handler
	ready  http.Handler
}

// New 装配数据面。
func New(o Options) (*Plane, error) {
	if o.Log == nil {
		o.Log = obs.Nop()
	}
	if o.Config.MaxStreams == 0 {
		o.Config = tunnel.DefaultConfig()
	}
	p := &Plane{o: o, Log: o.Log}
	p.Auth = auth.NewContext(o.Control, nil)
	p.Metrics = obs.NewMetrics(func() (int, int) { return p.Hub.Counts() })
	p.Hub = tunnel.NewHub(tunnel.HubOptions{
		Control: o.Control, Auth: p.Auth, NodeID: o.NodeID, Config: o.Config,
		HandshakeTimeout: o.HandshakeTimeout, RouteTTL: o.RouteTTL, Log: o.Log, Metrics: p.Metrics,
		ClientKick: o.ClientKick,
	})
	lookup := o.Lookup
	if lookup == nil {
		lookup = p.Hub.Lookup
	}
	e, err := edge.New(edge.Options{
		NodeID: o.NodeID, Addressing: o.Addressing, Auth: p.Auth, Control: o.Control,
		Lookup: lookup, Log: o.Log, Metrics: p.Metrics, CORSOrigins: o.CORSOrigins,
		WSPing: o.WSPing, WSIdle: o.WSIdle, ClientIP: o.ClientIP, Traffic: o.Traffic, Poller: o.Poller,
		Gate: o.Gate,
	})
	if err != nil {
		return nil, err
	}
	p.Edge = e
	p.health = obs.HealthHandler(obs.HealthOptions{
		Mode: o.Mode, Node: o.NodeID, Version: o.Version, Counts: p.Hub.Counts,
	})
	p.ready = obs.ReadyHandler(o.Probes)
	return p, nil
}

func isUpgrade(r *http.Request) bool { return r.Header.Get("Upgrade") != "" }

// ServeHTTP 实现 http.Handler。
func (p *Plane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isUpgrade(r) {
		switch {
		case p.Edge.Addressed(r):
			p.Edge.ServeUpgrade(w, r)
		case pathOf(r) == gen.PathTunnel:
			p.Hub.HandleUpgrade(w, r)
		case p.o.UpgradeExtra != nil && p.o.UpgradeExtra(w, r):
		default:
			p.Edge.ServeUpgrade(w, r)
		}
		return
	}
	if p.Edge.Addressed(r) {
		p.Edge.ServeRequest(w, r)
		return
	}
	if r.Method == http.MethodGet {
		switch pathOf(r) {
		case "/health":
			p.health.ServeHTTP(w, r)
			return
		case "/ready":
			p.ready.ServeHTTP(w, r)
			return
		}
	}
	if p.o.Fallback != nil && p.o.Fallback(w, r) {
		return
	}
	httperr.Write(w, gen.ErrNotFound, httperr.Options{})
}

func pathOf(r *http.Request) string {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	return path
}

// Shutdown 按顺序收尾：不再接新通道 → 隧道发 GOAWAY 并等排空 → 断开边缘连接 → 停边缘。
func (p *Plane) Shutdown(grace time.Duration) {
	p.Edge.StopAccepting()
	p.Hub.Shutdown(grace)
	p.Edge.CloseAll()
	p.Edge.Close()
}
