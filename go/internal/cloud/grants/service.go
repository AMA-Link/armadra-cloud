package grants

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 审计动作与作业种类（设计 §6.1）。
const (
	ActionCreate       = "grant.create"
	ActionUpdate       = "grant.update"
	ActionRevoke       = "grant.revoke"
	ActionExpire       = "grant.expire"
	ActionAccessRevoke = "access.revoke"
	ActionAssertion    = "assertion.issue"

	// JobExpire 在授予到期那一刻写撤销并撤令牌（run_at = expires_at_ms，dedupe = grantId）。
	JobExpire = "grant.expire"
	// JobExpireDue 每小时扫一遍到期却还没撤销的授予，补插 JobExpire（作业被去重挡掉或丢了的兜底）。
	JobExpireDue = "grant.expireDue"
	// JobIssuedGC 每小时删已到期的签发索引与撤销行（设计 §6.1 的 issued.gc）。
	JobIssuedGC = "issued.gc"

	expireBatch = 100
	// maxAccessRows 是 sources.accessList 一次最多列的账号数。
	maxAccessRows = 500
	// maxListedGrants 是 grants.list 一次最多答的授予数。
	maxListedGrants = 1000
)

// workspacePattern 是 scp 里 ws:<id> 的 id（协议包 scopeEntrySchema）。
var workspacePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// cleanWorkspaces 校验并去重排序：每项符合 scopeEntrySchema，至多 64 项。
func cleanWorkspaces(ids []string) ([]string, error) {
	out := sortedUnique(ids)
	if len(out) > MaxScopeEntries {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	for _, id := range out {
		if !workspacePattern.MatchString(id) {
			return nil, core.Fail(gen.ErrBadRequest)
		}
	}
	return out, nil
}

// Options 是授予服务的装配。
type Options struct {
	Core   *core.Context
	Store  saas.Store
	Limits *httpx.Limits
	// Relay 签发索引、撤销已签令牌与踢人（经 tx.AfterCommit，提交后才发）、查在线。
	Relay  relayctl.Control
	Policy *Policy
	// RequireEmailVerified 为真时未验证邮箱的账号不能授予（设计 §4.3）。
	RequireEmailVerified bool
	Log                  *obs.Logger
	// Hook 给测试在断言签发的两个交错点停住（"signed"：令牌已签、索引未登记；"indexed"：索引已登记、复核之前）。生产不设。
	Hook func(stage string)
}

// Service 是授予与断言服务。
type Service struct {
	c        *core.Context
	store    saas.Store
	limits   *httpx.Limits
	relay    relayctl.Control
	policy   *Policy
	sources  *core.Sources
	verified bool
	log      *obs.Logger
	hook     func(stage string)
}

// New 建授予服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Service{
		c: options.Core, store: options.Store, limits: options.Limits, relay: options.Relay, policy: options.Policy,
		sources: core.NewSources(options.Core), verified: options.RequireEmailVerified, log: log, hook: options.Hook,
	}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"grants.list":          s.list,
		"grants.create":        s.create,
		"grants.update":        s.update,
		"grants.revoke":        s.revoke,
		"grants.effective":     s.effective,
		"sources.assertion":    s.assertion,
		"sources.accessList":   s.accessList,
		"sources.accessRevoke": s.accessRevoke,
	}
	for name, handler := range handlers {
		handlers[name] = wrap(handler)
	}
	return handlers
}

// Register 把到期作业的处理器登记进运行器。
func (s *Service) Register(runner *jobs.Runner) {
	runner.Handle(JobExpire, s.expire)
	runner.Handle(JobExpireDue, s.expireDue)
	runner.Every(JobExpireDue, jobs.Hourly)
	runner.Handle(JobIssuedGC, s.issuedGC)
	runner.Every(JobIssuedGC, jobs.Hourly)
}

// issuedGC 是 issued.gc：删已到期的签发索引（cloud_issued_tokens，带 tenant_id）与撤销行——令牌过期之后，
// 撤不撤都无所谓了。
func (s *Service) issuedGC(ctx context.Context, _ saas.Job) error {
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		_, err := tx.Issued().GC(ctx, s.tenant(), s.nowMs())
		return err
	})
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

// ---- 线上形状 ----

type grantOut struct {
	GrantID      string         `json:"grantId"`
	SourceID     string         `json:"sourceId"`
	Subject      map[string]any `json:"subject"`
	Role         string         `json:"role"`
	WorkspaceIDs []string       `json:"workspaceIds"`
	ExpiresAtMs  *int64         `json:"expiresAtMs"`
	GrantedBy    string         `json:"grantedBy"`
	CreatedAtMs  int64          `json:"createdAtMs"`
	RevokedAtMs  *int64         `json:"revokedAtMs"`
}

func viewOf(g saas.Grant) grantOut {
	subject := map[string]any{"kind": g.SubjectKind}
	switch g.SubjectKind {
	case saas.GrantSubjectTeam:
		subject["teamId"] = g.SubjectID
	case saas.GrantSubjectGroup:
		subject["groupId"] = g.SubjectID
	default:
		subject["accountId"] = g.SubjectID
	}
	ws := sortedUnique(g.WorkspaceIDs)
	return grantOut{
		GrantID: g.GrantID, SourceID: g.SourceID, Subject: subject, Role: g.Role, WorkspaceIDs: ws, ExpiresAtMs: g.ExpiresAtMs,
		GrantedBy: g.GrantedBy, CreatedAtMs: g.CreatedAtMs, RevokedAtMs: g.RevokedAtMs,
	}
}

func (s *Service) record(ctx context.Context, tx saas.Tx, ip, actorKind, actorID, teamID, sourceID, action, targetKind, targetID string, jti ...string) error {
	entry := saas.AuditEntry{
		TeamID: teamID, ActorKind: actorKind, ActorID: actorID, Action: action,
		TargetKind: targetKind, TargetID: targetID, SourceID: sourceID, IPPrefix: audit.IPPrefix(ip),
	}
	if len(jti) > 0 {
		entry.JTI = jti[0]
	}
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), entry)
}

// ---- 管理权 ----

// find 读调用者看得到的一台未撤销的主机；看不到（不存在、别人的、别的团队的）答 not_found。
func (s *Service) find(ctx context.Context, r saas.Reader, viewerID, sourceID string) (saas.Host, error) {
	rows, err := s.policy.Hosts(ctx, r, saas.HostFilter{ViewerID: viewerID, SourceID: sourceID, Limit: 1})
	if err != nil {
		return saas.Host{}, err
	}
	if len(rows) == 0 {
		return saas.Host{}, core.Fail(gen.ErrNotFound)
	}
	return rows[0], nil
}

// denyManage 是看得到却管不了时的错误：团队主机答 team_role_insufficient，个人空间答 source_access_denied。
func denyManage(h saas.Host) error {
	if h.TeamID != "" {
		return core.Fail(gen.ErrTeamRoleInsufficient)
	}
	return core.Fail(gen.ErrSourceAccessDenied)
}

// manage 在事务里核管理权：团队主机先锁团队行（团队 → 账号 → 其余的顺序）并核成员，再锁主机、重读，
// 看不到答 not_found，看得到但管不了答 denyManage。
func (s *Service) manage(ctx context.Context, tx saas.Tx, accountID, sourceID string) (saas.Host, error) {
	first, err := s.find(ctx, tx, accountID, sourceID)
	if err != nil {
		return saas.Host{}, err
	}
	if first.TeamID != "" {
		if _, err := teams.Lock(ctx, tx, s.tenant(), accountID, first.TeamID, teams.MinStaff); err != nil {
			return saas.Host{}, err
		}
	}
	if err := tx.Hosts().LockSource(ctx, s.tenant(), sourceID); err != nil {
		return saas.Host{}, err
	}
	h, err := s.find(ctx, tx, accountID, sourceID)
	if err != nil {
		return saas.Host{}, err
	}
	if !h.CanManage(accountID) {
		return saas.Host{}, denyManage(h)
	}
	return h, nil
}

// manageRead 是 manage 的只读版（列表与有效权限，不写库不加锁）。
func (s *Service) manageRead(ctx context.Context, accountID, sourceID string) (saas.Host, error) {
	h, err := s.find(ctx, s.store.Read(), accountID, sourceID)
	if err != nil {
		return saas.Host{}, err
	}
	if !h.CanManage(accountID) {
		return saas.Host{}, denyManage(h)
	}
	return h, nil
}

// ---- 有效权限的快照与收窄即撤销（设计 §4.8）----

// effectiveOf 读事实并合成一个账号对一台主机的有效权限。
func (s *Service) effectiveOf(ctx context.Context, r saas.Reader, accountID, sourceID string) (Effective, saas.AccessBasis, error) {
	return s.effectiveAt(ctx, r, accountID, sourceID, s.nowMs())
}

func (s *Service) effectiveAt(ctx context.Context, r saas.Reader, accountID, sourceID string, atMs int64) (Effective, saas.AccessBasis, error) {
	basis, err := r.AccessBasis(ctx, s.tenant(), accountID, sourceID)
	if err != nil {
		return Effective{}, basis, err
	}
	return Compose(basis, accountID, atMs), basis, nil
}

// snapshot 是一批账号此刻对主机的有效权限。
func (s *Service) snapshot(ctx context.Context, r saas.Reader, sourceID string, accounts []string) (map[string]Effective, error) {
	return s.snapshotAt(ctx, r, sourceID, accounts, s.nowMs())
}

// snapshotAt 是一批账号在 atMs 那一刻对主机的有效权限（到期作业用到期前一毫秒的快照作「之前」）。
func (s *Service) snapshotAt(ctx context.Context, r saas.Reader, sourceID string, accounts []string, atMs int64) (map[string]Effective, error) {
	out := make(map[string]Effective, len(accounts))
	for _, id := range accounts {
		eff, _, err := s.effectiveAt(ctx, r, id, sourceID, atMs)
		if err != nil {
			return nil, err
		}
		out[id] = eff
	}
	return out, nil
}

// affected 是改动一条授予会影响到的账号：账号主体就是它自己，团队主体是团队全体成员。
func (s *Service) affected(ctx context.Context, r saas.Reader, g saas.Grant) ([]string, error) {
	switch g.SubjectKind {
	case saas.GrantSubjectTeam:
		return r.TeamAccountIDs(ctx, s.tenant(), g.SubjectID)
	case saas.GrantSubjectGroup:
		return nil, nil
	}
	return []string{g.SubjectID}, nil
}

// narrowedAccounts 是 after 相对 before 收窄了的账号（升序）。
func narrowedAccounts(before, after map[string]Effective) []string {
	var out []string
	for id, was := range before {
		if Narrowed(was, after[id]) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// revokeAccess 在提交后撤销这些账号在该主机上已签的断言与中继令牌并踢掉他们的连接（重连时按新权限重新签）。
// 失败只记日志：重试与全量重放由 S8 的 relayctl 兜底。
func (s *Service) revokeAccess(tx saas.Tx, sourceID string, accounts []string, reason string) {
	if len(accounts) == 0 {
		return
	}
	accounts = slices.Clone(accounts)
	tx.AfterCommit(func(ctx context.Context) {
		for _, id := range accounts {
			if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{Subject: id, SourceID: sourceID}); err != nil {
				s.log.Warn("撤销已签令牌失败", obs.Operation("grants.revoke"), obs.SourceID(sourceID), obs.Error(err))
			}
		}
		for _, id := range accounts {
			if err := s.relay.Kick(ctx, relayctl.KickFilter{SourceID: sourceID, Subject: id}, reason); err != nil {
				s.log.Warn("踢掉连接失败", obs.Operation("grants.kick"), obs.SourceID(sourceID), obs.Error(err))
			}
		}
	})
}

// ---- grants.list / effective ----

func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		SourceID  string `json:"sourceId"`
		AccountID string `json:"accountId"`
		GroupID   string `json:"groupId"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if in.GroupID != "" {
		return nil, core.Fail(gen.ErrPlanFeatureUnavailable) // 分组 M2
	}
	if in.SourceID == "" && in.AccountID == "" {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	out := []grantOut{}
	if in.SourceID != "" {
		if _, err := s.manageRead(ctx, account.AccountID, in.SourceID); err != nil {
			return nil, err
		}
		rows, err := s.store.Read().ListGrants(ctx, s.tenant(), saas.GrantQuery{SourceID: in.SourceID, AccountID: in.AccountID, Limit: maxListedGrants})
		if err != nil {
			return nil, err
		}
		for _, g := range rows {
			out = append(out, viewOf(g))
		}
		return map[string]any{"grants": out}, nil
	}
	// 只给了账号：列出以他为主体、且在调用者能管的主机上的授予。
	rows, err := s.store.Read().ListGrants(ctx, s.tenant(), saas.GrantQuery{AccountID: in.AccountID, Limit: maxListedGrants})
	if err != nil {
		return nil, err
	}
	manageable := map[string]bool{}
	for _, g := range rows {
		ok, seen := manageable[g.SourceID]
		if !seen {
			h, err := s.find(ctx, s.store.Read(), account.AccountID, g.SourceID)
			ok = err == nil && h.CanManage(account.AccountID)
			if err != nil && !isNotFound(err) {
				return nil, err
			}
			manageable[g.SourceID] = ok
		}
		if ok {
			out = append(out, viewOf(g))
		}
	}
	return map[string]any{"grants": out}, nil
}

func isNotFound(err error) bool {
	var coreErr *core.Error
	return errors.As(err, &coreErr) && coreErr.Code == gen.ErrNotFound
}

func (s *Service) effective(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		SourceID  string `json:"sourceId"`
		AccountID string `json:"accountId"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if _, err := s.manageRead(ctx, account.AccountID, in.SourceID); err != nil {
		return nil, err
	}
	eff, _, err := s.effectiveOf(ctx, s.store.Read(), in.AccountID, in.SourceID)
	if err != nil {
		return nil, err
	}
	from := make([]map[string]any, 0, len(eff.From))
	for _, source := range eff.From {
		row := map[string]any{
			"via": source.Via, "grantId": nil, "role": source.Role, "workspaceIds": nonNil(source.WorkspaceIDs), "expiresAtMs": source.ExpiresAtMs,
		}
		if source.GrantID != "" {
			row["grantId"] = source.GrantID
		}
		from = append(from, row)
	}
	var role any
	if !eff.None() {
		role = eff.Role
	}
	return map[string]any{
		"sourceId": in.SourceID, "accountId": in.AccountID, "role": role, "workspaceIds": nonNil(eff.WorkspaceIDs),
		"expiresAtMs": eff.ExpiresAtMs, "from": from,
	}, nil
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// ---- grants.create ----

type subjectIn struct {
	Kind      string `json:"kind"`
	TeamID    string `json:"teamId"`
	AccountID string `json:"accountId"`
	GroupID   string `json:"groupId"`
}

func (s *Service) create(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		SourceID     string    `json:"sourceId"`
		Subject      subjectIn `json:"subject"`
		Role         string    `json:"role"`
		WorkspaceIDs []string  `json:"workspaceIds"`
		ExpiresAtMs  *int64    `json:"expiresAtMs"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	if s.verified && account.EmailVerifiedAtMs == nil {
		return nil, core.Fail(gen.ErrEmailUnverified)
	}
	if in.Subject.Kind == saas.GrantSubjectGroup {
		return nil, core.Fail(gen.ErrPlanFeatureUnavailable) // 分组 M2
	}
	if !ValidRole(in.Role) {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	nowMs := s.nowMs()
	if in.ExpiresAtMs != nil && *in.ExpiresAtMs <= nowMs {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	workspaces, err := cleanWorkspaces(in.WorkspaceIDs)
	if err != nil {
		return nil, err
	}
	var out grantOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		h, err := s.manage(ctx, tx, account.AccountID, in.SourceID)
		if err != nil {
			return err
		}
		kind, subjectID, err := s.subjectOf(ctx, tx, h, in.Subject)
		if err != nil {
			return err
		}
		if kind == saas.GrantSubjectGuest && h.TeamID != "" && len(workspaces) == 0 {
			return core.Fail(gen.ErrBadRequest) // 团队 guest 只能被授予到具体的工作空间，不给整台主机
		}
		existing, err := tx.ListGrants(ctx, s.tenant(), saas.GrantQuery{SourceID: h.SourceID})
		if err != nil {
			return err
		}
		for _, g := range existing {
			if g.SubjectKind == kind && g.SubjectID == subjectID {
				return core.Fail(gen.ErrConflict) // 同主机同主体只有一条有效授予
			}
		}
		grant := saas.Grant{
			GrantID: core.NewUUID(), SourceID: h.SourceID, TeamID: h.TeamID, SubjectKind: kind, SubjectID: subjectID, Role: in.Role,
			WorkspaceIDs: workspaces, ExpiresAtMs: in.ExpiresAtMs, GrantedBy: account.AccountID,
			Origin: saas.GrantOriginGrant, CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
		}
		if err := tx.Grants().Insert(ctx, s.tenant(), grant); err != nil {
			return err
		}
		if err := s.scheduleExpiry(ctx, tx, grant.GrantID, grant.ExpiresAtMs, nowMs); err != nil {
			return err
		}
		if err := s.record(ctx, tx, call.IP, saas.ActorAccount, account.AccountID, h.TeamID, h.SourceID, ActionCreate, "grant", grant.GrantID); err != nil {
			return err
		}
		out = viewOf(grant)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// subjectOf 核对主体的合法性（设计 §4.6）并答库里存的 (kind, subjectID)。
func (s *Service) subjectOf(ctx context.Context, tx saas.Tx, h saas.Host, subject subjectIn) (kind, subjectID string, err error) {
	team := h.TeamID != ""
	switch subject.Kind {
	case saas.GrantSubjectTeam:
		if !team || subject.TeamID != h.TeamID {
			return "", "", core.Fail(gen.ErrBadRequest)
		}
		return subject.Kind, h.TeamID, nil
	case saas.GrantSubjectMember, saas.GrantSubjectGuest:
	default:
		return "", "", core.Fail(gen.ErrBadRequest)
	}
	if subject.AccountID == "" || subject.AccountID == h.OwnerAccountID {
		return "", "", core.Fail(gen.ErrBadRequest)
	}
	if subject.Kind == saas.GrantSubjectMember && !team {
		return "", "", core.Fail(gen.ErrBadRequest)
	}
	target, err := tx.Account(ctx, s.tenant(), subject.AccountID)
	if err != nil {
		return "", "", err
	}
	if target == nil || target.Status != saas.AccountActive {
		return "", "", core.Fail(gen.ErrNotFound)
	}
	if subject.Kind == saas.GrantSubjectGuest {
		if !hosts.GuestsAllowed(h) {
			return "", "", core.Fail(gen.ErrGuestsDisabled)
		}
		if !team && target.EmailVerifiedAtMs == nil {
			return "", "", core.Fail(gen.ErrEmailUnverified)
		}
	}
	if !team {
		return subject.Kind, subject.AccountID, nil
	}
	access, err := tx.TeamAccess(ctx, s.tenant(), h.TeamID, subject.AccountID)
	if err != nil {
		return "", "", err
	}
	if access == nil {
		return "", "", core.Fail(gen.ErrNotFound)
	}
	switch subject.Kind {
	case saas.GrantSubjectMember:
		if !isStaff(access.Role) {
			return "", "", core.Fail(gen.ErrBadRequest)
		}
	case saas.GrantSubjectGuest:
		if access.Role != saas.RoleGuest {
			return "", "", core.Fail(gen.ErrBadRequest)
		}
		if !teams.SettingsOf(access.Team).AllowGuests {
			return "", "", core.Fail(gen.ErrGuestsDisabled)
		}
	}
	return subject.Kind, subject.AccountID, nil
}

type expirePayload struct {
	GrantID string `json:"grantId"`
}

// scheduleExpiry 把到期作业排到授予到期的那一刻（先撤掉旧的，改到期时间时用）；没有到期就只撤旧的。
func (s *Service) scheduleExpiry(ctx context.Context, tx saas.Tx, grantID string, expiresAtMs *int64, nowMs int64) error {
	if err := tx.Jobs().Cancel(ctx, s.tenant(), JobExpire, grantID); err != nil {
		return err
	}
	if expiresAtMs == nil {
		return nil
	}
	payload, err := json.Marshal(expirePayload{GrantID: grantID})
	if err != nil {
		return err
	}
	_, _, err = tx.Jobs().Enqueue(ctx, s.tenant(), saas.NewJob{Kind: JobExpire, Payload: payload, RunAtMs: *expiresAtMs, DedupeKey: grantID}, nowMs)
	return err
}

// ---- grants.update / revoke ----

// lockGrant 按「团队 → 主机 → 授予」的顺序锁住授予所在的链，并核管理权；授予不存在、已撤销或看不到它的主机答 not_found。
func (s *Service) lockGrant(ctx context.Context, tx saas.Tx, accountID, grantID string) (saas.Grant, saas.Host, error) {
	g, err := tx.Grant(ctx, s.tenant(), grantID)
	if err != nil {
		return saas.Grant{}, saas.Host{}, err
	}
	if g == nil || g.RevokedAtMs != nil {
		return saas.Grant{}, saas.Host{}, core.Fail(gen.ErrNotFound)
	}
	h, err := s.manage(ctx, tx, accountID, g.SourceID)
	if err != nil {
		return saas.Grant{}, saas.Host{}, err
	}
	locked, err := tx.Grants().Lock(ctx, s.tenant(), grantID)
	if err != nil {
		return saas.Grant{}, saas.Host{}, err
	}
	if locked == nil || locked.RevokedAtMs != nil {
		return saas.Grant{}, saas.Host{}, core.Fail(gen.ErrNotFound)
	}
	return *locked, h, nil
}

func (s *Service) update(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		GrantID      string   `json:"grantId"`
		Role         *string  `json:"role"`
		WorkspaceIDs []string `json:"workspaceIds"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	_, hasWorkspaces := call.Input["workspaceIds"]
	rawExpiry, hasExpiry := call.Input["expiresAtMs"]
	if in.Role == nil && !hasWorkspaces && !hasExpiry {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	patch := saas.GrantPatch{Role: in.Role}
	if in.Role != nil && !ValidRole(*in.Role) {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	if hasWorkspaces {
		ws, err := cleanWorkspaces(in.WorkspaceIDs)
		if err != nil {
			return nil, err
		}
		patch.WorkspaceIDs = &ws
	}
	nowMs := s.nowMs()
	if hasExpiry {
		if rawExpiry == nil {
			patch.ClearExpiry = true
		} else {
			value, ok := rawExpiry.(float64)
			if !ok || int64(value) <= nowMs {
				return nil, core.Fail(gen.ErrBadRequest)
			}
			expires := int64(value)
			patch.ExpiresAtMs = &expires
		}
	}
	var out grantOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		g, h, err := s.lockGrant(ctx, tx, account.AccountID, in.GrantID)
		if err != nil {
			return err
		}
		if patch.WorkspaceIDs != nil && len(*patch.WorkspaceIDs) == 0 && g.SubjectKind == saas.GrantSubjectGuest && g.TeamID != "" {
			return core.Fail(gen.ErrBadRequest) // 团队 guest 不能改成整台主机
		}
		accounts, err := s.affected(ctx, tx, g)
		if err != nil {
			return err
		}
		before, err := s.snapshot(ctx, tx, g.SourceID, accounts)
		if err != nil {
			return err
		}
		ok, err := tx.Grants().Update(ctx, s.tenant(), g.GrantID, patch, nowMs)
		if err != nil {
			return err
		}
		if !ok {
			return core.Fail(gen.ErrNotFound)
		}
		after, err := s.snapshot(ctx, tx, g.SourceID, accounts)
		if err != nil {
			return err
		}
		if hasExpiry {
			if err := s.scheduleExpiry(ctx, tx, g.GrantID, patch.ExpiresAtMs, nowMs); err != nil {
				return err
			}
		}
		s.revokeAccess(tx, g.SourceID, narrowedAccounts(before, after), "grant_changed")
		if err := s.record(ctx, tx, call.IP, saas.ActorAccount, account.AccountID, h.TeamID, g.SourceID, ActionUpdate, "grant", g.GrantID); err != nil {
			return err
		}
		fresh, err := tx.Grant(ctx, s.tenant(), g.GrantID)
		if err != nil {
			return err
		}
		out = viewOf(*fresh)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) revoke(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	grantID, _ := call.Input["grantId"].(string)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		g, h, err := s.lockGrant(ctx, tx, account.AccountID, grantID)
		if err != nil {
			return err
		}
		accounts, err := s.affected(ctx, tx, g)
		if err != nil {
			return err
		}
		before, err := s.snapshot(ctx, tx, g.SourceID, accounts)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		if ok, err := tx.Grants().Revoke(ctx, s.tenant(), g.GrantID, account.AccountID, nowMs); err != nil || !ok {
			if err == nil {
				err = core.Fail(gen.ErrNotFound)
			}
			return err
		}
		after, err := s.snapshot(ctx, tx, g.SourceID, accounts)
		if err != nil {
			return err
		}
		if err := tx.Jobs().Cancel(ctx, s.tenant(), JobExpire, g.GrantID); err != nil {
			return err
		}
		s.revokeAccess(tx, g.SourceID, narrowedAccounts(before, after), "grant_revoked")
		return s.record(ctx, tx, call.IP, saas.ActorAccount, account.AccountID, h.TeamID, g.SourceID, ActionRevoke, "grant", g.GrantID)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// ---- grant.expire ----

// expire 是 grant.expire：到期那一刻写 revoked_at_ms（revoked_by = system），对权限真正收窄的账号撤销他在这台主机上的已签令牌并踢人。
// 可重放：授予已撤销、到期时间被改到以后就直接结束。
func (s *Service) expire(ctx context.Context, job saas.Job) error {
	var payload expirePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.GrantID == "" {
		return errors.New("作业载荷不合格式")
	}
	peek, err := s.store.Read().Grant(ctx, s.tenant(), payload.GrantID)
	if err != nil || peek == nil || peek.RevokedAtMs != nil {
		return err
	}
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		if peek.TeamID != "" {
			found, err := tx.Teams().LockTeam(ctx, s.tenant(), peek.TeamID)
			if err != nil || !found {
				return err
			}
		}
		if err := tx.Hosts().LockSource(ctx, s.tenant(), peek.SourceID); err != nil {
			return err
		}
		g, err := tx.Grants().Lock(ctx, s.tenant(), payload.GrantID)
		nowMs := s.nowMs()
		if err != nil || g == nil || g.RevokedAtMs != nil || g.ExpiresAtMs == nil || *g.ExpiresAtMs > nowMs {
			return err
		}
		accounts, err := s.affected(ctx, tx, *g)
		if err != nil {
			return err
		}
		// 「之前」取到期前一毫秒（授予还生效），「之后」取此刻（授予已撤销）：只撤真正收窄的账号，
		// 另有来路（别的授予、所有者）而权限没变的人不受牵连。
		before, err := s.snapshotAt(ctx, tx, g.SourceID, accounts, *g.ExpiresAtMs-1)
		if err != nil {
			return err
		}
		if ok, err := tx.Grants().Revoke(ctx, s.tenant(), g.GrantID, saas.GrantRevokedBySystem, nowMs); err != nil || !ok {
			return err
		}
		after, err := s.snapshot(ctx, tx, g.SourceID, accounts)
		if err != nil {
			return err
		}
		s.revokeAccess(tx, g.SourceID, narrowedAccounts(before, after), "grant_expired")
		return s.record(ctx, tx, "", saas.ActorSystem, "", g.TeamID, g.SourceID, ActionExpire, "grant", g.GrantID)
	})
}

// expireDue 是 grant.expireDue：给到期却没撤销的授予补插 grant.expire。
func (s *Service) expireDue(ctx context.Context, _ saas.Job) error {
	nowMs := s.nowMs()
	due, err := s.store.Read().DueGrants(ctx, s.tenant(), nowMs, expireBatch)
	if err != nil {
		return err
	}
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		for _, id := range due {
			payload, err := json.Marshal(expirePayload{GrantID: id})
			if err != nil {
				return err
			}
			if _, _, err := tx.Jobs().Enqueue(ctx, s.tenant(), saas.NewJob{Kind: JobExpire, Payload: payload, RunAtMs: nowMs, DedupeKey: id}, nowMs); err != nil {
				return err
			}
		}
		return nil
	})
}
