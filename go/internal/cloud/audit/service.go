package audit

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 一页最多这么多行（协议包 PAGE_LIMIT_DEFAULT / PAGE_LIMIT_MAX）。
const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

// Authorizer 核对调用者是团队里够格的成员并给出团队范围。它由装配方用 teams.Require 注入（要求 owner / admin）：
// 审计包不 import teams（teams 已经 import 审计包来写日志），TeamScope 的唯一构造点仍在 teams。
type Authorizer func(ctx context.Context, r saas.Reader, tenantID, accountID, teamID string) (saas.TeamScope, error)

// Options 是审计读取与保留期的装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Authorize 把 teamId 变成已核过角色的 TeamScope（audit.list 要 owner / admin）。
	Authorize Authorizer
	// Partitions 建与删审计分区；nil = 不跑分区作业。
	Partitions Partitions
	// TrafficRetention 是流量桶的保留期（TRAFFIC_RETENTION_DAYS）；0 = 不跑 traffic.retention。
	TrafficRetention time.Duration
	Log              *obs.Logger
}

// Service 是审计读取与保留期服务。
type Service struct {
	c          *core.Context
	store      saas.Store
	authorize  Authorizer
	partitions Partitions
	traffic    time.Duration
	log        *obs.Logger
}

// New 建服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Service{
		c: options.Core, store: options.Store, authorize: options.Authorize, partitions: options.Partitions,
		traffic: options.TrafficRetention, log: log,
	}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程：audit.list（团队 owner / admin）与 me.securityLog（本人）。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{"audit.list": s.list, "me.securityLog": s.securityLog}
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

// pageInput 是两个列表共用的分页与时间范围入参。
type pageInput struct {
	FromMs int64  `json:"fromMs"`
	ToMs   int64  `json:"toMs"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func (p pageInput) query() (saas.AuditQuery, int, error) {
	q := saas.AuditQuery{FromMs: p.FromMs, ToMs: p.ToMs}
	if p.Cursor != "" {
		after, err := saas.DecodeAuditCursor(p.Cursor)
		if err != nil {
			return q, 0, core.Fail(gen.ErrBadRequest)
		}
		q.After = after
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	limit = min(limit, maxPageLimit)
	q.Limit = limit + 1 // 多取一行判断有没有下一页
	return q, limit, nil
}

// next 把多取的一行换成 nextCursor。
func next(rows []saas.AuditEntry, limit int) ([]saas.AuditEntry, any) {
	if len(rows) <= limit {
		return rows, nil
	}
	last := rows[limit-1]
	return rows[:limit], saas.AuditCursor{AtMs: last.AtMs, EventID: last.EventID}.Encode()
}

func orNil(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

type actorOut struct {
	Kind string  `json:"kind"`
	ID   *string `json:"id"`
}

type targetOut struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// eventOut 是 audit.list 的一行（协议包 auditEventSchema）：只有主体、动作、对象、时间、jti、IP 前缀。
type eventOut struct {
	EventID  string     `json:"eventId"`
	TeamID   *string    `json:"teamId"`
	Actor    actorOut   `json:"actor"`
	Action   string     `json:"action"`
	Target   *targetOut `json:"target"`
	JTI      *string    `json:"jti"`
	IPPrefix *string    `json:"ipPrefix"`
	AtMs     int64      `json:"atMs"`
}

// EventView 是审计行的线上形状；运营后台的最近动作也用它。
func EventView(e saas.AuditEntry) eventOut {
	out := eventOut{
		EventID: e.EventID, TeamID: orNil(e.TeamID), Actor: actorOut{Kind: e.ActorKind, ID: orNil(e.ActorID)},
		Action: e.Action, JTI: orNil(e.JTI), IPPrefix: orNil(e.IPPrefix), AtMs: e.AtMs,
	}
	if e.TargetKind != "" {
		out.Target = &targetOut{Kind: e.TargetKind, ID: e.TargetID}
	}
	return out
}

// list 是 audit.list：团队 owner / admin；筛选 fromMs（含）、toMs（不含）、actor、sourceId、action（前缀）。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		pageInput
		TeamID   string `json:"teamId"`
		Actor    string `json:"actor"`
		SourceID string `json:"sourceId"`
		Action   string `json:"action"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	scope, err := s.authorize(ctx, s.store.Read(), s.tenant(), account.AccountID, in.TeamID)
	if err != nil {
		return nil, err
	}
	q, limit, err := in.query()
	if err != nil {
		return nil, err
	}
	q.Actor, q.SourceID, q.ActionPrefix = in.Actor, in.SourceID, in.Action
	var rows []saas.AuditEntry
	err = s.store.Tx(ctx, func(tx saas.Tx) (err error) {
		rows, err = tx.Audit().ListTeam(ctx, scope, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	rows, cursor := next(rows, limit)
	events := make([]eventOut, 0, len(rows))
	for _, row := range rows {
		events = append(events, EventView(row))
	}
	return map[string]any{"events": events, "nextCursor": cursor}, nil
}

// securityEventOut 是 me.securityLog 的一行（协议包 securityEventSchema）。
type securityEventOut struct {
	EventID    string  `json:"eventId"`
	Action     string  `json:"action"`
	DeviceName *string `json:"deviceName"`
	IPPrefix   *string `json:"ipPrefix"`
	AtMs       int64   `json:"atMs"`
}

// securityLog 是 me.securityLog：本人的登录、新设备、改口令等；只读自己的行。
func (s *Service) securityLog(ctx context.Context, call *api.Call) (any, error) {
	var in pageInput
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	q, limit, err := in.query()
	if err != nil {
		return nil, err
	}
	var rows []saas.AuditEntry
	err = s.store.Tx(ctx, func(tx saas.Tx) (err error) {
		rows, err = tx.Audit().ListSecurity(ctx, s.tenant(), account.AccountID, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	rows, cursor := next(rows, limit)
	events := make([]securityEventOut, 0, len(rows))
	for _, row := range rows {
		events = append(events, securityEventOut{
			EventID: row.EventID, Action: row.Action, DeviceName: orNil(row.DeviceName), IPPrefix: orNil(row.IPPrefix), AtMs: row.AtMs,
		})
	}
	return map[string]any{"events": events, "nextCursor": cursor}, nil
}
