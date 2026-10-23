package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// sink 记下撤销钩子的每一次回调。
type sink struct {
	mu    sync.Mutex
	calls []string
}

func (s *sink) add(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}
func (s *sink) DeviceRevoked(_ context.Context, subject, deviceID string) {
	s.add("device:" + subject + ":" + deviceID)
}
func (s *sink) DevicesRevoked(_ context.Context, subject, keep string) {
	s.add("devices:" + subject + ":" + keep)
}
func (s *sink) LinkRevoked(_ context.Context, sourceID, linkID string) {
	s.add("link:" + sourceID + ":" + linkID)
}
func (s *sink) SourceRevoked(_ context.Context, sourceID string) { s.add("source:" + sourceID) }
func (s *sink) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestNewDeviceIsPure(t *testing.T) {
	record, refresh := core.NewDevice("sub", core.DeviceInput{Platform: "web", Name: "n"}, core.TransportCookie, core.SessionScopeOperator, 1000, time.Hour)
	eq(t, record.Transport, "cookie", "传输")
	eq(t, record.Scope, "operator", "范围")
	eq(t, record.ExpiresAtMs, int64(1000+3_600_000), "运营会话的绝对寿命")
	eq(t, record.RefreshHash, core.RefreshHash(refresh), "刷新令牌哈希")
	bearer, _ := core.NewDevice("sub", core.DeviceInput{Platform: "web", Name: "n"}, core.TransportBearer, core.SessionScopeSession, 1000, 0)
	eq(t, bearer.Transport, "", "bearer 在记录里留空")
	eq(t, bearer.Scope, "", "session 在记录里留空")
	eq(t, bearer.ExpiresAtMs, int64(1000)+core.RefreshTTL.Milliseconds(), "缺省寿命")
}

func TestOperatorSessionDoesNotExtendOnRefresh(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	nowMs := e.h.Clock.Ms()
	device, refresh := core.NewDevice("acct:alice", core.DeviceInput{Platform: "web", Name: "ops"}, core.TransportCookie, core.SessionScopeOperator, nowMs, core.OperatorSessionTTL)
	e.must(e.h.Context.Ports.Devices.Insert(e.ctx, e.h.Context.TenantID, device, nowMs))
	e.h.Clock.Set(nowMs + 30*60*1000)
	next, err := core.Refresh(e.ctx, e.h.Context, refresh)
	e.must(err)
	eq(t, next.ExpiresAtMs, device.ExpiresAtMs, "刷新不延长运营会话")
	e.h.Clock.Set(nowMs + 61*60*1000)
	_, err = core.Refresh(e.ctx, e.h.Context, next.RefreshToken)
	eq(t, code(err), "session_expired", "过了 1 小时刷新失败")
}

func TestRevocationSinkCallbacks(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	recorder := &sink{}
	e.h.Context.Revocations = recorder
	owner := e.owner()
	// 登出一台设备。
	revoked, err := core.RevokeDevice(e.ctx, e.h.Context, owner.DeviceID)
	e.must(err)
	eq(t, revoked, true, "撤销")
	// 刷新令牌重放撤销整台设备。
	session, err := e.login("alice", password)
	e.must(err)
	_, err = core.Refresh(e.ctx, e.h.Context, session.RefreshToken)
	e.must(err)
	_, err = core.Refresh(e.ctx, e.h.Context, session.RefreshToken)
	eq(t, code(err), "session_revoked", "重放")
	// 改口令。
	session, err = e.login("alice", password)
	e.must(err)
	caller, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+session.AccessToken)
	e.must(err)
	_, err = core.NewAccounts(e.h.Context).ChangePassword(e.ctx, caller, core.ChangePasswordInput{Password: password, NewPassword: "kestrel-ember-orchard-77"}, "198.51.100.1")
	e.must(err)
	want := []string{
		"device:acct:alice:" + owner.DeviceID,
		"device:acct:alice:" + session.Device.DeviceID,
		"devices:acct:alice:" + caller.DeviceID,
	}
	got := recorder.list()
	if len(got) < 3 {
		t.Fatalf("回调太少：%v", got)
	}
	eq(t, got[0], want[0], "登出")
	// 重放的那台是上一次登录的设备。
	found := false
	for _, call := range got {
		found = found || call == "devices:acct:alice:"+caller.DeviceID
	}
	eq(t, found, true, "改口令回调保留当前设备")
}

func TestRevocationSinkLinkAndSource(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	recorder := &sink{}
	e.h.Context.Revocations = recorder
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, core.CreateLinkInput{SourceID: sourceID, Role: "editor", Label: "x", ExpiresAtMs: e.h.Clock.Ms() + 3_600_000})
	e.must(err)
	e.must(links.Revoke(e.ctx, owner, created.LinkID))
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	got := recorder.list()
	eq(t, got[len(got)-2], "link:"+sourceID+":"+created.LinkID, "链接撤销回调")
	eq(t, got[len(got)-1], "source:"+sourceID, "源撤销回调")
}

func TestVerifyPasswordLocksAndClears(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	account, err := e.h.Context.Ports.Accounts.Get(e.ctx, e.h.Context.TenantID, "acct:alice")
	e.must(err)
	nowMs := e.h.Clock.Ms()
	// 没有账号：等时派生，答 credentials_invalid。
	eq(t, code(core.VerifyPassword(e.ctx, e.h.Context, nil, "x", nowMs)), "credentials_invalid", "未知账号")
	for i := 0; i < 5; i++ {
		eq(t, code(core.VerifyPassword(e.ctx, e.h.Context, account, "wrong", nowMs)), "credentials_invalid", "口令不对")
		account, err = e.h.Context.Ports.Accounts.Get(e.ctx, e.h.Context.TenantID, "acct:alice")
		e.must(err)
	}
	eq(t, code(core.VerifyPassword(e.ctx, e.h.Context, account, password, nowMs)), "account_locked", "锁着不比对")
	e.h.Clock.Set(nowMs + 2*60*1000)
	account, err = e.h.Context.Ports.Accounts.Get(e.ctx, e.h.Context.TenantID, "acct:alice")
	e.must(err)
	e.must(core.VerifyPassword(e.ctx, e.h.Context, account, password, e.h.Clock.Ms()))
	account, err = e.h.Context.Ports.Accounts.Get(e.ctx, e.h.Context.TenantID, "acct:alice")
	e.must(err)
	eq(t, account.Failures, int64(0), "成功清零")
}

func TestGrantExtraTeamAndTrackedJTI(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	source, err := e.h.Context.Ports.Sources.Get(e.ctx, e.h.Context.TenantID, sourceID)
	e.must(err)
	device, err := e.h.Context.Ports.Devices.Get(e.ctx, e.h.Context.TenantID, owner.DeviceID)
	e.must(err)
	grant, err := sources.Grant(e.ctx, *source, *device, core.GrantExtra{
		Name: "Alice", Email: "alice@example.com", EmailVerified: true,
		Team: &gen.AssertionClaimsTeam{TeamID: "t-1", Role: "admin"}, Role: "editor", TrackJTI: true,
	})
	e.must(err)
	assertion, relay := claimsOf(t, grant.Assertion), claimsOf(t, grant.RelayToken)
	team := assertion["team"].(map[string]any)
	eq(t, team["teamId"], any("t-1"), "断言的 team")
	eq(t, team["role"], any("admin"), "断言的团队角色")
	eq(t, assertion["role"], any("editor"), "断言的 role")
	eq(t, assertion["email"], any("alice@example.com"), "email")
	eq(t, assertion["emailVerified"], any(true), "emailVerified")
	eq(t, relay["team"], any("t-1"), "中继令牌的 team")
	prefix := "s." + owner.DeviceID + "." + sourceID + "."
	if got := assertion["jti"].(string); len(got) <= len(prefix) || got[:len(prefix)] != prefix {
		t.Fatalf("断言 jti 前缀：%v", got)
	}
	eq(t, grant.AssertionJTI, assertion["jti"].(string), "返回断言 jti")
	eq(t, grant.RelayTokenJTI, relay["jti"].(string), "返回中继令牌 jti")
	// 个人中转的形状不变：不带 team，断言 jti 是 32 位 hex。
	plain, err := sources.Grant(e.ctx, *source, *device, core.GrantExtra{})
	e.must(err)
	_, hasTeam := claimsOf(t, plain.Assertion)["team"]
	eq(t, hasTeam, false, "personal 断言不带 team")
	eq(t, len(claimsOf(t, plain.Assertion)["jti"].(string)), 32, "personal 断言 jti")
}

func TestRelayForTeamAddsTeamClaim(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	source, err := e.h.Context.Ports.Sources.Get(e.ctx, e.h.Context.TenantID, sourceID)
	e.must(err)
	info, err := sources.RelayForTeam(e.ctx, *source, "t-9")
	e.must(err)
	eq(t, claimsOf(t, info.TunnelToken)["team"], any("t-9"), "隧道令牌的 team")
	plain, err := sources.RelayFor(e.ctx, *source)
	e.must(err)
	_, has := claimsOf(t, plain.TunnelToken)["team"]
	eq(t, has, false, "personal 隧道令牌不带 team")
}

// 运营范围的设备寿命收到 1 小时：不给取 1 小时，给得更长也收，给得更短保留。
func TestNewDeviceOperatorLifetimeIsCapped(t *testing.T) {
	d := core.DeviceInput{Platform: "browser", Name: "n"}
	for _, c := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"不给", 0, time.Hour}, {"更长", 30 * 24 * time.Hour, time.Hour}, {"更短", 10 * time.Minute, 10 * time.Minute},
	} {
		record, _ := core.NewDevice("s", d, core.TransportCookie, core.SessionScopeOperator, 1000, c.ttl)
		eq(t, record.ExpiresAtMs, int64(1000)+c.want.Milliseconds(), "运营会话寿命："+c.name)
	}
	record, _ := core.NewDevice("s", d, core.TransportCookie, core.SessionScopeSession, 1000, 0)
	eq(t, record.ExpiresAtMs, int64(1000)+core.RefreshTTL.Milliseconds(), "普通会话缺省寿命不变")
}

// 未知名字与存在的账号一样：连续失败达阈值后答 account_locked，锁着时也做口令派生。
func TestUnknownAccountsLockLikeRealOnes(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	e.addAccount("alice", "acct:alice")
	accounts := core.NewAccounts(e.h.Context)
	attempt := func(name string) error {
		_, err := accounts.Login(e.ctx, core.LoginInput{Account: name, Password: "wrong-password-123", Device: core.DeviceInput{Platform: "desktop", Name: "t"}}, "198.51.100.1")
		return err
	}
	for i := 0; i < 5; i++ {
		eq(t, code(attempt("alice")), "credentials_invalid", "已注册的失败")
		eq(t, code(attempt("nobody")), "credentials_invalid", "未注册的失败")
	}
	eq(t, code(attempt("alice")), "account_locked", "已注册的第 6 次")
	eq(t, code(attempt("nobody")), "account_locked", "未注册的第 6 次")
}
