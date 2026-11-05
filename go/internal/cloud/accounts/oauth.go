package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 第三方登录（GitHub、Google，设计 §4.3）：auth.oauthStart / oauthFinish / oauthUnlink。

// OAuth 是第三方登录的依赖。
type OAuth struct {
	// Client 换身份；Configured 的提供方才开。
	Client *oauth.Client
	// States 存授权状态（Redis）。
	States oauth.States
	// OnError 记提供方请求失败（只给错误，不含 code 与令牌）；nil = 不记。
	OnError func(provider string, err error)
}

// displayNameMax 是显示名的长度上限（协议包 me.displayName）。
const displayNameMax = 128

// oauthStart 是 auth.oauthStart：按 IP 限流 → 提供方要配了（否则 404）→ redirectUri 严格等于
// APP_ORIGIN + /login/oauth/<provider>（否则 400）→ 生成 state 与 PKCE 校验串，存 10 分钟、一次性 →
// Cookie 传输时下发浏览器绑定随机数。
func (s *Service) oauthStart(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Provider    string `json:"provider"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitOAuthStartIP, call.IP); err != nil {
		return nil, err
	}
	return s.beginAuthorization(ctx, call, in.Provider, in.RedirectURI, oauth.State{Purpose: oauth.PurposeLogin})
}

// beginAuthorization 是三种目的共用的发起：提供方要配了（否则 404）→ redirectUri 严格等于回调地址（否则 400）→
// 存 state（带目的与发起者）→ Cookie 传输时下发浏览器绑定随机数。record 只填 Purpose、AccountID、DeviceID。
func (s *Service) beginAuthorization(ctx context.Context, call *api.Call, provider, redirectURI string, record oauth.State) (any, error) {
	if !s.oauth.Client.Configured(provider) {
		return nil, core.Fail(gen.ErrNotFound)
	}
	if redirectURI != s.appOrigin+"/login/oauth/"+provider {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	state, verifier := core.RandomToken(32), core.RandomToken(32)
	nonce := ""
	if s.gate.TransportFor(call.Request) == core.TransportCookie {
		nonce = core.RandomToken(32)
	}
	record.Provider, record.RedirectURI, record.Verifier, record.Nonce = provider, redirectURI, verifier, nonce
	record.ExpiresAtMs = s.nowMs() + oauth.StateTTL.Milliseconds()
	if err := s.oauth.States.Put(ctx, state, record); err != nil {
		return nil, err
	}
	authorizationURL, _ := s.oauth.Client.AuthorizationURL(provider, state, verifier, redirectURI)
	if nonce != "" {
		call.Header.Add("Set-Cookie", httpx.SetOAuth(nonce, httpx.OAuthCookieMaxAge))
	}
	return map[string]any{"authorizationUrl": authorizationURL, "state": state, "expiresAtMs": record.ExpiresAtMs}, nil
}

// takeAuthorization 取走 state 并核对回调：一次性（不存在、已用、过期、提供方或目的对不上 → 401 credentials_invalid）；
// 提权与绑定还要账号和设备都是发起者；浏览器绑定（Cookie 传输或发起时下发过随机数，就要求 Cookie 与记录一致，
// 用后清除）。state 取走后无论成败，浏览器上的绑定 Cookie 都清掉。
func (s *Service) takeAuthorization(ctx context.Context, call *api.Call, provider, state, purpose string, caller *core.Caller) (*oauth.State, error) {
	record, err := s.oauth.States.Take(ctx, state)
	if err != nil {
		return nil, err
	}
	invalid := core.Fail(gen.ErrCredentialsInvalid)
	if record == nil {
		return nil, invalid
	}
	viaCookie := s.gate.TransportFor(call.Request) == core.TransportCookie
	nonce, hasCookie := httpx.OAuthNonce(call.Request)
	if viaCookie || record.Nonce != "" || hasCookie {
		call.Header.Add("Set-Cookie", httpx.SetOAuth("", 0))
	}
	if record.ExpiresAtMs <= s.nowMs() || record.Provider != provider || record.PurposeOf() != purpose {
		return nil, invalid
	}
	if caller != nil && (record.AccountID != caller.AccountID || record.DeviceID != caller.DeviceID) {
		return nil, invalid
	}
	if viaCookie || record.Nonce != "" {
		if record.Nonce == "" || !hasCookie || subtle.ConstantTimeCompare([]byte(nonce), []byte(record.Nonce)) != 1 {
			return nil, invalid
		}
	}
	return record, nil
}

// exchangeIdentity 用 code 换出提供方的身份：提供方拒绝 → 401，不可用 → 503（记 OnError）。
func (s *Service) exchangeIdentity(ctx context.Context, provider, code string, record *oauth.State) (oauth.Identity, error) {
	ident, err := s.oauth.Client.Exchange(ctx, provider, code, record.Verifier, record.RedirectURI)
	switch {
	case errors.Is(err, oauth.ErrRejected):
		return ident, core.Fail(gen.ErrCredentialsInvalid)
	case err != nil:
		if s.oauth.OnError != nil {
			s.oauth.OnError(provider, err)
		}
		return ident, core.Fail(gen.ErrUnavailable)
	}
	return ident, nil
}

// stepUpOAuthStart 是 auth.stepUpOAuthStart：会话（账号本人）→ 按 IP 限流 → 同 oauthStart，目的是 reauth，
// 绑定账号与设备。
func (s *Service) stepUpOAuthStart(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		Provider    string `json:"provider"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitOAuthStartIP, call.IP); err != nil {
		return nil, err
	}
	return s.beginAuthorization(ctx, call, in.Provider, in.RedirectURI, oauth.State{
		Purpose: oauth.PurposeReauth, AccountID: account.AccountID, DeviceID: caller.DeviceID,
	})
}

// reauthIdentity 核提权的回调：state 的目的、账号、设备与浏览器随机数都对得上，换出的 (提供方, 主体) 是本账号已绑定的身份；
// 否则 401 credentials_invalid。答已绑定的那条身份。
func (s *Service) reauthIdentity(ctx context.Context, call *api.Call, caller core.Caller, account *saas.Account, in struct{ Provider, State, Code string }) (*saas.OAuthIdentity, error) {
	invalid := core.Fail(gen.ErrCredentialsInvalid)
	if !s.oauth.Client.Configured(in.Provider) {
		return nil, invalid
	}
	record, err := s.takeAuthorization(ctx, call, in.Provider, in.State, oauth.PurposeReauth, &caller)
	if err != nil {
		return nil, err
	}
	ident, err := s.exchangeIdentity(ctx, in.Provider, in.Code, record)
	if err != nil {
		return nil, err
	}
	bound, err := s.store.Read().OAuthIdentity(ctx, s.tenant(), in.Provider, ident.Subject)
	if err != nil {
		return nil, err
	}
	if bound == nil || bound.AccountID != account.AccountID {
		return nil, invalid
	}
	return bound, nil
}

// oauthFinish 是 auth.oauthFinish：按 IP 限流 → 取走 state（一次性；不存在、过期、提供方对不上 → 401）→
// 浏览器绑定（Cookie 传输或发起时下发过随机数，就要求 Cookie 与记录一致，用后清除）→ 用 code 与 code_verifier
// 换身份（访问令牌只用一次）→ 邮箱必须已验证 → 已绑定则登录；未绑定且本地没有同邮箱账号则建账号；
// 本地有同邮箱账号：口令登录关闭时绑定并登录，开着时 409。会话经 createSession，与 auth.login 同一条路径。
func (s *Service) oauthFinish(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Provider string           `json:"provider"`
		State    string           `json:"state"`
		Code     string           `json:"code"`
		Device   core.DeviceInput `json:"device"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitOAuthFinishIP, call.IP); err != nil {
		return nil, err
	}
	if !s.oauth.Client.Configured(in.Provider) {
		return nil, core.Fail(gen.ErrCredentialsInvalid)
	}
	invalid := core.Fail(gen.ErrCredentialsInvalid)
	record, err := s.takeAuthorization(ctx, call, in.Provider, in.State, oauth.PurposeLogin, nil)
	if err != nil {
		return nil, err
	}
	ident, err := s.exchangeIdentity(ctx, in.Provider, in.Code, record)
	if err != nil {
		return nil, err
	}
	if !ident.EmailVerified || ident.Email == "" {
		return nil, core.Fail(gen.ErrEmailUnverified)
	}
	email := normalizeEmail(ident.Email)
	identityRow := saas.OAuthIdentity{
		CredentialID: core.NewUUID(), Provider: in.Provider, Subject: ident.Subject,
		Email: email, Login: ident.Login, Name: ident.Name, CreatedAtMs: s.nowMs(),
	}

	read := s.store.Read()
	bound, err := read.OAuthIdentity(ctx, s.tenant(), in.Provider, ident.Subject)
	if err != nil {
		return nil, err
	}
	var (
		account *saas.Account
		steps   = sessionSteps{}
	)
	loginLog := func(tx saas.Tx, device store.DeviceRecord) error {
		who := audit.Security{AccountID: account.AccountID, DeviceName: device.Name, IP: call.IP}
		if err := audit.WriteSecurity(ctx, tx, s.tenant(), s.nowMs(), who, audit.SecurityOAuthLogin); err != nil {
			return err
		}
		if bound == nil {
			return nil
		}
		return tx.Accounts().TouchOAuthIdentity(ctx, s.tenant(), account.AccountID, in.Provider, s.nowMs())
	}
	steps.After = loginLog
	switch {
	case bound != nil:
		// 已绑定：登录那个账号（身份以主体为准，邮箱后来改过也不影响）。
		if account, err = read.Account(ctx, s.tenant(), bound.AccountID); err != nil {
			return nil, err
		}
		if account == nil {
			return nil, invalid
		}
	default:
		existing, err := read.AccountByEmail(ctx, s.tenant(), email)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			if account, steps, err = s.oauthNewAccount(ctx, call, email, ident, &identityRow, loginLog); err != nil {
				return nil, err
			}
		} else {
			if !s.passwordOff {
				// 口令登录开着：不自动合并（防抢注与接管），用户先用原方式登录。
				return nil, core.FailWith(gen.ErrConflict, map[string]any{"reason": "account_exists"})
			}
			account = existing
			steps.Before = func(tx saas.Tx) error { return s.oauthBind(ctx, tx, existing, &identityRow) }
		}
	}

	out, err := s.createSession(ctx, call, account, in.Device, steps)
	switch {
	case errors.Is(err, saas.ErrEmailTaken):
		// 并发下同邮箱的账号刚被别人建出来。
		return nil, core.FailWith(gen.ErrConflict, map[string]any{"reason": "account_exists"})
	case errors.Is(err, saas.ErrConflict):
		return nil, core.FailWith(gen.ErrConflict, map[string]any{"reason": "identity_linked"})
	case err != nil:
		return nil, err
	}
	return out, nil
}

// oauthNewAccount 备好新账号（邮箱已验证、没有口令、显示名取提供方给的名字）和建账号的事务步骤。
// 注册暂停时 503 registrations_paused。新账号的第一台设备不发新设备提醒，写 security.register。
func (s *Service) oauthNewAccount(ctx context.Context, call *api.Call, email string, ident oauth.Identity, row *saas.OAuthIdentity, loginLog func(saas.Tx, store.DeviceRecord) error) (*saas.Account, sessionSteps, error) {
	incident, err := s.store.Read().Incident(ctx, s.tenant())
	if err != nil {
		return nil, sessionSteps{}, err
	}
	if incident.RegistrationsPaused {
		return nil, sessionSteps{}, core.Fail(gen.ErrRegistrationsPaused)
	}
	nowMs := s.nowMs()
	accountID := newAccountID()
	account := &saas.Account{
		AccountRecord: store.AccountRecord{AccountID: accountID, Name: accountID, Password: identity.NoPassword(), CreatedAtMs: nowMs, UpdatedAtMs: nowMs},
		Email:         email, EmailVerifiedAtMs: &nowMs, DisplayName: oauthDisplayName(ident, email),
		Locale: localeOf(call.Request), Status: saas.AccountActive, Scopes: []string{},
	}
	row.AccountID = accountID
	return account, sessionSteps{
		QuietNewDevice: true,
		Before: func(tx saas.Tx) error {
			if err := tx.Accounts().Create(ctx, s.tenant(), *account); err != nil {
				return err
			}
			if err := tx.Accounts().InsertOAuthIdentity(ctx, s.tenant(), *row); err != nil {
				return err
			}
			who := audit.Security{AccountID: accountID, IP: call.IP}
			return audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityRegister)
		},
		After: loginLog,
	}, nil
}

// oauthBind 在事务里把第三方身份绑到已有的本地账号（口令登录关闭时，提供方给的已验证邮箱与本地账号相同即视为同一人）：
// 锁账号后重读并核对邮箱仍是提供方给的那个（改走了答 conflict）；账号在该提供方已有别的身份答 conflict。本地邮箱还没验证过的账号，先前可能是别人抢注的：
// 这次登录就是邮箱的所有权证明，把邮箱记成已验证，同时清掉口令并撤销它的全部设备。
func (s *Service) oauthBind(ctx context.Context, tx saas.Tx, account *saas.Account, row *saas.OAuthIdentity) error {
	if found, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
		return err
	} else if !found {
		return core.Fail(gen.ErrCredentialsInvalid)
	}
	fresh, err := tx.Account(ctx, s.tenant(), account.AccountID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.Status != saas.AccountActive {
		return core.Fail(gen.ErrAccountDisabled)
	}
	// 事务外读到的同邮箱账号，到锁住之前邮箱可能已被改走（改邮箱确认落在这个窗口里）：邮箱不再相同就不绑，
	// 否则会跳过下面「未验证则清口令、撤设备」的分支，把身份绑到邮箱已不属于它的账号上。
	if normalizeEmail(fresh.Email) != row.Email {
		return core.FailWith(gen.ErrConflict, map[string]any{"reason": "email_changed"})
	}
	existing, err := tx.OAuthIdentities(ctx, s.tenant(), account.AccountID)
	if err != nil {
		return err
	}
	for _, have := range existing {
		if have.Provider == row.Provider {
			return core.FailWith(gen.ErrConflict, map[string]any{"reason": "provider_linked"})
		}
	}
	nowMs := s.nowMs()
	if fresh.EmailVerifiedAtMs == nil {
		if err := tx.Accounts().SetEmailVerified(ctx, s.tenant(), account.AccountID, nowMs); err != nil {
			return err
		}
		if err := core.ClearPassword(ctx, s.bound(tx), account.AccountID, nowMs); err != nil {
			return err
		}
		if err := s.killPasswordReset(ctx, tx, account.AccountID, nowMs); err != nil {
			return err
		}
		if err := s.killEmailChange(ctx, tx, account.AccountID, nowMs); err != nil {
			return err
		}
	}
	row.AccountID = account.AccountID
	return tx.Accounts().InsertOAuthIdentity(ctx, s.tenant(), *row)
}

// oauthDisplayName：提供方给的名字，没有就用登录名，再没有用邮箱的本地部分；截到长度上限。
func oauthDisplayName(ident oauth.Identity, email string) string {
	name := strings.TrimSpace(ident.Name)
	if name == "" {
		name = strings.TrimSpace(ident.Login)
	}
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	if runes := []rune(name); len(runes) > displayNameMax {
		name = string(runes[:displayNameMax])
	}
	return name
}

// oauthUnlink 是 auth.oauthUnlink：要会话（账号本人）→ 这个提供方没绑过 404 → 解绑后至少还要有一种登录方式
// （口令，口令登录关闭时不算；或另一个第三方），否则 409 conflict。
func (s *Service) oauthUnlink(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		Provider string `json:"provider"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		if found, err := tx.Accounts().LockAccount(ctx, s.tenant(), account.AccountID); err != nil {
			return err
		} else if !found {
			return core.Fail(gen.ErrUnauthenticated)
		}
		fresh, err := tx.Account(ctx, s.tenant(), account.AccountID)
		if err != nil {
			return err
		}
		if fresh == nil || fresh.Status != saas.AccountActive {
			return core.Fail(gen.ErrUnauthenticated)
		}
		identities, err := tx.OAuthIdentities(ctx, s.tenant(), account.AccountID)
		if err != nil {
			return err
		}
		linked, others := false, 0
		for _, have := range identities {
			if have.Provider == in.Provider {
				linked = true
			} else {
				others++
			}
		}
		if !linked {
			return core.Fail(gen.ErrNotFound)
		}
		if others == 0 && (s.passwordOff || !fresh.Password.IsSet()) {
			return core.Fail(gen.ErrConflict)
		}
		if _, err := tx.Accounts().DeleteOAuthIdentity(ctx, s.tenant(), account.AccountID, in.Provider); err != nil {
			return err
		}
		who := audit.Security{AccountID: account.AccountID, DeviceName: "", IP: call.IP}
		return audit.WriteSecurity(ctx, tx, s.tenant(), s.nowMs(), who, audit.SecurityOAuthUnlink)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}
