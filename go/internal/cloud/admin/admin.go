package admin

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 运营动作的审计名（actor.kind = operator，设计 §4.9）。
const (
	ActionAccountDisable = "account.disable"
	ActionAccountEnable  = "account.enable"
	ActionTeamSuspend    = "team.suspend"
	ActionTeamResume     = "team.resume"
	ActionNodeDrain      = "relay.drain"
	ActionIncidentSet    = "incident.set"
)

const (
	// pageMax 是运营列表一页最多多少行（设计 §4.9：搜索最多 50 行）。
	pageMax = 50
	// recentActions 是详情里附的最近运营动作条数。
	recentActions = 5
	// IncidentSyncEvery 是各控制面实例把事故开关从库同步给节点的间隔（设计 §4.9）。
	IncidentSyncEvery = 5 * time.Second
	// incidentRepushTicks：开关没变也每隔这么多次同步重写一遍（节点侧的键丢了也能自愈）。
	incidentRepushTicks = 12
)

// Options 是运营后台的装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Relay 读节点表、发排空命令、写事故开关、撤销令牌并踢人。
	Relay relayctl.Control
	Log   *obs.Logger
	// IncidentEvery 缺省 IncidentSyncEvery（测试可缩短）。
	IncidentEvery time.Duration
}

// Service 是运营后台服务。
type Service struct {
	c             *core.Context
	store         saas.Store
	relay         relayctl.Control
	log           *obs.Logger
	incidentEvery time.Duration
}

// New 建服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	every := options.IncidentEvery
	if every <= 0 {
		every = IncidentSyncEvery
	}
	return &Service{c: options.Core, store: options.Store, relay: options.Relay, log: log, incidentEvery: every}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的 admin.* 过程（M1 最小集）。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"admin.tenants": s.tenants, "admin.accounts": s.accounts, "admin.account": s.account, "admin.accountStatus": s.accountStatus,
		"admin.teams": s.teams, "admin.team": s.team, "admin.teamStatus": s.teamStatus,
		"admin.relayNodes": s.relayNodes, "admin.relayNodeDrain": s.relayNodeDrain,
		"admin.incident": s.incident, "admin.incidentSet": s.incidentSet,
	}
	for name, handler := range handlers {
		handlers[name] = func(ctx context.Context, call *api.Call) (any, error) {
			out, err := handler(ctx, call)
			return out, httpx.MapError(err)
		}
	}
	return handlers
}

// operator 要求调用方是运营会话（Cookie 或 Bearer，scope = operator），并重读账号：运营范围被摘掉或账号被停用
// 的账号，即使会话还没到期也立刻不能用。HTTP 层的闸已经保证 admin.* 只有运营会话进得来，这里是第二道。
func (s *Service) operator(ctx context.Context, call *api.Call) (*saas.Account, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if caller.Scope != core.SessionScopeOperator || caller.AccountID == "" {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	account, err := s.store.Read().Account(ctx, s.tenant(), caller.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Status != saas.AccountActive {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	if !account.HasScope(saas.ScopeOperator) {
		return nil, core.Fail(gen.ErrForbidden)
	}
	return account, nil
}

// record 在业务事务里写一条运营动作：actor.kind = operator，理由进 reason 列，team_id 是对象团队（没有则空）。
func (s *Service) record(ctx context.Context, tx saas.Tx, call *api.Call, operator, teamID, action, targetKind, targetID, reason string) error {
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), saas.AuditEntry{
		TeamID: teamID, ActorKind: saas.ActorOperator, ActorID: operator, Action: action,
		TargetKind: targetKind, TargetID: targetID, IPPrefix: audit.IPPrefix(call.IP), Reason: reason,
	})
}

// ---- 列表与详情 ----

type listInput struct {
	Q      string `json:"q"`
	Status string `json:"status"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func (in listInput) query() (saas.AdminQuery, int, error) {
	q := saas.AdminQuery{Q: in.Q, Status: in.Status}
	if in.Cursor != "" {
		after, err := saas.DecodeAdminCursor(in.Cursor)
		if err != nil {
			return q, 0, core.Fail(gen.ErrBadRequest)
		}
		q.After = after
	}
	limit := in.Limit
	if limit <= 0 || limit > pageMax {
		limit = pageMax
	}
	q.Limit = limit + 1
	return q, limit, nil
}

// cursorOf 把多取的一行换成 nextCursor。
func cursorOf[T any](rows []T, limit int, key func(T) (int64, string)) ([]T, any) {
	if len(rows) <= limit {
		return rows, nil
	}
	createdAt, id := key(rows[limit-1])
	return rows[:limit], saas.AdminCursor{CreatedAtMs: createdAt, ID: id}.Encode()
}

type tenantOut struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Plan        string `json:"plan"`
	HostCount   int    `json:"hostCount"`
	Status      string `json:"status"`
	CreatedAtMs int64  `json:"createdAtMs"`
}

func (s *Service) tenants(ctx context.Context, call *api.Call) (any, error) {
	var in listInput
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	q, limit, err := in.query()
	if err != nil {
		return nil, err
	}
	q.Status = "" // admin.tenants 没有状态筛选
	rows, err := s.store.Read().AdminTenants(ctx, s.tenant(), q)
	if err != nil {
		return nil, err
	}
	rows, cursor := cursorOf(rows, limit, func(r saas.AdminTenant) (int64, string) { return r.CreatedAtMs, r.ID })
	out := make([]tenantOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantOut{Kind: r.Kind, ID: r.ID, Name: r.Name, Plan: r.Plan, HostCount: r.HostCount, Status: r.Status, CreatedAtMs: r.CreatedAtMs})
	}
	return map[string]any{"tenants": out, "nextCursor": cursor}, nil
}

type accountOut struct {
	AccountID       string  `json:"accountId"`
	Email           *string `json:"email"`
	DisplayName     string  `json:"displayName"`
	EmailVerified   bool    `json:"emailVerified"`
	Status          string  `json:"status"`
	Plan            string  `json:"plan"`
	HostCount       int     `json:"hostCount"`
	TeamCount       int     `json:"teamCount"`
	CreatedAtMs     int64   `json:"createdAtMs"`
	LastSeenAtMs    *int64  `json:"lastSeenAtMs"`
	Seats           *int    `json:"seats,omitempty"`
	RelayBytesMonth *int64  `json:"relayBytesMonth,omitempty"`
	RecentActions   *[]any  `json:"recentActions,omitempty"`
}

func accountView(a saas.AdminAccount) accountOut {
	return accountOut{
		AccountID: a.AccountID, Email: a.Email, DisplayName: a.DisplayName, EmailVerified: a.EmailVerified, Status: a.Status,
		Plan: a.Plan, HostCount: a.HostCount, TeamCount: a.TeamCount, CreatedAtMs: a.CreatedAtMs, LastSeenAtMs: a.LastSeenAtMs,
	}
}

func (s *Service) accounts(ctx context.Context, call *api.Call) (any, error) {
	var in listInput
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	q, limit, err := in.query()
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Read().AdminAccounts(ctx, s.tenant(), q)
	if err != nil {
		return nil, err
	}
	rows, cursor := cursorOf(rows, limit, func(r saas.AdminAccount) (int64, string) { return r.CreatedAtMs, r.AccountID })
	out := make([]accountOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, accountView(r))
	}
	return map[string]any{"accounts": out, "nextCursor": cursor}, nil
}

// month 是此刻所在 UTC 月的起止（详情里的「本月流量」）。
func (s *Service) month() (start, end int64) {
	now := time.UnixMilli(s.nowMs()).UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.UnixMilli(), first.AddDate(0, 1, 0).UnixMilli()
}

func (s *Service) accountDetail(ctx context.Context, accountID string) (accountOut, error) {
	start, end := s.month()
	reader := s.store.Read()
	row, err := reader.AdminAccount(ctx, s.tenant(), accountID, start, end)
	if err != nil {
		return accountOut{}, err
	}
	if row == nil {
		return accountOut{}, core.Fail(gen.ErrNotFound)
	}
	actions, err := s.recentOf(ctx, "account", accountID)
	if err != nil {
		return accountOut{}, err
	}
	out := accountView(*row)
	seats, bytes := 1, row.RelayBytesMonth
	out.Seats, out.RelayBytesMonth, out.RecentActions = &seats, &bytes, &actions
	return out, nil
}

func (s *Service) recentOf(ctx context.Context, kind, id string) ([]any, error) {
	rows, err := s.store.Read().OperatorActions(ctx, s.tenant(), kind, id, recentActions)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, audit.EventView(row))
	}
	return out, nil
}

func (s *Service) account(ctx context.Context, call *api.Call) (any, error) {
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	id, _ := call.Input["accountId"].(string)
	return s.accountDetail(ctx, id)
}

type teamOut struct {
	TeamID          string `json:"teamId"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	Plan            string `json:"plan"`
	MemberCount     int    `json:"memberCount"`
	HostCount       int    `json:"hostCount"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	DeletedAtMs     *int64 `json:"deletedAtMs"`
	Seats           *int   `json:"seats,omitempty"`
	RelayBytesMonth *int64 `json:"relayBytesMonth,omitempty"`
	RecentActions   *[]any `json:"recentActions,omitempty"`
}

func teamView(t saas.AdminTeam) teamOut {
	return teamOut{
		TeamID: t.TeamID, Slug: t.Slug, Name: t.Name, Status: t.Status, Plan: t.Plan, MemberCount: t.MemberCount, HostCount: t.HostCount,
		CreatedAtMs: t.CreatedAtMs, DeletedAtMs: t.DeletedAtMs,
	}
}

func (s *Service) teams(ctx context.Context, call *api.Call) (any, error) {
	var in listInput
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	q, limit, err := in.query()
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Read().AdminTeams(ctx, s.tenant(), q)
	if err != nil {
		return nil, err
	}
	rows, cursor := cursorOf(rows, limit, func(r saas.AdminTeam) (int64, string) { return r.CreatedAtMs, r.TeamID })
	out := make([]teamOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamView(r))
	}
	return map[string]any{"teams": out, "nextCursor": cursor}, nil
}

func (s *Service) teamDetail(ctx context.Context, teamID string) (teamOut, error) {
	start, end := s.month()
	row, err := s.store.Read().AdminTeam(ctx, s.tenant(), teamID, start, end)
	if err != nil {
		return teamOut{}, err
	}
	if row == nil {
		return teamOut{}, core.Fail(gen.ErrNotFound)
	}
	actions, err := s.recentOf(ctx, "team", teamID)
	if err != nil {
		return teamOut{}, err
	}
	out := teamView(*row)
	seats, bytes := row.Seats, row.RelayBytesMonth
	out.Seats, out.RelayBytesMonth, out.RecentActions = &seats, &bytes, &actions
	return out, nil
}

func (s *Service) team(ctx context.Context, call *api.Call) (any, error) {
	if _, err := s.operator(ctx, call); err != nil {
		return nil, err
	}
	id, _ := call.Input["teamId"].(string)
	return s.teamDetail(ctx, id)
}
