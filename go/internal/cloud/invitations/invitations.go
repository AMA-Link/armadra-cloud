// Package invitations 是团队邀请的过程（设计 §4.4）：create / list / revoke / resend / get / accept / decline。
// 邮件里的链接是 <APP_ORIGIN>/invite/<invitationId>#<令牌>：令牌只在片段里、库里只存哈希，由发信作业生成
// （见 cloud/mail）。主机邀请（kind = host）随 S6，这里答 501 not_implemented。
package invitations

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// TTL 是邀请的有效期（创建与重发都从当时起算）。
const TTL = 7 * 24 * time.Hour

// JobExpire 是每小时把到期的 pending 邀请落库成 expired 的作业。
const JobExpire = "invitations.expire"

// 邀请的审计动作。
const (
	ActionCreate  = "invitation.create"
	ActionRevoke  = "invitation.revoke"
	ActionResend  = "invitation.resend"
	ActionAccept  = "invitation.accept"
	ActionDecline = "invitation.decline"
)

// Options 是邀请服务的装配。
type Options struct {
	Core   *core.Context
	Store  saas.Store
	Limits *httpx.Limits
	Mail   *mail.Service
	// RequireEmailVerified 为真时未验证邮箱的账号不能邀请或接受（设计 §4.3）。
	RequireEmailVerified bool
	Log                  *obs.Logger
}

// Service 是邀请服务。
type Service struct {
	c        *core.Context
	store    saas.Store
	limits   *httpx.Limits
	mail     *mail.Service
	verified bool
	log      *obs.Logger
}

// New 建邀请服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Service{
		c: options.Core, store: options.Store, limits: options.Limits, mail: options.Mail,
		verified: options.RequireEmailVerified, log: log,
	}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"invitations.create":  s.create,
		"invitations.list":    s.list,
		"invitations.revoke":  s.revoke,
		"invitations.resend":  s.resend,
		"invitations.get":     s.get,
		"invitations.accept":  s.accept,
		"invitations.decline": s.decline,
	}
	for name, handler := range handlers {
		handlers[name] = wrap(handler)
	}
	return handlers
}

func wrap(handler api.Handler) api.Handler {
	return func(ctx context.Context, call *api.Call) (any, error) {
		out, err := handler(ctx, call)
		return out, httpx.MapError(err)
	}
}

// Register 把到期作业登记进运行器。
func (s *Service) Register(runner *jobs.Runner) {
	runner.Handle(JobExpire, s.expire)
	runner.Every(JobExpire, jobs.Hourly)
}

// expire 是 invitations.expire：把到期的 pending 落库成 expired（读时本来就按过期答，这里让库里的状态与列表一致）。
func (s *Service) expire(ctx context.Context, _ saas.Job) error {
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		expired, err := tx.Invitations().ExpireDue(ctx, s.tenant(), s.nowMs())
		if err == nil && len(expired) > 0 {
			s.log.Info("已把到期的邀请落库为过期", obs.Operation(JobExpire), obs.Count(len(expired)))
		}
		return err
	})
}

// me 是调用者：账号本人且账号可用。
func (s *Service) me(ctx context.Context, call *api.Call) (*saas.Account, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := core.RequireOwner(caller); err != nil {
		return nil, err
	}
	account, err := s.store.Read().Account(ctx, s.tenant(), caller.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Status != saas.AccountActive {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	return account, nil
}

// verifiedMe 在 me 之上要求邮箱已验证（REQUIRE_EMAIL_VERIFIED）。
func (s *Service) verifiedMe(ctx context.Context, call *api.Call) (*saas.Account, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if s.verified && account.EmailVerifiedAtMs == nil {
		return nil, core.Fail(gen.ErrEmailUnverified)
	}
	return account, nil
}

func (s *Service) record(ctx context.Context, tx saas.Tx, call *api.Call, actor, teamID, action, invitationID string) error {
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), saas.AuditEntry{
		TeamID: teamID, ActorKind: saas.ActorAccount, ActorID: actor, Action: action, TargetKind: "invitation", TargetID: invitationID,
		IPPrefix: audit.IPPrefix(call.IP),
	})
}

// state 是对外报的状态：待处理但已过期的按 expired 答（读时判，落库随 invitations.expire）。
func state(inv saas.Invitation, nowMs int64) string {
	if inv.State == saas.InvitationPending && inv.ExpiresAtMs <= nowMs {
		return saas.InvitationExpired
	}
	return inv.State
}

// invitationOut 是邀请的线上形状（协议包 invitationSchema，团队邀请）。
type invitationOut struct {
	InvitationID string `json:"invitationId"`
	Kind         string `json:"kind"`
	Email        string `json:"email"`
	State        string `json:"state"`
	TeamID       string `json:"teamId,omitempty"`
	TeamRole     string `json:"teamRole,omitempty"`
	InvitedBy    string `json:"invitedBy"`
	CreatedAtMs  int64  `json:"createdAtMs"`
	ExpiresAtMs  int64  `json:"expiresAtMs"`
	DecidedAtMs  *int64 `json:"decidedAtMs"`
}

func view(inv saas.Invitation, nowMs int64) invitationOut {
	return invitationOut{
		InvitationID: inv.InvitationID, Kind: inv.Kind, Email: inv.Email, State: state(inv, nowMs), TeamID: inv.TeamID,
		TeamRole: string(inv.TeamRole), InvitedBy: inv.InvitedBy, CreatedAtMs: inv.CreatedAtMs, ExpiresAtMs: inv.ExpiresAtMs,
		DecidedAtMs: inv.DecidedAtMs,
	}
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// matchesToken 常量时间比较令牌哈希；库里没有哈希（邮件还没发出）一律不符。
func matchesToken(inv *saas.Invitation, token string) bool {
	given := []byte(mail.HashToken(token))
	stored := []byte("-")
	if inv != nil && inv.TokenHash != "" {
		stored = []byte(inv.TokenHash)
	}
	// 不管邀请存不存在都比较一次，答案与耗时不泄露邀请 id 有没有对上。
	return subtle.ConstantTimeCompare(given, stored) == 1 && inv != nil
}

// create 是 invitations.create（团队邀请）：团队 admin；guest 受 allowGuests 约束；已是成员 409 member_exists；
// 同邮箱有待处理的 409 conflict；成员数或 guest 数（待处理邀请也计入）受额度约束；按邀请人每小时 50 次。
// 事务里先把同 (团队, 邮箱) 下已过期的 pending 改成 expired，否则它们会占着部分唯一索引挡住重新邀请。
func (s *Service) create(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Kind   string        `json:"kind"`
		TeamID string        `json:"teamId"`
		Email  string        `json:"email"`
		Role   saas.TeamRole `json:"role"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if in.Kind != saas.InvitationTeam {
		return nil, core.Fail(gen.ErrNotImplemented) // 主机邀请随 S6
	}
	account, err := s.verifiedMe(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitInviteAcct, account.AccountID); err != nil {
		return nil, err
	}
	email := normalizeEmail(in.Email)
	var out invitationOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := teams.Lock(ctx, tx, s.tenant(), account.AccountID, in.TeamID, teams.MinAdmin)
		if err != nil {
			return err
		}
		if in.Role == saas.TeamRole("guest") && !teams.SettingsOf(access.Team).AllowGuests {
			return core.Fail(gen.ErrGuestsDisabled)
		}
		nowMs := s.nowMs()
		invitee, err := tx.AccountByEmail(ctx, s.tenant(), email)
		if err != nil {
			return err
		}
		if invitee != nil {
			if existing, err := tx.TeamAccess(ctx, s.tenant(), in.TeamID, invitee.AccountID); err != nil {
				return err
			} else if existing != nil {
				return core.Fail(gen.ErrMemberExists)
			}
		}
		if err := tx.Invitations().ExpireStale(ctx, access.Scope, email, nowMs); err != nil {
			return err
		}
		if pending, err := tx.Invitations().HasPending(ctx, access.Scope, email, nowMs); err != nil {
			return err
		} else if pending {
			return core.Fail(gen.ErrConflict)
		}
		if err := s.checkSeat(ctx, tx, access, in.Role, nowMs); err != nil {
			return err
		}
		inv := saas.Invitation{
			InvitationID: core.NewUUID(), Kind: saas.InvitationTeam, Email: email, TeamID: in.TeamID, TeamRole: in.Role,
			InvitedBy: account.AccountID, CreatedAtMs: nowMs, ExpiresAtMs: nowMs + TTL.Milliseconds(), State: saas.InvitationPending,
		}
		if err := tx.Invitations().Insert(ctx, access.Scope, inv); err != nil {
			return err
		}
		if err := s.enqueueMail(ctx, tx, access.Team, inv, account, invitee); err != nil {
			return err
		}
		out = view(inv, nowMs)
		return s.record(ctx, tx, call, account.AccountID, in.TeamID, ActionCreate, inv.InvitationID)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkSeat 核对再加一个待处理的邀请是否越过额度：成员数 / guest 数加上待处理的邀请。
func (s *Service) checkSeat(ctx context.Context, tx saas.Tx, access teams.Access, role saas.TeamRole, nowMs int64) error {
	seats, err := tx.Teams().Seats(ctx, access.Scope)
	if err != nil {
		return err
	}
	pending, err := tx.Invitations().PendingSeats(ctx, access.Scope, nowMs)
	if err != nil {
		return err
	}
	e, err := tx.Entitlement(ctx, s.tenant(), saas.SpaceTeam, access.Scope.TeamID())
	if err != nil {
		return err
	}
	lim := quota.ForTeam(saas.FreePlan, nil)
	if e != nil {
		lim = quota.ForTeam(e.Plan, e.Overrides)
	}
	if role == saas.TeamRole("guest") {
		return quota.Check(quota.Guests, seats.Guests+pending.Guests, lim.Guests)
	}
	return quota.Check(quota.Members, seats.Members+pending.Members, lim.Members)
}

// enqueueMail 在事务里插邀请邮件：语言取收件账号的 locale，没有账号的收件人取邀请人的。
func (s *Service) enqueueMail(ctx context.Context, tx saas.Tx, team saas.Team, inv saas.Invitation, inviter, invitee *saas.Account) error {
	locale := inviter.Locale
	if invitee != nil {
		locale = invitee.Locale
	}
	return s.mail.Enqueue(ctx, tx, mail.Mail{
		Template: mail.TeamInvitation, Locale: locale, To: inv.Email, InvitationID: inv.InvitationID,
		Params: mail.Params{TeamName: team.Name, InviterName: inviter.DisplayName, Role: string(inv.TeamRole)},
	})
}

// list 是 invitations.list：box = sent（缺省）是自己能管的团队邀请，可按 teamId 过滤；box = received 是发给
// 自己邮箱的待处理邀请。主机邀请的筛选（sourceId）随 S6。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if _, has := call.Input["sourceId"]; has {
		return nil, core.Fail(gen.ErrNotImplemented)
	}
	nowMs := s.nowMs()
	box, _ := call.Input["box"].(string)
	out := []invitationOut{}
	if box == "received" {
		received, err := s.store.Read().ReceivedInvitations(ctx, s.tenant(), account.Email, nowMs)
		if err != nil {
			return nil, err
		}
		for _, inv := range received {
			out = append(out, view(inv, nowMs))
		}
		return map[string]any{"invitations": out}, nil
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		var teamIDs []string
		if id, ok := call.Input["teamId"].(string); ok {
			teamIDs = []string{id}
		} else {
			accesses, err := tx.TeamsOf(ctx, s.tenant(), account.AccountID)
			if err != nil {
				return err
			}
			for _, a := range accesses {
				if !a.Team.Deleted() && (a.Role == "owner" || a.Role == "admin") {
					teamIDs = append(teamIDs, a.Team.TeamID)
				}
			}
		}
		for _, id := range teamIDs {
			access, err := teams.Require(ctx, tx, s.tenant(), account.AccountID, id, teams.MinAdmin)
			if err != nil {
				return err
			}
			list, err := tx.Invitations().List(ctx, access.Scope)
			if err != nil {
				return err
			}
			for _, inv := range list {
				out = append(out, view(inv, nowMs))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"invitations": out}, nil
}

// lockForManage 找邀请、锁它所属的团队行、核调用者是 admin、再锁邀请行；返回团队访问与邀请。
// 邀请不存在、不是团队邀请答 404；不是成员也答 404，角色不够 403。
func (s *Service) lockForManage(ctx context.Context, tx saas.Tx, accountID, invitationID string) (teams.Access, *saas.Invitation, error) {
	probe, err := tx.Invitation(ctx, s.tenant(), invitationID)
	if err != nil {
		return teams.Access{}, nil, err
	}
	if probe == nil || probe.Kind != saas.InvitationTeam {
		return teams.Access{}, nil, core.Fail(gen.ErrNotFound)
	}
	access, err := teams.Lock(ctx, tx, s.tenant(), accountID, probe.TeamID, teams.MinAdmin)
	if err != nil {
		return teams.Access{}, nil, err
	}
	inv, err := tx.Invitations().Lock(ctx, s.tenant(), invitationID)
	if err != nil {
		return teams.Access{}, nil, err
	}
	if inv == nil || inv.TeamID != probe.TeamID {
		return teams.Access{}, nil, core.Fail(gen.ErrNotFound)
	}
	return access, inv, nil
}

// revoke 是 invitations.revoke（团队 admin）：待处理（含已过期还没落库的）改 revoked；已撤销的再撤销是空操作；
// 已接受或已拒绝的答 409 conflict。排队还没发出的邮件在发信时发现邀请不是待处理，自己丢弃。
func (s *Service) revoke(ctx context.Context, call *api.Call) (any, error) {
	id, _ := call.Input["invitationId"].(string)
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, inv, err := s.lockForManage(ctx, tx, account.AccountID, id)
		if err != nil {
			return err
		}
		switch inv.State {
		case saas.InvitationRevoked:
			return nil
		case saas.InvitationAccepted, saas.InvitationDeclined:
			return core.Fail(gen.ErrConflict)
		}
		if err := tx.Invitations().Decide(ctx, s.tenant(), id, saas.InvitationRevoked, account.AccountID, s.nowMs()); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionRevoke, id)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// resend 是 invitations.resend（团队 admin）：换新令牌（发信作业里生成）、重新计 7 天、旧链接作废；
// 已撤销答 410 invitation_revoked，已接受或已拒绝答 409 conflict；已过期的可以重发（回到待处理，要再核额度）；
// 按邀请每小时 5 次。
func (s *Service) resend(ctx context.Context, call *api.Call) (any, error) {
	id, _ := call.Input["invitationId"].(string)
	account, err := s.verifiedMe(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitInviteResend, id); err != nil {
		return nil, err
	}
	var out invitationOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, inv, err := s.lockForManage(ctx, tx, account.AccountID, id)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		switch inv.State {
		case saas.InvitationRevoked:
			return core.Fail(gen.ErrInvitationRevoked)
		case saas.InvitationAccepted, saas.InvitationDeclined:
			return core.Fail(gen.ErrConflict)
		}
		if state(*inv, nowMs) == saas.InvitationExpired {
			// 回到待处理会重新占一个席位，也可能已经有人被邀请进来了。
			if invitee, err := tx.AccountByEmail(ctx, s.tenant(), inv.Email); err != nil {
				return err
			} else if invitee != nil {
				if existing, err := tx.TeamAccess(ctx, s.tenant(), inv.TeamID, invitee.AccountID); err != nil {
					return err
				} else if existing != nil {
					return core.Fail(gen.ErrMemberExists)
				}
			}
			if err := s.checkSeat(ctx, tx, access, inv.TeamRole, nowMs); err != nil {
				return err
			}
		}
		if err := tx.Invitations().Reissue(ctx, s.tenant(), id, nowMs+TTL.Milliseconds()); err != nil {
			return err
		}
		invitee, err := tx.AccountByEmail(ctx, s.tenant(), inv.Email)
		if err != nil {
			return err
		}
		if err := s.enqueueMail(ctx, tx, access.Team, *inv, account, invitee); err != nil {
			return err
		}
		inv.State, inv.ExpiresAtMs, inv.DecidedAtMs = saas.InvitationPending, nowMs+TTL.Milliseconds(), nil
		out = view(*inv, nowMs)
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionResend, id)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// maskEmail 遮掩邮箱：留本地部分第一个字符和域名（a***@example.com）。
func maskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "***"
	}
	return string([]rune(local)[:1]) + "***@" + domain
}

// preview 是 invitations.get 的摘要（协议包 invitationPreviewSchema）。
type preview struct {
	Kind        string `json:"kind"`
	State       string `json:"state"`
	TeamName    string `json:"teamName,omitempty"`
	TeamRole    string `json:"teamRole,omitempty"`
	InviterName string `json:"inviterName"`
	EmailHint   string `json:"emailHint"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

// get 是 invitations.get（匿名）：按 IP 每分钟 30 次；邀请 id 与令牌哈希必须都对上，否则一律 404 not_found
// （不区分「没有这条邀请」与「令牌不对」）；已撤销 410、已过期 410；其余只答遮掩后的摘要。
func (s *Service) get(ctx context.Context, call *api.Call) (any, error) {
	id, _ := call.Input["invitationId"].(string)
	token, _ := call.Input["token"].(string)
	if err := s.limits.Check(ctx, httpx.LimitTokenIP, call.IP); err != nil {
		return nil, err
	}
	reader := s.store.Read()
	inv, err := reader.Invitation(ctx, s.tenant(), id)
	if err != nil {
		return nil, err
	}
	if !matchesToken(inv, token) || inv.Kind != saas.InvitationTeam {
		return nil, core.Fail(gen.ErrNotFound)
	}
	nowMs := s.nowMs()
	switch state(*inv, nowMs) {
	case saas.InvitationRevoked:
		return nil, core.Fail(gen.ErrInvitationRevoked)
	case saas.InvitationExpired:
		return nil, core.Fail(gen.ErrInvitationExpired)
	}
	out := preview{
		Kind: inv.Kind, State: state(*inv, nowMs), TeamRole: string(inv.TeamRole), EmailHint: maskEmail(inv.Email), ExpiresAtMs: inv.ExpiresAtMs,
	}
	team, err := reader.TeamSummary(ctx, s.tenant(), inv.TeamID)
	if err != nil {
		return nil, err
	}
	if team == nil || team.Deleted {
		return nil, core.Fail(gen.ErrNotFound) // 团队已删除或清除
	}
	out.TeamName = mail.SanitizeLabel(team.Name, 128)
	if inviter, err := reader.Account(ctx, s.tenant(), inv.InvitedBy); err != nil {
		return nil, err
	} else if inviter != nil {
		out.InviterName = mail.SanitizeLabel(inviter.DisplayName, 64)
	}
	return out, nil
}

// accept 是 invitations.accept（要会话、邮箱已验证）：邀请 id 与令牌都对上 → 先锁团队行再锁邀请行 → 账号邮箱
// 必须等于邀请的邮箱（403 invitation_email_mismatch）→ 已接受的原样答；已撤销 / 过期 410 → teams.Join 核对
// 团队状态、重复成员、guest 设置与额度并插成员。
func (s *Service) accept(ctx context.Context, call *api.Call) (any, error) {
	id, _ := call.Input["invitationId"].(string)
	token, _ := call.Input["token"].(string)
	account, err := s.verifiedMe(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTokenIP, call.IP); err != nil {
		return nil, err
	}
	var teamID string
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		probe, err := tx.Invitation(ctx, s.tenant(), id)
		if err != nil {
			return err
		}
		if !matchesToken(probe, token) || probe.Kind != saas.InvitationTeam {
			return core.Fail(gen.ErrNotFound)
		}
		// 加锁顺序：团队行 → 邀请行。
		if found, err := tx.Teams().LockTeam(ctx, s.tenant(), probe.TeamID); err != nil {
			return err
		} else if !found {
			return core.Fail(gen.ErrNotFound)
		}
		inv, err := tx.Invitations().Lock(ctx, s.tenant(), id)
		if err != nil {
			return err
		}
		if inv == nil || !matchesToken(inv, token) {
			return core.Fail(gen.ErrNotFound)
		}
		if inv.Email != account.Email {
			return core.Fail(gen.ErrInvitationEmailMismatch)
		}
		nowMs := s.nowMs()
		teamID = inv.TeamID
		switch state(*inv, nowMs) {
		case saas.InvitationAccepted:
			return nil // 已接受的再接受答原结果
		case saas.InvitationRevoked:
			return core.Fail(gen.ErrInvitationRevoked)
		case saas.InvitationExpired:
			return core.Fail(gen.ErrInvitationExpired)
		case saas.InvitationDeclined:
			return core.Fail(gen.ErrConflict)
		}
		if err := teams.Join(ctx, tx, s.tenant(), nowMs, *inv, account.AccountID); err != nil {
			return err
		}
		if err := tx.Invitations().Decide(ctx, s.tenant(), id, saas.InvitationAccepted, account.AccountID, nowMs); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, inv.TeamID, ActionAccept, id)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"teamId": teamID}, nil
}

// decline 是 invitations.decline（要会话）：令牌与邮箱都对上才算；待处理改 declined，已拒绝的再拒绝是空操作；
// 已撤销 410、已过期 410；已接受的答 409 conflict。
func (s *Service) decline(ctx context.Context, call *api.Call) (any, error) {
	id, _ := call.Input["invitationId"].(string)
	token, _ := call.Input["token"].(string)
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTokenIP, call.IP); err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		inv, err := tx.Invitations().Lock(ctx, s.tenant(), id)
		if err != nil {
			return err
		}
		if !matchesToken(inv, token) || inv.Kind != saas.InvitationTeam {
			return core.Fail(gen.ErrNotFound)
		}
		if inv.Email != account.Email {
			return core.Fail(gen.ErrInvitationEmailMismatch)
		}
		nowMs := s.nowMs()
		switch state(*inv, nowMs) {
		case saas.InvitationDeclined:
			return nil
		case saas.InvitationRevoked:
			return core.Fail(gen.ErrInvitationRevoked)
		case saas.InvitationExpired:
			return core.Fail(gen.ErrInvitationExpired)
		case saas.InvitationAccepted:
			return core.Fail(gen.ErrConflict)
		}
		if err := tx.Invitations().Decide(ctx, s.tenant(), id, saas.InvitationDeclined, account.AccountID, nowMs); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, inv.TeamID, ActionDecline, id)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
