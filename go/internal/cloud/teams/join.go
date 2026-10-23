package teams

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// Join 把受邀者加进团队，给 invitations.accept 在它的事务里调用。调用方必须已经：锁了团队行（LockTeam）、
// 核过令牌与账号邮箱，并锁了这条邀请且它仍是待处理。受邀者还不是成员，没有 TeamScope 可核；邀请就是授权，
// 所以范围只在这里按邀请的团队与角色构造，不交给调用方。
//
// 核对：团队在恢复期内 410 team_deleted；已是成员 409 member_exists；邀请的是 guest 而团队关了 guest
// 403 team_policy_denied（details.setting = allowGuests）；成员数或 guest 数越过额度 429 limit_reached
// （待处理的邀请也计入，这条邀请自己除外）。
func Join(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, inv saas.Invitation, accountID string) error {
	scope := teamscope.Issue(tenantID, inv.TeamID, inv.TeamRole)
	team, err := tx.Teams().Get(ctx, scope)
	if err != nil {
		return err
	}
	if team == nil {
		return core.Fail(gen.ErrNotFound)
	}
	if team.Deleted() {
		return core.Fail(gen.ErrTeamDeleted)
	}
	existing, err := tx.TeamAccess(ctx, tenantID, inv.TeamID, accountID)
	if err != nil {
		return err
	}
	if existing != nil {
		return core.Fail(gen.ErrMemberExists)
	}
	guest := inv.TeamRole == teamscope.Guest
	if guest && !SettingsOf(*team).AllowGuests {
		return core.FailWith(gen.ErrTeamPolicyDenied, map[string]any{"setting": "allowGuests"})
	}
	seats, err := tx.Teams().Seats(ctx, scope)
	if err != nil {
		return err
	}
	pending, err := tx.Invitations().PendingSeats(ctx, scope, nowMs)
	if err != nil {
		return err
	}
	// 这条邀请自己还在待处理里，已经算进了 pending；接受它不该重复计。
	if inv.Pending(nowMs) {
		if guest {
			pending.Guests--
		} else {
			pending.Members--
		}
	}
	e, err := tx.Entitlement(ctx, tenantID, saas.SpaceTeam, inv.TeamID)
	if err != nil {
		return err
	}
	lim := quota.ForTeam(saas.FreePlan, nil)
	if e != nil {
		lim = quota.ForTeam(e.Plan, e.Overrides)
	}
	if guest {
		if err := quota.Check(quota.Guests, seats.Guests+pending.Guests, lim.Guests); err != nil {
			return err
		}
	} else if err := quota.Check(quota.Members, seats.Members+pending.Members, lim.Members); err != nil {
		return err
	}
	return tx.Teams().AddMember(ctx, scope, accountID, inv.TeamRole, inv.InvitedBy, nowMs)
}
