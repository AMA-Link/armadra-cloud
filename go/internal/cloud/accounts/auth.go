package accounts

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
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

// register 是 auth.register：按 IP 限流 → 事故开关 → 挑战 → 口令策略 → 事务（邮箱唯一、插账号、建设备）。
func (s *Service) register(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Email       string               `json:"email"`
		Password    string               `json:"password"`
		DisplayName string               `json:"displayName"`
		Device      core.DeviceInput     `json:"device"`
		Challenge   *core.ChallengeToken `json:"challenge"`
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
	if err := s.c.Challenge.Check(ctx, core.ScopeRegister, in.Challenge, call.IP); err != nil {
		return nil, err
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
	locale := localeOf(call.Request)
	account := saas.Account{
		AccountRecord: store.AccountRecord{AccountID: accountID, Name: accountID, Password: hashed, CreatedAtMs: nowMs, UpdatedAtMs: nowMs},
		Email:         email, DisplayName: displayName, Locale: locale, Status: saas.AccountActive, Scopes: []string{},
	}
	device, refresh := core.NewDevice(accountID, in.Device, s.gate.TransportFor(call.Request), core.SessionScopeSession, nowMs, 0)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Accounts().Create(ctx, s.tenant(), account); err != nil {
			return err
		}
		if err := tx.Ports().Devices.Insert(ctx, s.tenant(), device, nowMs); err != nil {
			return err
		}
		who := audit.Security{AccountID: accountID, DeviceName: device.Name, IP: call.IP}
		if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityRegister); err != nil {
			return err
		}
		return s.mail.Enqueue(ctx, tx, mail.Mail{
			Template: mail.VerifyEmail, Locale: locale, To: email, AccountID: accountID,
			Params: mail.Params{DisplayName: displayName},
		})
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
		// 运营登录不在这里建会话：口令对了答两步验证挑战，auth.mfaVerify 通过才建运营会话（mfa.go）。
		return s.operatorLogin(ctx, account, in.Device)
	}
	nowMs := s.nowMs()
	device, refresh := core.NewDevice(account.AccountID, in.Device, s.gate.TransportFor(call.Request), core.SessionScopeSession, nowMs, 0)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		return s.recordLogin(ctx, tx, account, device, call.IP, nowMs)
	})
	if err != nil {
		return nil, err
	}
	return s.respond(ctx, call, device, refresh, account)
}

// recordLogin 在事务里把登录的设备插进库、写登录的安全日志；新设备另写 security.newDevice 并发提醒。
// 登录（auth.login）与设备码兑换（auth.devicePoll）共用。
//
// 新设备 = 同平台同名的设备 30 天内没出现过，或者这个账号 30 天内没有从同一个 IP 前缀、用同名设备登录过
// （同名的设备名谁都能起，光靠名字挡不住别人冒用常用设备名登录）。
func (s *Service) recordLogin(ctx context.Context, tx saas.Tx, account *saas.Account, device store.DeviceRecord, ip string, nowMs int64) error {
	known, err := tx.Ports().Devices.ListBySubject(ctx, s.tenant(), account.AccountID)
	if err != nil {
		return err
	}
	fresh := isNewDevice(known, device, nowMs)
	if prefix := audit.IPPrefix(ip); !fresh && prefix != "" {
		seen, err := tx.Audit().SeenLogin(ctx, s.tenant(), account.AccountID, device.Name, prefix, nowMs-newDeviceWindow.Milliseconds())
		if err != nil {
			return err
		}
		fresh = !seen
	}
	if err := tx.Ports().Devices.Insert(ctx, s.tenant(), device, nowMs); err != nil {
		return err
	}
	who := audit.Security{AccountID: account.AccountID, DeviceName: device.Name, IP: ip}
	if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityLogin); err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityNewDevice); err != nil {
		return err
	}
	return s.mail.Enqueue(ctx, tx, mail.Mail{
		Template: mail.NewDevice, Locale: account.Locale, To: account.Email,
		Params: mail.Params{DisplayName: greetName(account), DeviceName: device.Name, Platform: device.Platform},
	})
}

// newDeviceWindow 是「新设备」的回看期：同平台同名的设备 30 天内出现过就不算新。
const newDeviceWindow = 30 * 24 * time.Hour

// isNewDevice 报告 device 相对账号已有的设备是不是新的（已撤销的行只要还在也算出现过）。
func isNewDevice(known []store.DeviceRecord, device store.DeviceRecord, nowMs int64) bool {
	cutoff := nowMs - newDeviceWindow.Milliseconds()
	for _, row := range known {
		if row.Platform != device.Platform || row.Name != device.Name {
			continue
		}
		seen := row.CreatedAtMs
		if row.LastSeenAtMs != nil && *row.LastSeenAtMs > seen {
			seen = *row.LastSeenAtMs
		}
		if seen >= cutoff {
			return false
		}
	}
	return true
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

// logout 是 auth.logout：撤销本设备（经撤销钩子撤已签令牌、踢人）。Cookie 会话在 Origin 通过之后无论成败都清掉该范围的
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
		// 先过 Origin 再清 Cookie：兄弟子域（*.src.<域>）的页面没法借一个 POST 强制登出受害者。
		// 过了 Origin 之后无论认证、CSRF 成败都清（CSRF 失败时清是可接受的）。
		if err := s.gate.CheckOrigin(call.Request); err != nil {
			return nil, err
		}
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
	var (
		device  store.DeviceRecord
		refresh string
	)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		// 事务里改口令、撤销其他设备、给当前设备换新刷新令牌；撤销钩子经 AfterCommit 在提交后投递。
		bound := s.bound(tx)
		if _, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
			return err
		}
		if err := core.SetPasswordNames(ctx, bound, account.AccountID, policyNames(account.Email, account.DisplayName), in.NewPassword, nowMs, caller.DeviceID); err != nil {
			return err
		}
		var found bool
		var err error
		device, refresh, found, err = core.RotateRefresh(ctx, bound, caller.DeviceID, nowMs)
		if err != nil {
			return err
		}
		if !found {
			return core.Fail(gen.ErrUnauthenticated)
		}
		// 口令变了，之前发出的「改邮箱确认」链接一律作废（否则拿到旧邮件的人能在改口令之后把邮箱改走）。
		if err := s.killEmailChange(ctx, tx, account.AccountID, nowMs); err != nil {
			return err
		}
		who := audit.Security{AccountID: account.AccountID, DeviceName: device.Name, IP: call.IP}
		if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityPasswordChanged); err != nil {
			return err
		}
		return s.mail.Enqueue(ctx, tx, mail.Mail{
			Template: mail.PasswordChanged, Locale: account.Locale, To: account.Email,
			Params: mail.Params{DisplayName: greetName(account)},
		})
	})
	if err != nil {
		return nil, err
	}
	return s.respond(ctx, call, device, refresh, account)
}

// stepUp 是 auth.stepUp：按设备限流（所有尝试都计，失败不会因此少算）→ 口令（运营账号另认两步验证码 totpCode）→
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
		// 用户侧两步验证是 M2；M1 只有运营账号登记了 TOTP，其余账号没有可验的凭据。同一个码不能第二次用。
		ok, err := false, error(nil)
		if account.HasScope(saas.ScopeOperator) && s.wrapper != nil {
			ok, err = admin.VerifyAccountTOTP(ctx, s.store, s.wrapper, s.tenant(), account.AccountID, *in.TOTPCode, s.nowMs())
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, core.Fail(gen.ErrMfaInvalid)
		}
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
