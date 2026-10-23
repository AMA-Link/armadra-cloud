package teams

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// memberGroup 是成员所在的分组（M2）。
type memberGroup struct {
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
}

// memberOut 是成员的线上形状（协议包 teamMemberSchema）。groups 是 M2 的分组，M1 恒为空数组；
// reachableHosts 只对 guest 有值。
type memberOut struct {
	AccountID      string        `json:"accountId"`
	Email          *string       `json:"email"`
	DisplayName    string        `json:"displayName"`
	Role           string        `json:"role"`
	JoinedAtMs     int64         `json:"joinedAtMs"`
	Groups         []memberGroup `json:"groups"`
	ReachableHosts *int          `json:"reachableHosts,omitempty"`
}

func memberView(m saas.TeamMember) memberOut {
	out := memberOut{
		AccountID: m.AccountID, DisplayName: m.DisplayName, Role: string(m.Role), JoinedAtMs: m.JoinedAtMs, Groups: []memberGroup{},
	}
	if m.Email != "" {
		email := m.Email
		out.Email = &email
	}
	if m.Role == teamscope.Guest {
		reachable := m.ReachableHosts
		out.ReachableHosts = &reachable
	}
	return out
}

// members 是 teams.members：owner、admin、member、billing 看得到成员列表，guest 看不到。
func (s *Service) members(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	var out []memberOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Require(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinDirectory)
		if err != nil {
			return err
		}
		members, err := tx.Teams().Members(ctx, access.Scope, s.nowMs())
		if err != nil {
			return err
		}
		out = make([]memberOut, 0, len(members))
		for _, m := range members {
			out = append(out, memberView(m))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"members": out}, nil
}

// narrows 报告角色变化是否收窄了权限：降级（owner > admin > member）或变成 billing / guest。
// 收窄走撤销流程（设计 §4.8）：撤销此人在团队各主机上已签的令牌并踢掉在线连接。
func narrows(from, to saas.TeamRole) bool {
	if from == to {
		return false
	}
	if to == teamscope.Billing || to == teamscope.Guest {
		return true
	}
	rank := map[saas.TeamRole]int{teamscope.Owner: 3, teamscope.Admin: 2, teamscope.Member: 1}
	return rank[to] < rank[from]
}

// memberRole 是 teams.memberRole（admin）：admin 不能改 owner；降最后一个 owner 答 409 team_last_owner；
// guest 与非 guest 之间互转要核对设置与额度；收窄走撤销流程。
func (s *Service) memberRole(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		AccountID string        `json:"accountId"`
		Role      saas.TeamRole `json:"role"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	var out memberOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinAdmin)
		if err != nil {
			return err
		}
		target, err := tx.Teams().Member(ctx, access.Scope, in.AccountID)
		if err != nil {
			return err
		}
		if target == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if access.Role == teamscope.Admin && target.Role == teamscope.Owner {
			return core.Fail(gen.ErrTeamRoleInsufficient)
		}
		if target.Role != in.Role {
			if err := s.checkRoleChange(ctx, tx, access, *target, in.Role); err != nil {
				return err
			}
			if err := tx.Teams().SetRole(ctx, access.Scope, target.AccountID, in.Role); err != nil {
				return err
			}
			if narrows(target.Role, in.Role) {
				if err := s.revokeMember(ctx, tx, access, account.AccountID, target.AccountID); err != nil {
					return err
				}
			}
			if err := s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionRole, "member", target.AccountID); err != nil {
				return err
			}
			target.Role = in.Role
		}
		out = memberView(*target)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkRoleChange 核对角色变化的前置条件：最后一个 owner 不能降级；变成 guest 要设置允许且 guest 额度有余；
// guest 变成正式成员要成员额度有余（待处理的邀请也计入）。
func (s *Service) checkRoleChange(ctx context.Context, tx saas.Tx, access Access, target saas.TeamMember, to saas.TeamRole) error {
	if target.Role == teamscope.Owner {
		other, err := tx.Teams().OldestOwner(ctx, access.Scope, target.AccountID)
		if err != nil {
			return err
		}
		if other == "" {
			return core.Fail(gen.ErrTeamLastOwner)
		}
	}
	if (to == teamscope.Guest) == (target.Role == teamscope.Guest) {
		return nil
	}
	seats, err := tx.Teams().Seats(ctx, access.Scope)
	if err != nil {
		return err
	}
	pending, err := tx.Invitations().PendingSeats(ctx, access.Scope, s.nowMs())
	if err != nil {
		return err
	}
	lim, err := limits(ctx, tx, s.tenant(), access)
	if err != nil {
		return err
	}
	if to == teamscope.Guest {
		if !SettingsOf(access.Team).AllowGuests {
			return core.Fail(gen.ErrGuestsDisabled)
		}
		return quota.Check(quota.Guests, seats.Guests+pending.Guests, lim.Guests)
	}
	return quota.Check(quota.Members, seats.Members+pending.Members, lim.Members)
}

// revokeMember 撤销此人在团队里的授予，提交后撤销他在团队各主机上已签的令牌并踢掉在线连接。
func (s *Service) revokeMember(ctx context.Context, tx saas.Tx, access Access, actorID, accountID string) error {
	if _, err := tx.Teams().RevokeMemberGrants(ctx, access.Scope, accountID, actorID, s.nowMs()); err != nil {
		return err
	}
	hosts, err := tx.Teams().HostIDs(ctx, access.Scope)
	if err != nil {
		return err
	}
	s.afterRevokeAccess(tx, accountID, hosts, "member_changed")
	return nil
}

// removeMember 是移除与离开共用的一步：最后一个 owner 不能走；此人登记的主机 owner 改成团队里加入最早的 owner；
// 撤销授予与已签令牌；删成员（分组成员级联）。
func (s *Service) removeMember(ctx context.Context, tx saas.Tx, access Access, actorID string, target saas.TeamMember) error {
	if target.Role == teamscope.Owner {
		other, err := tx.Teams().OldestOwner(ctx, access.Scope, target.AccountID)
		if err != nil {
			return err
		}
		if other == "" {
			return core.Fail(gen.ErrTeamLastOwner)
		}
	}
	heir, err := tx.Teams().OldestOwner(ctx, access.Scope, target.AccountID)
	if err != nil {
		return err
	}
	if heir != "" {
		if err := tx.Teams().ReassignHosts(ctx, access.Scope, target.AccountID, heir, s.nowMs()); err != nil {
			return err
		}
	}
	if err := s.revokeMember(ctx, tx, access, actorID, target.AccountID); err != nil {
		return err
	}
	return tx.Teams().RemoveMember(ctx, access.Scope, target.AccountID)
}

// memberRemove 是 teams.memberRemove（admin）：admin 不能移除 owner。
func (s *Service) memberRemove(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		AccountID string `json:"accountId"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinAdmin)
		if err != nil {
			return err
		}
		target, err := tx.Teams().Member(ctx, access.Scope, in.AccountID)
		if err != nil {
			return err
		}
		if target == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if access.Role == teamscope.Admin && target.Role == teamscope.Owner {
			return core.Fail(gen.ErrTeamRoleInsufficient)
		}
		if err := s.removeMember(ctx, tx, access, account.AccountID, *target); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionRemove, "member", target.AccountID)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// leave 是 teams.leave（任一角色）：最后一个 owner 不能离开。
func (s *Service) leave(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinAny)
		if err != nil {
			return err
		}
		target, err := tx.Teams().Member(ctx, access.Scope, account.AccountID)
		if err != nil {
			return err
		}
		if target == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if err := s.removeMember(ctx, tx, access, account.AccountID, *target); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionLeave, "member", account.AccountID)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// transferOwnership 是 teams.transferOwnership（owner）：目标必须是 admin（否则 400 bad_request）；
// 同一事务里目标升 owner、调用者降 admin，提交时检查「至少一个 owner」。
func (s *Service) transferOwnership(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		AccountID string `json:"accountId"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinOwner)
		if err != nil {
			return err
		}
		target, err := tx.Teams().Member(ctx, access.Scope, in.AccountID)
		if err != nil {
			return err
		}
		if target == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if target.Role != teamscope.Admin {
			return core.Fail(gen.ErrBadRequest)
		}
		if err := tx.Teams().SetRole(ctx, access.Scope, target.AccountID, teamscope.Owner); err != nil {
			return err
		}
		if err := tx.Teams().SetRole(ctx, access.Scope, account.AccountID, teamscope.Admin); err != nil {
			return err
		}
		if err := s.revokeMember(ctx, tx, access, account.AccountID, account.AccountID); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionTransfer, "member", target.AccountID)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
