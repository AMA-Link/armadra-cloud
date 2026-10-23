package accounts

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/httpx"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 运营账号的两步验证（设计 §4.9）：auth.login { scope: "operator" } 口令对了之后答 MFA 挑战，
// auth.mfaVerify 凭 TOTP 验证码换运营会话（1 小时绝对寿命，Cookie 传输下发 __Host-armadra-ops）。

// SecurityOperatorLogin 是运营会话建立时写进本人安全日志的动作。
const SecurityOperatorLogin = "security.operatorLogin"

// operatorLogin 是 scope: "operator" 的登录：非运营账号 403 forbidden；没有两步验证 403 operator_mfa_required；
// 否则存一个 5 分钟的挑战，答 { mfa: { challengeId, methods: ["totp"] } }（口令已在调用前验过）。
func (s *Service) operatorLogin(ctx context.Context, account *saas.Account, device core.DeviceInput) (any, error) {
	if !account.HasScope(saas.ScopeOperator) {
		return nil, core.Fail(gen.ErrForbidden)
	}
	has, err := s.store.Read().HasCredential(ctx, s.tenant(), account.AccountID, "totp")
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, core.Fail(gen.ErrOperatorMfaRequired)
	}
	id := core.RandomToken(32)
	challenge := admin.Challenge{AccountID: account.AccountID, Device: device, ExpiresAtMs: s.nowMs() + admin.MFAChallengeTTL.Milliseconds()}
	if err := s.mfa.Create(ctx, id, challenge); err != nil {
		return nil, err
	}
	return map[string]any{"mfa": map[string]any{"challengeId": id, "methods": []string{"totp"}}}, nil
}

// operatorAccount 重读账号：必须仍然可用且仍有运营范围。
func (s *Service) operatorAccount(ctx context.Context, accountID string) (*saas.Account, error) {
	account, err := s.store.Read().Account(ctx, s.tenant(), accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Status != saas.AccountActive || !account.HasScope(saas.ScopeOperator) {
		return nil, nil
	}
	return account, nil
}

// mfaVerify 是 auth.mfaVerify：挑战（5 分钟，每个最多错 5 次）→ 按账号限流（mfa.acct，10 分钟 10 次）→ TOTP
// 验证码（±1 步，同一时间步只认一次）→ 建运营设备（1 小时绝对寿命，刷新不延长）。
func (s *Service) mfaVerify(ctx context.Context, call *api.Call) (any, error) {
	var in struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
	}
	if err := call.Bind(&in); err != nil {
		return nil, err
	}
	challenge, err := s.mfa.Get(ctx, in.ChallengeID)
	if err != nil {
		return nil, err
	}
	nowMs := s.nowMs()
	if challenge == nil || challenge.ExpiresAtMs <= nowMs {
		if challenge != nil {
			_, _ = s.mfa.Consume(ctx, in.ChallengeID)
		}
		return nil, core.Fail(gen.ErrMfaExpired)
	}
	if err := s.limits.Check(ctx, httpx.LimitMFAAcct, challenge.AccountID); err != nil {
		return nil, err
	}
	account, err := s.operatorAccount(ctx, challenge.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		_, _ = s.mfa.Consume(ctx, in.ChallengeID)
		return nil, core.Fail(gen.ErrMfaExpired)
	}
	ok, err := admin.VerifyAccountTOTP(ctx, s.store, s.wrapper, s.tenant(), account.AccountID, in.Code, nowMs)
	if err != nil {
		return nil, err
	}
	if !ok {
		if _, err := s.mfa.Fail(ctx, in.ChallengeID); err != nil {
			return nil, err
		}
		return nil, core.Fail(gen.ErrMfaInvalid)
	}
	if consumed, err := s.mfa.Consume(ctx, in.ChallengeID); err != nil {
		return nil, err
	} else if !consumed {
		return nil, core.Fail(gen.ErrMfaExpired)
	}
	device, refresh := core.NewDevice(account.AccountID, challenge.Device, s.gate.TransportFor(call.Request), core.SessionScopeOperator, nowMs, core.OperatorSessionTTL)
	err = s.store.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Ports().Devices.Insert(ctx, s.tenant(), device, nowMs); err != nil {
			return err
		}
		who := audit.Security{AccountID: account.AccountID, DeviceName: device.Name, IP: call.IP}
		return audit.WriteSecurity(ctx, tx, s.tenant(), nowMs, who, SecurityOperatorLogin)
	})
	if err != nil {
		return nil, err
	}
	return s.respond(ctx, call, device, refresh, account)
}
