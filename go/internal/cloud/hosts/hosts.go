// Package hosts 是主机的过程（设计 §4.5）：登记到空间（个人空间与团队空间，含 core 版本门与主机配额）、
// 目录（筛选与游标分页）、改名 / 标签 / 设置、撤销，以及隧道令牌（sources.heartbeatHint）。
//
// 可见性在存储层的目录查询里（saas.HostFilter.ViewerID），S5a 只有两种来路：主机所有者，
// 以及团队主机的团队 owner / admin；授予带来的可见性随 S5b 并进同一条查询。
package hosts

import (
	"context"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 主机的审计动作。
const (
	ActionRegister = "host.register"
	ActionUpdate   = "host.update"
	ActionRevoke   = "host.revoke"
)

// Directory 是主机目录的可见性来源（SaaS 实现方案 §4.2）：只返回 filter.ViewerID 此刻能看到的主机，
// 可见性（所有者、团队角色、授予）由实现决定；r 可以是只读连接，也可以是事务。grants.Policy 实现它。
type Directory interface {
	Hosts(ctx context.Context, r saas.Reader, filter saas.HostFilter) ([]saas.Host, error)
}

// Options 是主机服务的装配。
type Options struct {
	Core   *core.Context
	Store  saas.Store
	Limits *httpx.Limits
	// Relay 查在线、签发索引、撤销已签令牌、踢人与关隧道（经 tx.AfterCommit，提交后才发）。
	Relay relayctl.Control
	// Directory 是目录的可见性来源；缺省只看所有者与团队角色（没有授予）——生产装配传 grants.Policy。
	Directory Directory
	// RequireEmailVerified 为真时未验证邮箱的账号不能要注册令牌（设计 §4.3）。
	RequireEmailVerified bool
	Log                  *obs.Logger
	// Guests 答链接访客（guest:<linkId>:… 主体）此刻仍站得住时链接指向的主机（me.sources 用）；生产装配传 grants.Policy。
	// nil = 访客调 me.sources 答 forbidden。
	Guests core.GuestHosts
}

// Service 是主机服务。
type Service struct {
	c        *core.Context
	store    saas.Store
	limits   *httpx.Limits
	relay    relayctl.Control
	dir      Directory
	sources  *core.Sources
	verified bool
	log      *obs.Logger
	guests   core.GuestHosts
}

// New 建主机服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	dir := options.Directory
	if dir == nil {
		dir = storeDirectory{now: options.Core.NowMs, tenant: options.Core.TenantID}
	}
	return &Service{
		c: options.Core, store: options.Store, limits: options.Limits, relay: options.Relay, dir: dir,
		sources: core.NewSources(options.Core), verified: options.RequireEmailVerified, log: log, guests: options.Guests,
	}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"sources.registrationToken": s.registrationToken,
		"sources.register":          s.register,
		"sources.heartbeatHint":     s.heartbeatHint,
		"sources.list":              s.list,
		"sources.get":               s.get,
		"sources.update":            s.update,
		"sources.revoke":            s.revoke,
		"me.sources":                s.meSources,
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

// record 在业务事务里写一条主机审计（团队主机记团队，个人空间的 team_id 为空）。
func (s *Service) record(ctx context.Context, tx saas.Tx, ip, actor, teamID, sourceID, action string) error {
	return audit.Write(ctx, tx, s.tenant(), s.nowMs(), saas.AuditEntry{
		TeamID: teamID, ActorKind: saas.ActorAccount, ActorID: actor, Action: action,
		TargetKind: "host", TargetID: sourceID, SourceID: sourceID, IPPrefix: audit.IPPrefix(ip),
	})
}

// hostLimit 是空间此刻的主机额度（计划 + overrides）。
func (s *Service) hostLimit(ctx context.Context, r saas.Reader, teamID, accountID string) (int, error) {
	if teamID != "" {
		e, err := r.Entitlement(ctx, s.tenant(), saas.SpaceTeam, teamID)
		if err != nil {
			return 0, err
		}
		if e == nil {
			return quota.ForTeam(saas.FreePlan, nil).Hosts, nil
		}
		return quota.ForTeam(e.Plan, e.Overrides).Hosts, nil
	}
	e, err := r.Entitlement(ctx, s.tenant(), saas.SpacePersonal, accountID)
	if err != nil {
		return 0, err
	}
	if e == nil {
		return quota.ForPersonal(saas.FreePlan, nil).Hosts, nil
	}
	return quota.ForPersonal(e.Plan, e.Overrides).Hosts, nil
}

// spaceIn 是入参里的 space。
type spaceIn struct {
	Kind   string `json:"kind"`
	TeamID string `json:"teamId"`
}

// spaceOf 取入参里的 space；没给 present 为 false。
func spaceOf(call *api.Call) (space spaceIn, present bool, err error) {
	raw, ok := call.Input["space"]
	if !ok || raw == nil {
		return spaceIn{}, false, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return spaceIn{}, false, err
	}
	if err := json.Unmarshal(data, &space); err != nil {
		return spaceIn{}, false, core.Fail(gen.ErrBadRequest)
	}
	return space, true, nil
}

// storeDirectory 是不带授予的缺省目录：直接问存储（时钟取控制核的）。
type storeDirectory struct {
	now    func() int64
	tenant string
}

func (d storeDirectory) Hosts(ctx context.Context, r saas.Reader, filter saas.HostFilter) ([]saas.Host, error) {
	filter.NowMs = d.now()
	return r.ListHosts(ctx, d.tenant, filter)
}
