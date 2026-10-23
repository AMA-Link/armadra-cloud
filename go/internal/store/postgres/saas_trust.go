package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 新设备待信任请求的 PostgreSQL 实现（迁移 0007 trust_requests；设计 §4.7）。没有校验码。

const trustColumns = `request_id, source_id, pending_id, COALESCE(team_id, ''), account_id, device_id, device_name, platform, state,
	requested_at_ms, expires_at_ms, resolved_at_ms, COALESCE(resolved_by, '')`

func scanTrust(row scanner) (saas.TrustRequest, error) {
	var t saas.TrustRequest
	err := row.Scan(&t.RequestID, &t.SourceID, &t.PendingID, &t.TeamID, &t.AccountID, &t.DeviceID, &t.DeviceName, &t.Platform, &t.State,
		&t.RequestedAtMs, &t.ExpiresAtMs, &t.ResolvedAtMs, &t.ResolvedBy)
	return t, err
}

// trustViewSelect 联出通知要用的信息：主机名与所有者、请求者的邮箱与显示名、查询者的团队角色和团队设置。
// 只含主机未撤销、团队未在恢复期、请求者仍是团队成员的请求。
const trustViewSelect = `SELECT t.request_id, t.source_id, t.pending_id, COALESCE(t.team_id, ''), t.account_id, t.device_id, t.device_name,
	  t.platform, t.state, t.requested_at_ms, t.expires_at_ms, t.resolved_at_ms, COALESCE(t.resolved_by, ''),
	  s.name, COALESCE(p.owner_account_id, ''), a.email, a.display_name, rm.role, COALESCE(vm.role, ''), tm.settings::text
	FROM trust_requests t
	JOIN sources s ON s.tenant_id = t.tenant_id AND s.source_id = t.source_id AND s.revoked_at_ms IS NULL
	JOIN source_spaces p ON p.tenant_id = t.tenant_id AND p.source_id = t.source_id AND p.team_id IS NOT DISTINCT FROM t.team_id
	JOIN teams tm ON tm.tenant_id = t.tenant_id AND tm.team_id = t.team_id AND tm.deleted_at_ms IS NULL
	JOIN team_members rm ON rm.tenant_id = t.tenant_id AND rm.team_id = t.team_id AND rm.account_id = t.account_id
	JOIN accounts a ON a.tenant_id = t.tenant_id AND a.account_id = t.account_id
	LEFT JOIN team_members vm ON vm.tenant_id = t.tenant_id AND vm.team_id = t.team_id AND vm.account_id = $2`

func scanTrustView(row scanner) (saas.TrustView, error) {
	var (
		v         saas.TrustView
		role      string
		requester string
		settings  string
	)
	err := row.Scan(&v.RequestID, &v.SourceID, &v.PendingID, &v.TeamID, &v.AccountID, &v.DeviceID, &v.DeviceName, &v.Platform,
		&v.State, &v.RequestedAtMs, &v.ExpiresAtMs, &v.ResolvedAtMs, &v.ResolvedBy, &v.SourceName, &v.SourceOwnerID,
		&v.MemberEmail, &v.MemberName, &requester, &role, &settings)
	v.RequesterRole = saas.TeamRole(requester)
	v.ViewerRole = saas.TeamRole(role)
	v.TeamSettings = []byte(settings)
	return v, err
}

func (r reader) TrustView(ctx context.Context, tenantID, requestID, viewerID string) (*saas.TrustView, error) {
	v, err := scanTrustView(r.q.QueryRow(ctx, trustViewSelect+" WHERE t.tenant_id = $1 AND t.request_id = $3", tenantID, viewerID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r reader) PendingTrust(ctx context.Context, tenantID, viewerID, teamID string, nowMs int64, limit int) ([]saas.TrustView, error) {
	return list(ctx, r.q, scanTrustView, trustViewSelect+`
		WHERE t.tenant_id = $1 AND vm.account_id IS NOT NULL AND ($3 = '' OR t.team_id = $3)
		  AND t.state = 'pending' AND t.expires_at_ms > $4
		  AND (
		    (p.owner_account_id = $2 AND vm.role IN ('owner', 'admin', 'member'))
		    OR (rm.role <> 'guest' AND vm.role IN ('owner', 'admin')
		        AND COALESCE(tm.settings->>'deviceEndorsers', 'owner_admin') = 'owner_admin')
		    OR (rm.role IN ('owner', 'admin', 'member') AND t.account_id = $2
		        AND COALESCE((tm.settings->>'memberSelfEndorse')::boolean, true))
		  )
		ORDER BY t.requested_at_ms DESC, t.request_id LIMIT $5`, tenantID, viewerID, teamID, nowMs, limit)
}

type trustRepo struct{ q querier }

func (r trustRepo) Lock(ctx context.Context, tenantID, requestID string) (*saas.TrustRequest, error) {
	t, err := scanTrust(r.q.QueryRow(ctx,
		"SELECT "+trustColumns+" FROM trust_requests WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE", tenantID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r trustRepo) ByPending(ctx context.Context, tenantID, sourceID, pendingID string) (*saas.TrustRequest, error) {
	t, err := scanTrust(r.q.QueryRow(ctx,
		"SELECT "+trustColumns+" FROM trust_requests WHERE tenant_id = $1 AND source_id = $2 AND pending_id = $3 FOR UPDATE",
		tenantID, sourceID, pendingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r trustRepo) LastRejected(ctx context.Context, tenantID, sourceID, deviceID string) (*saas.TrustRequest, error) {
	t, err := scanTrust(r.q.QueryRow(ctx,
		"SELECT "+trustColumns+` FROM trust_requests WHERE tenant_id = $1 AND source_id = $2 AND device_id = $3 AND state = 'rejected'
		 ORDER BY resolved_at_ms DESC, request_id LIMIT 1`, tenantID, sourceID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r trustRepo) ExpireStale(ctx context.Context, tenantID, sourceID, deviceID string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		`UPDATE trust_requests SET state = 'expired', resolved_at_ms = $1, resolved_by = 'system'
		 WHERE tenant_id = $2 AND source_id = $3 AND device_id = $4 AND state = 'pending' AND expires_at_ms <= $1`,
		nowMs, tenantID, sourceID, deviceID)
	return err
}

func (r trustRepo) Supersede(ctx context.Context, tenantID, sourceID, deviceID string, nowMs int64) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `UPDATE trust_requests SET state = 'expired', resolved_at_ms = $1, resolved_by = 'superseded'
	    WHERE tenant_id = $2 AND source_id = $3 AND device_id = $4 AND state = 'pending' RETURNING request_id`,
		nowMs, tenantID, sourceID, deviceID)
}

func (r trustRepo) Insert(ctx context.Context, tenantID string, t saas.TrustRequest) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO trust_requests (tenant_id, request_id, source_id, pending_id, team_id, account_id, device_id, device_name, platform,
		   state, requested_at_ms, expires_at_ms)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, 'pending', $10, $11)`,
		tenantID, t.RequestID, t.SourceID, t.PendingID, t.TeamID, t.AccountID, t.DeviceID, t.DeviceName, t.Platform,
		t.RequestedAtMs, t.ExpiresAtMs)
	return err
}

func (r trustRepo) Reopen(ctx context.Context, tenantID, requestID, newRequestID, teamID, deviceName, platform string, nowMs, expiresAtMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`UPDATE trust_requests SET request_id = $1, state = 'pending', requested_at_ms = $2, expires_at_ms = $3,
		   resolved_at_ms = NULL, resolved_by = NULL, team_id = NULLIF($4, ''), device_name = $5, platform = $6
		 WHERE tenant_id = $7 AND request_id = $8 AND state = 'rejected'`,
		newRequestID, nowMs, expiresAtMs, teamID, deviceName, platform, tenantID, requestID)
	return tag.RowsAffected() == 1, err
}

func (r trustRepo) Resolve(ctx context.Context, tenantID, requestID, state, resolvedBy string, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`UPDATE trust_requests SET state = $1, resolved_at_ms = $2, resolved_by = NULLIF($3, '')
		 WHERE tenant_id = $4 AND request_id = $5 AND state = 'pending'`, state, nowMs, resolvedBy, tenantID, requestID)
	return tag.RowsAffected() == 1, err
}

func (r trustRepo) ExpireDue(ctx context.Context, tenantID string, nowMs int64) ([]saas.TrustRequest, error) {
	return list(ctx, r.q, scanTrust,
		`UPDATE trust_requests SET state = 'expired', resolved_at_ms = $1, resolved_by = 'system'
		 WHERE tenant_id = $2 AND state = 'pending' AND expires_at_ms <= $1 RETURNING `+trustColumns, nowMs, tenantID)
}

func (r trustRepo) GC(ctx context.Context, tenantID string, beforeMs int64) (int, error) {
	tag, err := r.q.Exec(ctx,
		"DELETE FROM trust_requests WHERE tenant_id = $1 AND state <> 'pending' AND resolved_at_ms < $2", tenantID, beforeMs)
	return int(tag.RowsAffected()), err
}
