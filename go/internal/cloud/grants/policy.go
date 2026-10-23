package grants

import (
	"context"
	"errors"
	"sync"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 目录一页的行数。
const maxVisibleLimit = 200

// PolicyOptions 是访问策略的装配。
type PolicyOptions struct {
	Core  *core.Context
	Store saas.Store
	// Relay 查主机在线时隧道上报的 coreVersion（版本门取它，不取登记时的）。
	Relay relayctl.Control
	Log   *obs.Logger
}

// Policy 是 core.AccessPolicy 的 SaaS 实现（设计 §4.2、§4.6）：可见性来自授予与团队角色，管理权是主机所有者
// 或团队 owner / admin，意图是有效权限合成的断言声明。链接访客（guest:<linkId>:… 主体）走 core 现有的链接路径。
type Policy struct {
	c        *core.Context
	store    saas.Store
	relay    relayctl.Control
	personal core.AccessPolicy
	log      *obs.Logger
}

var _ core.AccessPolicy = (*Policy)(nil)

// NewPolicy 建访问策略；装配时把它设为 core.Context.Policy。
func NewPolicy(options PolicyOptions) *Policy {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Policy{c: options.Core, store: options.Store, relay: options.Relay, personal: core.NewPersonalPolicy(options.Core), log: log}
}

func (p *Policy) tenant() string { return p.c.TenantID }

// Hosts 是主机目录的可见性来源（hosts.Directory）：只返回 filter.ViewerID 此刻能看到的主机；r 可以是事务。
func (p *Policy) Hosts(ctx context.Context, r saas.Reader, filter saas.HostFilter) ([]saas.Host, error) {
	filter.NowMs = p.c.NowMs()
	return r.ListHosts(ctx, p.tenant(), filter)
}

var _ hosts.Directory = (*Policy)(nil)

func viaOf(h saas.Host, viewerID string) string {
	switch {
	case h.OwnerAccountID == viewerID:
		return "owner"
	case h.TeamID != "":
		return "team"
	}
	// 个人空间里被授予的访客：契约的来路枚举没有 guest，按分享给他的人算 link。
	return "link"
}

func visibleOf(h saas.Host, viewerID string) core.VisibleSource {
	return core.VisibleSource{Source: h.SourceRecord, Via: viaOf(h, viewerID), Owner: h.OwnerAccountID == viewerID}
}

// Visible 实现 core.AccessPolicy：按 (registered_at_ms, source_id) 游标分页，next 为空表示没有下一页。
func (p *Policy) Visible(ctx context.Context, caller core.Caller, filter core.SourceFilter) ([]core.VisibleSource, string, error) {
	if caller.AccountID == "" {
		return p.personal.Visible(ctx, caller, filter)
	}
	limit := filter.Limit
	if limit <= 0 || limit > maxVisibleLimit {
		limit = maxVisibleLimit
	}
	query := saas.HostFilter{ViewerID: caller.AccountID, Limit: limit + 1}
	if filter.Cursor != "" {
		after, err := saas.DecodeHostCursor(filter.Cursor)
		if err != nil {
			return nil, "", core.Fail(gen.ErrBadRequest)
		}
		query.After = after
	}
	rows, err := p.Hosts(ctx, p.store.Read(), query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = saas.HostCursor{RegisteredAtMs: last.RegisteredAtMs, SourceID: last.SourceID}.Encode()
	}
	out := make([]core.VisibleSource, 0, len(rows))
	for _, h := range rows {
		out = append(out, visibleOf(h, caller.AccountID))
	}
	return out, next, nil
}

// Access 实现 core.AccessPolicy：单台主机此刻看不看得到（不用 Visible 的第一页代替）。看得到不等于可达：
// 团队 owner / admin 看得到全部主机，能不能签断言由 Intent 按有效权限判。
func (p *Policy) Access(ctx context.Context, caller core.Caller, sourceID string) (core.VisibleSource, bool, error) {
	if caller.AccountID == "" {
		return p.personal.Access(ctx, caller, sourceID)
	}
	rows, err := p.Hosts(ctx, p.store.Read(), saas.HostFilter{ViewerID: caller.AccountID, SourceID: sourceID, Limit: 1})
	if err != nil || len(rows) == 0 {
		return core.VisibleSource{}, false, err
	}
	return visibleOf(rows[0], caller.AccountID), true, nil
}

// CanManage 实现 core.AccessPolicy：主机所有者，或团队主机的团队 owner / admin。看不到的答 not_found，
// 看得到但管不了答 forbidden。target.LinkID 由链接解析所属的源；target 全空（不带过滤的 links.list）一律拒绝。
func (p *Policy) CanManage(ctx context.Context, caller core.Caller, target core.ManageTarget, _ core.ManageAction) error {
	if caller.AccountID == "" {
		return core.Fail(gen.ErrForbidden)
	}
	sourceID := target.SourceID
	if sourceID == "" && target.LinkID != "" {
		link, err := p.c.Ports.Links.Get(ctx, p.tenant(), target.LinkID)
		if err != nil {
			return err
		}
		if link == nil {
			return core.Fail(gen.ErrNotFound)
		}
		sourceID = link.SourceID
	}
	if sourceID == "" {
		return core.Fail(gen.ErrForbidden)
	}
	rows, err := p.Hosts(ctx, p.store.Read(), saas.HostFilter{ViewerID: caller.AccountID, SourceID: sourceID, Limit: 1})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return core.Fail(gen.ErrNotFound)
	}
	if !rows[0].CanManage(caller.AccountID) {
		return core.Fail(gen.ErrForbidden)
	}
	return nil
}

// 令牌寿命与 scp 的界限。
const (
	// minTokenLife 是授予剩余寿命的下限（毫秒）：不足就不签。
	minTokenLife = 1000
	// MaxScopeEntries 是断言 scp 的项数上限（协议包 assertion-claims）。
	MaxScopeEntries = 64
)

var errScopeTooLarge = errors.New("scp 项数超限")

// suspended 报告账号所在团队的这台主机的团队已停用（成员才看得到；删除的团队与外人不算）。
func suspended(b saas.AccessBasis) bool {
	return b.SourceFound && !b.SourceRevoked && b.TeamID != "" && b.TeamSuspended && !b.TeamDeleted && b.TeamRole != ""
}

// signedView 是签断言时依据的事实：有效权限与团队角色。签发方登记索引后再算一遍，对不上（收窄了）就撤自己签的令牌。
type signedView struct {
	extra    core.GrantExtra
	eff      Effective
	teamRole saas.TeamRole
}

// Intent 实现 core.AccessPolicy：按有效权限合成断言要带的声明（设计 §4.6 步骤 2、4–6）。
// 团队停用 team_suspended；没有有效权限 source_access_denied；core 版本不到门槛 source_access_denied（不签降级断言）。
func (p *Policy) Intent(ctx context.Context, caller core.Caller, source store.SourceRecord) (core.GrantExtra, error) {
	if caller.AccountID == "" {
		return p.personal.Intent(ctx, caller, source)
	}
	view, err := p.intent(ctx, caller.AccountID, source)
	return view.extra, err
}

func (p *Policy) intent(ctx context.Context, accountID string, source store.SourceRecord) (signedView, error) {
	reader := p.store.Read()
	account, err := reader.Account(ctx, p.tenant(), accountID)
	if err != nil {
		return signedView{}, err
	}
	if account == nil {
		return signedView{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	basis, err := reader.AccessBasis(ctx, p.tenant(), accountID, source.SourceID)
	if err != nil {
		return signedView{}, err
	}
	if suspended(basis) {
		return signedView{}, core.Fail(gen.ErrTeamSuspended)
	}
	nowMs := p.c.NowMs()
	eff := Compose(basis, accountID, nowMs)
	if eff.None() {
		return signedView{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	// 授予快到期了（不足 1 秒）签不出有用的令牌；scp 超过协议上限（多个授予的范围并集）也不签。
	if eff.ExpiresAtMs != nil && *eff.ExpiresAtMs-nowMs < minTokenLife {
		return signedView{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	if len(eff.WorkspaceIDs) > MaxScopeEntries {
		p.log.Warn("合成后的 scp 超过上限，不签断言", obs.Operation("grants.intent"), obs.SourceID(source.SourceID), obs.Error(errScopeTooLarge))
		return signedView{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	if err := p.versionGate(ctx, source, basis, eff); err != nil {
		return signedView{}, err
	}
	extra := core.GrantExtra{
		Name: account.DisplayName, Email: account.Email, EmailVerified: account.EmailVerifiedAtMs != nil,
		Role: eff.Role, Scp: eff.ScopeClaims(), TrackJTI: true,
	}
	if extra.Scp == nil {
		extra.Scp = []string{}
	}
	if eff.ExpiresAtMs != nil {
		extra.ExpiresAtMs = *eff.ExpiresAtMs // 令牌寿命不超过授予的到期
	}
	if basis.TeamID != "" {
		extra.Team = &gen.AssertionClaimsTeam{TeamID: basis.TeamID, Role: string(basis.TeamRole)}
	} else if eff.Owner() {
		extra.Role = "" // 个人空间的所有者与个人中转同形：不带 role
	}
	return signedView{extra: extra, eff: eff, teamRole: basis.TeamRole}, nil
}

// versionGate 是版本门（契约 §30、设计 §4.6 步骤 5）：取在线隧道上报的 coreVersion，不在线才用登记时的。
// 团队主机要求不低于 MIN_CORE_VERSION_TEAM_ASSERTION；个人空间主机上的访客（来路只有 guest 授予）要求不低于
// MIN_CORE_VERSION_ROLE_ONLY_ASSERTION；所有者本人访问个人空间主机不受门限。解析不了的版本不放行。
func (p *Policy) versionGate(ctx context.Context, source store.SourceRecord, basis saas.AccessBasis, eff Effective) error {
	var min string
	switch {
	case basis.TeamID != "":
		min = gen.MinCoreVersionTeamAssertion
	case eff.GuestOnly():
		min = gen.MinCoreVersionRoleOnlyAssertion
	default:
		return nil
	}
	version := source.CoreVersion
	online, ok, err := p.relay.Route(ctx, source.SourceID)
	if err != nil {
		return err
	}
	if ok {
		version = online
	}
	if !hosts.AtLeast(version, min) {
		return core.Fail(gen.ErrSourceAccessDenied)
	}
	return nil
}

// AccessChanged 是给 Watcher.Refresh 的内部事件：某账号对某台主机的可见性变了（授予 / 撤销 / 成员变动）。
// S8 的领域事件（grantChanged、memberChanged）转成它。它是 core.InternalEvent，不会进客户端的事件队列。
// AccountID 空只允许 Visible = false（对所有连接移除，例如主机撤销）；放宽（Visible = true）必须带具体账号，
// 否则会把别人看不到的主机露给所有连接——这样的事件被忽略。
type AccessChanged struct {
	SourceID  string
	AccountID string
	Visible   bool
}

// EventSource 实现 core.Event。
func (e AccessChanged) EventSource() string { return e.SourceID }

// InternalOnly 实现 core.InternalEvent。
func (AccessChanged) InternalOnly() {}

// Watch 实现 core.AccessPolicy：连接建立时解析可见集合（取全部页），之后靠 Refresh 更新。
func (p *Policy) Watch(ctx context.Context, caller core.Caller) (core.Watcher, error) {
	if caller.AccountID == "" {
		return p.personal.Watch(ctx, caller)
	}
	w := &watcher{account: caller.AccountID, visible: map[string]struct{}{}}
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, next, err := p.Visible(ctx, caller, core.SourceFilter{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, entry := range page {
			w.visible[entry.Source.SourceID] = struct{}{}
		}
		if next == "" {
			return w, nil
		}
		if seen[next] || next == cursor {
			return nil, core.Fail(gen.ErrInternal)
		}
		seen[next] = true
		cursor = next
	}
}

// watcher 是一个 me-stream 连接的事件可见性：Sees / Refresh / Close 可以被不同的 goroutine 并发调用，
// Close 之后仍可能被调用，那时 Sees 答 false、Refresh 什么也不做。
type watcher struct {
	account string

	mu      sync.RWMutex
	closed  bool
	visible map[string]struct{}
}

var _ core.Watcher = (*watcher)(nil)

func (w *watcher) Sees(sourceID string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return false
	}
	_, ok := w.visible[sourceID]
	return ok
}

func (w *watcher) Refresh(event core.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	switch e := event.(type) {
	case core.SourceRevoked:
		delete(w.visible, e.SourceID)
	case *core.SourceRevoked:
		if e != nil {
			delete(w.visible, e.SourceID)
		}
	case AccessChanged:
		w.apply(e)
	case *AccessChanged:
		if e != nil {
			w.apply(*e)
		}
	}
}

func (w *watcher) apply(e AccessChanged) {
	if e.AccountID != "" && e.AccountID != w.account {
		return
	}
	if e.Visible {
		if e.AccountID == "" {
			return // 放宽必须点名账号
		}
		w.visible[e.SourceID] = struct{}{}
		return
	}
	delete(w.visible, e.SourceID)
}

func (w *watcher) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.visible = nil
}
