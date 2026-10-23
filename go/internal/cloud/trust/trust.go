// Package trust 是新设备信任的云端部分（设计 §4.7、能力设计 §11.4、契约 §31）：团队成员第一次从新设备连团队主机会停在
// device_pending，要有人从自己已受信的设备背书。**云端不担保设备，也不参与批准**——批准走端到端通道发到主机，
// 云端看不到也改不了背书内容、也没有校验码；这里只做两件事：把待信任请求（成员、设备名、平台、主机、过期时间）
// 登记下来给背书人看（trust.list），以及记下背书人的拒绝（trust.reject）。主机经 sources.trustRequest 上报、
// sources.trustResolve 回报（Authorization: Source <jws>）。
package trust

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 规则里的时长。
const (
	// MaxTTL 是待信任请求的有效期上限：主机报的 expiresAtMs 至多比现在晚这么久（库里的 CHECK 同样守）。
	MaxTTL = 24 * time.Hour
	// Cooldown 是同一 (主机, 设备) 被拒绝之后不再登记新请求、不再通知背书人的时长。
	Cooldown = time.Hour
	// retention 是已解决的请求留多久再清（要长于冷却期）。
	retention = 7 * 24 * time.Hour
	// maxListed 是 trust.list 一次最多答的请求数。
	maxListed = 500
)

// JobExpire 是每小时把到期的 pending 落库成 expired 的作业，顺带清走早已解决的行。
const JobExpire = "trust.expire"

// ActionReject 是 trust.reject 的审计动作。
const ActionReject = "trust.reject"

// Options 是信任服务的装配。
type Options struct {
	Core   *core.Context
	Store  saas.Store
	Limits *httpx.Limits
	Log    *obs.Logger
}

// Service 是信任服务。
type Service struct {
	c       *core.Context
	store   saas.Store
	limits  *httpx.Limits
	sources *core.Sources
	log     *obs.Logger
}

// New 建信任服务。
func New(options Options) *Service {
	log := options.Log
	if log == nil {
		log = obs.Nop()
	}
	return &Service{c: options.Core, store: options.Store, limits: options.Limits, sources: core.NewSources(options.Core), log: log}
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"sources.trustRequest": s.request,
		"sources.trustResolve": s.resolve,
		"trust.list":           s.list,
		"trust.reject":         s.reject,
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

// expire 是 trust.expire：到期的 pending 改 expired，并清走早已解决的行。可重放。
func (s *Service) expire(ctx context.Context, _ saas.Job) error {
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		nowMs := s.nowMs()
		expired, err := tx.Trust().ExpireDue(ctx, s.tenant(), nowMs)
		if err != nil {
			return err
		}
		removed, err := tx.Trust().GC(ctx, s.tenant(), nowMs-retention.Milliseconds())
		if err != nil {
			return err
		}
		if len(expired) > 0 || removed > 0 {
			s.log.Info("已处理到期的待信任请求", obs.Operation(JobExpire), obs.Count(len(expired)+removed))
		}
		return nil
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

// ---- 主机上报 ----

type requestOut struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
}

// request 是 sources.trustRequest（Source 认证）：
//
//	按源限流（trust.src）→ expiresAtMs 至多比现在晚 24 小时 → 锁主机 → 源在团队里、账号是团队成员且对这台主机有访问权
//	（否则 source_access_denied）→ deviceId 是该账号未撤销的设备（否则 not_found，设备名与平台取自 devices 表）→
//	同 (源, pendingId) 重报答同一个 requestId（pending 答 pending；已拒绝且仍在冷却期答 rejected；其余已解决的 409）→
//	同 (源, 设备) 最近一次被拒绝在冷却期内答 rejected（不建新请求）→ 先把同 (源, 设备) 过期的 pending 改 expired、
//	仍待处理的（主机换了条目 id）作废，再插新的 pending。
func (s *Service) request(ctx context.Context, call *api.Call) (any, error) {
	source, err := s.sources.AuthenticateSource(ctx, call.Authorization)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTrustSource, source.SourceID); err != nil {
		return nil, err
	}
	var in struct {
		PendingID   string `json:"pendingId"`
		AccountID   string `json:"accountId"`
		DeviceID    string `json:"deviceId"`
		ExpiresAtMs int64  `json:"expiresAtMs"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	if in.ExpiresAtMs <= nowMs || in.ExpiresAtMs > nowMs+MaxTTL.Milliseconds() {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	var out requestOut
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		out = requestOut{}
		space, err := tx.SpaceOf(ctx, s.tenant(), source.SourceID)
		if err != nil {
			return err
		}
		if space == nil || space.TeamID == "" || space.Revoked {
			return core.Fail(gen.ErrSourceAccessDenied) // 源已不在团队里（个人空间没有团队背书）
		}
		if err := tx.Hosts().LockSource(ctx, s.tenant(), source.SourceID); err != nil {
			return err
		}
		basis, err := tx.AccessBasis(ctx, s.tenant(), in.AccountID, source.SourceID)
		if err != nil {
			return err
		}
		if basis.TeamID != space.TeamID || basis.TeamRole == "" || basis.TeamDeleted || grants.Compose(basis, in.AccountID, nowMs).None() {
			return core.Fail(gen.ErrSourceAccessDenied)
		}
		device, err := tx.Ports().Devices.Get(ctx, s.tenant(), in.DeviceID)
		if err != nil {
			return err
		}
		if device == nil || device.Subject != in.AccountID || !store.IsLiveDevice(*device, nowMs) {
			return core.Fail(gen.ErrNotFound)
		}
		existing, err := tx.Trust().ByPending(ctx, s.tenant(), source.SourceID, in.PendingID)
		if err != nil {
			return err
		}
		reopen := ""
		if existing != nil {
			var reopenable bool
			out, reopenable, err = s.again(*existing, in.AccountID, in.DeviceID, nowMs)
			if err != nil || !reopenable {
				return err
			}
			reopen = existing.RequestID // 冷却已过的 rejected：同一个条目 id 重新开成 pending
		} else if last, err := tx.Trust().LastRejected(ctx, s.tenant(), source.SourceID, in.DeviceID); err != nil {
			return err
		} else if last != nil && last.ResolvedAtMs != nil && nowMs-*last.ResolvedAtMs < Cooldown.Milliseconds() {
			out = requestOut{RequestID: last.RequestID, Status: saas.TrustRejected}
			return nil
		}
		if err := tx.Trust().ExpireStale(ctx, s.tenant(), source.SourceID, in.DeviceID, nowMs); err != nil {
			return err
		}
		if _, err := tx.Trust().Supersede(ctx, s.tenant(), source.SourceID, in.DeviceID, nowMs); err != nil {
			return err
		}
		requestID := core.NewUUID()
		if reopen != "" {
			ok, err := tx.Trust().Reopen(ctx, s.tenant(), reopen, requestID, nowMs, in.ExpiresAtMs)
			if err == nil && !ok {
				err = core.Fail(gen.ErrConflict)
			}
			if err != nil {
				return err
			}
		} else if err := tx.Trust().Insert(ctx, s.tenant(), saas.TrustRequest{
			RequestID: requestID, SourceID: source.SourceID, PendingID: in.PendingID, TeamID: space.TeamID, AccountID: in.AccountID,
			DeviceID: in.DeviceID, DeviceName: device.Name, Platform: device.Platform, RequestedAtMs: nowMs, ExpiresAtMs: in.ExpiresAtMs,
		}); err != nil {
			return err
		}
		out = requestOut{RequestID: requestID, Status: saas.TrustPending}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// again 是同 (源, pendingId) 重报的答复：同一个请求，账号与设备必须一致（否则是主机在复用条目 id，409）。
// pending 未过期答 pending；rejected 在冷却期内答 rejected，冷却结束后返回 reopen = true（同一个条目 id 重新开成 pending，新的 requestId）；
// 其余已解决（批准、过期）的 409。
func (s *Service) again(existing saas.TrustRequest, accountID, deviceID string, nowMs int64) (out requestOut, reopen bool, err error) {
	if existing.AccountID != accountID || existing.DeviceID != deviceID {
		return requestOut{}, false, core.Fail(gen.ErrConflict)
	}
	switch existing.State {
	case saas.TrustPending:
		if existing.ExpiresAtMs > nowMs {
			return requestOut{RequestID: existing.RequestID, Status: saas.TrustPending}, false, nil
		}
	case saas.TrustRejected:
		if existing.ResolvedAtMs != nil && nowMs-*existing.ResolvedAtMs < Cooldown.Milliseconds() {
			return requestOut{RequestID: existing.RequestID, Status: saas.TrustRejected}, false, nil
		}
		return requestOut{}, true, nil
	}
	return requestOut{}, false, core.Fail(gen.ErrConflict)
}

// resolve 是 sources.trustResolve（Source 认证）：core 回报条目已批准（trusted）或过期（expired）；按 (源, pendingId) 找请求，
// 没有 404，已解决 409，否则改状态。
func (s *Service) resolve(ctx context.Context, call *api.Call) (any, error) {
	source, err := s.sources.AuthenticateSource(ctx, call.Authorization)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTrustSource, source.SourceID); err != nil {
		return nil, err
	}
	var in struct {
		PendingID string `json:"pendingId"`
		Result    string `json:"result"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	state := saas.TrustTrusted
	if in.Result == saas.TrustExpired {
		state = saas.TrustExpired
	} else if in.Result != saas.TrustTrusted {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Hosts().LockSource(ctx, s.tenant(), source.SourceID); err != nil {
			return err
		}
		request, err := tx.Trust().ByPending(ctx, s.tenant(), source.SourceID, in.PendingID)
		if err != nil {
			return err
		}
		if request == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if request.State != saas.TrustPending {
			return core.Fail(gen.ErrConflict)
		}
		ok, err := tx.Trust().Resolve(ctx, s.tenant(), request.RequestID, state, saas.TrustResolvedBySource, s.nowMs())
		if err == nil && !ok {
			err = core.Fail(gen.ErrConflict)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// ---- 背书人 ----

// isEndorser 按团队设置 deviceEndorsers / memberSelfEndorse 收窄谁是背书人（契约 §31；与 PendingTrust 的 SQL 条件同义）：
// 主机所有者（仍是 owner / admin / member）总是；团队 owner / admin 在 deviceEndorsers = owner_admin（缺省）时是；
// 请求者本人在 memberSelfEndorse 开着时是。guest 的请求只给主机所有者（core 的 authorizeEndorsement 只认 owner / admin / member），
// 所以后两条对 guest 请求不成立。
func isEndorser(view saas.TrustView, viewerID string) bool {
	role := view.ViewerRole
	if role == "" {
		return false
	}
	settings := teams.SettingsOf(saas.Team{Settings: view.TeamSettings})
	staff := func(r saas.TeamRole) bool { return r == saas.RoleOwner || r == saas.RoleAdmin || r == saas.RoleMember }
	switch {
	case viewerID == view.SourceOwnerID && staff(role):
		return true
	case view.RequesterRole == saas.RoleGuest:
		return false
	case settings.DeviceEndorsers == "owner_admin" && (role == saas.RoleOwner || role == saas.RoleAdmin):
		return true
	case settings.MemberSelfEndorse && viewerID == view.AccountID && staff(view.RequesterRole):
		return true
	}
	return false
}

type memberOut struct {
	AccountID   string  `json:"accountId"`
	Email       *string `json:"email"`
	DisplayName string  `json:"displayName"`
}

type deviceOut struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// requestView 是 trustRequestSchema 的线上形状：没有校验码。
type requestView struct {
	RequestID     string    `json:"requestId"`
	SourceID      string    `json:"sourceId"`
	SourceName    string    `json:"sourceName"`
	TeamID        *string   `json:"teamId"`
	Member        memberOut `json:"member"`
	Device        deviceOut `json:"device"`
	RequestedAtMs int64     `json:"requestedAtMs"`
	ExpiresAtMs   int64     `json:"expiresAtMs"`
}

func viewOf(v saas.TrustView) requestView {
	out := requestView{
		RequestID: v.RequestID, SourceID: v.SourceID, SourceName: v.SourceName,
		Member: memberOut{AccountID: v.AccountID, DisplayName: v.MemberName}, Device: deviceOut{Name: v.DeviceName, Platform: v.Platform},
		RequestedAtMs: v.RequestedAtMs, ExpiresAtMs: v.ExpiresAtMs,
	}
	if v.TeamID != "" {
		team := v.TeamID
		out.TeamID = &team
	}
	if v.MemberEmail != "" {
		email := v.MemberEmail
		out.Member.Email = &email
	}
	return out
}

// list 是 trust.list：调用者能背书的待处理请求（所在团队里，按团队设置收窄），给 space 只列那个空间
// （团队：不是成员 404；个人空间没有团队背书，答空）。
func (s *Service) list(ctx context.Context, call *api.Call) (any, error) {
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	teamID := ""
	if raw, ok := call.Input["space"].(map[string]any); ok {
		kind, _ := raw["kind"].(string)
		if kind == saas.SpacePersonal {
			return map[string]any{"requests": []requestView{}}, nil
		}
		teamID, _ = raw["teamId"].(string)
		if _, err := teams.Require(ctx, s.store.Read(), s.tenant(), account.AccountID, teamID, teams.MinAny); err != nil {
			return nil, err
		}
	}
	views, err := s.store.Read().PendingTrust(ctx, s.tenant(), account.AccountID, teamID, s.nowMs(), maxListed)
	if err != nil {
		return nil, err
	}
	out := []requestView{}
	for _, v := range views {
		if isEndorser(v, account.AccountID) {
			out = append(out, viewOf(v))
		}
	}
	return map[string]any{"requests": out}, nil
}

// reject 是 trust.reject：背书人才行（否则 team_role_insufficient；不是团队成员或请求不存在 404）；只记拒绝——请求作废、
// 开始冷却，不通知主机，设备继续停在 device_pending。已拒绝的再拒绝是空操作；已批准或已过期的 409。
func (s *Service) reject(ctx context.Context, call *api.Call) (any, error) {
	requestID, _ := call.Input["requestId"].(string)
	account, err := s.me(ctx, call)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		probe, err := tx.TrustView(ctx, s.tenant(), requestID, account.AccountID)
		if err != nil {
			return err
		}
		if probe == nil || probe.ViewerRole == "" {
			return core.Fail(gen.ErrNotFound)
		}
		// 团队行 → 请求行；锁之后按最新的团队角色与设置再判一次。
		if _, err := teams.Lock(ctx, tx, s.tenant(), account.AccountID, probe.TeamID, teams.MinAny); err != nil {
			return err
		}
		view, err := tx.TrustView(ctx, s.tenant(), requestID, account.AccountID)
		if err != nil {
			return err
		}
		if view == nil {
			return core.Fail(gen.ErrNotFound)
		}
		if !isEndorser(*view, account.AccountID) {
			return core.Fail(gen.ErrTeamRoleInsufficient)
		}
		request, err := tx.Trust().Lock(ctx, s.tenant(), requestID)
		if err != nil {
			return err
		}
		if request == nil {
			return core.Fail(gen.ErrNotFound)
		}
		nowMs := s.nowMs()
		switch {
		case request.State == saas.TrustRejected:
			return nil
		case request.State != saas.TrustPending, request.ExpiresAtMs <= nowMs:
			return core.Fail(gen.ErrConflict)
		}
		if ok, err := tx.Trust().Resolve(ctx, s.tenant(), requestID, saas.TrustRejected, account.AccountID, nowMs); err != nil || !ok {
			if err == nil {
				err = core.Fail(gen.ErrConflict)
			}
			return err
		}
		return audit.Write(ctx, tx, s.tenant(), nowMs, saas.AuditEntry{
			TeamID: request.TeamID, ActorKind: saas.ActorAccount, ActorID: account.AccountID, Action: ActionReject,
			TargetKind: "trust_request", TargetID: requestID, SourceID: request.SourceID, IPPrefix: audit.IPPrefix(call.IP),
		})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
