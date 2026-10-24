// Package billing 是计划与用量的过程（设计 §4.10，契约 §26）：billing.plan 与 billing.usage，按空间。
// M1 只有 Free 计划，没有支付；checkout、portal、invoices、webhook 仍答 501（M2）。
// 团队空间的用量与 teams.usage 用同一个统计函数（teams.TeamUsage）。
package billing

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const dayMs = int64(24 * time.Hour / time.Millisecond)

// Options 是计划与用量服务的装配。
type Options struct {
	Core  *core.Context
	Store saas.Store
	// Relay 给团队用量的同时连接数。
	Relay relayctl.Control
	Log   *obs.Logger
}

// Service 是计划与用量服务。
type Service struct {
	c     *core.Context
	store saas.Store
	live  *teams.Live
}

// New 建服务。
func New(options Options) *Service {
	return &Service{c: options.Core, store: options.Store, live: teams.NewLive(options.Relay, options.Log)}
}

func (s *Service) tenant() string { return s.c.TenantID }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{"billing.plan": s.plan, "billing.usage": s.usage}
	for name, handler := range handlers {
		handlers[name] = func(ctx context.Context, call *api.Call) (any, error) {
			out, err := handler(ctx, call)
			return out, httpx.MapError(err)
		}
	}
	return handlers
}

// spaceOf 取入参里的空间：个人空间是调用者本人的；团队空间按 min 收窄（读：owner、admin、billing；写用 teams.MinBilling；别的角色 403，不是成员 404）。
type space struct {
	kind   string
	teamID string
	access teams.Access
	// accountID 是调用者（个人空间的所有者）。
	accountID string
}

func (s *Service) spaceOf(ctx context.Context, call *api.Call, min teams.Min) (space, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return space{}, err
	}
	if err := core.RequireOwner(caller); err != nil {
		return space{}, err
	}
	account, err := s.store.Read().Account(ctx, s.tenant(), caller.AccountID)
	if err != nil {
		return space{}, err
	}
	if account == nil || account.Status != saas.AccountActive {
		return space{}, core.Fail(gen.ErrUnauthenticated)
	}
	raw, _ := call.Input["space"].(map[string]any)
	switch raw["kind"] {
	case saas.SpacePersonal:
		return space{kind: saas.SpacePersonal, accountID: account.AccountID}, nil
	case saas.SpaceTeam:
		id, _ := raw["teamId"].(string)
		access, err := teams.Require(ctx, s.store.Read(), s.tenant(), account.AccountID, id, min)
		if err != nil {
			return space{}, err
		}
		return space{kind: saas.SpaceTeam, teamID: id, access: access, accountID: account.AccountID}, nil
	}
	return space{}, core.Fail(gen.ErrBadRequest)
}

// wire 是答案里的 space 字段。
func (sp space) wire() map[string]any {
	if sp.kind == saas.SpaceTeam {
		return map[string]any{"kind": saas.SpaceTeam, "teamId": sp.teamID}
	}
	return map[string]any{"kind": saas.SpacePersonal}
}

// spaceID 是 entitlements 里的空间 id。
func (sp space) spaceID() string {
	if sp.kind == saas.SpaceTeam {
		return sp.teamID
	}
	return sp.accountID
}

// plan 是 billing.plan：计划代号、状态、席位、本周期结束（M1 周期 = UTC 自然月）。没有 entitlements 行 = free / active。
func (s *Service) plan(ctx context.Context, call *api.Call) (any, error) {
	sp, err := s.spaceOf(ctx, call, teams.MinBillingRead)
	if err != nil {
		return nil, err
	}
	reader := s.store.Read()
	entitlement, err := reader.Entitlement(ctx, s.tenant(), sp.kind, sp.spaceID())
	if err != nil {
		return nil, err
	}
	out := map[string]any{"space": sp.wire(), "plan": saas.FreePlan, "status": "active", "seats": 1}
	if entitlement != nil {
		out["plan"], out["status"] = entitlement.Plan, entitlement.Status
	}
	if sp.kind == saas.SpaceTeam {
		var seats saas.Seats
		err := s.store.Tx(ctx, func(tx saas.Tx) (err error) {
			seats, err = tx.Teams().Seats(ctx, sp.access.Scope)
			return err
		})
		if err != nil {
			return nil, err
		}
		out["seats"] = seats.Members
	}
	_, end, err := teams.MonthOf("", time.UnixMilli(s.c.NowMs()))
	if err != nil {
		return nil, err
	}
	out["periodEndsAtMs"] = end.UnixMilli()
	return out, nil
}

// usage 是 billing.usage：当前（或指定）周期的用量对照额度；granularity = day 时另给本周期每天一条的中继字节。
func (s *Service) usage(ctx context.Context, call *api.Call) (any, error) {
	sp, err := s.spaceOf(ctx, call, teams.MinBillingRead)
	if err != nil {
		return nil, err
	}
	period, _ := call.Input["period"].(string)
	start, end, err := teams.MonthOf(period, time.UnixMilli(s.c.NowMs()))
	if err != nil {
		return nil, err
	}
	granularity, _ := call.Input["granularity"].(string)
	withDays := granularity == "day"
	var (
		entries []teams.UsageEntry
		days    []saas.DayBytes
	)
	if sp.kind == saas.SpaceTeam {
		entries, days, err = teams.TeamUsage(ctx, s.store, s.live, s.tenant(), sp.access, start, end, withDays)
	} else {
		entries, days, err = s.personalUsage(ctx, sp.accountID, start, end, withDays)
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"space": sp.wire(), "period": start.Format("2006-01"), "usage": entries}
	if withDays {
		out["series"] = fill(days, start, end)
	}
	return out, nil
}

// personalUsage 是个人空间的用量：主机数、名下未删除的团队数、流量。
func (s *Service) personalUsage(ctx context.Context, accountID string, start, end time.Time, withDays bool) (entries []teams.UsageEntry, days []saas.DayBytes, err error) {
	var (
		hosts, owned int
		bytes        int64
		limit        = quota.ForPersonal(saas.FreePlan, nil)
	)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		entitlement, err := tx.Entitlement(ctx, s.tenant(), saas.SpacePersonal, accountID)
		if err != nil {
			return err
		}
		if entitlement != nil {
			limit = quota.ForPersonal(entitlement.Plan, entitlement.Overrides)
		}
		if hosts, err = tx.Hosts().CountPersonal(ctx, s.tenant(), accountID, ""); err != nil {
			return err
		}
		if owned, err = tx.OwnedTeamCount(ctx, s.tenant(), accountID); err != nil {
			return err
		}
		if bytes, err = tx.PersonalTraffic(ctx, s.tenant(), accountID, start.UnixMilli(), end.UnixMilli()); err != nil {
			return err
		}
		if withDays {
			days, err = tx.PersonalTrafficDays(ctx, s.tenant(), accountID, start.UnixMilli(), end.UnixMilli())
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return []teams.UsageEntry{
		{Metric: quota.Hosts, Value: int64(hosts), Limit: teams.LimitOf(int64(limit.Hosts))},
		{Metric: quota.Teams, Value: int64(owned), Limit: teams.LimitOf(int64(limit.Teams))},
		{Metric: quota.Traffic, Value: bytes, Limit: teams.LimitOf(limit.TrafficBytes)},
	}, days, nil
}

type seriesPoint struct {
	StartMs    int64 `json:"startMs"`
	RelayBytes int64 `json:"relayBytes"`
}

// fill 把有流量的天补成「周期内每天一条」，没有流量的天是 0（startMs 是当天 UTC 零点）。
func fill(days []saas.DayBytes, start, end time.Time) []seriesPoint {
	byDay := make(map[int64]int64, len(days))
	for _, d := range days {
		byDay[d.StartMs] = d.Bytes
	}
	out := []seriesPoint{}
	for at := start.UnixMilli(); at < end.UnixMilli(); at += dayMs {
		out = append(out, seriesPoint{StartMs: at, RelayBytes: byDay[at]})
	}
	return out
}
