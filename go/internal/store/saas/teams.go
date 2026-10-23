package saas

import (
	"context"
	"encoding/json"
)

// 团队的状态（迁移 0005 teams.status）。
const (
	TeamActive    = "active"
	TeamSuspended = "suspended"
)

// 空间的种类（entitlements.space_kind）。
const (
	SpacePersonal = "personal"
	SpaceTeam     = "team"
)

// FreePlan 是没有 entitlements 行的空间的计划代号。
const FreePlan = "free"

// Team 是团队行。Settings 是 teamSettings 的 JSON，形状取自协议包。
type Team struct {
	TeamID       string
	Slug         string
	Name         string
	AvatarURL    *string
	Settings     json.RawMessage
	Status       string
	StatusReason *string
	CreatedBy    string
	CreatedAtMs  int64
	UpdatedAtMs  int64
	DeletedAtMs  *int64
	PurgeAtMs    *int64
}

// Deleted 报告团队是否在删除后的恢复期内。
func (t Team) Deleted() bool { return t.DeletedAtMs != nil }

// TeamMember 是团队成员行加账号的邮箱与显示名。
type TeamMember struct {
	AccountID   string
	Email       string
	DisplayName string
	Role        TeamRole
	JoinedAtMs  int64
	InvitedBy   *string
	// ReachableHosts 只对 guest 有值：被直接授予、尚未撤销也没到期的主机数。
	ReachableHosts int
}

// TeamAccess 是团队加某个账号在其中的角色。
type TeamAccess struct {
	Team Team
	Role TeamRole
	// Plan 是团队的计划代号（entitlements，没有行 = free）。
	Plan string
}

// TeamSummary 是团队的名字与是否在恢复期内。
type TeamSummary struct {
	Name    string
	Deleted bool
}

// Entitlement 是空间的计划（entitlements 行）；Overrides 是逐项放宽额度的 JSON 对象。
type Entitlement struct {
	Plan      string
	Status    string
	Overrides json.RawMessage
}

// TeamEntitlement 是一个团队的计划与 overrides（没有 entitlements 行的团队是 Free、overrides 为空）。
type TeamEntitlement struct {
	TeamID    string
	Plan      string
	Overrides json.RawMessage
}

// Seats 是团队里占额度的人数：非 guest 成员与 guest 成员。
type Seats struct {
	Members int
	Guests  int
}

// TeamReader 是团队的只读查询（Reader 嵌入它）。第一个参数后都不收裸 teamID 以外的范围：
// 这些方法只用来「核成员与角色」之前的查找，答案不含其他成员的信息。
type TeamReader interface {
	// TeamAccess 读团队和账号在其中的角色；团队不存在或账号不是成员都答 nil（两者对调用方不可区分）。
	TeamAccess(ctx context.Context, tenantID, teamID, accountID string) (*TeamAccess, error)
	// TeamsOf 是账号所属的全部团队（含恢复期内的），按加入顺序。
	TeamsOf(ctx context.Context, tenantID, accountID string) ([]TeamAccess, error)
	// OwnedTeamCount 是账号作为 owner 且未删除的团队数（teams 额度的用量）。
	OwnedTeamCount(ctx context.Context, tenantID, accountID string) (int, error)
	// Entitlement 读空间的计划；没有行答 nil（即 Free）。
	Entitlement(ctx context.Context, tenantID, spaceKind, spaceID string) (*Entitlement, error)
	// TeamStaffIDs 是团队 owner 与 admin 的账号 id，升序（事件受众：邀请、授予变动要通知管理者）。
	TeamStaffIDs(ctx context.Context, tenantID, teamID string) ([]string, error)
	// TeamEntitlements 是租户全部团队的计划与 overrides（含恢复期内的），按 team_id 升序：serve 启动与
	// Redis 订阅重新就绪时全量重写 relay:quota:team:*（设计 §7.2）。
	TeamEntitlements(ctx context.Context, tenantID string) ([]TeamEntitlement, error)
	// TeamSummary 读团队的名字与删除状态（匿名的邀请摘要用：调用方已核过邀请令牌）；团队不存在答 nil。
	TeamSummary(ctx context.Context, tenantID, teamID string) (*TeamSummary, error)
	// DueTeams 是恢复期已满、该清除的团队 id（purge_at_ms <= nowMs），最多 limit 个。
	DueTeams(ctx context.Context, tenantID string, nowMs int64, limit int) ([]string, error)
}

// TeamRepo 是团队相关的仓储。团队范围的方法收 TeamScope，不收裸 teamID；查询都带 team_id。
type TeamRepo interface {
	// LockTeam 对团队行 SELECT … FOR UPDATE，答它存不存在。改成员前先锁它，
	// 与 team_owner_guard 触发器用同一把锁（设计 §3.4 0005）。同一事务里必须先于 LockAccount，否则 ErrLockOrder。
	LockTeam(ctx context.Context, tenantID, teamID string) (found bool, err error)
	// LockTeams 一次锁多个团队：升序去重再逐个锁。同一事务里单个调用 LockTeam 的 id 也必须升序。
	LockTeams(ctx context.Context, tenantID string, teamIDs ...string) error

	// Create 插团队行、owner 成员与 Free 计划的 entitlements 行（owner 成员与团队同事务，约束在提交时检查）；
	// slug 被占用答 ErrTeamSlugTaken。创建者还没有 TeamScope，所以这是唯一收裸 id 的写方法。
	Create(ctx context.Context, tenantID string, team Team, ownerID string) error
	// Get 读团队行。
	Get(ctx context.Context, scope TeamScope) (*Team, error)
	// Members 是团队全部成员，按加入顺序；guest 带 ReachableHosts（按 nowMs 判到期）。
	Members(ctx context.Context, scope TeamScope, nowMs int64) ([]TeamMember, error)
	// Member 读一个成员；不是成员答 nil。
	Member(ctx context.Context, scope TeamScope, accountID string) (*TeamMember, error)
	// Seats 数占额度的成员：非 guest 与 guest。
	Seats(ctx context.Context, scope TeamScope) (Seats, error)
	// Rename 改名与头像；avatar 为 nil 表示不改，指向空串表示清除。
	Rename(ctx context.Context, scope TeamScope, name string, avatar *string, nowMs int64) error
	// SetSettings 整份写设置。
	SetSettings(ctx context.Context, scope TeamScope, settings json.RawMessage, nowMs int64) error
	// SetRole 改成员角色；最后一个 owner 被降级时提交失败（ErrTeamLastOwner）。
	SetRole(ctx context.Context, scope TeamScope, accountID string, role TeamRole) error
	// AddMember 插成员；已是成员答 ErrMemberExists。
	AddMember(ctx context.Context, scope TeamScope, accountID string, role TeamRole, invitedBy string, nowMs int64) error
	// RemoveMember 删成员（分组成员级联）。
	RemoveMember(ctx context.Context, scope TeamScope, accountID string) error
	// OldestOwner 是加入最早的 owner（排除 exclude）；没有答空串。
	OldestOwner(ctx context.Context, scope TeamScope, exclude string) (string, error)
	// SetDeleted 置删除时刻与清除时刻；Restore 清掉它们。
	SetDeleted(ctx context.Context, scope TeamScope, deletedAtMs, purgeAtMs int64) error
	Restore(ctx context.Context, scope TeamScope, nowMs int64) error
	// HostIDs 是团队未撤销的主机（source_spaces ⋈ sources），按 id 升序。
	HostIDs(ctx context.Context, scope TeamScope) ([]string, error)
	// ReassignHosts 把 from 登记的团队主机的所有者改成 to。
	ReassignHosts(ctx context.Context, scope TeamScope, from, to string, nowMs int64) error
	// RevokeMemberGrants 撤销以账号为主体（member / guest）的团队授予，答受影响的主机；revokedBy 是操作者。
	RevokeMemberGrants(ctx context.Context, scope TeamScope, accountID, revokedBy string, nowMs int64) ([]string, error)
	// CountMemberHosts 数账号在团队里登记的未撤销主机。
	CountMemberHosts(ctx context.Context, scope TeamScope, accountID string) (int, error)
	// Usage 是团队的当前用量：未撤销主机数与本月 e2e 流量字节（relay_traffic_buckets.team_id）。
	Usage(ctx context.Context, scope TeamScope, monthStartMs, monthEndMs int64) (hosts int, trafficBytes int64, err error)

	// Purge 清除团队（恢复期已满，purge_at_ms <= nowMs）：撤销主机、删授予、邀请、链接、待信任、注册令牌、source_spaces、
	// entitlements 与团队行（成员、分组级联），审计行保留。清除是系统作业，没有调用者，所以收裸 id；
	// 返回被撤销的主机，调用方（作业）据此撤销令牌。已经清走或恢复期未满的团队答 purged = false。
	Purge(ctx context.Context, tenantID, teamID string, nowMs int64) (purged bool, sourceIDs []string, err error)
}
