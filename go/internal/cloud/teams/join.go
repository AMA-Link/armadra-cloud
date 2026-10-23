package teams

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// Join 把受邀者加进团队并把邀请置为 accepted，给 invitations.accept 在它的事务里调用。它自己先锁团队行、
// 再锁邀请行（加锁顺序：团队 → 邀请），核对邀请是团队邀请、仍待处理（撤销 / 过期 / 已拒绝 / 已接受各有各的码）、
// 账号邮箱等于邀请邮箱（403 invitation_email_mismatch）。令牌由调用方核。受邀者还不是成员，没有 TeamScope 可核；
// 邀请就是授权，所以范围只在这里按邀请的团队与角色构造，不交给调用方。
//
// 其余核对：团队在恢复期内 410 team_deleted；已是成员 409 member_exists；邀请人已不是 admin 以上 409 conflict
// （他被降级或移除时邀请本应一并撤销，这里是兜底）；邀请的是 guest 而团队关了 guest 403 team_policy_denied
// （details.setting = allowGuests）；成员数或 guest 数越过额度 429 limit_reached（待处理的邀请也计入，这条邀请自己除外）。
// 答团队 id。
func Join(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, invitationID string, account *saas.Account) (string, error) {
	probe, err := tx.Invitation(ctx, tenantID, invitationID)
	if err != nil {
		return "", err
	}
	if probe == nil || probe.Kind != saas.InvitationTeam {
		return "", core.Fail(gen.ErrNotFound)
	}
	if found, err := tx.Teams().LockTeam(ctx, tenantID, probe.TeamID); err != nil {
		return "", err
	} else if !found {
		return "", core.Fail(gen.ErrNotFound)
	}
	inv, err := tx.Invitations().Lock(ctx, tenantID, invitationID)
	if err != nil {
		return "", err
	}
	if inv == nil || inv.Kind != saas.InvitationTeam || inv.TeamID != probe.TeamID {
		return "", core.Fail(gen.ErrNotFound)
	}
	if inv.Email != account.Email {
		return "", core.Fail(gen.ErrInvitationEmailMismatch)
	}
	switch {
	case inv.State == saas.InvitationRevoked:
		return "", core.Fail(gen.ErrInvitationRevoked)
	case inv.State == saas.InvitationExpired || (inv.State == saas.InvitationPending && inv.ExpiresAtMs <= nowMs):
		return "", core.Fail(gen.ErrInvitationExpired)
	case inv.State != saas.InvitationPending:
		return "", core.Fail(gen.ErrConflict)
	}
	scope := teamscope.Issue(tenantID, inv.TeamID, inv.TeamRole)
	team, err := tx.Teams().Get(ctx, scope)
	if err != nil {
		return "", err
	}
	if team == nil {
		return "", core.Fail(gen.ErrNotFound)
	}
	if team.Deleted() {
		return "", core.Fail(gen.ErrTeamDeleted)
	}
	existing, err := tx.TeamAccess(ctx, tenantID, inv.TeamID, account.AccountID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", core.Fail(gen.ErrMemberExists)
	}
	inviter, err := tx.TeamAccess(ctx, tenantID, inv.TeamID, inv.InvitedBy)
	if err != nil {
		return "", err
	}
	if inviter == nil || (inviter.Role != teamscope.Owner && inviter.Role != teamscope.Admin) {
		return "", core.Fail(gen.ErrConflict)
	}
	guest := inv.TeamRole == teamscope.Guest
	if guest && !SettingsOf(*team).AllowGuests {
		return "", core.FailWith(gen.ErrTeamPolicyDenied, map[string]any{"setting": "allowGuests"})
	}
	seats, err := tx.Teams().Seats(ctx, scope)
	if err != nil {
		return "", err
	}
	pending, err := tx.Invitations().PendingSeats(ctx, scope, nowMs)
	if err != nil {
		return "", err
	}
	// 这条邀请自己还在待处理里，已经算进了 pending；接受它不该重复计。
	if guest {
		pending.Guests--
	} else {
		pending.Members--
	}
	e, err := tx.Entitlement(ctx, tenantID, saas.SpaceTeam, inv.TeamID)
	if err != nil {
		return "", err
	}
	lim := quota.ForTeam(saas.FreePlan, nil)
	if e != nil {
		lim = quota.ForTeam(e.Plan, e.Overrides)
	}
	if guest {
		if err := quota.Check(quota.Guests, seats.Guests+pending.Guests, lim.Guests); err != nil {
			return "", err
		}
	} else if err := quota.Check(quota.Members, seats.Members+pending.Members, lim.Members); err != nil {
		return "", err
	}
	if err := tx.Teams().AddMember(ctx, scope, account.AccountID, inv.TeamRole, inv.InvitedBy, nowMs); err != nil {
		return "", err
	}
	if err := tx.Invitations().Decide(ctx, tenantID, invitationID, saas.InvitationAccepted, account.AccountID, nowMs); err != nil {
		return "", err
	}
	return inv.TeamID, nil
}

// EnsureGuestMember 给被接受的主机邀请用（设计 §4.7）：接受者还不是团队成员就先加成 guest 成员，已是成员就原样答他的角色。
// 调用方已锁团队行（LockTeam）。团队在恢复期内 410 team_deleted；团队关了 guest 403 team_policy_denied
// （details.setting = allowGuests）；guest 数越过额度 429 limit_reached（待处理的团队邀请也计入）。
// 受邀者还不是成员，没有 TeamScope 可核；主机邀请就是授权，所以范围只在这里按邀请所在的团队构造。
func EnsureGuestMember(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, teamID string, account *saas.Account, invitedBy string) (saas.TeamRole, error) {
	scope := teamscope.Issue(tenantID, teamID, teamscope.Guest)
	team, err := tx.Teams().Get(ctx, scope)
	if err != nil {
		return "", err
	}
	if team == nil {
		return "", core.Fail(gen.ErrNotFound)
	}
	if team.Deleted() {
		return "", core.Fail(gen.ErrTeamDeleted)
	}
	existing, err := tx.TeamAccess(ctx, tenantID, teamID, account.AccountID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.Role, nil
	}
	if !SettingsOf(*team).AllowGuests {
		return "", core.FailWith(gen.ErrTeamPolicyDenied, map[string]any{"setting": "allowGuests"})
	}
	seats, err := tx.Teams().Seats(ctx, scope)
	if err != nil {
		return "", err
	}
	pending, err := tx.Invitations().PendingSeats(ctx, scope, nowMs)
	if err != nil {
		return "", err
	}
	e, err := tx.Entitlement(ctx, tenantID, saas.SpaceTeam, teamID)
	if err != nil {
		return "", err
	}
	lim := quota.ForTeam(saas.FreePlan, nil)
	if e != nil {
		lim = quota.ForTeam(e.Plan, e.Overrides)
	}
	if err := quota.Check(quota.Guests, seats.Guests+pending.Guests, lim.Guests); err != nil {
		return "", err
	}
	if err := tx.Teams().AddMember(ctx, scope, account.AccountID, teamscope.Guest, invitedBy, nowMs); err != nil {
		return "", err
	}
	return teamscope.Guest, nil
}
