package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 运营后台的查询与动作（设计 §4.9，迁移 0004 / 0005 / 0009）。读出来的都是元数据：没有内容、令牌与口令。
// 每条 SQL 都带 tenant_id；运营不是团队成员，所以动作收裸 id，调用方已核过运营身份。

// likeContains 是「包含」匹配的 ILIKE 模式（输入里的 % _ \ 按字面量）。
func likeContains(q string) string { return "%" + likePrefix(q) + "%" }

// 账号与团队的聚合子查询（主机数：未撤销；团队数：未删除）。
const (
	acctHosts = `(SELECT count(*) FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
	  WHERE p.tenant_id = a.tenant_id AND p.owner_account_id = a.account_id AND s.revoked_at_ms IS NULL)::int`
	acctTeams = `(SELECT count(*) FROM team_members m JOIN teams t ON t.tenant_id = m.tenant_id AND t.team_id = m.team_id
	  WHERE m.tenant_id = a.tenant_id AND m.account_id = a.account_id AND t.deleted_at_ms IS NULL)::int`
	teamHosts = `(SELECT count(*) FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
	  WHERE p.tenant_id = t.tenant_id AND p.team_id = t.team_id AND s.revoked_at_ms IS NULL)::int`
	teamMembers = `(SELECT count(*) FROM team_members m WHERE m.tenant_id = t.tenant_id AND m.team_id = t.team_id)::int`
	teamSeats   = `(SELECT count(*) FROM team_members m WHERE m.tenant_id = t.tenant_id AND m.team_id = t.team_id AND m.role <> 'guest')::int`
)

const adminAccountSelect = `SELECT a.account_id, a.email, a.display_name, a.email_verified_at_ms IS NOT NULL, a.status,
  COALESCE(e.plan, 'free'), ` + acctHosts + `, ` + acctTeams + `, a.created_at_ms,
  (SELECT max(d.last_seen_at_ms) FROM devices d WHERE d.tenant_id = a.tenant_id AND d.subject = a.account_id)
  FROM accounts a LEFT JOIN entitlements e ON e.tenant_id = a.tenant_id AND e.space_kind = 'personal' AND e.space_id = a.account_id`

func scanAdminAccount(row scanner) (saas.AdminAccount, error) {
	var a saas.AdminAccount
	err := row.Scan(&a.AccountID, &a.Email, &a.DisplayName, &a.EmailVerified, &a.Status, &a.Plan,
		&a.HostCount, &a.TeamCount, &a.CreatedAtMs, &a.LastSeenAtMs)
	return a, err
}

const adminTeamSelect = `SELECT t.team_id, t.slug, t.name, t.status, COALESCE(e.plan, 'free'), ` + teamMembers + `, ` + teamHosts + `,
  t.created_at_ms, t.deleted_at_ms, ` + teamSeats + `
  FROM teams t LEFT JOIN entitlements e ON e.tenant_id = t.tenant_id AND e.space_kind = 'team' AND e.space_id = t.team_id`

func scanAdminTeam(row scanner) (saas.AdminTeam, error) {
	var t saas.AdminTeam
	err := row.Scan(&t.TeamID, &t.Slug, &t.Name, &t.Status, &t.Plan, &t.MemberCount, &t.HostCount, &t.CreatedAtMs, &t.DeletedAtMs, &t.Seats)
	return t, err
}

// adminWhere 拼运营列表的筛选（参数从 $first 起编号）：搜索词、状态、游标。cols 是各表里对应的列名。
type adminCols struct {
	search              []string
	status, created, id string
}

func adminWhere(q saas.AdminQuery, c adminCols, first int) (string, []any) {
	var (
		where strings.Builder
		args  []any
	)
	if q.Q != "" {
		args = append(args, likeContains(q.Q))
		parts := make([]string, len(c.search))
		for i, col := range c.search {
			parts[i] = fmt.Sprintf("%s ILIKE $%d", col, first+len(args)-1)
		}
		where.WriteString(" AND (" + strings.Join(parts, " OR ") + ")")
	}
	if q.Status != "" && c.status != "" {
		args = append(args, q.Status)
		fmt.Fprintf(&where, " AND %s = $%d", c.status, first+len(args)-1)
	}
	if q.After != nil {
		args = append(args, q.After.CreatedAtMs, q.After.ID)
		fmt.Fprintf(&where, " AND (%s, %s) < ($%d, $%d)", c.created, c.id, first+len(args)-2, first+len(args)-1)
	}
	return where.String(), args
}

func limitClause(limit int, args []any) (string, []any) {
	if limit <= 0 {
		return "", args
	}
	args = append(args, limit)
	return fmt.Sprintf(" LIMIT $%d", len(args)), args
}

func (r reader) AdminAccounts(ctx context.Context, tenantID string, q saas.AdminQuery) ([]saas.AdminAccount, error) {
	where, args := adminWhere(q, adminCols{search: []string{"a.email", "a.display_name"}, status: "a.status", created: "a.created_at_ms", id: "a.account_id"}, 2)
	args = append([]any{tenantID}, args...)
	limit, args := limitClause(q.Limit, args)
	return list(ctx, r.q, scanAdminAccount, adminAccountSelect+" WHERE a.tenant_id = $1"+where+" ORDER BY a.created_at_ms DESC, a.account_id DESC"+limit, args...)
}

func (r reader) AdminAccount(ctx context.Context, tenantID, accountID string, monthStartMs, monthEndMs int64) (*saas.AdminAccount, error) {
	account, err := one(oneOf(scanAdminAccount(r.q.QueryRow(ctx, adminAccountSelect+" WHERE a.tenant_id = $1 AND a.account_id = $2", tenantID, accountID))))
	if err != nil || account == nil {
		return account, err
	}
	account.RelayBytesMonth, err = r.PersonalTraffic(ctx, tenantID, accountID, monthStartMs, monthEndMs)
	return account, err
}

func (r reader) AdminTeams(ctx context.Context, tenantID string, q saas.AdminQuery) ([]saas.AdminTeam, error) {
	where, args := adminWhere(q, adminCols{search: []string{"t.name", "t.slug"}, status: "t.status", created: "t.created_at_ms", id: "t.team_id"}, 2)
	args = append([]any{tenantID}, args...)
	limit, args := limitClause(q.Limit, args)
	return list(ctx, r.q, scanAdminTeam, adminTeamSelect+" WHERE t.tenant_id = $1"+where+" ORDER BY t.created_at_ms DESC, t.team_id DESC"+limit, args...)
}

func (r reader) AdminTeam(ctx context.Context, tenantID, teamID string, monthStartMs, monthEndMs int64) (*saas.AdminTeam, error) {
	team, err := one(oneOf(scanAdminTeam(r.q.QueryRow(ctx, adminTeamSelect+" WHERE t.tenant_id = $1 AND t.team_id = $2", tenantID, teamID))))
	if err != nil || team == nil {
		return team, err
	}
	err = r.q.QueryRow(ctx,
		`SELECT COALESCE(sum(bytes_to_source + bytes_to_client), 0)::bigint FROM relay_traffic_buckets
		 WHERE tenant_id = $1 AND team_id = $2 AND kind = 'e2e' AND bucket_start_ms >= $3 AND bucket_start_ms < $4`,
		tenantID, teamID, monthStartMs, monthEndMs).Scan(&team.RelayBytesMonth)
	return team, err
}

// oneOf 把 (值, 错误) 换成 (指针, 错误)，配 one() 把「没有这一行」变成 nil。
func oneOf[R any](row R, err error) (*R, error) {
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r reader) AdminTenants(ctx context.Context, tenantID string, q saas.AdminQuery) ([]saas.AdminTenant, error) {
	where, args := adminWhere(q, adminCols{search: []string{"s1", "s2"}, created: "created_at_ms", id: "id"}, 2)
	args = append([]any{tenantID}, args...)
	limit, args := limitClause(q.Limit, args)
	return list(ctx, r.q, func(s scanner) (saas.AdminTenant, error) {
		var t saas.AdminTenant
		err := s.Scan(&t.Kind, &t.ID, &t.Name, &t.Plan, &t.HostCount, &t.Status, &t.CreatedAtMs)
		return t, err
	}, `SELECT kind, id, name, plan, hosts, status, created_at_ms FROM (
	      SELECT 'account' AS kind, a.account_id AS id, a.display_name AS name, COALESCE(e.plan, 'free') AS plan,
	        `+acctHosts+` AS hosts, a.status AS status, a.created_at_ms AS created_at_ms,
	        COALESCE(a.email, '') AS s1, a.display_name AS s2, a.tenant_id AS tenant_id
	      FROM accounts a LEFT JOIN entitlements e ON e.tenant_id = a.tenant_id AND e.space_kind = 'personal' AND e.space_id = a.account_id
	      UNION ALL
	      SELECT 'team', t.team_id, t.name, COALESCE(e.plan, 'free'), `+teamHosts+`, t.status, t.created_at_ms,
	        t.name, t.slug, t.tenant_id
	      FROM teams t LEFT JOIN entitlements e ON e.tenant_id = t.tenant_id AND e.space_kind = 'team' AND e.space_id = t.team_id
	    ) u WHERE tenant_id = $1`+where+` ORDER BY created_at_ms DESC, id DESC`+limit, args...)
}

func (r reader) OperatorActions(ctx context.Context, tenantID, targetKind, targetID string, limit int) ([]saas.AuditEntry, error) {
	return list(ctx, r.q, scanAudit,
		"SELECT "+auditColumns+` FROM audit_log WHERE tenant_id = $1 AND actor_kind = 'operator'
		 AND target_kind = $2 AND target_id = $3 ORDER BY at_ms DESC, event_id DESC LIMIT $4`, tenantID, targetKind, targetID, limit)
}

func (r reader) IncidentAt(ctx context.Context, tenantID string) (saas.Incident, *int64, error) {
	var (
		raw []byte
		at  int64
	)
	err := r.q.QueryRow(ctx, "SELECT value, updated_at_ms FROM platform_settings WHERE tenant_id = $1 AND key = 'incident'", tenantID).Scan(&raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return saas.Incident{}, nil, nil
	}
	if err != nil {
		return saas.Incident{}, nil, err
	}
	var incident saas.Incident
	if err := json.Unmarshal(raw, &incident); err != nil {
		return saas.Incident{}, nil, store.Errorf(store.KindCorrupt, "platform_settings.incident 不合格式，拒绝启动")
	}
	return incident, &at, nil
}

func (r reader) TOTP(ctx context.Context, tenantID, accountID string) (*saas.TOTPCredential, error) {
	return one(oneOf(scanTOTP(r.q.QueryRow(ctx,
		`SELECT credential_id, secret_wrap, last_step FROM account_credentials
		 WHERE tenant_id = $1 AND account_id = $2 AND kind = 'totp'`, tenantID, accountID))))
}

func scanTOTP(row scanner) (saas.TOTPCredential, error) {
	var (
		c    saas.TOTPCredential
		wrap *string
	)
	err := row.Scan(&c.CredentialID, &wrap, &c.LastStep)
	if wrap != nil {
		c.SecretWrap = *wrap
	}
	return c, err
}

// ---- 写 ----

type opsRepo struct{ q querier }

func (r opsRepo) SetTeamStatus(ctx context.Context, tenantID, teamID, status string, reason *string, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		"UPDATE teams SET status = $3, status_reason = $4, updated_at_ms = $5 WHERE tenant_id = $1 AND team_id = $2",
		tenantID, teamID, status, reason, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r opsRepo) TeamHostIDs(ctx context.Context, tenantID, teamID string) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT p.source_id FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
	    WHERE p.tenant_id = $1 AND p.team_id = $2 AND s.revoked_at_ms IS NULL ORDER BY p.source_id`, tenantID, teamID)
}

func (r opsRepo) SetAccountStatus(ctx context.Context, tenantID, accountID, status string, reason *string, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		"UPDATE accounts SET status = $3, status_reason = $4, updated_at_ms = $5 WHERE tenant_id = $1 AND account_id = $2",
		tenantID, accountID, status, reason, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r opsRepo) RevokeDevices(ctx context.Context, tenantID, accountID string, nowMs int64) (int, error) {
	tag, err := r.q.Exec(ctx,
		"UPDATE devices SET revoked_at_ms = $3 WHERE tenant_id = $1 AND subject = $2 AND revoked_at_ms IS NULL", tenantID, accountID, nowMs)
	return int(tag.RowsAffected()), err
}

func (r opsRepo) RevokeOperatorDevices(ctx context.Context, tenantID, accountID string, nowMs int64) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `UPDATE devices SET revoked_at_ms = $3 WHERE tenant_id = $1 AND subject = $2 AND scope = 'operator' AND revoked_at_ms IS NULL
	    RETURNING device_id`, tenantID, accountID, nowMs)
}

func (r opsRepo) AddScope(ctx context.Context, tenantID, accountID, scope string, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`UPDATE accounts SET scopes = CASE WHEN $3 = ANY(scopes) THEN scopes ELSE array_append(scopes, $3) END, updated_at_ms = $4
		 WHERE tenant_id = $1 AND account_id = $2`, tenantID, accountID, scope, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r opsRepo) PutTOTP(ctx context.Context, tenantID, accountID, credentialID, secretWrap string, nowMs int64) error {
	if _, err := r.q.Exec(ctx, "DELETE FROM account_credentials WHERE tenant_id = $1 AND account_id = $2 AND kind = 'totp'", tenantID, accountID); err != nil {
		return err
	}
	_, err := r.q.Exec(ctx,
		`INSERT INTO account_credentials (tenant_id, credential_id, account_id, kind, secret_wrap, created_at_ms)
		 VALUES ($1, $2, $3, 'totp', $4, $5)`, tenantID, credentialID, accountID, secretWrap, nowMs)
	return err
}

func (r opsRepo) AcceptTOTPStep(ctx context.Context, tenantID, accountID string, step, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`UPDATE account_credentials SET last_step = $3, last_used_at_ms = $4
		 WHERE tenant_id = $1 AND account_id = $2 AND kind = 'totp' AND (last_step IS NULL OR last_step < $3)`,
		tenantID, accountID, step, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r opsRepo) UpdateIncident(ctx context.Context, tenantID string, patch saas.IncidentPatch, updatedBy string, nowMs int64) (saas.Incident, error) {
	// 先保证有行（全关），再锁住它读-改-写：两位运营同时改不同的项不会互相覆盖。
	if _, err := r.q.Exec(ctx,
		`INSERT INTO platform_settings (tenant_id, key, value, updated_at_ms, updated_by)
		 VALUES ($1, 'incident', '{"assertionsPaused":false,"registrationsPaused":false,"rejectNewConnections":false}', $2, NULL)
		 ON CONFLICT (tenant_id, key) DO NOTHING`, tenantID, nowMs); err != nil {
		return saas.Incident{}, err
	}
	var raw []byte
	if err := r.q.QueryRow(ctx, "SELECT value FROM platform_settings WHERE tenant_id = $1 AND key = 'incident' FOR UPDATE", tenantID).Scan(&raw); err != nil {
		return saas.Incident{}, err
	}
	var incident saas.Incident
	if err := json.Unmarshal(raw, &incident); err != nil {
		return saas.Incident{}, store.Errorf(store.KindCorrupt, "platform_settings.incident 不合格式，拒绝启动")
	}
	if patch.AssertionsPaused != nil {
		incident.AssertionsPaused = *patch.AssertionsPaused
	}
	if patch.RegistrationsPaused != nil {
		incident.RegistrationsPaused = *patch.RegistrationsPaused
	}
	if patch.RejectNewConnections != nil {
		incident.RejectNewConnections = *patch.RejectNewConnections
	}
	value, err := json.Marshal(incident)
	if err != nil {
		return saas.Incident{}, err
	}
	_, err = r.q.Exec(ctx,
		"UPDATE platform_settings SET value = $2, updated_at_ms = $3, updated_by = NULLIF($4, '') WHERE tenant_id = $1 AND key = 'incident'",
		tenantID, value, nowMs, updatedBy)
	return incident, err
}
