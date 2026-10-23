package saas

import (
	"context"
)

// 授予的主体、来路与角色（迁移 0007 source_grants）。
const (
	GrantSubjectTeam   = "team"
	GrantSubjectMember = "member"
	GrantSubjectGroup  = "group"
	GrantSubjectGuest  = "guest"

	GrantOriginGrant      = "grant"
	GrantOriginInvitation = "invitation"

	// GrantRevokedBySystem 是到期由 grant.expire 写撤销时的 revoked_by。
	GrantRevokedBySystem = "system"
)

// Grant 是授予行：主机 × 主体 × 角色 × 范围 × 到期。
type Grant struct {
	GrantID  string
	SourceID string
	// TeamID 是主机所属的团队；个人空间的主机为空。
	TeamID string
	// SubjectKind 是 GrantSubject*；SubjectID：team → 团队 id；member / guest → 账号 id；group → 分组 id。
	SubjectKind string
	SubjectID   string
	Role        string
	// WorkspaceIDs 空 = 整台主机。
	WorkspaceIDs []string
	ExpiresAtMs  *int64
	GrantedBy    string
	Origin       string
	InvitationID string
	CreatedAtMs  int64
	UpdatedAtMs  int64
	RevokedAtMs  *int64
	RevokedBy    string
}

// ActiveAt 报告授予在 nowMs 是否生效：没撤销、没到期。
func (g Grant) ActiveAt(nowMs int64) bool {
	return g.RevokedAtMs == nil && (g.ExpiresAtMs == nil || *g.ExpiresAtMs > nowMs)
}

// GrantQuery 选授予行（只取没撤销的）。
type GrantQuery struct {
	// SourceID 非空只取这台主机的。
	SourceID string
	// AccountID 非空只取以这个账号为主体（member / guest）的。
	AccountID string
	// Limit 大于 0 时最多取这么多行。
	Limit int
}

// GrantPatch 是对授予的部分更新：非 nil 的字段才写；ClearExpiry 清掉到期。
type GrantPatch struct {
	Role         *string
	WorkspaceIDs *[]string
	ExpiresAtMs  *int64
	ClearExpiry  bool
}

// AccessBasis 是合成有效权限（grants.Effective）所需的库内事实：账号、主机、空间、团队与适用的授予。
// 不含任何合成逻辑，合成是纯函数。
type AccessBasis struct {
	// AccountActive：账号存在且没停用。
	AccountActive bool
	// SourceFound：主机有空间行；SourceRevoked：主机已撤销。
	SourceFound   bool
	SourceRevoked bool
	CoreVersion   string
	OwnerID       string
	// TeamID 空 = 个人空间。
	TeamID        string
	TeamDeleted   bool
	TeamSuspended bool
	// TeamRole 是账号在主机所属团队里的角色；不是成员或个人空间为空。
	TeamRole TeamRole
	// Grants 是适用于这个账号的、没撤销的授予：以它为主体的 member / guest，以及主机所属团队的 team 授予。
	// 到期由纯函数按时钟判断。
	Grants []Grant
}

// GrantReader 是授予的只读查询（Reader 嵌入它）。
type GrantReader interface {
	// Grant 按 id 读授予（含已撤销的）；没有答 nil。
	Grant(ctx context.Context, tenantID, grantID string) (*Grant, error)
	// ListGrants 列出没撤销的授予，按创建时间、id 升序。
	ListGrants(ctx context.Context, tenantID string, query GrantQuery) ([]Grant, error)
	// AccessBasis 读合成有效权限的事实；主机没有空间行答 SourceFound = false。
	AccessBasis(ctx context.Context, tenantID, accountID, sourceID string) (AccessBasis, error)
	// AccessAccounts 是可能经由某个来路到达主机的账号：所有者、member / guest 授予的主体、团队主机的全体成员，升序去重。
	AccessAccounts(ctx context.Context, tenantID, sourceID string) ([]string, error)
	// TeamAccountIDs 是团队全体成员的账号 id，升序。
	TeamAccountIDs(ctx context.Context, tenantID, teamID string) ([]string, error)
	// DueGrants 是到期（expires_at_ms <= nowMs）却还没撤销的授予 id，最多 limit 个。
	DueGrants(ctx context.Context, tenantID string, nowMs int64, limit int) ([]string, error)
}

// GrantRepo 是授予的仓储。写之前调用方已经按「团队行 → 主机」的顺序加过锁。
type GrantRepo interface {
	// Lock 读授予并加行锁（FOR UPDATE，含已撤销的）；没有答 nil。
	Lock(ctx context.Context, tenantID, grantID string) (*Grant, error)
	// Insert 插一条授予；同主机同主体已有有效的答 ErrConflict。
	Insert(ctx context.Context, tenantID string, grant Grant) error
	// Update 改没撤销的授予；没有或已撤销答 false。
	Update(ctx context.Context, tenantID, grantID string, patch GrantPatch, nowMs int64) (bool, error)
	// Revoke 撤销没撤销的授予（revokedBy 是操作者或 GrantRevokedBySystem）；没有或已撤销答 false。
	Revoke(ctx context.Context, tenantID, grantID, revokedBy string, nowMs int64) (bool, error)
	// RevokeAccount 撤销以账号为主体（member / guest）、在这台主机上没撤销的全部授予，答被撤销的授予 id。
	RevokeAccount(ctx context.Context, tenantID, sourceID, accountID, revokedBy string, nowMs int64) ([]string, error)
}
