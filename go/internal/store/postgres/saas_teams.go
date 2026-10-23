package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 团队的 PostgreSQL 实现（迁移 0005–0007；设计 §3.4、§4.4）。每条 SQL 都带 tenant_id；
// 团队范围的方法都从 TeamScope 取 tenantID 与 teamID，不收裸 id。

const teamColumns = `t.team_id, t.slug, t.name, t.avatar_url, t.settings, t.status, t.status_reason, t.created_by,
	t.created_at_ms, t.updated_at_ms, t.deleted_at_ms, t.purge_at_ms`

func scanTeam(row scanner, extra ...any) (saas.Team, error) {
	var (
		t        saas.Team
		settings []byte
	)
	dest := append([]any{&t.TeamID, &t.Slug, &t.Name, &t.AvatarURL, &settings, &t.Status, &t.StatusReason, &t.CreatedBy,
		&t.CreatedAtMs, &t.UpdatedAtMs, &t.DeletedAtMs, &t.PurgeAtMs}, extra...)
	if err := row.Scan(dest...); err != nil {
		return t, err
	}
	t.Settings = settings
	return t, nil
}

func scanTeamAccess(row scanner) (saas.TeamAccess, error) {
	var role string
	var plan string
	t, err := scanTeam(row, &role, &plan)
	return saas.TeamAccess{Team: t, Role: saas.TeamRole(role), Plan: plan}, err
}

const teamAccessQuery = `SELECT ` + teamColumns + `, m.role, COALESCE(e.plan, 'free')
	FROM team_members m
	JOIN teams t ON t.tenant_id = m.tenant_id AND t.team_id = m.team_id
	LEFT JOIN entitlements e ON e.tenant_id = t.tenant_id AND e.space_kind = 'team' AND e.space_id = t.team_id`

func (r reader) TeamAccess(ctx context.Context, tenantID, teamID, accountID string) (*saas.TeamAccess, error) {
	access, err := scanTeamAccess(r.q.QueryRow(ctx,
		teamAccessQuery+" WHERE m.tenant_id = $1 AND m.team_id = $2 AND m.account_id = $3", tenantID, teamID, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &access, err
}

func (r reader) TeamsOf(ctx context.Context, tenantID, accountID string) ([]saas.TeamAccess, error) {
	return list(ctx, r.q, scanTeamAccess,
		teamAccessQuery+" WHERE m.tenant_id = $1 AND m.account_id = $2 ORDER BY m.joined_at_ms, t.team_id", tenantID, accountID)
}

func (r reader) OwnedTeamCount(ctx context.Context, tenantID, accountID string) (int, error) {
	var n int
	err := r.q.QueryRow(ctx,
		`SELECT count(*) FROM team_members m JOIN teams t ON t.tenant_id = m.tenant_id AND t.team_id = m.team_id
		 WHERE m.tenant_id = $1 AND m.account_id = $2 AND m.role = 'owner' AND t.deleted_at_ms IS NULL`,
		tenantID, accountID).Scan(&n)
	return n, err
}

func (r reader) Entitlement(ctx context.Context, tenantID, spaceKind, spaceID string) (*saas.Entitlement, error) {
	var e saas.Entitlement
	var overrides []byte
	err := r.q.QueryRow(ctx,
		"SELECT plan, status, overrides FROM entitlements WHERE tenant_id = $1 AND space_kind = $2 AND space_id = $3",
		tenantID, spaceKind, spaceID).Scan(&e.Plan, &e.Status, &overrides)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	e.Overrides = overrides
	return &e, err
}

func (r reader) DueTeams(ctx context.Context, tenantID string, nowMs int64, limit int) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT team_id FROM teams WHERE tenant_id = $1 AND purge_at_ms IS NOT NULL AND purge_at_ms <= $2
	    ORDER BY purge_at_ms, team_id LIMIT $3`, tenantID, nowMs, limit)
}

func (r teamRepo) Create(ctx context.Context, tenantID string, t saas.Team, ownerID string) error {
	if _, err := r.q.Exec(ctx,
		`INSERT INTO teams (tenant_id, team_id, slug, name, avatar_url, settings, status, created_by, created_at_ms, updated_at_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $8)`,
		tenantID, t.TeamID, t.Slug, t.Name, t.AvatarURL, []byte(t.Settings), t.CreatedBy, t.CreatedAtMs); err != nil {
		return err
	}
	if _, err := r.q.Exec(ctx,
		`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ($1, $2, $3, 'owner', $4)`,
		tenantID, t.TeamID, ownerID, t.CreatedAtMs); err != nil {
		return err
	}
	_, err := r.q.Exec(ctx,
		`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, status, overrides, updated_at_ms, updated_by)
		 VALUES ($1, 'team', $2, 'free', 'active', '{}', $3, $4)`, tenantID, t.TeamID, t.CreatedAtMs, ownerID)
	return err
}

func (r teamRepo) Get(ctx context.Context, scope saas.TeamScope) (*saas.Team, error) {
	t, err := scanTeam(r.q.QueryRow(ctx, "SELECT "+teamColumns+" FROM teams t WHERE t.tenant_id = $1 AND t.team_id = $2",
		scope.TenantID(), scope.TeamID()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

const memberColumns = `m.account_id, COALESCE(a.email, ''), a.display_name, m.role, m.joined_at_ms, m.invited_by`

func scanMember(row scanner, extra ...any) (saas.TeamMember, error) {
	var m saas.TeamMember
	var role string
	err := row.Scan(append([]any{&m.AccountID, &m.Email, &m.DisplayName, &role, &m.JoinedAtMs, &m.InvitedBy}, extra...)...)
	m.Role = saas.TeamRole(role)
	return m, err
}

const memberFrom = ` FROM team_members m JOIN accounts a ON a.tenant_id = m.tenant_id AND a.account_id = m.account_id`

func (r teamRepo) Members(ctx context.Context, scope saas.TeamScope, nowMs int64) ([]saas.TeamMember, error) {
	return list(ctx, r.q, func(s scanner) (saas.TeamMember, error) {
		var reachable int
		m, err := scanMember(s, &reachable)
		if m.Role == saas.TeamRole("guest") {
			m.ReachableHosts = reachable
		}
		return m, err
	}, "SELECT "+memberColumns+`, (SELECT count(DISTINCT g.source_id)::int FROM source_grants g
	      JOIN sources s ON s.tenant_id = g.tenant_id AND s.source_id = g.source_id AND s.revoked_at_ms IS NULL
	      WHERE g.tenant_id = m.tenant_id AND g.team_id = m.team_id AND g.subject_kind = 'guest' AND g.subject_id = m.account_id
	        AND g.revoked_at_ms IS NULL AND (g.expires_at_ms IS NULL OR g.expires_at_ms > $3))`+memberFrom+
		" WHERE m.tenant_id = $1 AND m.team_id = $2 ORDER BY m.joined_at_ms, m.account_id",
		scope.TenantID(), scope.TeamID(), nowMs)
}

func (r teamRepo) Member(ctx context.Context, scope saas.TeamScope, accountID string) (*saas.TeamMember, error) {
	m, err := scanMember(r.q.QueryRow(ctx,
		"SELECT "+memberColumns+memberFrom+" WHERE m.tenant_id = $1 AND m.team_id = $2 AND m.account_id = $3",
		scope.TenantID(), scope.TeamID(), accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

func (r teamRepo) Seats(ctx context.Context, scope saas.TeamScope) (saas.Seats, error) {
	var s saas.Seats
	err := r.q.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE role <> 'guest'), count(*) FILTER (WHERE role = 'guest')
		 FROM team_members WHERE tenant_id = $1 AND team_id = $2`, scope.TenantID(), scope.TeamID()).Scan(&s.Members, &s.Guests)
	return s, err
}

func (r teamRepo) Rename(ctx context.Context, scope saas.TeamScope, name string, avatar *string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		`UPDATE teams SET name = $1, updated_at_ms = $2,
		   avatar_url = CASE WHEN $3::boolean THEN NULLIF($4::text, '') ELSE avatar_url END
		 WHERE tenant_id = $5 AND team_id = $6`,
		name, nowMs, avatar != nil, stringOrEmpty(avatar), scope.TenantID(), scope.TeamID())
	return err
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (r teamRepo) SetSettings(ctx context.Context, scope saas.TeamScope, settings json.RawMessage, nowMs int64) error {
	_, err := r.q.Exec(ctx, "UPDATE teams SET settings = $1, updated_at_ms = $2 WHERE tenant_id = $3 AND team_id = $4",
		[]byte(settings), nowMs, scope.TenantID(), scope.TeamID())
	return err
}

func (r teamRepo) SetRole(ctx context.Context, scope saas.TeamScope, accountID string, role saas.TeamRole) error {
	_, err := r.q.Exec(ctx, "UPDATE team_members SET role = $1 WHERE tenant_id = $2 AND team_id = $3 AND account_id = $4",
		string(role), scope.TenantID(), scope.TeamID(), accountID)
	return err
}

func (r teamRepo) AddMember(ctx context.Context, scope saas.TeamScope, accountID string, role saas.TeamRole, invitedBy string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms, invited_by) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
		scope.TenantID(), scope.TeamID(), accountID, string(role), nowMs, invitedBy)
	return err
}

func (r teamRepo) RemoveMember(ctx context.Context, scope saas.TeamScope, accountID string) error {
	_, err := r.q.Exec(ctx, "DELETE FROM team_members WHERE tenant_id = $1 AND team_id = $2 AND account_id = $3",
		scope.TenantID(), scope.TeamID(), accountID)
	return err
}

func (r teamRepo) OldestOwner(ctx context.Context, scope saas.TeamScope, exclude string) (string, error) {
	var id string
	err := r.q.QueryRow(ctx,
		`SELECT account_id FROM team_members WHERE tenant_id = $1 AND team_id = $2 AND role = 'owner' AND account_id <> $3
		 ORDER BY joined_at_ms, account_id LIMIT 1`, scope.TenantID(), scope.TeamID(), exclude).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (r teamRepo) SetDeleted(ctx context.Context, scope saas.TeamScope, deletedAtMs, purgeAtMs int64) error {
	_, err := r.q.Exec(ctx,
		"UPDATE teams SET deleted_at_ms = $1, purge_at_ms = $2, updated_at_ms = $1 WHERE tenant_id = $3 AND team_id = $4",
		deletedAtMs, purgeAtMs, scope.TenantID(), scope.TeamID())
	return err
}

func (r teamRepo) Restore(ctx context.Context, scope saas.TeamScope, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		"UPDATE teams SET deleted_at_ms = NULL, purge_at_ms = NULL, updated_at_ms = $1 WHERE tenant_id = $2 AND team_id = $3",
		nowMs, scope.TenantID(), scope.TeamID())
	return err
}

func (r teamRepo) HostIDs(ctx context.Context, scope saas.TeamScope) ([]string, error) {
	return teamHostIDs(ctx, r.q, scope.TenantID(), scope.TeamID())
}

func teamHostIDs(ctx context.Context, q querier, tenantID, teamID string) ([]string, error) {
	return list(ctx, q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT p.source_id FROM source_spaces p
	    JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
	    WHERE p.tenant_id = $1 AND p.team_id = $2 AND s.revoked_at_ms IS NULL ORDER BY p.source_id`, tenantID, teamID)
}

func (r teamRepo) ReassignHosts(ctx context.Context, scope saas.TeamScope, from, to string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		`UPDATE source_spaces SET owner_account_id = $1, updated_at_ms = $2
		 WHERE tenant_id = $3 AND team_id = $4 AND owner_account_id = $5`,
		to, nowMs, scope.TenantID(), scope.TeamID(), from)
	return err
}

func (r teamRepo) RevokeMemberGrants(ctx context.Context, scope saas.TeamScope, accountID, revokedBy string, nowMs int64) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `UPDATE source_grants SET revoked_at_ms = $1, revoked_by = $2, updated_at_ms = $1
	    WHERE tenant_id = $3 AND team_id = $4 AND subject_kind IN ('member', 'guest') AND subject_id = $5 AND revoked_at_ms IS NULL
	    RETURNING source_id`, nowMs, revokedBy, scope.TenantID(), scope.TeamID(), accountID)
}

func (r teamRepo) CountMemberHosts(ctx context.Context, scope saas.TeamScope, accountID string) (int, error) {
	var n int
	err := r.q.QueryRow(ctx,
		`SELECT count(*) FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 WHERE p.tenant_id = $1 AND p.team_id = $2 AND p.owner_account_id = $3 AND s.revoked_at_ms IS NULL`,
		scope.TenantID(), scope.TeamID(), accountID).Scan(&n)
	return n, err
}

func (r teamRepo) Usage(ctx context.Context, scope saas.TeamScope, monthStartMs, monthEndMs int64) (int, int64, error) {
	var hosts int
	if err := r.q.QueryRow(ctx,
		`SELECT count(*) FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 WHERE p.tenant_id = $1 AND p.team_id = $2 AND s.revoked_at_ms IS NULL`,
		scope.TenantID(), scope.TeamID()).Scan(&hosts); err != nil {
		return 0, 0, err
	}
	var bytes int64
	err := r.q.QueryRow(ctx,
		`SELECT COALESCE(sum(bytes_to_source + bytes_to_client), 0)::bigint FROM relay_traffic_buckets
		 WHERE tenant_id = $1 AND team_id = $2 AND kind = 'e2e' AND bucket_start_ms >= $3 AND bucket_start_ms < $4`,
		scope.TenantID(), scope.TeamID(), monthStartMs, monthEndMs).Scan(&bytes)
	return hosts, bytes, err
}

func (r teamRepo) Purge(ctx context.Context, tenantID, teamID string, nowMs int64) (bool, []string, error) {
	var exists bool
	if err := r.q.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM teams WHERE tenant_id = $1 AND team_id = $2 AND purge_at_ms IS NOT NULL AND purge_at_ms <= $3)",
		tenantID, teamID, nowMs).Scan(&exists); err != nil || !exists {
		return false, nil, err
	}
	// 含已撤销的：它们的子行也要删，外键才放行。
	sources, err := list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, "SELECT source_id FROM source_spaces WHERE tenant_id = $1 AND team_id = $2 ORDER BY source_id", tenantID, teamID)
	if err != nil {
		return false, nil, err
	}
	ids := store.NonNil(sources)
	steps := []struct {
		sql  string
		args []any
	}{
		{"UPDATE sources SET revoked_at_ms = COALESCE(revoked_at_ms, $2) WHERE tenant_id = $1 AND source_id = ANY($3)", []any{tenantID, nowMs, ids}},
		{"DELETE FROM source_grants WHERE tenant_id = $1 AND (team_id = $2 OR source_id = ANY($3))", []any{tenantID, teamID, ids}},
		{"DELETE FROM invitations WHERE tenant_id = $1 AND (team_id = $2 OR source_id = ANY($3))", []any{tenantID, teamID, ids}},
		{"DELETE FROM links WHERE tenant_id = $1 AND source_id = ANY($2)", []any{tenantID, ids}},
		{"DELETE FROM trust_requests WHERE tenant_id = $1 AND (team_id = $2 OR source_id = ANY($3))", []any{tenantID, teamID, ids}},
		{"DELETE FROM source_spaces WHERE tenant_id = $1 AND team_id = $2", []any{tenantID, teamID}},
		{"DELETE FROM registration_tokens WHERE tenant_id = $1 AND team_id = $2", []any{tenantID, teamID}},
		{"DELETE FROM entitlements WHERE tenant_id = $1 AND space_kind = 'team' AND space_id = $2", []any{tenantID, teamID}},
	}
	for _, step := range steps {
		if _, err := r.q.Exec(ctx, step.sql, step.args...); err != nil {
			return false, nil, err
		}
	}
	// 成员与分组随团队行级联删除；审计行没有外键，保留。
	if _, err := r.q.Exec(ctx, "DELETE FROM teams WHERE tenant_id = $1 AND team_id = $2", tenantID, teamID); err != nil {
		return false, nil, err
	}
	return true, sources, nil
}
