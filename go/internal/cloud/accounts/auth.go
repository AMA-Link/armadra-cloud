package accounts

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// newAccountID 是 UUIDv4 小写。
func newAccountID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("accounts: 系统随机源不可用：" + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// register 是 auth.register：按 IP 限流 → 事故开关 → 口令策略 → 事务（邮箱唯一、插账号、建设备）。
func (s *Service) register(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Email       string           `json:"email"`
		Password    string           `json:"password"`
		DisplayName string           `json:"displayName"`
		Device      core.DeviceInput `json:"device"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitRegisterIP, call.IP); err != nil {
		return nil, err
	}
	incident, err := s.store.Read().Incident(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	if incident.RegistrationsPaused {
		return nil, core.Fail(gen.ErrRegistrationsPaused)
	}
	email, displayName := normalizeEmail(in.Email), strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	if err := core.CheckPasswordNames(s.c, in.Password, policyNames(email, displayName)); err != nil {
		return nil, err
	}
	hashed, err := identity.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	accountID := newAccountID()
	account := saas.Account{
		AccountRecord: store.AccountRecord{AccountID: accountID, Name: accountID, Password: hashed, CreatedAtMs: nowMs, UpdatedAtMs: nowMs},
		Email:         email, DisplayName: displayName, Locale: "zh-CN", Status: saas.AccountActive, Scopes: []string{},
	}
	device, refresh := core.NewDevice(accountID, in.Device, s.gate.TransportFor(call.Request), core.SessionScopeSession, nowMs, 0)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Accounts().Create(ctx, s.tenant(), account); err != nil {
			return err
		}
		return tx.Ports().Devices.Insert(ctx, s.tenant(), device, nowMs)
	})
	if err != nil {
		return nil, err
	}
	return s.respond(ctx, call, device, refresh, &account)
}

// login 是 auth.login：按 IP 限流 → 挑战 → 按邮箱找账号 → 口令（锁定、失败计数、未知账号等时派生）→
// 停用 → 运营范围 → 建设备。
func (s *Service) login(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Account   string               `json:"account"`
		Password  string               `json:"password"`
		Device    core.DeviceInput     `json:"device"`
		Challenge *core.ChallengeToken `json:"challenge"`
		Scope     string               `json:"scope"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitLoginIP, call.IP); err != nil {
		return nil, err
	}
	if err := s.c.Challenge.Check(ctx, core.ScopeLogin, in.Challenge, call.IP); err != nil {
		return nil, err
	}
	email := normalizeEmail(in.Account)
	if err := s.limits.Check(ctx, httpx.LimitLoginAcct, email); err != nil {
		return nil, err
	}
	account, err := s.store.Read().AccountByEmail(ctx, s.tenant(), email)
	if err != nil {
		return nil, err
	}
	if account == nil {
		// 未注册的邮箱同样计失败、同样锁定（达阈值后也答 account_locked），锁着时也做口令派生：
		// 答案与时延都和已注册的账号一致，不暴露邮箱是否注册。
		return nil, s.unknown.Login(ctx, s.c.NowMs, email, in.Password)
	}
	if err := core.VerifyPassword(ctx, s.c, &account.AccountRecord, in.Password, s.nowMs()); err != nil {
		return nil, err
	}
	// 停用在口令比对之后答，不让没有口令的人枚举停用账号。
	if account.Status == saas.AccountDisabled {
		return nil, core.Fail(gen.ErrAccountDisabled)
	}
	if in.Scope == "operator" {
		// 运营登录不在这里建会话：口令对了答两步验证挑战，auth.mfaVerify 通过才建运营会话（S7）。
		return nil, s.operatorLogin(ctx, account)
	}
	nowMs := s.nowMs()
	device, refresh := core.NewDevice(account.AccountID, in.Device, s.gate.TransportFor(call.Request), core.SessionScopeSession, nowMs, 0)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		return tx.Ports().Devices.Insert(ctx, s.tenant(), device, nowMs)
	})
	if err != nil {
		return nil, err
	}
	return s.respond(ctx, call, device, refresh, account)
}

// operatorLogin 是 scope: "operator" 的登录（设计 §4.9）：非运营账号 403 forbidden；
// 没有两步验证 403 operator_mfa_required；已登记两步验证的运营账号要走挑战（auth.mfaVerify），
// 那一步随 S7 加入，本批答 501。总是返回错误。
func (s *Service) operatorLogin(ctx context.Context, account *saas.Account) error {
	if !account.HasScope(saas.ScopeOperator) {
		return core.Fail(gen.ErrForbidden)
	}
	has, err := s.store.Read().HasCredential(ctx, s.tenant(), account.AccountID, "totp")
	if err != nil {
		return err
	}
	if !has {
		return core.Fail(gen.ErrOperatorMfaRequired)
	}
	return core.Fail(gen.ErrNotImplemented)
}

// refresh 是 auth.refresh：刷新凭据来自请求体（Bearer）或刷新 Cookie（Cookie，要 Origin，免 CSRF）；
// 设备的传输与范围必须和凭据来路、请求的 scope 对得上；旧令牌重放撤销整台设备。
func (s *Service) refresh(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		RefreshToken *string `json:"refreshToken"`
		Scope        string  `json:"scope"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	token, viaCookie, err := s.gate.RefreshToken(call, in.RefreshToken)
	if err != nil {
		return nil, err
	}
	deviceID, _, _ := strings.Cut(token, ".")
	device, err := s.c.Ports.Devices.Get(ctx, s.tenant(), deviceID)
	if err != nil {
		return nil, err
	}
	if device != nil {
		wantScope := core.SessionScopeSession
		if in.Scope == "operator" {
			wantScope = core.SessionScopeOperator
		}
		if (core.TransportOf(*device) == core.TransportCookie) != viaCookie || core.ScopeOf(*device) != wantScope {
			return nil, core.Fail(gen.ErrSessionRevoked)
		}
	}
	session, err := core.Refresh(ctx, s.c, token)
	if err != nil {
		return nil, err
	}
	account, err := s.store.Read().Account(ctx, s.tenant(), session.Account.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Status != saas.AccountActive {
		return nil, core.Fail(gen.ErrSessionRevoked)
	}
	refreshed, err := s.c.Ports.Devices.Get(ctx, s.tenant(), session.Device.DeviceID)
	if err != nil || refreshed == nil {
		return nil, core.Fail(gen.ErrSessionRevoked)
	}
	return s.respond(ctx, call, *refreshed, session.RefreshToken, account)
}

// logout 是 auth.logout：撤销本设备（经撤销钩子撤已签令牌、踢人）。Cookie 会话无论成败都清掉该范围的
// 两枚 Cookie；访问 Cookie 缺失或过期时，改用同范围的刷新 Cookie 找到设备再撤销（仍要求 Origin 等于控制台
// 来源、CSRF 用这台设备的值），这样令牌过期的浏览器也能登出。
func (s *Service) logout(ctx context.Context, call *api.Call) (any, error) {
	scope := core.SessionScopeSession
	if operator, _ := call.Input["scope"].(string); operator == "operator" {
		scope = core.SessionScopeOperator
	}
	access, refresh := httpx.CookieNames(scope)
	cookies := httpx.ParseCookies(call.Request)
	viaCookie := call.Request.Header.Get("Authorization") == "" && (cookies.Has(access) || cookies.Has(refresh))
	if viaCookie {
		s.gate.ClearSession(call.Header, scope)
	}
	caller, err := call.Caller(ctx)
	if err != nil {
		if code, _ := core.CodeOf(err); viaCookie && code == gen.ErrUnauthenticated {
			return s.logoutByRefreshCookie(ctx, call, scope, cookies.Value(refresh))
		}
		return nil, err
	}
	if _, err := core.RevokeDevice(ctx, s.c, caller.DeviceID); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// logoutByRefreshCookie 用刷新 Cookie 登出：令牌必须是这台 cookie 设备当前的刷新令牌，范围对得上。
func (s *Service) logoutByRefreshCookie(ctx context.Context, call *api.Call, scope core.SessionScope, token string) (any, error) {
	if token == "" {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	if err := s.gate.CheckOrigin(call.Request); err != nil {
		return nil, err
	}
	deviceID, _, _ := strings.Cut(token, ".")
	device, err := s.c.Ports.Devices.Get(ctx, s.tenant(), deviceID)
	if err != nil {
		return nil, err
	}
	if device == nil || device.RevokedAtMs != nil || core.TransportOf(*device) != core.TransportCookie ||
		core.ScopeOf(*device) != scope || !core.SameHex(core.RefreshHash(token), device.RefreshHash) {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	if err := s.gate.CheckCSRF(call.Request, device.DeviceID); err != nil {
		return nil, err
	}
	if _, err := core.RevokeDevice(ctx, s.c, device.DeviceID); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// changePassword 是 auth.changePassword：按 IP 限流 → 挑战 → 新口令策略 → 旧口令（计入锁定）→
// 换口令、撤销本人其他设备（经撤销钩子）→ 当前设备的刷新令牌换新。
func (s *Service) changePassword(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		Password    string               `json:"password"`
		NewPassword string               `json:"newPassword"`
		Challenge   *core.ChallengeToken `json:"challenge"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitLoginIP, call.IP); err != nil {
		return nil, err
	}
	if err := s.c.Challenge.Check(ctx, core.ScopeChangePassword, in.Challenge, call.IP); err != nil {
		return nil, err
	}
	if err := core.CheckPasswordNames(s.c, in.NewPassword, policyNames(account.Email, account.DisplayName)); err != nil {
		return nil, err
	}
	if err := core.VerifyPassword(ctx, s.c, &account.AccountRecord, in.Password, s.nowMs()); err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	if err := core.SetPasswordNames(ctx, s.c, account.AccountID, policyNames(account.Email, account.DisplayName), in.NewPassword, nowMs, caller.DeviceID); err != nil {
		return nil, err
	}
	device, refresh, found, err := core.RotateRefresh(ctx, s.c, caller.DeviceID, nowMs)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, core.Fail(gen.ErrUnauthenticated)
	}
	return s.respond(ctx, call, device, refresh, account)
}

// stepUp 是 auth.stepUp：按设备限流（所有尝试都计，失败不会因此少算）→ 口令（运营会话另认两步验证码，S7）→
// 把设备提权 10 分钟。M1 不要求任何过程提权。
func (s *Service) stepUp(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		Password         *string        `json:"password"`
		TOTPCode         *string        `json:"totpCode"`
		PasskeyAssertion map[string]any `json:"passkeyAssertion"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	// 入参里「至少给一项」的约束 JSON Schema 表达不了，这里自己判。
	if in.Password == nil && in.TOTPCode == nil && in.PasskeyAssertion == nil {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitStepUpDevice, caller.DeviceID); err != nil {
		return nil, err
	}
	switch {
	case in.Password != nil:
		if err := core.VerifyPassword(ctx, s.c, &account.AccountRecord, *in.Password, s.nowMs()); err != nil {
			return nil, err
		}
	case in.TOTPCode != nil:
		// 用户侧两步验证是 M2；运营账号的验证码在 S7。这里没有可验的凭据。
		return nil, core.Fail(gen.ErrMfaInvalid)
	default:
		// 通行密钥是 M2。
		return nil, core.Fail(gen.ErrNotImplemented)
	}
	until := s.nowMs() + stepUpTTL.Milliseconds()
	err = s.c.Ports.Devices.Update(ctx, s.tenant(), caller.DeviceID, func(device *store.DeviceRecord) error {
		if device == nil || device.RevokedAtMs != nil {
			return core.Fail(gen.ErrUnauthenticated)
		}
		device.ElevatedUntilMs = &until
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"elevatedUntilMs": until}, nil
}
