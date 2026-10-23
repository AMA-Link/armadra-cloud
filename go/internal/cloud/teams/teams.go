// Package teams 是团队的过程（设计 §4.4）：teams.* 的 14 个过程、清除作业，以及 TeamScope 的唯一构造点
// Require / Lock。团队范围的仓储方法只收 TeamScope，TeamScope 只能在这里、核过成员与角色之后构造，
// 跨团队读取在编译期就写不出来（repo.rules.json 守住只有本包能 import store/saas/teamscope）。
package teams

import (
	"context"
	"encoding/json"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/events"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// RecoveryPeriod 是团队删除后的恢复期（设计 §10 Q7）。
const RecoveryPeriod = 30 * 24 * time.Hour

// Min 是处理器要求的最低角色。
type Min int

// 角色门槛。合同里的 scope 写 team:member 的过程，处理器再按能力设计 §2.1 收窄。
const (
	// MinAny：任一角色（含 guest、billing）。
	MinAny Min = iota
	// MinDirectory：能看团队目录——owner、admin、member、billing（guest 只看被授予的主机）。
	MinDirectory
	// MinStaff：owner、admin、member。
	MinStaff
	// MinAdmin：owner、admin。
	MinAdmin
	// MinOwner：仅 owner。
	MinOwner
)

func (m Min) allows(role saas.TeamRole) bool {
	switch m {
	case MinAny:
		return role.Valid()
	case MinDirectory:
		return role != teamscope.Guest && role.Valid()
	case MinStaff:
		return role == teamscope.Owner || role == teamscope.Admin || role == teamscope.Member
	case MinAdmin:
		return role == teamscope.Owner || role == teamscope.Admin
	case MinOwner:
		return role == teamscope.Owner
	}
	return false
}

// Access 是核过成员与角色之后的团队访问：范围、团队行、调用者的角色与团队的计划。
type Access struct {
	Scope saas.TeamScope
	Team  saas.Team
	Role  saas.TeamRole
	Plan  string
}

// RequireOptions 调整 Require。
type RequireOptions struct {
	// AllowDeleted 允许恢复期内的团队通过（teams.restore）。
	AllowDeleted bool
}

// Require 是 TeamScope 的唯一构造点：不是成员答 404 not_found（不泄露存在），角色不够 403
// team_role_insufficient，团队在恢复期内 410 team_deleted（teams.restore 例外）。
// 团队停用只挡签断言与登记主机，不在这里挡。
func Require(ctx context.Context, r saas.Reader, tenantID, accountID, teamID string, min Min, options ...RequireOptions) (Access, error) {
	access, err := r.TeamAccess(ctx, tenantID, teamID, accountID)
	if err != nil {
		return Access{}, err
	}
	if access == nil {
		return Access{}, core.Fail(gen.ErrNotFound)
	}
	if !min.allows(access.Role) {
		return Access{}, core.Fail(gen.ErrTeamRoleInsufficient)
	}
	allowDeleted := len(options) > 0 && options[0].AllowDeleted
	if access.Team.Deleted() && !allowDeleted {
		return Access{}, core.Fail(gen.ErrTeamDeleted)
	}
	return Access{
		Scope: teamscope.Issue(tenantID, teamID, access.Role), Team: access.Team, Role: access.Role, Plan: access.Plan,
	}, nil
}

// Lock 在事务里先锁团队行再 Require：核对在锁之后，被并发降级 / 移除的调用者看到的是最新的角色。
// 团队不存在答 404（与不是成员同码）。
func Lock(ctx context.Context, tx saas.Tx, tenantID, accountID, teamID string, min Min, options ...RequireOptions) (Access, error) {
	found, err := tx.Teams().LockTeam(ctx, tenantID, teamID)
	if err != nil {
		return Access{}, err
	}
	if !found {
		return Access{}, core.Fail(gen.ErrNotFound)
	}
	return Require(ctx, tx, tenantID, accountID, teamID, min, options...)
}

// Options 是团队服务的装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Relay 写团队并发额度、撤销已签令牌与踢人（经 tx.AfterCommit，提交后才发）、判断主机在线。
	Relay relayctl.Control
	// RequireEmailVerified 为真时未验证邮箱的账号不能建团队（设计 §4.3）。
	RequireEmailVerified bool
	Log                  *obs.Logger
	// Events 在提交之后发 memberChanged（成员加入、离开、改角色，团队删除）；nil = 不发。
	Events *events.Publisher
}

// Service 是团队服务。
type Service struct {
	c        *core.Context
	store    saas.Store
	relay    relayctl.Control
	verified bool
	log      *obs.Logger
	events   *events.Publisher
}

// New 建团队服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Service{c: options.Core, store: options.Store, relay: options.Relay, verified: options.RequireEmailVerified, log: log, events: options.Events}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"teams.create":            s.create,
		"teams.list":              s.list,
		"teams.get":               s.get,
		"teams.update":            s.update,
		"teams.delete":            s.delete,
		"teams.restore":           s.restore,
		"teams.transferOwnership": s.transferOwnership,
		"teams.members":           s.members,
		"teams.memberRole":        s.memberRole,
		"teams.memberRemove":      s.memberRemove,
		"teams.leave":             s.leave,
		"teams.settings":          s.settings,
		"teams.settingsUpdate":    s.settingsUpdate,
		"teams.usage":             s.usage,
	}
	for name, handler := range handlers {
		handlers[name] = wrap(handler)
	}
	return handlers
}

// wrap 把存储层按约束映射出的业务错误换成控制核的错误。
func wrap(handler api.Handler) api.Handler {
	return func(ctx context.Context, call *api.Call) (any, error) {
		out, err := handler(ctx, call)
		return out, httpx.MapError(err)
	}
}

// me 是调用者：账号本人（访客会话不是账号）且账号可用。
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

// teamID 取路径里的团队 id。
func teamID(call *api.Call) string {
	id, _ := call.Input["teamId"].(string)
	return id
}

// record 在业务事务里写一条团队审计。
func (s *Service) record(ctx context.Context, tx saas.Tx, call *api.Call, actor, teamID, action, targetKind, targetID string) error {
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), saas.AuditEntry{
		TeamID: teamID, ActorKind: saas.ActorAccount, ActorID: actor, Action: action, TargetKind: targetKind, TargetID: targetID,
		IPPrefix: audit.IPPrefix(call.IP),
	})
}

// 团队审计的动作名。
const (
	ActionCreate   = "team.create"
	ActionUpdate   = "team.update"
	ActionDelete   = "team.delete"
	ActionRestore  = "team.restore"
	ActionPurge    = "team.purge"
	ActionTransfer = "team.transfer"
	ActionSettings = "team.settings"
	ActionRole     = "member.role"
	ActionRemove   = "member.remove"
	ActionLeave    = "member.leave"
)

// teamOut 是团队的线上形状（协议包 teamSchema）。
type teamOut struct {
	TeamID      string  `json:"teamId"`
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	AvatarURL   *string `json:"avatarUrl"`
	Role        string  `json:"role"`
	Plan        string  `json:"plan"`
	CreatedAtMs int64   `json:"createdAtMs"`
	DeletedAtMs *int64  `json:"deletedAtMs"`
	PurgeAtMs   *int64  `json:"purgeAtMs"`
}

func teamView(t saas.Team, role saas.TeamRole, plan string) teamOut {
	return teamOut{
		TeamID: t.TeamID, Slug: t.Slug, Name: t.Name, AvatarURL: t.AvatarURL, Role: string(role), Plan: plan,
		CreatedAtMs: t.CreatedAtMs, DeletedAtMs: t.DeletedAtMs, PurgeAtMs: t.PurgeAtMs,
	}
}

// 事务提交之后才执行的外部副作用：失败只记日志（重试与全量重写由 S8 的 relayctl 兜底）。

// afterRevokeAccess 在提交后撤销账号在团队各主机上已签的令牌并踢掉在线连接（设计 §4.8）。
func (s *Service) afterRevokeAccess(tx saas.Tx, accountID string, sourceIDs []string, reason string) {
	tx.AfterCommit(func(ctx context.Context) {
		for _, sourceID := range sourceIDs {
			if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{Subject: accountID, SourceID: sourceID}); err != nil {
				s.log.Warn("撤销成员的已签令牌失败", obs.Operation("teams.revoke"), obs.SourceID(sourceID), obs.Error(err))
			}
			if err := s.relay.Kick(ctx, relayctl.KickFilter{SourceID: sourceID, Subject: accountID}, reason); err != nil {
				s.log.Warn("踢掉成员的连接失败", obs.Operation("teams.kick"), obs.SourceID(sourceID), obs.Error(err))
			}
		}
	})
}

// afterRevokeHosts 在提交后撤销这些主机上的全部已签令牌并踢掉全部连接（团队删除，隧道保留；清除时另关隧道）。
func (s *Service) afterRevokeHosts(tx saas.Tx, sourceIDs []string, reason string, closeTunnel bool) {
	tx.AfterCommit(func(ctx context.Context) {
		for _, sourceID := range sourceIDs {
			if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{SourceID: sourceID}); err != nil {
				s.log.Warn("撤销主机的已签令牌失败", obs.Operation("teams.revoke"), obs.SourceID(sourceID), obs.Error(err))
			}
			if err := s.relay.Kick(ctx, relayctl.KickFilter{SourceID: sourceID}, reason); err != nil {
				s.log.Warn("踢掉主机的连接失败", obs.Operation("teams.kick"), obs.SourceID(sourceID), obs.Error(err))
			}
			if closeTunnel {
				if err := s.relay.CloseTunnel(ctx, sourceID); err != nil {
					s.log.Warn("关闭主机的隧道失败", obs.Operation("teams.closeTunnel"), obs.SourceID(sourceID), obs.Error(err))
				}
			}
		}
	})
}

// afterSetQuota 在提交后把团队的并发额度写给节点（relay:quota:team:<id>，值 {concurrent}）。
func (s *Service) afterSetQuota(tx saas.Tx, teamID string, concurrent int) {
	tx.AfterCommit(func(ctx context.Context) {
		if err := s.relay.SetTeamQuota(ctx, teamID, relayctl.Quota{Concurrent: concurrent}); err != nil {
			s.log.Warn("写团队并发额度失败，节点按最严格的缺省", obs.Operation("teams.quota"), obs.Error(err))
		}
	})
}

// limits 是团队此刻的额度（计划 + overrides）。
func limits(ctx context.Context, r saas.Reader, tenantID string, access Access) (quota.Limits, error) {
	e, err := r.Entitlement(ctx, tenantID, saas.SpaceTeam, access.Scope.TeamID())
	if err != nil {
		return quota.Limits{}, err
	}
	if e == nil {
		return quota.ForTeam(saas.FreePlan, nil), nil
	}
	return quota.ForTeam(e.Plan, e.Overrides), nil
}

// DefaultSettings 是 teams.create 写入的缺省设置（设计 §4.4；allowAnonymousLinks 缺省关，能力设计 Q2）。
func DefaultSettings() Settings {
	return Settings{
		MembersCanRegisterHosts: true, DefaultHostRole: "viewer", AllowGuests: true, AllowAnonymousLinks: false,
		DeviceEndorsers: "owner_admin", MemberSelfEndorse: true, RequireMFA: false, AllowedEmailDomains: []string{},
	}
}

// Settings 是 teamSettings 的线上形状（协议包 teamSettingsSchema）。
type Settings struct {
	MembersCanRegisterHosts bool     `json:"membersCanRegisterHosts"`
	DefaultHostRole         string   `json:"defaultHostRole"`
	AllowGuests             bool     `json:"allowGuests"`
	AllowAnonymousLinks     bool     `json:"allowAnonymousLinks"`
	DeviceEndorsers         string   `json:"deviceEndorsers"`
	MemberSelfEndorse       bool     `json:"memberSelfEndorse"`
	RequireMFA              bool     `json:"requireMfa"`
	SessionMaxAgeMs         *int64   `json:"sessionMaxAgeMs"`
	AllowedEmailDomains     []string `json:"allowedEmailDomains"`
}

// SettingsOf 解出团队行里的设置；库里的值坏了（不该发生）当缺省，只会更严。
func SettingsOf(team saas.Team) Settings {
	settings := DefaultSettings()
	if err := json.Unmarshal(team.Settings, &settings); err != nil {
		return DefaultSettings()
	}
	if settings.AllowedEmailDomains == nil {
		settings.AllowedEmailDomains = []string{}
	}
	return settings
}
