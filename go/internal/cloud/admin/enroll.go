package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 命令行（operators add / totp）的业务：直接对库，不经 HTTP。运营身份只能这样设置，接口改不了。
var (
	// ErrNoAccount 表示邮箱没有对应的账号。
	ErrNoAccount = errors.New("没有这个账号")
	// ErrNotOperator 表示账号还不是运营账号（先 operators add）。
	ErrNotOperator = errors.New("这个账号还不是运营账号，先运行 armadra-cloud operators add")
	// ErrTOTPExists 表示账号已经登记过两步验证。
	ErrTOTPExists = errors.New("这个账号已经登记过两步验证；要换种子请加 --reset")
)

// AddOperator 给邮箱对应的账号加运营范围（幂等），写一条系统审计。
func AddOperator(ctx context.Context, db saas.Store, tenantID, email string, nowMs int64) (accountID string, err error) {
	account, err := db.Read().AccountByEmail(ctx, tenantID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return "", err
	}
	if account == nil {
		return "", ErrNoAccount
	}
	err = db.Tx(ctx, func(tx saas.Tx) error {
		if _, err := tx.Accounts().LockAccount(ctx, tenantID, account.AccountID); err != nil {
			return err
		}
		if _, err := tx.Ops().AddScope(ctx, tenantID, account.AccountID, saas.ScopeOperator, nowMs); err != nil {
			return err
		}
		return audit.Write(ctx, tx, tenantID, nowMs, saas.AuditEntry{
			ActorKind: saas.ActorSystem, Action: "operator.add", TargetKind: "account", TargetID: account.AccountID,
		})
	})
	return account.AccountID, err
}

// EnrollTOTP 为运营账号登记两步验证：生成 20 字节种子，用主密钥包裹后落库，答 otpauth 地址（调用方只在终端打印一次）。
// 已登记过的拒绝，除非 reset（换种子：旧的作废，之前的登录挑战也就验不过了）。
func EnrollTOTP(ctx context.Context, db saas.Store, wrapper *store.Wrapper, tenantID, email, issuer string, reset bool, nowMs int64) (uri string, err error) {
	account, err := db.Read().AccountByEmail(ctx, tenantID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return "", err
	}
	if account == nil {
		return "", ErrNoAccount
	}
	if !account.HasScope(saas.ScopeOperator) {
		return "", ErrNotOperator
	}
	seed := NewTOTPSeed()
	wrap := wrapper.SealSecret(seed, TOTPAAD(tenantID, account.AccountID))
	err = db.Tx(ctx, func(tx saas.Tx) error {
		if _, err := tx.Accounts().LockAccount(ctx, tenantID, account.AccountID); err != nil {
			return err
		}
		existing, err := tx.TOTP(ctx, tenantID, account.AccountID)
		if err != nil {
			return err
		}
		if existing != nil && !reset {
			return ErrTOTPExists
		}
		if err := tx.Ops().PutTOTP(ctx, tenantID, account.AccountID, core.RandomToken(16), wrap, nowMs); err != nil {
			return err
		}
		return audit.Write(ctx, tx, tenantID, nowMs, saas.AuditEntry{
			ActorKind: saas.ActorSystem, Action: "operator.totp", TargetKind: "account", TargetID: account.AccountID,
		})
	})
	if err != nil {
		return "", err
	}
	return OTPAuthURI(issuer, account.Email, seed), nil
}

// VerifyAccountTOTP 校验账号的两步验证码：验证码对且时间步比上次用过的更大才算过（同一个码不能用第二次）。
// 没有登记、码不对、重放都答 false；库或密钥出问题答错误。
func VerifyAccountTOTP(ctx context.Context, db saas.Store, wrapper *store.Wrapper, tenantID, accountID, code string, nowMs int64) (bool, error) {
	cred, err := db.Read().TOTP(ctx, tenantID, accountID)
	if err != nil || cred == nil {
		return false, err
	}
	seed, err := wrapper.OpenSecret(cred.SecretWrap, TOTPAAD(tenantID, accountID))
	if err != nil {
		return false, err
	}
	step, ok := VerifyTOTP(seed, code, time.UnixMilli(nowMs))
	if !ok {
		return false, nil
	}
	var accepted bool
	err = db.Tx(ctx, func(tx saas.Tx) (err error) {
		accepted, err = tx.Ops().AcceptTOTPStep(ctx, tenantID, accountID, step, nowMs)
		return err
	})
	return accepted, err
}
