package postgres

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

type deviceCodeRepo struct{ q querier }

const deviceCodeColumns = `device_code_hash, user_code, status, platform, name, push_public_key, account_id,
  coalesce(ip_prefix, ''), created_at_ms, expires_at_ms, last_poll_at_ms, decided_at_ms`

func toDeviceCode(row scanner) (*saas.DeviceCode, error) {
	var c saas.DeviceCode
	err := row.Scan(&c.DeviceCodeHash, &c.UserCode, &c.Status, &c.Platform, &c.Name, &c.PushPublicKey, &c.AccountID,
		&c.IPPrefix, &c.CreatedAtMs, &c.ExpiresAtMs, &c.LastPollAtMs, &c.DecidedAtMs)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r deviceCodeRepo) Create(ctx context.Context, tenantID string, c saas.DeviceCode, nowMs int64) (bool, error) {
	// 过了期还停在 pending / approved 的行占着唯一索引：先落成 expired，别让它挡住新的用户码。
	if _, err := r.q.Exec(ctx,
		`UPDATE device_codes SET status = 'expired'
		 WHERE tenant_id = $1 AND user_code = $2 AND status IN ('pending', 'approved') AND expires_at_ms <= $3`,
		tenantID, c.UserCode, nowMs); err != nil {
		return false, err
	}
	tag, err := r.q.Exec(ctx,
		`INSERT INTO device_codes (tenant_id, device_code_hash, user_code, status, platform, name, push_public_key,
		   ip_prefix, created_at_ms, expires_at_ms)
		 VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $8, $9)
		 ON CONFLICT DO NOTHING`,
		tenantID, c.DeviceCodeHash, c.UserCode, c.Platform, c.Name, c.PushPublicKey, c.IPPrefix, c.CreatedAtMs, c.ExpiresAtMs)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r deviceCodeRepo) Get(ctx context.Context, tenantID, hash string) (*saas.DeviceCode, error) {
	return one(toDeviceCode(r.q.QueryRow(ctx,
		"SELECT "+deviceCodeColumns+" FROM device_codes WHERE tenant_id = $1 AND device_code_hash = $2 FOR UPDATE",
		tenantID, hash)))
}

func (r deviceCodeRepo) GetByUserCode(ctx context.Context, tenantID, userCode string) (*saas.DeviceCode, error) {
	return one(toDeviceCode(r.q.QueryRow(ctx,
		"SELECT "+deviceCodeColumns+` FROM device_codes WHERE tenant_id = $1 AND user_code = $2
		 ORDER BY created_at_ms DESC, device_code_hash LIMIT 1 FOR UPDATE`,
		tenantID, userCode)))
}

func (r deviceCodeRepo) Update(ctx context.Context, tenantID string, c saas.DeviceCode) error {
	_, err := r.q.Exec(ctx,
		`UPDATE device_codes SET status = $1, account_id = $2, last_poll_at_ms = $3, decided_at_ms = $4
		 WHERE tenant_id = $5 AND device_code_hash = $6`,
		c.Status, c.AccountID, c.LastPollAtMs, c.DecidedAtMs, tenantID, c.DeviceCodeHash)
	return err
}

func (r deviceCodeRepo) DeleteExpiredBefore(ctx context.Context, tenantID string, beforeMs int64) (int64, error) {
	tag, err := r.q.Exec(ctx, "DELETE FROM device_codes WHERE tenant_id = $1 AND expires_at_ms < $2", tenantID, beforeMs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
