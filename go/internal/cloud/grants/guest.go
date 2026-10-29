package grants

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/hosts"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/teams"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// guestAssertion 是链接访客（guest:<linkId>:… 主体）的 sources.assertion（设计 §4.7）：访客的断言走链接路径——名字、
// link 声明与范围取自链接（core 的 personal 策略），没有团队与角色声明；同样「签 → 登记签发索引 → 再读库复核」：
// 撤销链接（或主机、团队状态变化、访客设备失效）在库提交之后才撤索引里的令牌，所以复核看到仍有效，之后的撤销一定撤得到；
// 复核发现已失效就撤自己签的并答错误。团队或主机关了 guest 答 source_access_denied（与 GuestHost 一致，#154）。
// 访客断言不受版本门限制（链接路径是旧路径）。
func (s *Service) guestAssertion(ctx context.Context, ip string, caller core.Caller, sourceID string, device *core.DeviceInput) (any, error) {
	source, err := s.c.Ports.Sources.Get(ctx, s.tenant(), sourceID)
	if err != nil {
		return nil, err
	}
	if source != nil && source.RevokedAtMs != nil {
		return nil, core.Fail(gen.ErrSourceRevoked)
	}
	// Access 在 personal 策略里就是「链接未撤销且指向这台主机」。
	if _, ok, err := s.policy.Access(ctx, caller, sourceID); err != nil || !ok || source == nil {
		if err == nil {
			err = core.Fail(gen.ErrSourceAccessDenied)
		}
		return nil, err
	}
	record, err := s.c.Ports.Devices.Get(ctx, s.tenant(), caller.DeviceID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), sourceID)
	if err != nil {
		return nil, err
	}
	if err := guestSpaceState(space); err != nil {
		return nil, err
	}
	if !GuestsAllowedIn(space) {
		return nil, core.Fail(gen.ErrSourceAccessDenied)
	}
	extra, err := s.policy.Intent(ctx, caller, *source)
	if err != nil {
		return nil, err
	}
	extra.Device, extra.TrackJTI = device, true
	grant, err := s.sources.Grant(ctx, *source, *record, extra)
	if err != nil {
		return nil, err
	}
	s.stage("signed")
	issued := []relayctl.Issued{
		{Subject: record.Subject, SourceID: sourceID, DeviceID: record.DeviceID, JTI: grant.AssertionJTI, ExpMs: grant.AssertionExpiresAtMs},
		{Subject: record.Subject, SourceID: sourceID, DeviceID: record.DeviceID, JTI: grant.RelayTokenJTI, ExpMs: grant.RelayTokenExpiresAtMs},
	}
	for _, entry := range issued {
		if err := s.relay.IndexIssued(ctx, entry); err != nil {
			s.undo(ctx, issued)
			return nil, err
		}
	}
	s.stage("indexed")
	if err := s.recheckGuest(ctx, caller, sourceID); err != nil {
		s.undo(ctx, issued)
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		// 访客不是账号：审计的行动者记 system，actor_id 是访客主体（guest:<linkId>:…）。
		return s.record(ctx, tx, ip, saas.ActorSystem, record.Subject, space.TeamID, sourceID, ActionAssertion, "host", sourceID, grant.AssertionJTI)
	})
	if err != nil {
		s.undo(ctx, issued)
		return nil, err
	}
	return grant, nil
}

// recheckGuest 读库复核：访客设备还活着、主机还在、链接未撤销（Access）、主机所在团队没停用也不在恢复期内、主机与团队仍允许访客。
func (s *Service) recheckGuest(ctx context.Context, caller core.Caller, sourceID string) error {
	live, err := s.c.Ports.Devices.Get(ctx, s.tenant(), caller.DeviceID)
	if err != nil {
		return err
	}
	if live == nil || !core.IsLiveDevice(*live, s.nowMs()) {
		return core.Fail(gen.ErrUnauthenticated)
	}
	fresh, err := s.c.Ports.Sources.Get(ctx, s.tenant(), sourceID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.RevokedAtMs != nil {
		return core.Fail(gen.ErrSourceRevoked)
	}
	if _, ok, err := s.policy.Access(ctx, caller, sourceID); err != nil || !ok {
		if err == nil {
			err = core.Fail(gen.ErrSourceAccessDenied)
		}
		return err
	}
	space, err := s.store.Read().SpaceOf(ctx, s.tenant(), sourceID)
	if err != nil {
		return err
	}
	if err := guestSpaceState(space); err != nil {
		return err
	}
	if !GuestsAllowedIn(space) {
		return core.Fail(gen.ErrSourceAccessDenied)
	}
	return nil
}

// guestSpaceState：团队主机的团队在恢复期内访客进不来（source_access_denied，与账号路径一致），停用答 team_suspended；
// 主机没有空间行同样进不来。
func guestSpaceState(space *saas.HostSpace) error {
	switch {
	case space == nil:
		return core.Fail(gen.ErrSourceAccessDenied)
	case space.TeamID == "":
		return nil
	case space.TeamDeleted:
		return core.Fail(gen.ErrSourceAccessDenied)
	case space.TeamStatus == saas.TeamSuspended:
		return core.Fail(gen.ErrTeamSuspended)
	}
	return nil
}

// GuestsAllowedIn 报告主机设置与（团队主机的）团队设置是否都还允许访客（allowGuests）。links.accept 的 Admit 与
// GuestHost 共用：关了 guest 的主机上，链接当作失效，已进来的访客也续不了会话。
func GuestsAllowedIn(space *saas.HostSpace) bool {
	if space == nil || !hosts.GuestsAllowed(saas.Host{Settings: space.Settings}) {
		return false
	}
	return space.TeamID == "" || teams.SettingsOf(saas.Team{Settings: space.TeamSettings}).AllowGuests
}

// GuestHost 答链接访客此刻仍站得住时链接指向的那台主机（auth.refresh 与 me.sources 用，设计 §4.7 的 S6 实现注记）：
// 链接未撤销且指向一台未撤销的主机（core 的 personal 策略，与访客断言的 Access 同一判断）→ 主机有空间行、团队不在恢复期
// 也未停用（guestSpaceState，与访客断言一致）→ 主机与团队仍允许访客（GuestsAllowedIn，与 Admit 一致）。
// 站不住答带错误码的错误；调用方不是链接访客答 forbidden。
func (p *Policy) GuestHost(ctx context.Context, caller core.Caller) (store.SourceRecord, error) {
	if caller.Role != core.RoleGuest || caller.LinkID == "" {
		return store.SourceRecord{}, core.Fail(gen.ErrForbidden)
	}
	page, _, err := p.personal.Visible(ctx, caller, core.SourceFilter{})
	if err != nil {
		return store.SourceRecord{}, err
	}
	if len(page) != 1 {
		return store.SourceRecord{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	// personal 策略的 Visible 只给未撤销的主机，主机撤销在这里就挡住了。
	source := page[0].Source
	space, err := p.store.Read().SpaceOf(ctx, p.tenant(), source.SourceID)
	if err != nil {
		return store.SourceRecord{}, err
	}
	if err := guestSpaceState(space); err != nil {
		return store.SourceRecord{}, err
	}
	if !GuestsAllowedIn(space) {
		return store.SourceRecord{}, core.Fail(gen.ErrSourceAccessDenied)
	}
	return source, nil
}
