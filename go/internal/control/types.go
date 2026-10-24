// Package control 是隧道与边缘对控制面的全部依赖（中继设计 §2，平台设计 §17.2）。
//
//   - personal：Memory（进程内 map、事件总线、限流桶）；
//   - saas：Redis / PostgreSQL 实现，不在本包。
//
// tunnel、edge、auth 只依赖这里的接口，对模式零分支。
package control

import (
	"context"
	"time"
)

// Route 是路由表的一行：哪个节点、哪条隧道持有这个源。
type Route struct {
	Node        string
	TunnelID    string
	SinceMs     int64
	CoreVersion string
}

// PresenceType 是在线事件的种类。
type PresenceType string

// 在线事件的种类。
const (
	PresenceSourceOnline  PresenceType = "sourceOnline"
	PresenceSourceOffline PresenceType = "sourceOffline"
)

// PresenceEvent 是源上线 / 下线事件。CoreVersion 只在上线事件里有。
type PresenceEvent struct {
	Type        PresenceType
	SourceID    string
	Node        string
	TunnelID    string
	CoreVersion string
	AtMs        int64
}

// NodeCommandType 是节点命令的种类。
type NodeCommandType string

// 节点命令的种类。
const (
	CommandCloseTunnel NodeCommandType = "closeTunnel"
	CommandKick        NodeCommandType = "kick"
	// CommandDrain 让 Node 指定的节点排空（SaaS，G5）：不带 SourceID，老节点不认识、忽略。
	CommandDrain NodeCommandType = "drain"
)

// NodeCommand 是发给本节点的命令。Reason 只在 kick 里有。
//
// kick 的过滤（G5，SaaS 实施设计 §2.4 的 N2）：Subject 是中继令牌 sub 的精确值，SubjectPrefix 是前缀
// （访客 guest:<linkId>:），DeviceID 是 dev；给了的都要对上。老节点忽略这三项，按 SourceID 踢掉该源
// 全部连接（多踢不漏踢）。Node 只在 drain 里有。
type NodeCommand struct {
	Type          NodeCommandType
	SourceID      string
	Reason        string
	Subject       string
	SubjectPrefix string
	DeviceID      string
	Node          string
}

// LimitKind 是限流的种类。
type LimitKind string

// 限流的种类。
const (
	LimitTunnelConnectSrc   LimitKind = "tunnel.connect.src"
	LimitEdgeConnectIP      LimitKind = "edge.connect.ip"
	LimitEdgePreflightIP    LimitKind = "edge.preflight.ip"
	LimitEdgeConnectToken   LimitKind = "edge.connect.token"
	LimitEdgeConnectAcct    LimitKind = "edge.connect.acct"
	LimitEdgeConcurrentAcct LimitKind = "edge.concurrent.acct"
	LimitLoginIP            LimitKind = "login.ip"
	LimitLoginAcct          LimitKind = "login.acct"
	LimitLinkAcceptIP       LimitKind = "link.accept.ip"
)

// LimitResult 是一次限流判断的结果。RetryAfter 为 0 表示没有（速率类被拒时才有值）。
type LimitResult struct {
	Allowed    bool
	RetryAfter time.Duration
}

// LimitRule 是一种限流的规则：速率类是「窗口内最多 Max 次」；并发类是「同时最多 Max 个」。
type LimitRule struct {
	Concurrent bool
	Max        int
	Window     time.Duration
}

// DefaultLimitRules 是缺省限流（中继设计 §3.1、§4、§6；平台设计 §17.3）。
//
// 额度按真实使用定：iOS App 的来源是 capacitor://localhost，对中继的请求全部跨源，
// WebKit 的预检缓存按「来源 + URL」记、最长 10 分钟，一块画布打开时每个不同的路径都要
// 先预检一次；同一出口 IP 后面常有几台设备共用。WebSocket 并发按终端节点规模给，与
// 隧道单源的流数上限（256）对齐。
var DefaultLimitRules = map[LimitKind]LimitRule{
	LimitTunnelConnectSrc: {Max: 10, Window: time.Minute},
	// 无令牌或令牌无效的请求（含 401 路径）：按来源 IP 严格限流，防撞库与扫描。
	LimitEdgeConnectIP: {Max: 60, Window: time.Minute},
	// CORS 预检：单独按 IP 计，突发不挤占已鉴权的请求。
	LimitEdgePreflightIP: {Max: 1200, Window: time.Minute},
	// 带有效中继令牌的请求：按令牌（jti）、再按账号（sub）计数，不看 IP。
	LimitEdgeConnectToken:   {Max: 1200, Window: time.Minute},
	LimitEdgeConnectAcct:    {Max: 3600, Window: time.Minute},
	LimitEdgeConcurrentAcct: {Concurrent: true, Max: 256},
	LimitLoginIP:            {Max: 20, Window: time.Minute},
	LimitLoginAcct:          {Max: 20, Window: time.Minute},
	LimitLinkAcceptIP:       {Max: 20, Window: time.Minute},
}

// RouteTTL 是路由 TTL：心跳（20 秒）两次多一点；心跳 PONG 时 Touch。
const RouteTTL = 45 * time.Second

// RouteTable 是源到隧道的路由表。
type RouteTable interface {
	// Get 取路由；没有或已过期返回 (nil, nil)。
	Get(ctx context.Context, sourceID string) (*Route, error)
	Set(ctx context.Context, sourceID string, r Route, ttl time.Duration) error
	// Touch 心跳刷新 TTL；没有这条路由时什么也不做。
	Touch(ctx context.Context, sourceID string, ttl time.Duration) error
	// Delete 只删 tunnelID 相同的那条（CAS），避免旧隧道删掉替换它的新路由。
	Delete(ctx context.Context, sourceID, tunnelID string) error
}

// Presence 是源在线事件的总线。
type Presence interface {
	Publish(ctx context.Context, ev PresenceEvent) error
	// Subscribe 订阅事件，返回退订函数。
	Subscribe(fn func(PresenceEvent)) (cancel func())
}

// Limiter 是限流器。
type Limiter interface {
	// Hit 速率类：固定窗口计数；并发类：占一个名额，配 Release。
	Hit(ctx context.Context, kind LimitKind, key string) (LimitResult, error)
	// Release 归还并发类的名额；速率类什么也不做。
	Release(ctx context.Context, kind LimitKind, key string) error
}

// TokenStore 是令牌撤销表。
type TokenStore interface {
	IsRevoked(ctx context.Context, jti string) (bool, error)
	// Revoke 记到 exp 为止（令牌过期后撤销记录就没必要了）。
	Revoke(ctx context.Context, jti string, exp time.Time) error
}

// RevokedNotifier 是 Plane 可选实现的撤销通知：新的撤销记下之后（本节点写的、别的节点广播来的）回调 jti，
// 让已建立的连接立即断开，不必等轮询。回调在锁外执行，必须立即返回；cancel 取消订阅。
type RevokedNotifier interface {
	OnRevoked(fn func(jti string)) (cancel func())
}

// NodeCommands 是本节点的命令通道。
type NodeCommands interface {
	Subscribe(fn func(NodeCommand)) (cancel func())
}

// Plane 是控制面。
type Plane interface {
	Mode() string
	// Issuer 是断言 / 令牌的 iss；校验中继令牌与隧道令牌时逐字比较。
	Issuer() string
	// JWKS 返回验令牌用的公钥集（JWKS JSON 文档）。
	JWKS(ctx context.Context) ([]byte, error)
	Routes() RouteTable
	Presence() Presence
	Limits() Limiter
	Tokens() TokenStore
	// NodeCommands 只有 saas 与 Memory 有；可能为 nil。
	NodeCommands() NodeCommands
}

// Cluster 是装配方用到的控制面：Plane 加「发给全部节点」的命令通道与关闭。
// Memory、redis、postgres 三种实现都满足它。
type Cluster interface {
	Plane
	// Command 把命令发给全部节点（本节点立即收到，其余经广播）。
	Command(c NodeCommand)
	// Close 停掉后台协程与订阅连接（自己建的连接池一并关）。
	Close() error
}

// TeamQuota 是团队的并发额度（SaaS 实施设计 §2.4：键 relay:quota:team:<teamId> 与 kind: quota 广播）。
// Concurrent 是 edge.concurrent.team 的上限。
type TeamQuota struct {
	TeamID     string
	Concurrent int
}

// Incident 是事故开关（键 relay:incident 与 kind: incident 广播）：开着时节点拒绝新的隧道与
// 端到端通道升级（4429 limit_reached），在线连接保留。
type Incident struct {
	RejectNewConnections bool
	AtMs                 int64
}

// NodeInfo 是节点表 relay:nodes 的一项（SaaS 实施设计 §2.4、Go 设计 §3.6）。URL 是该节点自己的隧道
// 地址（wss://<node>.edge.<域>/t/v1），不是共用入口；InternodeURL 是别的节点转发通道时连的
// /x/v1 地址（缺省由 URL 把路径换成 /x/v1 得出）。
type NodeInfo struct {
	ID           string
	URL          string
	Weight       int
	Version      string
	Draining     bool
	Connections  int
	Tunnels      int
	InternodeURL string
}
