package events

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// Publisher 是领域服务发事件的口：在事务里登记，提交成功之后才按受众发出（回滚、重试时丢弃）。
// 受众在提交之后读库算（此时成员、授予都是新状态），随事件一起发。零值与 nil 都是空操作（测试里不装配事件）。
type Publisher struct {
	Core  *core.Context
	Store saas.Store
	Log   *obs.Logger
}

func (p *Publisher) enabled() bool { return p != nil && p.Core != nil && p.Store != nil }

func (p *Publisher) tenant() string { return p.Core.TenantID }

func (p *Publisher) warn(ctx context.Context, what string, err error) {
	if p.Log != nil {
		p.Log.Warn(what, obs.Operation("events.audience"), obs.Error(err))
	}
	_ = ctx
}

// emit 在提交之后算受众并发事件；受众算不出来（库出错）就不发：事件只是提示，客户端重拉。
func (p *Publisher) emit(tx saas.Tx, event core.Event, audience Audience) {
	if !p.enabled() {
		return
	}
	tx.AfterCommit(func(ctx context.Context) {
		accounts, err := audience(ctx, p.Store.Read())
		if err != nil {
			p.warn(ctx, "算事件受众失败，事件没有发出", err)
			return
		}
		accounts = uniqueSorted(accounts)
		if len(accounts) == 0 {
			return
		}
		p.Core.Emit(event, &core.Audience{AccountIDs: accounts})
	})
}

// MemberChanged：团队成员变化。受众是团队全部成员加上变化的那个账号（被移除的人也要收到）。
func (p *Publisher) MemberChanged(tx saas.Tx, teamID, accountID, change string) {
	if !p.enabled() {
		return
	}
	event := NewMemberChanged(teamID, accountID, change, p.Core.NowMs())
	p.emit(tx, event, func(ctx context.Context, r saas.Reader) ([]string, error) {
		members, err := r.TeamAccountIDs(ctx, p.tenant(), teamID)
		return append(members, accountID), err
	})
}

// TeamRemoved：团队被删除。受众是此刻的全部成员（事件不带 accountId）。
func (p *Publisher) TeamRemoved(tx saas.Tx, teamID string) {
	if !p.enabled() {
		return
	}
	event := NewMemberChanged(teamID, "", MemberRemoved, p.Core.NowMs())
	p.emit(tx, event, func(ctx context.Context, r saas.Reader) ([]string, error) {
		return r.TeamAccountIDs(ctx, p.tenant(), teamID)
	})
}

// TeamRestored：团队从恢复期里恢复。事件不带 accountId（团队整个回来了），受众是全体成员。
func (p *Publisher) TeamRestored(tx saas.Tx, teamID string) {
	if !p.enabled() {
		return
	}
	event := NewMemberChanged(teamID, "", MemberAdded, p.Core.NowMs())
	p.emit(tx, event, func(ctx context.Context, r saas.Reader) ([]string, error) {
		return r.TeamAccountIDs(ctx, p.tenant(), teamID)
	})
}

// InvitationChanged：邀请状态变化。受众是邀请人、团队 admin 以上，以及被邀请邮箱对应的账号——只推邮箱已验证的账号
// （与 invitations.list 的 received 同一条规则，设计 §4.4、§7.1）。
func (p *Publisher) InvitationChanged(tx saas.Tx, inv saas.Invitation, state string) {
	if !p.enabled() {
		return
	}
	event := NewInvitationChanged(inv.InvitationID, inv.TeamID, inv.SourceID, state, p.Core.NowMs())
	p.emit(tx, event, func(ctx context.Context, r saas.Reader) ([]string, error) {
		var out []string
		if inv.TeamID != "" {
			staff, err := r.TeamStaffIDs(ctx, p.tenant(), inv.TeamID)
			if err != nil {
				return nil, err
			}
			out = append(out, staff...)
			// 团队邀请的邀请人只在仍是 owner / admin 时才收（被移除或降级的邀请人收不到）。
			if slices.Contains(staff, inv.InvitedBy) {
				out = append(out, inv.InvitedBy)
			}
		} else {
			out = append(out, inv.InvitedBy)
		}
		if inv.SourceID != "" {
			if space, err := r.SpaceOf(ctx, p.tenant(), inv.SourceID); err != nil {
				return nil, err
			} else if space != nil {
				out = append(out, space.OwnerAccountID)
			}
		}
		invitee, err := r.AccountByEmail(ctx, p.tenant(), strings.ToLower(inv.Email))
		if err != nil {
			return nil, err
		}
		if invitee != nil && invitee.EmailVerifiedAtMs != nil {
			out = append(out, invitee.AccountID)
		}
		return out, nil
	})
}

// GrantChanged：主机的授予变化。受众是主机所有者、团队 admin 以上、授予主体涉及的账号（团队主体 = 团队全体成员）。
func (p *Publisher) GrantChanged(tx saas.Tx, grant saas.Grant) {
	if !p.enabled() {
		return
	}
	event := NewGrantChanged(grant.SourceID, grant.GrantID, grant.TeamID, p.Core.NowMs())
	p.emit(tx, event, func(ctx context.Context, r saas.Reader) ([]string, error) {
		var out []string
		space, err := r.SpaceOf(ctx, p.tenant(), grant.SourceID)
		if err != nil {
			return nil, err
		}
		if space != nil {
			out = append(out, space.OwnerAccountID)
			if space.TeamID != "" {
				staff, err := r.TeamStaffIDs(ctx, p.tenant(), space.TeamID)
				if err != nil {
					return nil, err
				}
				out = append(out, staff...)
			}
		}
		switch grant.SubjectKind {
		case saas.GrantSubjectTeam:
			members, err := r.TeamAccountIDs(ctx, p.tenant(), grant.SubjectID)
			if err != nil {
				return nil, err
			}
			out = append(out, members...)
		case saas.GrantSubjectGroup:
		default:
			out = append(out, grant.SubjectID)
		}
		return out, nil
	})
}

// Audience 在提交之后读库算一个事件的受众账号（重复与空 id 由发事件的一方去掉）。
type Audience func(ctx context.Context, r saas.Reader) ([]string, error)

// DeviceTrustRequested：有新设备待背书。受众由信任服务给（请求的背书人，与 trust.list 同一个判断）。
func (p *Publisher) DeviceTrustRequested(tx saas.Tx, requestID, sourceID, teamID string, audience Audience) {
	if !p.enabled() {
		return
	}
	p.emit(tx, NewDeviceTrustRequested(requestID, sourceID, teamID, p.Core.NowMs()), audience)
}

// DeviceTrustResolved：待信任请求已处理（批准、拒绝、过期或作废）。受众同上，按处理之后的状态算。
func (p *Publisher) DeviceTrustResolved(tx saas.Tx, requestID, outcome string, audience Audience) {
	if !p.enabled() {
		return
	}
	p.emit(tx, NewDeviceTrustResolved(requestID, outcome, p.Core.NowMs()), audience)
}

func uniqueSorted(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
