package teams

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/events"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// cleanName 去掉首尾空白；空名字不合格。
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", core.Fail(gen.ErrBadRequest)
	}
	return name, nil
}

// create 是 teams.create：邮箱已验证 → 配额 teams（锁账号行）→ slug 唯一 → 插团队、owner 成员、
// entitlements（free）→ 提交后经 relayctl 写团队并发额度 → 审计。
func (s *Service) create(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
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
	name, err := cleanName(in.Name)
	if err != nil {
		return nil, err
	}
	settings, err := json.Marshal(DefaultSettings())
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	team := saas.Team{
		TeamID: core.NewUUID(), Slug: in.Slug, Name: name, Settings: settings, CreatedBy: account.AccountID, CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		// 个人账号能建的团队数：锁账号行后计数，两个同时建团队的请求不会一起越过上限。
		if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
			return err
		}
		e, err := tx.Entitlement(ctx, s.tenant(), saas.SpacePersonal, account.AccountID)
		if err != nil {
			return err
		}
		limit := quota.ForPersonal(saas.FreePlan, nil)
		if e != nil {
			limit = quota.ForPersonal(e.Plan, e.Overrides)
		}
		owned, err := tx.OwnedTeamCount(ctx, s.tenant(), account.AccountID)
		if err != nil {
			return err
		}
		if err := quota.Check(quota.Teams, owned, limit.Teams); err != nil {
			return err
		}
		if err := tx.Teams().Create(ctx, s.tenant(), team, account.AccountID); err != nil {
			return err
		}
		s.afterSetQuota(tx, team.TeamID, quota.ForTeam(saas.FreePlan, nil).Concurrent)
		s.events.MemberChanged(tx, team.TeamID, account.AccountID, events.MemberAdded)
		return s.record(ctx, tx, call, account.AccountID, team.TeamID, ActionCreate, "team", team.TeamID)
	})
	if err != nil {
		return nil, err
	}
	return teamView(team, teamscope.Owner, saas.FreePlan), nil
}

// list 是 teams.list：自己所属的团队；恢复期内的只给 owner（只有 owner 能恢复）。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	accesses, err := s.store.Read().TeamsOf(ctx, s.tenant(), account.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]teamOut, 0, len(accesses))
	for _, a := range accesses {
		if a.Team.Deleted() && a.Role != teamscope.Owner {
			continue
		}
		out = append(out, teamView(a.Team, a.Role, a.Plan))
	}
	return map[string]any{"teams": out}, nil
}

// get 是 teams.get：任一角色。
func (s *Service) get(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	access, err := Require(ctx, s.store.Read(), s.tenant(), account.AccountID, teamID(call), MinAny)
	if err != nil {
		return nil, err
	}
	return teamView(access.Team, access.Role, access.Plan), nil
}

// update 是 teams.update：改名与头像（admin）。
func (s *Service) update(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Name      *string `json:"name"`
		AvatarURL *string `json:"avatarUrl"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	_, hasAvatar := call.Input["avatarUrl"]
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	var avatar *string
	if hasAvatar {
		empty := ""
		avatar = &empty
		if in.AvatarURL != nil {
			if err := checkAvatar(*in.AvatarURL); err != nil {
				return nil, err
			}
			avatar = in.AvatarURL
		}
	}
	var out teamOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinAdmin)
		if err != nil {
			return err
		}
		name := access.Team.Name
		if in.Name != nil {
			if name, err = cleanName(*in.Name); err != nil {
				return err
			}
		}
		nowMs := s.nowMs()
		if err := tx.Teams().Rename(ctx, access.Scope, name, avatar, nowMs); err != nil {
			return err
		}
		team, err := tx.Teams().Get(ctx, access.Scope)
		if err != nil {
			return err
		}
		out = teamView(*team, access.Role, access.Plan)
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionUpdate, "team", access.Scope.TeamID())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkAvatar：头像是 http(s) 地址，不带账号信息。
func checkAvatar(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return core.Fail(gen.ErrBadRequest)
	}
	return nil
}

// delete 是 teams.delete（owner）：有在线主机答 409 conflict；置删除与清除时刻（30 天），撤销团队全部已签令牌，
// 插 team.purge 作业（run_at = purge_at，dedupe = teamId）。
func (s *Service) delete(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	var purgeAt int64
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinOwner)
		if err != nil {
			return err
		}
		hosts, err := tx.Teams().HostIDs(ctx, access.Scope)
		if err != nil {
			return err
		}
		for _, sourceID := range hosts {
			if _, online, err := s.relay.Route(ctx, sourceID); err != nil {
				return err
			} else if online {
				return core.Fail(gen.ErrConflict)
			}
		}
		nowMs := s.nowMs()
		purgeAt = nowMs + RecoveryPeriod.Milliseconds()
		if err := tx.Teams().SetDeleted(ctx, access.Scope, nowMs, purgeAt); err != nil {
			return err
		}
		if err := enqueuePurge(ctx, tx, s.tenant(), access.Scope.TeamID(), purgeAt, nowMs); err != nil {
			return err
		}
		s.afterRevokeHosts(tx, hosts, "team_deleted", false)
		s.events.TeamRemoved(tx, access.Scope.TeamID())
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionDelete, "team", access.Scope.TeamID())
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"purgeAtMs": purgeAt}, nil
}

// restore 是 teams.restore（owner）：只在恢复期内；清两个时间、删 team.purge 作业。没在恢复期内的团队原样答。
func (s *Service) restore(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	var out teamOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinOwner, RequireOptions{AllowDeleted: true})
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		team := access.Team
		if team.Deleted() {
			// 清除时刻已过、作业还没来得及跑：不再能恢复。
			if team.PurgeAtMs != nil && *team.PurgeAtMs <= nowMs {
				return core.Fail(gen.ErrTeamDeleted)
			}
			// 恢复等于重新拥有一个团队：按个人账号的 teams 额度核对（团队→账号的加锁顺序），
			// 否则删 A、建 B、再恢复 A 就绕过了 Free 的 1 个团队。
			if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
				return err
			}
			e, err := tx.Entitlement(ctx, s.tenant(), saas.SpacePersonal, account.AccountID)
			if err != nil {
				return err
			}
			limit := quota.ForPersonal(saas.FreePlan, nil)
			if e != nil {
				limit = quota.ForPersonal(e.Plan, e.Overrides)
			}
			owned, err := tx.OwnedTeamCount(ctx, s.tenant(), account.AccountID)
			if err != nil {
				return err
			}
			if err := quota.Check(quota.Teams, owned, limit.Teams); err != nil {
				return err
			}
			if err := tx.Teams().Restore(ctx, access.Scope, nowMs); err != nil {
				return err
			}
			if err := tx.Jobs().Cancel(ctx, s.tenant(), JobPurge, access.Scope.TeamID()); err != nil {
				return err
			}
			team.DeletedAtMs, team.PurgeAtMs = nil, nil
			s.events.TeamRestored(tx, access.Scope.TeamID())
			if err := s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionRestore, "team", access.Scope.TeamID()); err != nil {
				return err
			}
		}
		out = teamView(team, access.Role, access.Plan)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// settings 是 teams.settings（owner、admin、member）。
func (s *Service) settings(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	access, err := Require(ctx, s.store.Read(), s.tenant(), account.AccountID, teamID(call), MinStaff)
	if err != nil {
		return nil, err
	}
	return SettingsOf(access.Team), nil
}

// settingsUpdate 是 teams.settingsUpdate（admin）：给了的字段覆盖，没给的不动；关 allowGuests 不自动移除已有 guest。
func (s *Service) settingsUpdate(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{}
	for key, value := range call.Input {
		if key != "teamId" {
			patch[key] = value
		}
	}
	var out Settings
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		access, err := Lock(ctx, tx, s.tenant(), account.AccountID, teamID(call), MinAdmin)
		if err != nil {
			return err
		}
		current := map[string]any{}
		if err := json.Unmarshal(access.Team.Settings, &current); err != nil {
			return err
		}
		for key, value := range patch {
			current[key] = value
		}
		merged, err := json.Marshal(current)
		if err != nil {
			return err
		}
		out = DefaultSettings()
		if err := json.Unmarshal(merged, &out); err != nil {
			return core.Fail(gen.ErrBadRequest)
		}
		if out.AllowedEmailDomains == nil {
			out.AllowedEmailDomains = []string{}
		}
		normalized, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if err := tx.Teams().SetSettings(ctx, access.Scope, normalized, s.nowMs()); err != nil {
			return err
		}
		return s.record(ctx, tx, call, account.AccountID, access.Scope.TeamID(), ActionSettings, "team", access.Scope.TeamID())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UsageEntry 是 teams.usage 与 billing.usage 的一项（协议包 usageEntrySchema）；limit 为 null 表示不限。
type UsageEntry struct {
	Metric string `json:"metric"`
	Value  int64  `json:"value"`
	Limit  *int64 `json:"limit"`
}

// LimitOf 把额度换成线上的 limit：不限（负数）是 null。
func LimitOf(limit int64) *int64 {
	if limit < 0 {
		return nil
	}
	return &limit
}

// MonthOf 解 period（YYYY-MM，空 = now 所在月），答该月 UTC 的起止；不合格式答 400。
func MonthOf(period string, now time.Time) (start, end time.Time, err error) {
	now = now.UTC()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if period != "" {
		if start, err = time.Parse("2006-01", period); err != nil {
			return start, end, core.Fail(gen.ErrBadRequest)
		}
	}
	return start, start.AddDate(0, 1, 0), nil
}

// TeamUsage 是团队在 [start, end) 的用量，对照当前额度：主机、成员、guest、同时连接、流量。
// teams.usage 与 billing.usage 用同一个统计函数；同时连接只有节点知道，控制面只展示额度（value 为 0）。
// withDays 为真时另给流量的按天序列。
func TeamUsage(ctx context.Context, store saas.Store, tenantID string, access Access, start, end time.Time, withDays bool) (entries []UsageEntry, days []saas.DayBytes, err error) {
	var (
		seats saas.Seats
		hosts int
		bytes int64
		lim   quota.Limits
	)
	// 团队范围的统计方法在 TeamRepo 上，所以开一个事务来读。
	err = store.Tx(ctx, func(tx saas.Tx) error {
		var err error
		if seats, err = tx.Teams().Seats(ctx, access.Scope); err != nil {
			return err
		}
		if hosts, bytes, err = tx.Teams().Usage(ctx, access.Scope, start.UnixMilli(), end.UnixMilli()); err != nil {
			return err
		}
		if lim, err = limits(ctx, tx, tenantID, access); err != nil {
			return err
		}
		if withDays {
			days, err = tx.Teams().TrafficDays(ctx, access.Scope, start.UnixMilli(), end.UnixMilli())
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return []UsageEntry{
		{quota.Hosts, int64(hosts), LimitOf(int64(lim.Hosts))},
		{quota.Members, int64(seats.Members), LimitOf(int64(lim.Members))},
		{quota.Guests, int64(seats.Guests), LimitOf(int64(lim.Guests))},
		{quota.Concurrent, 0, LimitOf(int64(lim.Concurrent))},
		{quota.Traffic, bytes, LimitOf(lim.TrafficBytes)},
	}, days, nil
}

// usage 是 teams.usage（owner、admin、member、billing）：主机、成员、guest、同时连接、本月流量，对照当前额度。
func (s *Service) usage(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	access, err := Require(ctx, s.store.Read(), s.tenant(), account.AccountID, teamID(call), MinDirectory)
	if err != nil {
		return nil, err
	}
	period, _ := call.Input["period"].(string)
	start, end, err := MonthOf(period, time.UnixMilli(s.nowMs()))
	if err != nil {
		return nil, err
	}
	entries, _, err := TeamUsage(ctx, s.store, s.tenant(), access, start, end, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"teamId": access.Scope.TeamID(), "period": start.Format("2006-01"), "usage": entries}, nil
}
