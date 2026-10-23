package saas

import (
	"context"
)

// 邀请的种类与状态（迁移 0007 invitations）。
const (
	InvitationTeam = "team"
	InvitationHost = "host"

	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationDeclined = "declined"
	InvitationRevoked  = "revoked"
	InvitationExpired  = "expired"
)

// Invitation 是邀请行。TokenHash 为空表示邮件作业还没发出；明文令牌只在邮件里。
type Invitation struct {
	InvitationID string
	Kind         string
	// Email 一律小写。
	Email string
	// TeamID、TeamRole 是团队邀请的；SourceID、HostRole、WorkspaceIDs 是主机邀请的（S6）。
	TeamID       string
	TeamRole     TeamRole
	SourceID     string
	HostRole     string
	WorkspaceIDs []string
	TokenHash    string
	State        string
	InvitedBy    string
	DecidedBy    string
	CreatedAtMs  int64
	ExpiresAtMs  int64
	// GrantExpiresAtMs 是主机邀请被接受后授予的到期；团队邀请恒为空。
	GrantExpiresAtMs *int64
	DecidedAtMs      *int64
	SentAtMs         *int64
	ResendCount      int
}

// Pending 报告邀请此刻是否仍待处理（状态为 pending 且没过期）。
func (i Invitation) Pending(nowMs int64) bool {
	return i.State == InvitationPending && i.ExpiresAtMs > nowMs
}

// InvitationReader 是邀请的只读查询（Reader 嵌入它）。
type InvitationReader interface {
	// Invitation 按 id 读邀请；没有答 nil。
	Invitation(ctx context.Context, tenantID, invitationID string) (*Invitation, error)
	// ReceivedInvitations 是发给邮箱的待处理邀请（没过期），按创建时间倒序。
	ReceivedInvitations(ctx context.Context, tenantID, email string, nowMs int64) ([]Invitation, error)
}

// InvitationRepo 是邀请的仓储。
type InvitationRepo interface {
	// Lock 读邀请并加行锁（FOR UPDATE）；没有答 nil。加锁顺序里排在团队行与账号行之后。
	Lock(ctx context.Context, tenantID, invitationID string) (*Invitation, error)
	// ExpireStale 把同一团队同一邮箱下已过期的 pending 改成 expired（插新 pending 前同事务先做，
	// 否则过期未处理的行会占着部分唯一索引挡住重新邀请）。
	ExpireStale(ctx context.Context, scope TeamScope, email string, nowMs int64) error
	// Insert 插一条团队邀请；同团队同邮箱已有 pending 答 ErrConflict。
	Insert(ctx context.Context, scope TeamScope, invitation Invitation) error
	// HasPending 报告同团队同邮箱是否已有待处理（没过期）的邀请。
	HasPending(ctx context.Context, scope TeamScope, email string, nowMs int64) (bool, error)
	// List 是团队发出的邀请，按创建时间倒序。
	List(ctx context.Context, scope TeamScope) ([]Invitation, error)
	// PendingSeats 数待处理（没过期）的团队邀请：非 guest 与 guest（它们也占额度）。
	PendingSeats(ctx context.Context, scope TeamScope, nowMs int64) (Seats, error)
	// Decide 置终态（accepted、declined、revoked）并记决定者与时刻。
	Decide(ctx context.Context, tenantID, invitationID, state, decidedBy string, nowMs int64) error
	// Reissue 重发：邀请回到 pending（已过期的也可以重发）、换新的有效期、清令牌哈希
	// （新令牌由发信作业生成）、重发次数加一；已有另一条 pending 占着唯一索引时答 ErrConflict。
	Reissue(ctx context.Context, tenantID, invitationID string, expiresAtMs int64) error
	// SetToken 记发信作业生成的令牌哈希与发出时刻（旧令牌随之作废）。
	SetToken(ctx context.Context, tenantID, invitationID, tokenHash string, nowMs int64) error
	// ExpireDue 把到期的 pending 落库成 expired，答它们的 id 与团队（job 发 invitationChanged 用）。
	ExpireDue(ctx context.Context, tenantID string, nowMs int64) ([]Invitation, error)
}
