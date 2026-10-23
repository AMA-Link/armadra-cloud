package grants

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// maxSignAttempts 是签发遇到并发收窄时重新合成的次数上限。
const maxSignAttempts = 3

func (s *Service) stage(name string) {
	if s.hook != nil {
		s.hook(name)
	}
}

// assertion 是 sources.assertion（设计 §4.6）：事故开关与限流 → 主机状态 → 有效权限与版本门（Policy.Intent）→
// 签断言与中继令牌 → 登记签发索引 → 再读库复核 → 审计。
//
// 顺序是「签令牌 → 登记签发索引 → 再读库复核」（§4.5 的 S5a 注记）：撤销在库提交之后才撤索引里的令牌，所以复核看到
// 权限未变，之后的撤销一定撤得到这两枚；复核发现已收窄（或主机已撤销）就自己撤掉自己签的，并按新权限重签或答错误——
// 与并发的授予收窄、成员降级、主机撤销交错都不漏撤。
func (s *Service) assertion(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		SourceID string            `json:"sourceId"`
		Device   *core.DeviceInput `json:"device"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if caller.AccountID == "" {
		// 链接访客（guest:<linkId>:…）随分享一批（S6）走链接路径；S5b 的云端没有这类会话。
		return nil, core.Fail(gen.ErrSourceAccessDenied)
	}
	incident, err := s.store.Read().Incident(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	if incident.AssertionsPaused {
		return nil, core.Fail(gen.ErrUnavailable)
	}
	if err := s.limits.Check(ctx, httpx.LimitAssertAcct, caller.AccountID); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxSignAttempts; attempt++ {
		grant, retry, err := s.issue(ctx, call.IP, caller, in.SourceID, in.Device)
		if err != nil || !retry {
			return grant, err
		}
	}
	// 一再被并发的收窄打断：让客户端稍后重试，不签一个已知过期的断言。
	return nil, core.Fail(gen.ErrUnavailable)
}

// issue 做一次签发；retry 为真表示复核时发现权限在签发期间收窄了（已撤掉自己签的令牌）。
func (s *Service) issue(ctx context.Context, ip string, caller core.Caller, sourceID string, device *core.DeviceInput) (grant core.Grant, retry bool, err error) {
	source, err := s.c.Ports.Sources.Get(ctx, s.tenant(), sourceID)
	if err != nil {
		return core.Grant{}, false, err
	}
	if source != nil && source.RevokedAtMs != nil {
		return core.Grant{}, false, core.Fail(gen.ErrSourceRevoked)
	}
	if _, ok, err := s.policy.Access(ctx, caller, sourceID); err != nil || !ok || source == nil {
		if err == nil {
			err = core.Fail(gen.ErrSourceAccessDenied)
		}
		return core.Grant{}, false, err
	}
	record, err := s.c.Ports.Devices.Get(ctx, s.tenant(), caller.DeviceID)
	if err != nil {
		return core.Grant{}, false, err
	}
	if record == nil {
		return core.Grant{}, false, core.Fail(gen.ErrUnauthenticated)
	}
	view, err := s.policy.intent(ctx, caller.AccountID, *source)
	if err != nil {
		return core.Grant{}, false, err
	}
	view.extra.Device = device
	grant, err = s.sources.Grant(ctx, *source, *record, view.extra)
	if err != nil {
		return core.Grant{}, false, err
	}
	s.stage("signed")
	issued := []relayctl.Issued{
		{Subject: caller.AccountID, SourceID: sourceID, DeviceID: record.DeviceID, JTI: grant.AssertionJTI, ExpMs: grant.AssertionExpiresAtMs},
		{Subject: caller.AccountID, SourceID: sourceID, DeviceID: record.DeviceID, JTI: grant.RelayTokenJTI, ExpMs: grant.RelayTokenExpiresAtMs},
	}
	for _, entry := range issued {
		if err := s.relay.IndexIssued(ctx, entry); err != nil {
			s.undo(ctx, issued)
			return core.Grant{}, false, err
		}
	}
	s.stage("indexed")
	// 复核：主机还在、权限没有比签发时更窄。
	fresh, err := s.c.Ports.Sources.Get(ctx, s.tenant(), sourceID)
	if err != nil {
		s.undo(ctx, issued)
		return core.Grant{}, false, err
	}
	if fresh == nil || fresh.RevokedAtMs != nil {
		s.undo(ctx, issued)
		return core.Grant{}, false, core.Fail(gen.ErrSourceRevoked)
	}
	eff, basis, err := s.effectiveOf(ctx, s.store.Read(), caller.AccountID, sourceID)
	if err != nil {
		s.undo(ctx, issued)
		return core.Grant{}, false, err
	}
	if eff.None() {
		s.undo(ctx, issued)
		return core.Grant{}, false, core.Fail(gen.ErrSourceAccessDenied)
	}
	if Narrowed(view.eff, eff) || basis.TeamRole != view.teamRole {
		s.undo(ctx, issued)
		return core.Grant{}, true, nil
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		return s.record(ctx, tx, ip, saas.ActorAccount, caller.AccountID, basis.TeamID, sourceID, ActionAssertion, "host", sourceID, grant.AssertionJTI)
	})
	if err != nil {
		s.undo(ctx, issued)
		return core.Grant{}, false, err
	}
	return grant, false, nil
}

// undo 撤掉自己刚签出的令牌（按 jti，只撤这几枚）；撤销失败只记日志。
func (s *Service) undo(ctx context.Context, issued []relayctl.Issued) {
	ctx = context.WithoutCancel(ctx)
	for _, entry := range issued {
		if _, err := s.relay.RevokeWhere(ctx, relayctl.Filter{JTI: entry.JTI}); err != nil {
			s.log.Warn("撤销自己签的令牌失败", obs.Operation("grants.undo"), obs.SourceID(entry.SourceID), obs.Error(err))
		}
	}
}
