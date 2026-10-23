package saas

import (
	"context"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// Host 是 SaaS 的一台主机：核心的源记录，加 source_spaces 的空间、所有者、标签与设置（迁移 0006）。
type Host struct {
	store.SourceRecord
	// TeamID 空 = 个人空间（归 OwnerAccountID）。
	TeamID         string
	OwnerAccountID string
	// OwnerName 是所有者的显示名。
	OwnerName string
	Tags      []string
	// Settings 是 hostSettings 的 JSON；nil = 缺省。
	Settings    json.RawMessage
	CreatedAtMs int64
	UpdatedAtMs int64
	// ViewerRole 是查询者在主机所属团队里的角色；个人空间为空。
	ViewerRole TeamRole
	// TeamDefaultRole 是主机所属团队设置里的 defaultHostRole（主机设置没写 defaultRole 时的预选值）；个人空间为空。
	TeamDefaultRole string
}

// HostCursor 是目录分页的游标：(registered_at_ms, source_id) 之后。
type HostCursor struct {
	RegisteredAtMs int64
	SourceID       string
}

// HostFilter 是目录查询：只返回 ViewerID 此刻能看到的主机（可见性在查询里，不在调用方）。
type HostFilter struct {
	// ViewerID 是查询者的账号 id。
	ViewerID string
	// SourceID 非空时只取这一台。
	SourceID string
	// SpaceKind 取 SpacePersonal / SpaceTeam；空 = 全部可达的空间。SpaceTeam 要给 TeamID。
	SpaceKind string
	TeamID    string
	// Revoked 为真只列已撤销的，否则只列未撤销的。
	Revoked bool
	// Tags 取「且」：主机要带齐。
	Tags []string
	// Query 是名称的模糊词（调用方不必转义，仓储自己转义 % _ \）。
	Query string
	// After 非空时只取排在它之后的。
	After *HostCursor
	// Limit 大于 0 时最多取这么多行。
	Limit int
}

// HostReader 是主机的只读查询（Reader 嵌入它）。
type HostReader interface {
	// ListHosts 按 (registered_at_ms, source_id) 升序列出 ViewerID 能看到的主机。
	ListHosts(ctx context.Context, tenantID string, filter HostFilter) ([]Host, error)
	// SpaceOf 读主机的空间行；没有答 nil。
	SpaceOf(ctx context.Context, tenantID, sourceID string) (*HostSpace, error)
}

// HostSpace 是主机的空间行连同源的撤销状态。
type HostSpace struct {
	SourceID       string
	TeamID         string
	OwnerAccountID string
	Revoked        bool
}

// HostPatch 是对 source_spaces 的部分更新：非 nil 的字段才写。
type HostPatch struct {
	Tags     *[]string
	Settings json.RawMessage
}

// RegistrationToken 是带空间的注册令牌（registration_tokens.team_id 空 = 个人空间）。
type RegistrationToken struct {
	TokenHash   string
	AccountID   string
	TeamID      string
	ExpiresAtMs int64
}

// HostRepo 是主机相关的仓储。
type HostRepo interface {
	// LockSource 对 (租户, 源) 取事务级咨询锁：登记同一个源 id 的并发事务排队。
	// 加锁顺序里它排在团队行与账号行之后。
	LockSource(ctx context.Context, tenantID, sourceID string) error
	// CountPersonal 数账号个人空间里未撤销的主机（不含 excludeSourceID）。
	CountPersonal(ctx context.Context, tenantID, accountID, excludeSourceID string) (int, error)
	// CountTeam 数团队里未撤销的主机（不含 excludeSourceID）。
	CountTeam(ctx context.Context, scope TeamScope, excludeSourceID string) (int, error)
	// UpsertSpace 写主机的空间行：同一空间的重登记保留标签与设置，换了空间则重置。
	UpsertSpace(ctx context.Context, tenantID string, space HostSpace, nowMs int64) error
	// Patch 改标签与设置；teamID 必须与库里的空间相符（空 = 个人空间），不符或没有这台主机答 false。
	Patch(ctx context.Context, tenantID, sourceID, teamID string, patch HostPatch, nowMs int64) (bool, error)
	// AddRegistration 写一枚注册令牌：账号同时有效的令牌已满 max 条答 false（什么也不写）。
	AddRegistration(ctx context.Context, tenantID string, token RegistrationToken, nowMs int64, max int) (bool, error)
	// PeekRegistration 读未过期的令牌但不消耗、不加锁：登记先靠它知道令牌属于哪个空间，再按「团队 → 账号」的顺序加锁。
	PeekRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*RegistrationToken, error)
	// ConsumeRegistration 一次性消耗未过期的令牌，答它的记录；没命中答 nil。
	ConsumeRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*RegistrationToken, error)
}
