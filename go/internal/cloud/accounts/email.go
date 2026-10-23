package accounts

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 邮箱验证、找回口令与改邮箱（设计 §4.3、§6.3）。邮件里的令牌只存哈希；令牌过期答 410
// email_token_expired，其余（不存在、已用、被新令牌作废、目的不符）答 400 email_token_invalid。

// liveToken 按明文令牌找库里的记录并判活：目的必须在 purposes 里。
func (s *Service) liveToken(ctx context.Context, token string, purposes ...string) (*saas.EmailToken, error) {
	record, err := s.store.Read().EmailToken(ctx, s.tenant(), mail.HashToken(token))
	if err != nil {
		return nil, err
	}
	if record == nil || record.ConsumedAtMs != nil {
		return nil, core.Fail(gen.ErrEmailTokenInvalid)
	}
	matched := false
	for _, purpose := range purposes {
		matched = matched || record.Purpose == purpose
	}
	if !matched {
		return nil, core.Fail(gen.ErrEmailTokenInvalid)
	}
	if record.ExpiresAtMs <= s.nowMs() {
		return nil, core.Fail(gen.ErrEmailTokenExpired)
	}
	return record, nil
}

// consume 在已锁账号的事务里一步消耗令牌（条件 UPDATE 同时核对目的、未用、未过期）：别的请求抢先用掉、
// 刚好过期、目的不符都按无效答，事务随之回滚。
func (s *Service) consume(ctx context.Context, tx saas.Tx, record *saas.EmailToken) (*saas.Account, error) {
	found, err := tx.Accounts().LockAccount(ctx, s.tenant(), record.AccountID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, core.Fail(gen.ErrEmailTokenInvalid)
	}
	consumed, err := tx.Accounts().ConsumeEmailToken(ctx, s.tenant(), record.TokenHash, record.Purpose, s.nowMs())
	if err != nil {
		return nil, err
	}
	account, err := tx.Account(ctx, s.tenant(), record.AccountID)
	if err != nil {
		return nil, err
	}
	if consumed == nil || consumed.AccountID != record.AccountID || account == nil || account.Status != saas.AccountActive {
		return nil, core.Fail(gen.ErrEmailTokenInvalid)
	}
	return account, nil
}

// killEmailChange 作废账号所有「改邮箱确认」：已发出的令牌记已用，排队还没发的邮件删掉。
func (s *Service) killEmailChange(ctx context.Context, tx saas.Tx, accountID string, nowMs int64) error {
	if _, err := tx.Accounts().InvalidateEmailTokens(ctx, s.tenant(), accountID, saas.EmailPurposeChange, nowMs); err != nil {
		return err
	}
	_, err := tx.Jobs().CancelUnsentMail(ctx, s.tenant(), accountID, mail.EmailChange)
	return err
}

// killPasswordReset 作废账号所有找回口令的链接：已发出的令牌记已用，排队还没发的邮件删掉。
func (s *Service) killPasswordReset(ctx context.Context, tx saas.Tx, accountID string, nowMs int64) error {
	if _, err := tx.Accounts().InvalidateEmailTokens(ctx, s.tenant(), accountID, saas.EmailPurposeReset, nowMs); err != nil {
		return err
	}
	_, err := tx.Jobs().CancelUnsentMail(ctx, s.tenant(), accountID, mail.PasswordReset)
	return err
}

// emailVerify 是 auth.emailVerify：验证邮箱（verify）或确认新邮箱（change）。
func (s *Service) emailVerify(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Token string `json:"token"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTokenIP, call.IP); err != nil {
		return nil, err
	}
	record, err := s.liveToken(ctx, in.Token, saas.EmailPurposeVerify, saas.EmailPurposeChange)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	var email string
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		account, err := s.consume(ctx, tx, record)
		if err != nil {
			return err
		}
		who := audit.Security{AccountID: account.AccountID, IP: call.IP}
		switch record.Purpose {
		case saas.EmailPurposeVerify:
			// 令牌发出后邮箱又改过：这枚令牌验证的是旧地址，作废。
			if account.Email != record.Email {
				return core.Fail(gen.ErrEmailTokenInvalid)
			}
			if err := tx.Accounts().SetEmailVerified(ctx, s.tenant(), account.AccountID, nowMs); err != nil {
				return err
			}
			if _, err := tx.Accounts().InvalidateEmailTokens(ctx, s.tenant(), account.AccountID, saas.EmailPurposeVerify, nowMs); err != nil {
				return err
			}
			email = account.Email
			return audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityEmailVerified)
		default:
			// 改邮箱：新地址再查唯一（被占用答 409 email_taken，事务回滚、令牌留着）。
			if err := tx.Accounts().ChangeEmail(ctx, s.tenant(), account.AccountID, record.Email, nowMs); err != nil {
				return err
			}
			// 同账号同目的的其余令牌一并作废；发往旧地址的验证令牌也不再有意义；
			// 寄往旧地址的找回口令链接同样作废（旧地址不再是账号的邮箱）。
			if err := s.killEmailChange(ctx, tx, account.AccountID, nowMs); err != nil {
				return err
			}
			if _, err := tx.Accounts().InvalidateEmailTokens(ctx, s.tenant(), account.AccountID, saas.EmailPurposeVerify, nowMs); err != nil {
				return err
			}
			if err := s.killPasswordReset(ctx, tx, account.AccountID, nowMs); err != nil {
				return err
			}
			email = record.Email
			if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityEmailChanged); err != nil {
				return err
			}
			return s.mail.Enqueue(ctx, tx, mail.Mail{
				Template: mail.EmailChanged, Locale: account.Locale, To: account.Email,
				Params: mail.Params{DisplayName: greetName(account), NewEmail: record.Email},
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"email": email, "emailVerified": true}, nil
}

// emailVerifyResend 是 auth.emailVerifyResend：已验证答 409 conflict；按账号每小时 3 次。
func (s *Service) emailVerifyResend(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if account.EmailVerifiedAtMs != nil {
		return nil, core.Fail(gen.ErrConflict)
	}
	if err := s.limits.Check(ctx, httpx.LimitVerifyAcct, account.AccountID); err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		return s.mail.Enqueue(ctx, tx, mail.Mail{
			Template: mail.VerifyEmail, Locale: account.Locale, To: account.Email, AccountID: account.AccountID,
			Params: mail.Params{DisplayName: greetName(account)},
		})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// passwordResetStart 是 auth.passwordResetStart：总答成功，不泄露邮箱是否注册。
// 次序：按 IP 限流 → 挑战 → 按邮箱限流 → 查账号 → 事务插发件。存在且未停用的账号插真发件，
// 其余插一行占位发件（Decoy）：两条路径做同样的库读写，答复与时延一致，占位行由 mail.send 立刻丢弃。
func (s *Service) passwordResetStart(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Email     string               `json:"email"`
		Challenge *core.ChallengeToken `json:"challenge"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitResetIP, call.IP); err != nil {
		return nil, err
	}
	if err := s.c.Challenge.Check(ctx, core.ScopeResetStart, in.Challenge, call.IP); err != nil {
		return nil, err
	}
	email := normalizeEmail(in.Email)
	if err := s.limits.Check(ctx, httpx.LimitResetEmail, email); err != nil {
		return nil, err
	}
	account, err := s.store.Read().AccountByEmail(ctx, s.tenant(), email)
	if err != nil {
		return nil, err
	}
	letter := mail.Mail{Template: mail.PasswordReset, Locale: localeOf(call.Request), To: email, Params: mail.Params{Decoy: true}}
	if account != nil && account.Status == saas.AccountActive {
		letter = mail.Mail{
			Template: mail.PasswordReset, Locale: account.Locale, To: account.Email, AccountID: account.AccountID,
			Params: mail.Params{DisplayName: greetName(account)},
		}
	}
	err = s.store.Tx(ctx, func(tx saas.Tx) error { return s.mail.Enqueue(ctx, tx, letter) })
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// passwordResetFinish 是 auth.passwordResetFinish：令牌过口令策略 → 换口令、清锁、撤销全部设备（经撤销钩子）→
// 不答会话。口令策略不过时事务回滚，令牌仍可用。
func (s *Service) passwordResetFinish(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitTokenIP, call.IP); err != nil {
		return nil, err
	}
	record, err := s.liveToken(ctx, in.Token, saas.EmailPurposeReset)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		account, err := s.consume(ctx, tx, record)
		if err != nil {
			return err
		}
		// 令牌发往的地址必须仍是账号当前的邮箱（中间改过邮箱的话，旧地址收到的链接不能再用来重置口令）。
		if account.Email != record.Email {
			return core.Fail(gen.ErrEmailTokenInvalid)
		}
		// 撤销钩子在提交之后才投递；core 的低层函数全部走本事务的端口。
		if err := core.SetPasswordNames(ctx, s.bound(tx), account.AccountID, policyNames(account.Email, account.DisplayName), in.NewPassword, nowMs, ""); err != nil {
			return err
		}
		if err := s.killPasswordReset(ctx, tx, account.AccountID, nowMs); err != nil {
			return err
		}
		// 找回成功也让之前发出的「改邮箱确认」链接作废：它们可能落在别人手里。
		if err := s.killEmailChange(ctx, tx, account.AccountID, nowMs); err != nil {
			return err
		}
		if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, audit.Security{AccountID: account.AccountID, IP: call.IP}, audit.SecurityPasswordReset); err != nil {
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
	return map[string]any{}, nil
}

// meEmailChange 是 me.emailChange：口令比对（计入锁定）→ 新地址未被占用 → 给新地址插确认邮件；
// 验证前旧邮箱照常生效。按账号每小时 3 次（与重发验证邮件同一个桶）。
func (s *Service) meEmailChange(ctx context.Context, call *api.Call) (any, error) {
	caller, err := call.Caller(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		NewEmail string `json:"newEmail"`
		Password string `json:"password"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	account, err := s.owner(ctx, caller)
	if err != nil {
		return nil, err
	}
	if err := s.limits.Check(ctx, httpx.LimitVerifyAcct, account.AccountID); err != nil {
		return nil, err
	}
	if err := core.VerifyPassword(ctx, s.c, &account.AccountRecord, in.Password, s.nowMs()); err != nil {
		return nil, err
	}
	newEmail := normalizeEmail(in.NewEmail)
	if newEmail == account.Email {
		return nil, core.Fail(gen.ErrBadRequest)
	}
	taken, err := s.store.Read().AccountByEmail(ctx, s.tenant(), newEmail)
	if err != nil {
		return nil, err
	}
	if taken != nil {
		return nil, core.Fail(gen.ErrEmailTaken)
	}
	nowMs := s.nowMs()
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		who := audit.Security{AccountID: account.AccountID, DeviceName: "", IP: call.IP}
		if err := audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, audit.SecurityEmailChangeAsked); err != nil {
			return err
		}
		return s.mail.Enqueue(ctx, tx, mail.Mail{
			Template: mail.EmailChange, Locale: account.Locale, To: newEmail, AccountID: account.AccountID,
			Params: mail.Params{DisplayName: greetName(account)},
		})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// auditDeviceDecision 是缺省的设备码决定钩子：批准或拒绝都写一条账号安全日志（设备名取发起设备报的名字）。
func (s *Service) auditDeviceDecision(ctx context.Context, tx saas.Tx, decision DeviceDecision) error {
	action := audit.SecurityDeviceDeny
	if decision.Approved {
		action = audit.SecurityDeviceApprove
	}
	return audit.WriteSecurity(ctx, tx, s.tenant(), s.nowMs(), audit.Security{AccountID: decision.AccountID, DeviceName: decision.Name}, action)
}

// greetName 是邮件里的称呼：只有邮箱已验证的账号才带显示名——显示名是注册者自己填的任意文字，
// 发给还没验证的地址就成了陌生人借我们的邮件说话。
func greetName(account *saas.Account) string {
	if account.EmailVerifiedAtMs == nil {
		return ""
	}
	return mail.SanitizeLabel(account.DisplayName, 64)
}
