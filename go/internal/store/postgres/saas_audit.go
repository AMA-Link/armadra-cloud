package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 审计与安全日志的读取、保留期清理、用量统计（设计 §4.9、§4.10、§6.1，迁移 0008、0009）。
// 读取一律带 tenant_id，团队审计另带 team_id（只收 TeamScope），账号安全日志只按本人 account_id。

const auditColumns = `at_ms, event_id, COALESCE(team_id, ''), COALESCE(account_id, ''), actor_kind, COALESCE(actor_id, ''),
  action, COALESCE(target_kind, ''), COALESCE(target_id, ''), COALESCE(source_id, ''), COALESCE(jti, ''),
  COALESCE(ip_prefix, ''), COALESCE(device_name, ''), COALESCE(reason, '')`

func scanAudit(row scanner) (saas.AuditEntry, error) {
	var e saas.AuditEntry
	err := row.Scan(&e.AtMs, &e.EventID, &e.TeamID, &e.AccountID, &e.ActorKind, &e.ActorID, &e.Action,
		&e.TargetKind, &e.TargetID, &e.SourceID, &e.JTI, &e.IPPrefix, &e.DeviceName, &e.Reason)
	return e, err
}

// auditFilter 把筛选拼成 WHERE 的后半段（参数从 $first 起编号）。
func auditFilter(q saas.AuditQuery, first int) (string, []any) {
	var (
		where strings.Builder
		args  []any
	)
	add := func(format string, value any) {
		args = append(args, value)
		fmt.Fprintf(&where, " AND "+format, first+len(args)-1)
	}
	if q.FromMs > 0 {
		add("at_ms >= $%d", q.FromMs)
	}
	if q.ToMs > 0 {
		add("at_ms < $%d", q.ToMs)
	}
	if q.Actor != "" {
		add("actor_id = $%d", q.Actor)
	}
	if q.SourceID != "" {
		add("source_id = $%d", q.SourceID)
	}
	if q.ActionPrefix != "" {
		add("action LIKE $%d || '%%'", likePrefix(q.ActionPrefix))
	}
	if q.After != nil {
		args = append(args, q.After.AtMs, q.After.EventID)
		fmt.Fprintf(&where, " AND (at_ms, event_id) < ($%d, $%d)", first+len(args)-2, first+len(args)-1)
	}
	return where.String(), args
}

func auditLimit(q saas.AuditQuery, args []any) (string, []any) {
	if q.Limit <= 0 {
		return "", args
	}
	args = append(args, q.Limit)
	return fmt.Sprintf(" LIMIT $%d", len(args)), args
}

func (r auditRepo) ListTeam(ctx context.Context, scope saas.TeamScope, q saas.AuditQuery) ([]saas.AuditEntry, error) {
	where, args := auditFilter(q, 3)
	args = append([]any{scope.TenantID(), scope.TeamID()}, args...)
	limit, args := auditLimit(q, args)
	return list(ctx, r.q, scanAudit,
		"SELECT "+auditColumns+" FROM audit_log WHERE tenant_id = $1 AND team_id = $2"+where+
			" ORDER BY at_ms DESC, event_id DESC"+limit, args...)
}

func (r auditRepo) ListSecurity(ctx context.Context, tenantID, accountID string, q saas.AuditQuery) ([]saas.AuditEntry, error) {
	// 安全日志只按本人和动作前缀取：Actor、SourceID 对它没有意义，忽略。
	q.Actor, q.SourceID = "", ""
	where, args := auditFilter(q, 3)
	args = append([]any{tenantID, accountID}, args...)
	limit, args := auditLimit(q, args)
	return list(ctx, r.q, scanAudit,
		"SELECT "+auditColumns+` FROM audit_log WHERE tenant_id = $1 AND account_id = $2 AND team_id IS NULL
		 AND action LIKE 'security.%'`+where+" ORDER BY at_ms DESC, event_id DESC"+limit, args...)
}

// ---- 保留期 ----

type retentionRepo struct{ q querier }

func (r retentionRepo) TeamsWithAuditBefore(ctx context.Context, tenantID string, beforeMs int64, afterTeamID string, limit int) ([]saas.RetentionTeam, error) {
	return list(ctx, r.q, func(s scanner) (saas.RetentionTeam, error) {
		var t saas.RetentionTeam
		return t, s.Scan(&t.TeamID, &t.Plan)
	}, `SELECT t.team_id, COALESCE(e.plan, 'free') FROM teams t
	    LEFT JOIN entitlements e ON e.tenant_id = t.tenant_id AND e.space_kind = 'team' AND e.space_id = t.team_id
	    WHERE t.tenant_id = $1 AND t.team_id > $3
	      AND EXISTS (SELECT 1 FROM audit_log a WHERE a.tenant_id = t.tenant_id AND a.team_id = t.team_id AND a.at_ms < $2)
	    ORDER BY t.team_id LIMIT $4`, tenantID, beforeMs, afterTeamID, limit)
}

func (r retentionRepo) PruneTeamAudit(ctx context.Context, tenantID, teamID string, beforeMs int64, limit int) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM audit_log WHERE (at_ms, event_id) IN (
		   SELECT at_ms, event_id FROM audit_log WHERE tenant_id = $1 AND team_id = $2 AND at_ms < $3 LIMIT $4)
		 AND tenant_id = $1 AND team_id = $2`, tenantID, teamID, beforeMs, limit)
	return int(tag.RowsAffected()), err
}

func (r retentionRepo) PruneSecurityLog(ctx context.Context, tenantID string, beforeMs int64, limit int) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM audit_log WHERE (at_ms, event_id) IN (
		   SELECT at_ms, event_id FROM audit_log WHERE tenant_id = $1 AND team_id IS NULL
		     AND action LIKE 'security.%' AND at_ms < $2 LIMIT $3)
		 AND tenant_id = $1 AND team_id IS NULL AND action LIKE 'security.%'`, tenantID, beforeMs, limit)
	return int(tag.RowsAffected()), err
}

func (r retentionRepo) PruneTraffic(ctx context.Context, tenantID string, beforeMs int64, limit int) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM relay_traffic_buckets WHERE ctid = ANY(ARRAY(
		   SELECT ctid FROM relay_traffic_buckets WHERE tenant_id = $1 AND bucket_start_ms < $2 LIMIT $3))
		 AND tenant_id = $1 AND bucket_start_ms < $2`, tenantID, beforeMs, limit)
	return int(tag.RowsAffected()), err
}

// ---- 用量 ----

const dayMs = int64(24 * 60 * 60 * 1000)

func scanDay(row scanner) (saas.DayBytes, error) {
	var d saas.DayBytes
	err := row.Scan(&d.StartMs, &d.Bytes)
	return d, err
}

func (r teamRepo) TrafficDays(ctx context.Context, scope saas.TeamScope, startMs, endMs int64) ([]saas.DayBytes, error) {
	return list(ctx, r.q, scanDay,
		`SELECT (bucket_start_ms / $5 * $5)::bigint AS day, sum(bytes_to_source + bytes_to_client)::bigint
		 FROM relay_traffic_buckets WHERE tenant_id = $1 AND team_id = $2 AND kind = 'e2e'
		   AND bucket_start_ms >= $3 AND bucket_start_ms < $4 GROUP BY day ORDER BY day`,
		scope.TenantID(), scope.TeamID(), startMs, endMs, dayMs)
}

// personalSources 是账号个人空间里的主机（含已撤销的：它们的流量照样计入）。
const personalSources = `source_id IN (SELECT source_id FROM source_spaces
  WHERE tenant_id = $1 AND team_id IS NULL AND owner_account_id = $2)`

func (r reader) PersonalTraffic(ctx context.Context, tenantID, accountID string, startMs, endMs int64) (int64, error) {
	var bytes int64
	err := r.q.QueryRow(ctx,
		`SELECT COALESCE(sum(bytes_to_source + bytes_to_client), 0)::bigint FROM relay_traffic_buckets
		 WHERE tenant_id = $1 AND team_id = '' AND kind = 'e2e' AND `+personalSources+`
		   AND bucket_start_ms >= $3 AND bucket_start_ms < $4`, tenantID, accountID, startMs, endMs).Scan(&bytes)
	return bytes, err
}

func (r reader) PersonalTrafficDays(ctx context.Context, tenantID, accountID string, startMs, endMs int64) ([]saas.DayBytes, error) {
	return list(ctx, r.q, scanDay,
		`SELECT (bucket_start_ms / $5 * $5)::bigint AS day, sum(bytes_to_source + bytes_to_client)::bigint
		 FROM relay_traffic_buckets WHERE tenant_id = $1 AND team_id = '' AND kind = 'e2e' AND `+personalSources+`
		   AND bucket_start_ms >= $3 AND bucket_start_ms < $4 GROUP BY day ORDER BY day`,
		tenantID, accountID, startMs, endMs, dayMs)
}
