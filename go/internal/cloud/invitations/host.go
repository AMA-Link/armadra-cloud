package invitations

import (
	"context"
	"errors"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 主机邀请（设计 §4.7、能力设计 §11.3）：按邮箱定向、要登录；被接受后落成云端授予加 guest 成员关系，
// 不在 core 里落邀请。邀请自身 7 天内有效（TTL），授予的到期是另一回事（invitations.grant_expires_at_ms，
// 缺省不过期）。

// GrantMaxTTL 是创建主机邀请时给的授予到期最长离现在多久（协议包 createInvitationInput：最长 30 天）。
const GrantMaxTTL = 30 * 24 * time.Hour

// maxListedHostInvitations 是 invitations.list 一次最多答的主机邀请数。
const maxListedHostInvitations = 500

// maxManagedHosts 是不带 sourceId 列主机邀请时最多看的主机数。
const maxManagedHosts = 2000

type hostInput struct {
	SourceID     string
	Email        string
	Role         string
	WorkspaceIDs []string
	// GrantExpiresAtMs 是创建入参里的 expiresAtMs：授予的到期，缺省不过期。
	GrantExpiresAtMs *int64
}

func isStaffRole(role saas.TeamRole) bool {
	return role == saas.RoleOwner || role == saas.RoleAdmin || role == saas.RoleMember
}

// createHost 是 invitations.create 的主机邀请：主机管理权（§4.6）+ 主机设置 allowGuests（团队主机另看团队的
// allowGuests）；受邀者是主机所有者答 member_exists；同邮箱有待处理的 409 conflict（过期的先改 expired）；
// 角色必须是四档之一，workspaceIds 按 scopeEntrySchema（grants.CleanWorkspaces）；团队主机上受邀者不是 owner / admin /
// member 时会成为 guest，而团队 guest 只能被授予具体的工作空间，所以 workspaceIds 不能为空（accept 时再核一次）。
func (s *Service) createHost(ctx context.Context, call *api.Call, account *saas.Account, in hostInput) (any, error) {
	if !grants.ValidRole(in.Role) {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	workspaces, err := grants.CleanWorkspaces(in.WorkspaceIDs)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	if in.GrantExpiresAtMs != nil && (*in.GrantExpiresAtMs <= nowMs || *in.GrantExpiresAtMs > nowMs+GrantMaxTTL.Milliseconds()) {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	email := normalizeEmail(in.Email)
	var out invitationOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		h, err := grants.ManageHost(ctx, tx, s.tenant(), nowMs, account.AccountID, in.SourceID)
		if err != nil {
			return err
		}
		if !hosts.GuestsAllowed(h) {
			return core.Fail(gen.ErrGuestsDisabled)
		}
		if h.TeamID != "" {
			ta, err := tx.TeamAccess(ctx, s.tenant(), h.TeamID, account.AccountID)
			if err != nil {
				return err
			}
			if ta == nil || !teams.SettingsOf(ta.Team).AllowGuests {
				return core.Fail(gen.ErrGuestsDisabled)
			}
		}
		invitee, err := tx.AccountByEmail(ctx, s.tenant(), email)
		if err != nil {
			return err
		}
		staff := false
		if invitee != nil {
			if invitee.AccountID == h.OwnerAccountID {
				return core.Fail(gen.ErrMemberExists)
			}
			if h.TeamID != "" {
				existing, err := tx.TeamAccess(ctx, s.tenant(), h.TeamID, invitee.AccountID)
				if err != nil {
					return err
				}
				if existing != nil {
					if existing.Role == saas.RoleBilling {
						return core.Fail(gen.ErrConflict) // 账单管理员不访问主机
					}
					staff = isStaffRole(existing.Role)
				}
			}
		}
		if h.TeamID != "" && !staff && len(workspaces) == 0 {
			return core.Fail(gen.ErrBadRequest)
		}
		if err := tx.Invitations().ExpireStaleHost(ctx, s.tenant(), h.SourceID, email, nowMs); err != nil {
			return err
		}
		if pending, err := tx.Invitations().HasPendingHost(ctx, s.tenant(), h.SourceID, email, nowMs); err != nil {
			return err
		} else if pending {
			return core.Fail(gen.ErrConflict)
		}
		inv := saas.Invitation{
			InvitationID: core.NewUUID(), Kind: saas.InvitationHost, Email: email, TeamID: h.TeamID, SourceID: h.SourceID,
			HostRole: in.Role, WorkspaceIDs: workspaces, InvitedBy: account.AccountID, CreatedAtMs: nowMs,
			ExpiresAtMs: nowMs + TTL.Milliseconds(), GrantExpiresAtMs: in.GrantExpiresAtMs, State: saas.InvitationPending,
		}
		if err := tx.Invitations().InsertHost(ctx, s.tenant(), inv); err != nil {
			return err
		}
		if err := s.enqueueHostMail(ctx, tx, h, inv, account, invitee); err != nil {
			return err
		}
		out = view(inv, nowMs)
		return s.recordFor(ctx, tx, call, account.AccountID, h.TeamID, h.SourceID, ActionCreate, inv.InvitationID)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// enqueueHostMail 在事务里插主机邀请邮件：语言取收件账号的 locale，没有账号的收件人取邀请人的。
func (s *Service) enqueueHostMail(ctx context.Context, tx saas.Tx, h saas.Host, inv saas.Invitation, inviter, invitee *saas.Account) error {
	locale := inviter.Locale
	if invitee != nil {
		locale = invitee.Locale
	}
	return s.mail.Enqueue(ctx, tx, mail.Mail{
		Template: mail.HostInvitation, Locale: locale, To: inv.Email, InvitationID: inv.InvitationID,
		Params: mail.Params{HostName: h.Name, InviterName: inviter.DisplayName, Role: inv.HostRole},
	})
}

// recheckHostReissue：已过期的主机邀请重发会回到待处理，把创建时的条件再核一遍（主机设置、受邀者身份）。
func (s *Service) recheckHostReissue(ctx context.Context, tx saas.Tx, h saas.Host, callerID string, inv saas.Invitation) error {
	if !hosts.GuestsAllowed(h) {
		return core.Fail(gen.ErrGuestsDisabled)
	}
	if h.TeamID != "" {
		ta, err := tx.TeamAccess(ctx, s.tenant(), h.TeamID, callerID)
		if err != nil {
			return err
		}
		if ta == nil || !teams.SettingsOf(ta.Team).AllowGuests {
			return core.Fail(gen.ErrGuestsDisabled)
		}
	}
	if invitee, err := tx.AccountByEmail(ctx, s.tenant(), inv.Email); err != nil {
		return err
	} else if invitee != nil && invitee.AccountID == h.OwnerAccountID {
		return core.Fail(gen.ErrMemberExists)
	}
	return nil
}

// listHostInvitations 追加调用者能管的主机邀请：给了 sourceId 只列那一台（要有管理权，看不到 404、管不了 403），
// 否则是调用者管得了的全部主机。
func (s *Service) listHostInvitations(ctx context.Context, tx saas.Tx, account *saas.Account, sourceID string, nowMs int64, out *[]invitationOut) error {
	var sourceIDs []string
	if sourceID != "" {
		h, err := grants.FindHost(ctx, tx, s.tenant(), nowMs, account.AccountID, sourceID)
		if err != nil {
			return err
		}
		if !h.CanManage(account.AccountID) {
			return grants.DenyManage(h)
		}
		sourceIDs = []string{sourceID}
	} else {
		visible, err := tx.ListHosts(ctx, s.tenant(), saas.HostFilter{ViewerID: account.AccountID, NowMs: nowMs, Limit: maxManagedHosts})
		if err != nil {
			return err
		}
		for _, h := range visible {
			if h.CanManage(account.AccountID) {
				sourceIDs = append(sourceIDs, h.SourceID)
			}
		}
	}
	list, err := tx.HostInvitations(ctx, s.tenant(), sourceIDs, maxListedHostInvitations)
	if err != nil {
		return err
	}
	for _, inv := range list {
		*out = append(*out, view(inv, nowMs))
	}
	return nil
}

// hostLabel 是主机邀请预览里的主机名（清理过）；主机已撤销、不存在或所在团队在恢复期内都答 404。
func (s *Service) hostLabel(ctx context.Context, reader saas.Reader, inv saas.Invitation) (string, error) {
	source, err := s.c.Ports.Sources.Get(ctx, s.tenant(), inv.SourceID)
	if err != nil {
		return "", err
	}
	if source == nil || source.RevokedAtMs != nil {
		return "", core.Fail(gen.ErrNotFound)
	}
	if inv.TeamID != "" {
		team, err := reader.TeamSummary(ctx, s.tenant(), inv.TeamID)
		if err != nil {
			return "", err
		}
		if team == nil || team.Deleted {
			return "", core.Fail(gen.ErrNotFound)
		}
	}
	return mail.SanitizeLabel(source.Name, 128), nil
}

// acceptHost 是 accept 里主机邀请的那一半（设计 §4.7），全部在调用方的事务里：
//
//	锁（团队行 → 主机 → 邀请行）→ 令牌与邮箱 → 已接受的原样答 → 撤销 / 过期 / 已拒绝 → 主机仍在 → 邀请人仍有管理权 →
//	主机设置 → 团队主机：接受者先成为 guest 成员（allowGuests、guest 额度）→ 授予（角色取高、范围并集）→ 置 accepted。
//
// 答（团队 id，主机 id）；个人空间主机的团队 id 为空。
func (s *Service) acceptHost(ctx context.Context, tx saas.Tx, call *api.Call, account *saas.Account, probe saas.Invitation, token string) (teamID, sourceID string, err error) {
	space, err := tx.SpaceOf(ctx, s.tenant(), probe.SourceID)
	if err != nil {
		return "", "", err
	}
	if space == nil {
		return "", "", core.Fail(gen.ErrNotFound)
	}
	if space.TeamID != "" {
		if found, err := tx.Teams().LockTeam(ctx, s.tenant(), space.TeamID); err != nil {
			return "", "", err
		} else if !found {
			return "", "", core.Fail(gen.ErrNotFound)
		}
	}
	if err := tx.Hosts().LockSource(ctx, s.tenant(), probe.SourceID); err != nil {
		return "", "", err
	}
	inv, err := tx.Invitations().Lock(ctx, s.tenant(), probe.InvitationID)
	if err != nil {
		return "", "", err
	}
	if inv == nil || !matchesToken(inv, token) || inv.Kind != saas.InvitationHost || inv.SourceID != probe.SourceID {
		return "", "", core.Fail(gen.ErrNotFound)
	}
	if inv.Email != account.Email {
		return "", "", core.Fail(gen.ErrInvitationEmailMismatch)
	}
	if inv.State == saas.InvitationAccepted {
		return inv.TeamID, inv.SourceID, nil // 已接受的再接受答原结果
	}
	nowMs := s.nowMs()
	switch {
	case inv.State == saas.InvitationRevoked:
		return "", "", core.Fail(gen.ErrInvitationRevoked)
	case inv.State == saas.InvitationExpired || (inv.State == saas.InvitationPending && inv.ExpiresAtMs <= nowMs):
		return "", "", core.Fail(gen.ErrInvitationExpired)
	case inv.State != saas.InvitationPending:
		return "", "", core.Fail(gen.ErrConflict)
	}
	// 授予的到期早于现在：接受了也得不到任何访问，按过期答。
	if inv.GrantExpiresAtMs != nil && *inv.GrantExpiresAtMs <= nowMs {
		return "", "", core.Fail(gen.ErrInvitationExpired)
	}
	if space.Revoked {
		return "", "", core.Fail(gen.ErrNotFound)
	}
	// 邀请人此刻仍要有管理权（被降级或移除时邀请本应一并作废，这里是兜底）。
	h, err := grants.FindHost(ctx, tx, s.tenant(), nowMs, inv.InvitedBy, inv.SourceID)
	if err != nil {
		if isNotFound(err) {
			return "", "", core.Fail(gen.ErrConflict)
		}
		return "", "", err
	}
	if !h.CanManage(inv.InvitedBy) || account.AccountID == h.OwnerAccountID {
		return "", "", core.Fail(gen.ErrConflict)
	}
	if !hosts.GuestsAllowed(h) {
		return "", "", core.Fail(gen.ErrGuestsDisabled)
	}
	subject := saas.GrantSubjectGuest
	if h.TeamID != "" {
		role, err := teams.EnsureGuestMember(ctx, tx, s.tenant(), nowMs, h.TeamID, account, inv.InvitedBy)
		if err != nil {
			return "", "", err
		}
		switch {
		case isStaffRole(role):
			subject = saas.GrantSubjectMember
		case role == saas.RoleGuest:
			// 团队 guest 只能被授予具体的工作空间；已有的 guest 在团队关了 guest 之后不再接受新的授予。
			if len(inv.WorkspaceIDs) == 0 {
				return "", "", core.Fail(gen.ErrConflict)
			}
			ta, err := tx.TeamAccess(ctx, s.tenant(), h.TeamID, account.AccountID)
			if err != nil {
				return "", "", err
			}
			if ta == nil || !teams.SettingsOf(ta.Team).AllowGuests {
				return "", "", core.FailWith(gen.ErrTeamPolicyDenied, map[string]any{"setting": "allowGuests"})
			}
		default:
			return "", "", core.Fail(gen.ErrConflict) // 账单管理员不访问主机
		}
	} else if account.EmailVerifiedAtMs == nil {
		return "", "", core.Fail(gen.ErrEmailUnverified) // 个人空间的访客授予要求已验证邮箱（与 grants.create 一致）
	}
	_, _, narrowed, err := grants.ApplyInvitation(ctx, tx, s.tenant(), nowMs, grants.InviteGrant{
		SourceID: h.SourceID, TeamID: h.TeamID, SubjectKind: subject, AccountID: account.AccountID, Role: inv.HostRole,
		WorkspaceIDs: inv.WorkspaceIDs, ExpiresAtMs: inv.GrantExpiresAtMs, GrantedBy: inv.InvitedBy, InvitationID: inv.InvitationID,
	})
	if err != nil {
		return "", "", err
	}
	if narrowed && s.relay != nil {
		// 合成结果比已有授予窄（角色降、范围缩或到期提前）：撤掉他在这台主机上已签的令牌，重连时按新权限重签。
		grants.RevokeAccess(tx, s.relay, s.log, h.SourceID, []string{account.AccountID}, "invitation_merged")
	}
	if err := tx.Invitations().Decide(ctx, s.tenant(), inv.InvitationID, saas.InvitationAccepted, account.AccountID, nowMs); err != nil {
		return "", "", err
	}
	if err := s.recordFor(ctx, tx, call, account.AccountID, h.TeamID, h.SourceID, ActionAccept, inv.InvitationID); err != nil {
		return "", "", err
	}
	return h.TeamID, h.SourceID, nil
}

func isNotFound(err error) bool {
	var coreErr *core.Error
	return errors.As(err, &coreErr) && coreErr.Code == gen.ErrNotFound
}
