package saas

import (
	"context"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 待信任请求的状态（迁移 0007 trust_requests.state）。
const (
	TrustPending  = "pending"
	TrustTrusted  = "trusted"
	TrustRejected = "rejected"
	TrustExpired  = "expired"
)

// 待信任请求被解决时的 resolved_by 里不是账号的取值。
const (
	// TrustResolvedBySource 是主机回报（sources.trustResolve）。
	TrustResolvedBySource = "source"
	// TrustResolvedBySystem 是到期作业。
	TrustResolvedBySystem = "system"
	// TrustResolvedBySuperseded 是同一设备上报了新的待信任条目，旧的作废。
	TrustResolvedBySuperseded = "superseded"
)

// TrustRequest 是新设备待信任请求行：云端只存通知所需的元数据，没有校验码（设计 §4.7）。
type TrustRequest struct {
	RequestID string
	SourceID  string
	// PendingID 是主机侧待信任条目的 id，兼幂等键。
	PendingID string
	TeamID    string
	AccountID string
	DeviceID  string
	// DeviceName 与 Platform 由云端从 devices 表填，不信主机报的。
	DeviceName    string
	Platform      string
	State         string
	RequestedAtMs int64
	ExpiresAtMs   int64
	ResolvedAtMs  *int64
	ResolvedBy    string
}

// TrustView 是待信任请求连同通知要用的联表信息：主机名与所有者、请求者的邮箱与显示名、查询者在团队里的角色与团队设置。
type TrustView struct {
	TrustRequest
	SourceName    string
	SourceOwnerID string
	MemberEmail   string
	MemberName    string
	// RequesterRole 是请求者在团队里的角色（guest 的请求只给主机所有者）。
	RequesterRole TeamRole
	// ViewerRole 是查询者在请求所属团队里的角色。
	ViewerRole TeamRole
	// TeamSettings 是团队设置 JSON（deviceEndorsers、memberSelfEndorse 在里面）。
	TeamSettings json.RawMessage
}

// TrustReader 是待信任请求的只读查询（Reader 嵌入它）。
type TrustReader interface {
	// TrustView 按 id 读一条请求，ViewerRole 为 viewerID 在团队里的角色（不是成员为空）；请求不存在、
	// 主机已撤销或团队在恢复期内答 nil。
	TrustView(ctx context.Context, tenantID, requestID, viewerID string) (*TrustView, error)
	// PendingTrust 列 viewerID 所在团队里待处理（没过期）的请求，teamID 非空时只列那个团队，按请求时间倒序，最多 limit 条。
	// 只含主机未撤销、团队未在恢复期、请求者仍是团队成员、且 viewerID 是背书人的（按团队设置 deviceEndorsers / memberSelfEndorse 在 SQL 里收窄，
	// 再 LIMIT，不会先截断再过滤）。
	PendingTrust(ctx context.Context, tenantID, viewerID, teamID string, nowMs int64, limit int) ([]TrustView, error)
}

// TrustRepo 是待信任请求的仓储。写之前调用方已经 LockSource（上报与回报）或锁了团队行（拒绝）。
type TrustRepo interface {
	// Lock 读请求并加行锁；没有答 nil。
	Lock(ctx context.Context, tenantID, requestID string) (*TrustRequest, error)
	// ByPending 按 (源, 主机侧条目 id) 读请求并加行锁；没有答 nil。
	ByPending(ctx context.Context, tenantID, sourceID, pendingID string) (*TrustRequest, error)
	// LastRejected 是 (源, 设备) 最近一次被拒绝的请求；没有答 nil。
	LastRejected(ctx context.Context, tenantID, sourceID, deviceID string) (*TrustRequest, error)
	// ExpireStale 把 (源, 设备) 下已过期的 pending 改成 expired（插新 pending 前同事务先做，否则它会占着部分唯一索引）。
	ExpireStale(ctx context.Context, tenantID, sourceID, deviceID string, nowMs int64) error
	// Supersede 把 (源, 设备) 下仍待处理的请求改成 expired（resolved_by = superseded），答它们的 id。
	Supersede(ctx context.Context, tenantID, sourceID, deviceID string, nowMs int64) ([]string, error)
	// Insert 插一条 pending；同 (源, 条目 id) 或同 (源, 设备) 已有 pending 答 ErrConflict。
	Insert(ctx context.Context, tenantID string, request TrustRequest) error
	// Reopen 把冷却已过的 rejected 请求重新开成 pending（新的 requestId、新的请求与过期时间），条目 id 不变；团队、设备名与平台按此刻的值刷新（设备可能改过名、主机可能换了团队）。
	Reopen(ctx context.Context, tenantID, requestID, newRequestID, teamID, deviceName, platform string, nowMs, expiresAtMs int64) (bool, error)
	// Resolve 把 pending 改成终态；不是 pending 答 false。
	Resolve(ctx context.Context, tenantID, requestID, state, resolvedBy string, nowMs int64) (bool, error)
	// ExpireDue 把到期的 pending 落库成 expired，答它们（通知要用）。
	ExpireDue(ctx context.Context, tenantID string, nowMs int64) ([]TrustRequest, error)
	// GC 删早于 beforeMs 解决的请求（冷却期早已过去）。
	GC(ctx context.Context, tenantID string, beforeMs int64) (int, error)
}

// LinkReader 是链接的 SaaS 侧只读查询（Reader 嵌入它）。链接本身在核心端口里。
type LinkReader interface {
	// LinksForSources 是这些主机名下的链接（含已撤销的），按创建顺序，最多 limit 条。
	LinksForSources(ctx context.Context, tenantID string, sourceIDs []string, limit int) ([]store.LinkRecord, error)
}
