package postgres

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 签发索引与撤销表的 PostgreSQL 实现（迁移 0009 cloud_issued_tokens、0002 relay_revoked_tokens；设计 §7.2）。
// 签发索引的查询、重放、清理一律带 tenant_id = $1，不跨租户扫表；撤销表只有 jti 与到期时间。

type issuedRepo struct{ q querier }

func scanIssued(row scanner) (saas.IssuedToken, error) {
	var t saas.IssuedToken
	err := row.Scan(&t.JTI, &t.AccountID, &t.SourceID, &t.DeviceID, &t.ExpMs)
	return t, err
}

func (r issuedRepo) Insert(ctx context.Context, tenantID string, t saas.IssuedToken) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO cloud_issued_tokens (tenant_id, jti, account_id, source_id, device_id, exp_ms)
		 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (tenant_id, jti) DO NOTHING`,
		tenantID, t.JTI, t.AccountID, t.SourceID, t.DeviceID, t.ExpMs)
	return err
}

func (r issuedRepo) Revoke(ctx context.Context, tenantID string, f saas.IssuedFilter, nowMs int64) ([]saas.IssuedToken, error) {
	if f.Empty() {
		return nil, nil
	}
	hit, err := list(ctx, r.q, scanIssued,
		`DELETE FROM cloud_issued_tokens WHERE tenant_id = $1 AND exp_ms > $2
		   AND ($3 = '' OR jti = $3) AND ($4 = '' OR account_id = $4)
		   AND ($5 = '' OR account_id LIKE $5 || '%') AND ($6 = '' OR source_id = $6)
		   AND ($7 = '' OR device_id = $7) AND ($8 = '' OR device_id <> $8)
		 RETURNING jti, account_id, source_id, device_id, exp_ms`,
		tenantID, nowMs, f.JTI, f.AccountID, likePrefix(f.AccountPrefix), f.SourceID, f.DeviceID, f.KeepDeviceID)
	if err != nil {
		return nil, err
	}
	for _, token := range hit {
		if _, err := r.q.Exec(ctx,
			"INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ($1, $2) ON CONFLICT (jti) DO NOTHING", token.JTI, token.ExpMs); err != nil {
			return nil, err
		}
	}
	return hit, nil
}

// likePrefix 把前缀里的 LIKE 通配符转义（配合 account_id LIKE $5 || '%'）。
func likePrefix(prefix string) string { return likeEscaper.Replace(prefix) }

func (r issuedRepo) RevokeToken(ctx context.Context, jti string, expMs int64) error {
	_, err := r.q.Exec(ctx, "INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ($1, $2) ON CONFLICT (jti) DO NOTHING", jti, expMs)
	return err
}

func (r issuedRepo) Active(ctx context.Context, tenantID string, nowMs int64) ([]saas.IssuedToken, error) {
	return list(ctx, r.q, scanIssued,
		`SELECT jti, account_id, source_id, device_id, exp_ms FROM cloud_issued_tokens
		 WHERE tenant_id = $1 AND exp_ms > $2 ORDER BY exp_ms, jti`, tenantID, nowMs)
}

func (r issuedRepo) Revoked(ctx context.Context, nowMs int64) ([]saas.IssuedToken, error) {
	return list(ctx, r.q, func(s scanner) (saas.IssuedToken, error) {
		var t saas.IssuedToken
		return t, s.Scan(&t.JTI, &t.ExpMs)
	}, "SELECT jti, exp_ms FROM relay_revoked_tokens WHERE exp_ms > $1 ORDER BY exp_ms, jti", nowMs)
}

func (r issuedRepo) GC(ctx context.Context, tenantID string, nowMs int64) (int, error) {
	issued, err := r.q.Exec(ctx, "DELETE FROM cloud_issued_tokens WHERE tenant_id = $1 AND exp_ms < $2", tenantID, nowMs)
	if err != nil {
		return 0, err
	}
	revoked, err := r.q.Exec(ctx, "DELETE FROM relay_revoked_tokens WHERE exp_ms < $1", nowMs)
	return int(issued.RowsAffected() + revoked.RowsAffected()), err
}
