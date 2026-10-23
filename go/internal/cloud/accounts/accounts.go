// Package accounts 是 SaaS 账号的过程（设计 §4.3）：注册、登录、刷新、登出、改口令、提权、设备码，以及
// me.get / update / devices / deviceRevoke。账号用邮箱登录，口令、锁定与会话复用控制核；
// 会话按请求的传输（Bearer 或 Cookie）下发。
package accounts

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// stepUpTTL 是 auth.stepUp 提权的有效期。
const stepUpTTL = 10 * time.Minute

// Options 是账号服务的装配。
type Options struct {
	Core   *core.Context
	Store  saas.Store
	Gate   *httpx.Gate
	Limits *httpx.Limits
	// Mail 把事务邮件插进发件箱（验证邮箱、找回口令、新设备提醒等）。
	Mail *mail.Service
	// Unknown 存未注册邮箱的失败计数与锁定（多实例共享的 Redis 版）；nil = 进程内计数。
	Unknown core.UnknownStore
	// AppOrigin 是控制台来源，设备码的 verificationUrl 以它开头。
	AppOrigin string
	// MFA 存运营登录的两步验证挑战（cloud:mfa:*）；Wrapper 解开 TOTP 种子。两者缺一，scope: "operator" 的登录答 500。
	MFA     admin.ChallengeStore
	Wrapper *store.Wrapper
	// OnDeviceDecision 在批准 / 拒绝设备码的事务里调用；nil = 写安全日志 security.deviceApprove /
	// security.deviceDeny（设计 §4.3）。返回错误会回滚这次决定。
	// 外部副作用（发邮件、推事件、Redis）一律经 tx.AfterCommit，不能在钩子里直接做：事务可能回滚或重试。
	OnDeviceDecision func(ctx context.Context, tx saas.Tx, decision DeviceDecision) error
}

// Service 是账号服务。
type Service struct {
	c      *core.Context
	store  saas.Store
	gate   *httpx.Gate
	limits *httpx.Limits
	mail   *mail.Service
	// unknown 给未注册的邮箱同样计失败、同样锁定：锁定不暴露邮箱有没有注册。
	unknown core.UnknownAccounts

	mfa     admin.ChallengeStore
	wrapper *store.Wrapper

	appOrigin        string
	onDeviceDecision func(ctx context.Context, tx saas.Tx, decision DeviceDecision) error
}

// New 建账号服务。
func New(options Options) *Service {
	s := &Service{
		c: options.Core, store: options.Store, gate: options.Gate, limits: options.Limits, mail: options.Mail,
		appOrigin: strings.TrimRight(options.AppOrigin, "/"), onDeviceDecision: options.OnDeviceDecision,
		mfa: options.MFA, wrapper: options.Wrapper,
	}
	if s.onDeviceDecision == nil {
		s.onDeviceDecision = s.auditDeviceDecision
	}
	s.unknown.Store = options.Unknown
	return s
}

func (s *Service) tenant() string { return s.c.TenantID }

func (s *Service) nowMs() int64 { return s.c.NowMs() }

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// policyNames 是口令里不许出现的名字：邮箱本地部分与显示名。
func policyNames(email, displayName string) []string {
	local, _, _ := strings.Cut(email, "@")
	return []string{local, displayName}
}

// accountInfo 是会话里的账号。
func accountInfo(a *saas.Account) core.AccountInfo {
	email := a.Email
	return core.AccountInfo{AccountID: a.AccountID, Email: &email, DisplayName: a.DisplayName, EmailVerified: a.EmailVerifiedAtMs != nil}
}

// sessionResult 是登录类答案：会话加 Cookie 传输时的 csrfToken。
type sessionResult struct {
	Session   core.Session `json:"session"`
	CSRFToken string       `json:"csrfToken,omitempty"`
}

// respond 把设备换成会话答复：Bearer 传输带 refreshToken；Cookie 传输不带，改写两枚 Cookie
// （访问令牌仍在体里，但只认从 Cookie 来：对 cookie 设备放进 Authorization 会被拒），并带 csrfToken。
func (s *Service) respond(ctx context.Context, call *api.Call, device store.DeviceRecord, refreshToken string, account *saas.Account) (sessionResult, error) {
	session, err := core.SessionFor(ctx, s.c, device, refreshToken)
	if err != nil {
		return sessionResult{}, err
	}
	session.Account = accountInfo(account)
	if core.TransportOf(device) != core.TransportCookie {
		return sessionResult{Session: session}, nil
	}
	s.gate.SetSession(call.Header, core.ScopeOf(device), session, refreshToken)
	session.RefreshToken = ""
	return sessionResult{Session: session, CSRFToken: s.gate.CSRFToken(device.DeviceID)}, nil
}

// bound 返回绑定到 SaaS 事务的控制核：端口换成 tx.Ports()，撤销钩子换成经 tx.AfterCommit 投递的版本
// （设计 §4.2、§4.11）。事务闭包里调 core 的撤销函数（SetPasswordNames、RevokeDevice 等）一律用它，
// 不用独立端口的 s.c：那会等本事务持有的行锁而挂死，钩子也会赶在提交之前把令牌撤掉。
func (s *Service) bound(tx saas.Tx) *core.Context {
	var sink core.RevocationSink
	if s.c.Revocations != nil {
		sink = relayctl.Deferred(tx, s.c.Revocations)
	}
	return s.c.Bound(tx.Ports(), sink)
}

// localeOf 按 Accept-Language 的首选语言定账号的界面语言（邮件语言取它）：en 开头用英文，其余用中文。
func localeOf(r *http.Request) string {
	if r == nil {
		return mail.LocaleZH
	}
	first, _, _ := strings.Cut(r.Header.Get("Accept-Language"), ",")
	tag, _, _ := strings.Cut(first, ";")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(tag)), "en") {
		return mail.LocaleEN
	}
	return mail.LocaleZH
}

// owner 要求调用方是账号本人（SaaS 的访客会话不是账号）并读出账号行。
func (s *Service) owner(ctx context.Context, caller core.Caller) (*saas.Account, error) {
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

// Handlers 是本服务实现的过程。
func (s *Service) Handlers() map[string]api.Handler {
	handlers := map[string]api.Handler{
		"auth.register":       s.register,
		"auth.login":          s.login,
		"auth.refresh":        s.refresh,
		"auth.mfaVerify":      s.mfaVerify,
		"auth.logout":         s.logout,
		"auth.changePassword": s.changePassword,
		"auth.stepUp":         s.stepUp,
		"auth.deviceStart":    s.deviceStart,
		"auth.devicePoll":     s.devicePoll,
		"auth.deviceApprove":  s.deviceApprove,
		"auth.deviceGet":      s.deviceGet,
		"auth.deviceDeny":     s.deviceDeny,

		"auth.emailVerify":         s.emailVerify,
		"auth.emailVerifyResend":   s.emailVerifyResend,
		"auth.passwordResetStart":  s.passwordResetStart,
		"auth.passwordResetFinish": s.passwordResetFinish,
		"me.emailChange":           s.meEmailChange,

		"me.get":          s.meGet,
		"me.update":       s.meUpdate,
		"me.devices":      s.meDevices,
		"me.deviceRevoke": s.meDeviceRevoke,
		"me.spaces":       s.meSpaces,
	}
	for name, handler := range handlers {
		handlers[name] = wrap(handler)
	}
	return handlers
}

// wrap 把存储层按约束映射出的业务错误换成控制核的错误：处理器只返回 core.Fail / core.FailWith，
// 其余错误原样往上走，由 HTTP 层答 500 internal，日志只记类名。
func wrap(handler api.Handler) api.Handler {
	return func(ctx context.Context, call *api.Call) (any, error) {
		out, err := handler(ctx, call)
		return out, httpx.MapError(err)
	}
}
