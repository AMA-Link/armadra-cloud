// Package links 是分享链接在 SaaS 的一层（设计 §4.7、能力设计 §11.3）：包住 core.Links，加管理权与主机设置
// （谁能签链接、guest 开关）、团队设置（匿名链接开关）、space 过滤，以及签发索引（链接访客的断言与中继令牌同样
// 「签 → 登记索引 → 复核」）。链接仍落成 core 的邀请：core 的条件加一与 max_uses 语义不变（10 个同时接受、
// max_uses = 5，恰好 5 个成功），撤销链接经撤销钩子撤掉并踢掉 guest:<linkId>: 前缀的访客。
package links

import (
	"context"
	"slices"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 链接的审计动作。
const (
	ActionCreate = "link.create"
	ActionRevoke = "link.revoke"
)

// maxManagedHosts 与 maxListedLinks 限定不带 sourceId 列链接时一次看多少。
const (
	maxManagedHosts = 2000
	maxListedLinks  = 2000
)

// Options 是链接服务的装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Relay 登记签发索引、撤销已签令牌。
	Relay relayctl.Control
	Log   *obs.Logger
	// Hook 给测试在链接访客签发的两个交错点停住（"signed"：令牌已签、索引未登记；"indexed"：索引已登记、复核之前）。生产不设。
	Hook func(stage string)
}

// Service 是链接服务。
type Service struct {
	c       *core.Context
	store   saas.Store
	relay   relayctl.Control
	sources *core.Sources
	links   *core.Links
	log     *obs.Logger
	hook    func(stage string)
}

// New 建链接服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	s := &Service{c: options.Core, store: options.Store, relay: options.Relay, sources: core.NewSources(options.Core), log: log, hook: options.Hook}
	s.links = core.NewLinks(options.Core, s.sources).WithHooks(core.LinkHooks{Admit: s.admit, Issued: s.issued})
	return s
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"links.create": s.create,
		"links.get":    s.get,
		"links.accept": s.accept,
		"links.list":   s.list,
		"links.update": s.update,
		"links.revoke": s.revoke,
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

// me 是调用者：账号本人（链接访客不是账号）且账号可用。
func (s *Service) me(ctx context.Context, call *api.Call) (core.Caller, *saas.Account, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return core.Caller{}, nil, err
	}
	if err := core.RequireOwner(caller); err != nil {
		return core.Caller{}, nil, err
	}
	account, err := s.store.Read().Account(ctx, s.tenant(), caller.AccountID)
	if err != nil {
		return core.Caller{}, nil, err
	}
	if account == nil || account.Status != saas.AccountActive {
		return core.Caller{}, nil, core.Fail(gen.ErrUnauthenticated)
	}
	return caller, account, nil
}

// bound 返回绑定到 SaaS 事务的控制核：端口换成 tx.Ports()，撤销钩子经 tx.AfterCommit 投递。
func (s *Service) bound(tx saas.Tx) *core.Context {
	var sink core.RevocationSink
	if s.c.Revocations != nil {
		sink = relayctl.Deferred(tx, s.c.Revocations)
	}
	return s.c.Bound(tx.Ports(), sink)
}

func (s *Service) record(ctx context.Context, tx saas.Tx, ip, actor, teamID, sourceID, action, linkID string) error {
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), saas.AuditEntry{
		TeamID: teamID, ActorKind: saas.ActorAccount, ActorID: actor, Action: action, TargetKind: "link", TargetID: linkID,
		SourceID: sourceID, IPPrefix: audit.IPPrefix(ip),
	})
}

// ---- links.create ----

type createInput struct {
	Kind         string           `json:"kind"`
	SourceID     string           `json:"sourceId"`
	InvitationID string           `json:"invitationId"`
	Label        string           `json:"label"`
	Role         string           `json:"role"`
	ExpiresAtMs  int64            `json:"expiresAtMs"`
	MaxUses      *int64           `json:"maxUses"`
	Scope        *store.LinkScope `json:"scope"`
}

// create 是 links.create：只有 source_invite（team_join 是 M2，答 bad_request）；
//
//	主机管理权与「谁能签链接」（主机设置 linkCreators：owner 仅主机所有者；owner_admin 缺省，加团队 owner / admin；
//	driver 再加有效角色为 driver 的团队成员——限在工作空间的 driver 只能签那些工作空间的链接）→ 主机与团队的 allowGuests
//	（guests_disabled）→ core 的建链接（期限、范围、角色）→ 审计。全在一个事务里，团队主机按「团队 → 主机」加锁。
func (s *Service) create(ctx context.Context, call *api.Call) (any, error) {
	var in createInput
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	_, account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if in.Kind != "source_invite" || !grants.ValidRole(in.Role) {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	var out core.LinkCreated
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		nowMs := s.nowMs()
		first, err := grants.FindHost(ctx, tx, s.tenant(), nowMs, account.AccountID, in.SourceID)
		if err != nil {
			return err
		}
		var team saas.Team
		if first.TeamID != "" {
			// 团队 guest 与账单管理员不签链接（team_role_insufficient）。
			access, err := teams.Lock(ctx, tx, s.tenant(), account.AccountID, first.TeamID, teams.MinStaff)
			if err != nil {
				return err
			}
			team = access.Team
		}
		if err := tx.Hosts().LockSource(ctx, s.tenant(), in.SourceID); err != nil {
			return err
		}
		h, err := grants.FindHost(ctx, tx, s.tenant(), nowMs, account.AccountID, in.SourceID)
		if err != nil {
			return err
		}
		if err := s.mayCreate(ctx, tx, h, account.AccountID, in.Scope, nowMs); err != nil {
			return err
		}
		if !hosts.GuestsAllowed(h) || (h.TeamID != "" && !teams.SettingsOf(team).AllowGuests) {
			return core.Fail(gen.ErrGuestsDisabled)
		}
		bound := s.bound(tx)
		created, err := core.NewLinks(bound, core.NewSources(bound)).CreateAuthorized(ctx, core.CreateLinkInput{
			SourceID: in.SourceID, InvitationID: in.InvitationID, Label: in.Label, Role: in.Role, ExpiresAtMs: in.ExpiresAtMs,
			MaxUses: in.MaxUses, Scope: in.Scope,
		})
		if err != nil {
			return err
		}
		out = created
		return s.record(ctx, tx, call.IP, account.AccountID, h.TeamID, h.SourceID, ActionCreate, created.LinkID)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// mayCreate 判定 accountID 能不能给这台主机签链接（主机设置 linkCreators）。
func (s *Service) mayCreate(ctx context.Context, tx saas.Tx, h saas.Host, accountID string, scope *store.LinkScope, nowMs int64) error {
	manager := h.CanManage(accountID)
	mode := hosts.LinkCreatorsOf(h)
	switch {
	case mode == "owner" && manager && h.OwnerAccountID == accountID, mode != "owner" && manager:
		return nil
	case manager:
		// 看得到也管得了，但主机设置只许主机所有者签。
		return core.FailWith(gen.ErrTeamPolicyDenied, map[string]any{"setting": "linkCreators"})
	case mode != "driver":
		return grants.DenyManage(h)
	}
	// driver 档：有效角色为 driver 的人也能签；限在工作空间的 driver 只能签落在那些工作空间里的链接。
	basis, err := tx.AccessBasis(ctx, s.tenant(), accountID, h.SourceID)
	if err != nil {
		return err
	}
	eff := grants.Compose(basis, accountID, nowMs)
	if eff.Role != "driver" {
		return grants.DenyManage(h)
	}
	if !eff.Whole && (scope == nil || scope.WorkspaceID == nil || !slices.Contains(eff.WorkspaceIDs, *scope.WorkspaceID)) {
		return grants.DenyManage(h)
	}
	return nil
}

// ---- links.get ----

// linkInfo 在 core 的落地页信息之上，把 requiresAccount 按团队设置补上（团队主机且 allowAnonymousLinks 关 = 要登录）。
func (s *Service) get(ctx context.Context, call *api.Call) (any, error) {
	linkID, _ := call.Input["linkId"].(string)
	info, err := s.links.Get(ctx, linkID)
	if err != nil {
		return nil, err
	}
	link, err := s.c.Ports.Links.Get(ctx, s.tenant(), linkID)
	if err != nil || link == nil {
		return nil, err
	}
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), link.SourceID)
	if err != nil {
		return nil, err
	}
	if space == nil || space.Revoked || space.TeamDeleted {
		return nil, core.Fail(gen.ErrLinkInvalid)
	}
	if space.TeamID != "" {
		info.RequiresAccount = !teamSettings(space).AllowAnonymousLinks
	}
	return info, nil
}

func teamSettings(space *saas.HostSpace) teams.Settings {
	return teams.SettingsOf(saas.Team{Settings: space.TeamSettings})
}

// ---- links.accept ----

// accept 是 links.accept：带会话的账号与匿名访客走同一条核心路径（按 IP 限流 → 人机挑战 → 秘密 → Admit → 条件加一 → 建访客设备 →
// 签断言与中继令牌 → Issued 登记签发索引并复核）。凭据缺失或不可用一律当匿名处理（只会让团队的匿名链接开关更严）。
func (s *Service) accept(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		LinkID    string               `json:"linkId"`
		Secret    string               `json:"secret"`
		Device    *core.DeviceInput    `json:"device"`
		Challenge *core.ChallengeToken `json:"challenge"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	accountID := ""
	if caller, err := call.Caller(ctx); err == nil && caller.AccountID != "" && caller.LinkID == "" {
		if account, err := s.store.Read().Account(ctx, s.tenant(), caller.AccountID); err == nil && account != nil && account.Status == saas.AccountActive {
			accountID = account.AccountID
		}
	}
	return s.links.Accept(ctx, core.AcceptInput{
		LinkID: in.LinkID, Secret: in.Secret, Device: in.Device, Challenge: in.Challenge, AccountID: accountID,
	}, call.IP)
}

// admit 是接受链接的 SaaS 约束（在消耗次数之前）：主机与团队仍有效（团队在恢复期内 link_invalid、停用 team_suspended）；
// 团队与主机的 allowGuests 关了当链接失效（与 core 的租户级 guest 开关一致）；团队主机且 allowAnonymousLinks 关时
// 必须带会话（403 anonymous_links_disabled）。
func (s *Service) admit(ctx context.Context, link store.LinkRecord, _ store.SourceRecord, accountID string) error {
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), link.SourceID)
	if err != nil {
		return err
	}
	if space == nil || space.Revoked {
		return core.Fail(gen.ErrLinkInvalid)
	}
	if err := guestHostState(space); err != nil {
		return err
	}
	if !hosts.GuestsAllowed(saas.Host{Settings: space.Settings}) {
		return core.Fail(gen.ErrLinkInvalid)
	}
	if space.TeamID != "" {
		settings := teamSettings(space)
		if !settings.AllowGuests {
			return core.Fail(gen.ErrLinkInvalid)
		}
		if !settings.AllowAnonymousLinks && accountID == "" {
			return core.Fail(gen.ErrAnonymousLinksDisabled)
		}
	}
	return nil
}

// GuestHostState 报告团队主机此刻能不能让链接访客进来：团队在恢复期内不能（link_invalid），停用答 team_suspended。
// 个人空间没有团队状态。
func guestHostState(space *saas.HostSpace) error {
	if space.TeamID == "" {
		return nil
	}
	if space.TeamDeleted {
		return core.Fail(gen.ErrLinkInvalid)
	}
	if space.TeamStatus == saas.TeamSuspended {
		return core.Fail(gen.ErrTeamSuspended)
	}
	return nil
}

// issued 是「签 → 登记签发索引 → 复核」的后两步：把断言与中继令牌的 jti 记进签发索引（先 PostgreSQL 后内层），再读库复核链接、
// 主机、团队、访客设备仍有效；任何一项不再有效就撤掉自己签的这两枚并答错误（调用方随后撤销刚建的访客设备）。
// 撤销链接在库提交之后才撤索引里的令牌，所以复核看到链接未撤销，之后的撤销一定撤得到；与并发的撤销交错也不漏。
func (s *Service) issued(ctx context.Context, link store.LinkRecord, source store.SourceRecord, device store.DeviceRecord, grant core.Grant) error {
	entries := []relayctl.Issued{
		{Subject: device.Subject, SourceID: source.SourceID, DeviceID: device.DeviceID, JTI: grant.AssertionJTI, ExpMs: grant.AssertionExpiresAtMs},
		{Subject: device.Subject, SourceID: source.SourceID, DeviceID: device.DeviceID, JTI: grant.RelayTokenJTI, ExpMs: grant.RelayTokenExpiresAtMs},
	}
	s.stage("signed")
	for _, entry := range entries {
		if err := s.relay.IndexIssued(ctx, entry); err != nil {
			s.undo(ctx, entries)
			return err
		}
	}
	s.stage("indexed")
	if err := s.recheck(ctx, link.LinkID, source.SourceID, device.DeviceID); err != nil {
		s.undo(ctx, entries)
		return err
	}
	return nil
}

func (s *Service) stage(name string) {
	if s.hook != nil {
		s.hook(name)
	}
}

// recheck 读库复核访客的链接、主机、团队与设备此刻是否仍有效。
func (s *Service) recheck(ctx context.Context, linkID, sourceID, deviceID string) error {
	fresh, err := s.c.Ports.Links.Get(ctx, s.tenant(), linkID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.RevokedAtMs != nil {
		return core.Fail(gen.ErrLinkInvalid)
	}
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), sourceID)
	if err != nil {
		return err
	}
	if space == nil || space.Revoked {
		return core.Fail(gen.ErrSourceRevoked)
	}
	if err := guestHostState(space); err != nil {
		return err
	}
	device, err := s.c.Ports.Devices.Get(ctx, s.tenant(), deviceID)
	if err != nil {
		return err
	}
	if device == nil || !core.IsLiveDevice(*device, s.nowMs()) {
		return core.Fail(gen.ErrUnauthenticated)
	}
	return nil
}

// undo 撤掉自己刚签出的令牌（按 jti，只撤这几枚）；撤销失败只记日志。
func (s *Service) undo(ctx context.Context, issued []relayctl.Issued) {
	ctx = context.WithoutCancel(ctx)
	for _, entry := range issued {
		if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{JTI: entry.JTI}); err != nil {
			s.log.Warn("撤销自己签的令牌失败", obs.Operation("links.undo"), obs.SourceID(entry.SourceID), obs.Error(err))
		}
	}
}

// ---- links.list / update / revoke ----

// list 是 links.list：账号能管的主机名下的链接，可按 sourceId、space 过滤；给了 sourceId 要有管理权
// （看不到 404、管不了 403；space 过滤把它排除在外则答空）；space 是团队时调用者要是成员（否则 404）。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	_, account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	sourceID, _ := call.Input["sourceId"].(string)
	nowMs := s.nowMs()
	r := s.store.Read()
	spaceKind, spaceTeam := "", ""
	if raw, ok := call.Input["space"].(map[string]any); ok {
		spaceKind, _ = raw["kind"].(string)
		if spaceKind == saas.SpaceTeam {
			spaceTeam, _ = raw["teamId"].(string)
			if _, err := teams.Require(ctx, r, s.tenant(), account.AccountID, spaceTeam, teams.MinAny); err != nil {
				return nil, err
			}
		}
	}
	var sourceIDs []string
	if sourceID != "" {
		h, err := grants.FindHost(ctx, r, s.tenant(), nowMs, account.AccountID, sourceID)
		if err != nil {
			return nil, err
		}
		if !h.CanManage(account.AccountID) {
			return nil, grants.DenyManage(h)
		}
		inSpace := spaceKind == "" || (spaceKind == saas.SpacePersonal && h.TeamID == "") || (spaceKind == saas.SpaceTeam && h.TeamID == spaceTeam)
		if inSpace {
			sourceIDs = []string{sourceID}
		}
	} else {
		visible, err := r.ListHosts(ctx, s.tenant(), saas.HostFilter{
			ViewerID: account.AccountID, NowMs: nowMs, SpaceKind: spaceKind, TeamID: spaceTeam, Limit: maxManagedHosts,
		})
		if err != nil {
			return nil, err
		}
		for _, h := range visible {
			if h.CanManage(account.AccountID) {
				sourceIDs = append(sourceIDs, h.SourceID)
			}
		}
	}
	records, err := r.LinksForSources(ctx, s.tenant(), sourceIDs, maxListedLinks)
	if err != nil {
		return nil, err
	}
	settings, err := s.c.Ports.Tenants.Get(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	out := make([]core.LinkSummary, 0, len(records))
	for _, record := range records {
		out = append(out, core.LinkSummaryOf(settings.Issuer, record))
	}
	return map[string]any{"links": out}, nil
}

// update 是 links.update：只改备注；管理权由 Policy.CanManage 按链接所属的主机判。
func (s *Service) update(ctx context.Context, call *api.Call) (any, error) {
	caller, _, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	linkID, _ := call.Input["linkId"].(string)
	label, _ := call.Input["label"].(string)
	return s.links.Update(ctx, caller, linkID, label)
}

// revoke 是 links.revoke：主机管理权；撤销链接与它名下的访客设备（同事务），提交后撤掉访客已签的令牌（撤销钩子）、
// 断开 guest:<linkId>: 前缀的连接、通知访客；审计。
func (s *Service) revoke(ctx context.Context, call *api.Call) (any, error) {
	_, account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	linkID, _ := call.Input["linkId"].(string)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		link, err := tx.Ports().Links.Get(ctx, s.tenant(), linkID)
		if err != nil {
			return err
		}
		if link == nil {
			return core.Fail(gen.ErrNotFound)
		}
		h, err := grants.ManageHost(ctx, tx, s.tenant(), s.nowMs(), account.AccountID, link.SourceID)
		if err != nil {
			return err
		}
		sourceID, ok, err := core.RevokeLink(ctx, s.bound(tx), linkID, s.nowMs())
		if err != nil {
			return err
		}
		if !ok {
			return core.Fail(gen.ErrNotFound)
		}
		tx.AfterCommit(func(context.Context) {
			if s.c.Relay != nil {
				// 节点命令都必须带 sourceId（设计 §2.4）。
				s.c.Relay.Disconnect(core.ConnectionFilter{SourceID: sourceID, SubjectPrefix: core.GuestPrefix(linkID)})
			}
			s.c.Emit(core.NewAccessRevoked(sourceID), &core.Audience{LinkID: linkID})
		})
		return s.record(ctx, tx, call.IP, account.AccountID, h.TeamID, h.SourceID, ActionRevoke, linkID)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
