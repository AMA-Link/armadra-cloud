package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 授予的 PostgreSQL 实现（迁移 0007 source_grants；设计 §3.4、§4.6）。

const grantColumns = `grant_id, source_id, COALESCE(team_id, ''), subject_kind, subject_id, role, workspace_ids,
	expires_at_ms, granted_by, origin, COALESCE(invitation_id, ''), created_at_ms, updated_at_ms, revoked_at_ms, COALESCE(revoked_by, '')`

func scanGrant(row scanner) (saas.Grant, error) {
	var g saas.Grant
	err := row.Scan(&g.GrantID, &g.SourceID, &g.TeamID, &g.SubjectKind, &g.SubjectID, &g.Role, &g.WorkspaceIDs,
		&g.ExpiresAtMs, &g.GrantedBy, &g.Origin, &g.InvitationID, &g.CreatedAtMs, &g.UpdatedAtMs, &g.RevokedAtMs, &g.RevokedBy)
	g.WorkspaceIDs = store.NonNil(g.WorkspaceIDs)
	return g, err
}

func (r reader) Grant(ctx context.Context, tenantID, grantID string) (*saas.Grant, error) {
	g, err := scanGrant(r.q.QueryRow(ctx,
		"SELECT "+grantColumns+" FROM source_grants WHERE tenant_id = $1 AND grant_id = $2", tenantID, grantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (r reader) ListGrants(ctx context.Context, tenantID string, q saas.GrantQuery) ([]saas.Grant, error) {
	query := "SELECT " + grantColumns + " FROM source_grants WHERE tenant_id = $1 AND revoked_at_ms IS NULL" +
		" AND ($2 = '' OR source_id = $2) AND ($3 = '' OR (subject_kind IN ('member', 'guest') AND subject_id = $3))" +
		" ORDER BY created_at_ms, grant_id"
	args := []any{tenantID, q.SourceID, q.AccountID}
	if q.Limit > 0 {
		query += " LIMIT $4"
		args = append(args, q.Limit)
	}
	return list(ctx, r.q, scanGrant, query, args...)
}

func (r reader) AccessBasis(ctx context.Context, tenantID, accountID, sourceID string) (saas.AccessBasis, error) {
	var (
		b           saas.AccessBasis
		accountOK   *string
		revoked     bool
		teamStatus  string
		teamDeleted bool
		role        string
	)
	err := r.q.QueryRow(ctx,
		`SELECT a.status, s.revoked_at_ms IS NOT NULL, s.core_version, p.owner_account_id, COALESCE(p.team_id, ''),
		        COALESCE(t.status, ''), COALESCE(t.deleted_at_ms IS NOT NULL, false), COALESCE(m.role, '')
		 FROM source_spaces p
		 JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 LEFT JOIN accounts a ON a.tenant_id = p.tenant_id AND a.account_id = $3
		 LEFT JOIN teams t ON t.tenant_id = p.tenant_id AND t.team_id = p.team_id
		 LEFT JOIN team_members m ON m.tenant_id = p.tenant_id AND m.team_id = p.team_id AND m.account_id = $3
		 WHERE p.tenant_id = $1 AND p.source_id = $2`, tenantID, sourceID, accountID).
		Scan(&accountOK, &revoked, &b.CoreVersion, &b.OwnerID, &b.TeamID, &teamStatus, &teamDeleted, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return saas.AccessBasis{}, nil
	}
	if err != nil {
		return saas.AccessBasis{}, err
	}
	b.SourceFound, b.SourceRevoked = true, revoked
	b.AccountActive = accountOK != nil && *accountOK == saas.AccountActive
	b.TeamDeleted, b.TeamSuspended = teamDeleted, teamStatus == saas.TeamSuspended
	b.TeamRole = saas.TeamRole(role)
	b.Grants, err = list(ctx, r.q, scanGrant,
		"SELECT "+grantColumns+` FROM source_grants WHERE tenant_id = $1 AND source_id = $2 AND revoked_at_ms IS NULL
		 AND ((subject_kind IN ('member', 'guest') AND subject_id = $3) OR (subject_kind = 'team' AND subject_id = $4))
		 ORDER BY created_at_ms, grant_id`, tenantID, sourceID, accountID, b.TeamID)
	return b, err
}

func (r reader) AccessBases(ctx context.Context, tenantID string, accountIDs []string, sourceID string) (map[string]saas.AccessBasis, error) {
	out := make(map[string]saas.AccessBasis, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		if _, dup := out[id]; !dup {
			out[id] = saas.AccessBasis{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	var (
		shared      saas.AccessBasis
		revoked     bool
		teamStatus  string
		teamDeleted bool
	)
	err := r.q.QueryRow(ctx,
		`SELECT s.revoked_at_ms IS NOT NULL, s.core_version, p.owner_account_id, COALESCE(p.team_id, ''),
		        COALESCE(t.status, ''), COALESCE(t.deleted_at_ms IS NOT NULL, false)
		 FROM source_spaces p
		 JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 LEFT JOIN teams t ON t.tenant_id = p.tenant_id AND t.team_id = p.team_id
		 WHERE p.tenant_id = $1 AND p.source_id = $2`, tenantID, sourceID).
		Scan(&revoked, &shared.CoreVersion, &shared.OwnerID, &shared.TeamID, &teamStatus, &teamDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	shared.SourceFound, shared.SourceRevoked = true, revoked
	shared.TeamDeleted, shared.TeamSuspended = teamDeleted, teamStatus == saas.TeamSuspended

	active := map[string]bool{}
	statuses, err := list(ctx, r.q, func(s scanner) ([2]string, error) {
		var row [2]string
		return row, s.Scan(&row[0], &row[1])
	}, "SELECT account_id, status FROM accounts WHERE tenant_id = $1 AND account_id = ANY($2)", tenantID, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range statuses {
		active[row[0]] = row[1] == saas.AccountActive
	}
	roles := map[string]saas.TeamRole{}
	if shared.TeamID != "" {
		members, err := list(ctx, r.q, func(s scanner) ([2]string, error) {
			var row [2]string
			return row, s.Scan(&row[0], &row[1])
		}, "SELECT account_id, role FROM team_members WHERE tenant_id = $1 AND team_id = $2 AND account_id = ANY($3)",
			tenantID, shared.TeamID, ids)
		if err != nil {
			return nil, err
		}
		for _, row := range members {
			roles[row[0]] = saas.TeamRole(row[1])
		}
	}
	grants, err := list(ctx, r.q, scanGrant,
		"SELECT "+grantColumns+` FROM source_grants WHERE tenant_id = $1 AND source_id = $2 AND revoked_at_ms IS NULL
		 AND ((subject_kind IN ('member', 'guest') AND subject_id = ANY($3)) OR (subject_kind = 'team' AND subject_id = $4))
		 ORDER BY created_at_ms, grant_id`, tenantID, sourceID, ids, shared.TeamID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		b := shared
		b.AccountActive, b.TeamRole = active[id], roles[id]
		out[id] = b
	}
	// 授予按创建顺序分发：账号主体的归它自己，团队主体的归全体。
	for _, g := range grants {
		if g.SubjectKind == saas.GrantSubjectTeam {
			if shared.TeamID == "" || g.SubjectID != shared.TeamID {
				continue
			}
			for _, id := range ids {
				b := out[id]
				b.Grants = append(b.Grants, g)
				out[id] = b
			}
			continue
		}
		if b, ok := out[g.SubjectID]; ok {
			b.Grants = append(b.Grants, g)
			out[g.SubjectID] = b
		}
	}
	return out, nil
}

func (r reader) SourceAccessBases(ctx context.Context, tenantID, accountID string, sourceIDs []string) (map[string]saas.AccessBasis, error) {
	out := make(map[string]saas.AccessBasis, len(sourceIDs))
	ids := make([]string, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		if _, dup := out[id]; !dup {
			out[id] = saas.AccessBasis{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	var status *string
	err := r.q.QueryRow(ctx, "SELECT status FROM accounts WHERE tenant_id = $1 AND account_id = $2", tenantID, accountID).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	active := status != nil && *status == saas.AccountActive
	type hostRow struct {
		sourceID    string
		basis       saas.AccessBasis
		teamStatus  string
		teamDeleted bool
		role        string
	}
	rows, err := list(ctx, r.q, func(s scanner) (hostRow, error) {
		var row hostRow
		err := s.Scan(&row.sourceID, &row.basis.SourceRevoked, &row.basis.CoreVersion, &row.basis.OwnerID, &row.basis.TeamID,
			&row.teamStatus, &row.teamDeleted, &row.role)
		return row, err
	}, `SELECT p.source_id, s.revoked_at_ms IS NOT NULL, s.core_version, p.owner_account_id, COALESCE(p.team_id, ''),
	        COALESCE(t.status, ''), COALESCE(t.deleted_at_ms IS NOT NULL, false), COALESCE(m.role, '')
	 FROM source_spaces p
	 JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
	 LEFT JOIN teams t ON t.tenant_id = p.tenant_id AND t.team_id = p.team_id
	 LEFT JOIN team_members m ON m.tenant_id = p.tenant_id AND m.team_id = p.team_id AND m.account_id = $3
	 WHERE p.tenant_id = $1 AND p.source_id = ANY($2)`, tenantID, ids, accountID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		b := row.basis
		b.SourceFound, b.AccountActive = true, active
		b.TeamDeleted, b.TeamSuspended = row.teamDeleted, row.teamStatus == saas.TeamSuspended
		b.TeamRole = saas.TeamRole(row.role)
		out[row.sourceID] = b
	}
	// 只取以这个账号为主体的 member / guest 授予，与主机所属团队的 team 授予（与 AccessBasis 同一条件）。
	grants, err := list(ctx, r.q, scanGrant,
		"SELECT "+grantColumns+` FROM source_grants g WHERE g.tenant_id = $1 AND g.source_id = ANY($2) AND g.revoked_at_ms IS NULL
		 AND ((g.subject_kind IN ('member', 'guest') AND g.subject_id = $3)
		   OR (g.subject_kind = 'team' AND g.subject_id = (
		     SELECT p.team_id FROM source_spaces p WHERE p.tenant_id = g.tenant_id AND p.source_id = g.source_id)))
		 ORDER BY g.created_at_ms, g.grant_id`, tenantID, ids, accountID)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if b, ok := out[g.SourceID]; ok && b.SourceFound {
			b.Grants = append(b.Grants, g)
			out[g.SourceID] = b
		}
	}
	return out, nil
}

func (r reader) AccessAccounts(ctx context.Context, tenantID, sourceID string) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT account_id FROM (
	      SELECT owner_account_id AS account_id FROM source_spaces WHERE tenant_id = $1 AND source_id = $2
	      UNION SELECT subject_id FROM source_grants
	        WHERE tenant_id = $1 AND source_id = $2 AND subject_kind IN ('member', 'guest') AND revoked_at_ms IS NULL
	      UNION SELECT m.account_id FROM source_spaces p JOIN team_members m ON m.tenant_id = p.tenant_id AND m.team_id = p.team_id
	        WHERE p.tenant_id = $1 AND p.source_id = $2 AND p.team_id IS NOT NULL
	    ) x ORDER BY account_id`, tenantID, sourceID)
}

func (r reader) TeamAccountIDs(ctx context.Context, tenantID, teamID string) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, "SELECT account_id FROM team_members WHERE tenant_id = $1 AND team_id = $2 ORDER BY account_id", tenantID, teamID)
}

func (r reader) DueGrants(ctx context.Context, tenantID string, nowMs int64, limit int) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT grant_id FROM source_grants
	    WHERE tenant_id = $1 AND revoked_at_ms IS NULL AND expires_at_ms IS NOT NULL AND expires_at_ms <= $2
	    ORDER BY expires_at_ms, grant_id LIMIT $3`, tenantID, nowMs, limit)
}

type grantRepo struct{ q querier }

func (r grantRepo) Lock(ctx context.Context, tenantID, grantID string) (*saas.Grant, error) {
	g, err := scanGrant(r.q.QueryRow(ctx,
		"SELECT "+grantColumns+" FROM source_grants WHERE tenant_id = $1 AND grant_id = $2 FOR UPDATE", tenantID, grantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (r grantRepo) Insert(ctx context.Context, tenantID string, g saas.Grant) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, workspace_ids,
		   expires_at_ms, granted_by, origin, invitation_id, created_at_ms, updated_at_ms)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13, $13)`,
		tenantID, g.GrantID, g.SourceID, g.TeamID, g.SubjectKind, g.SubjectID, g.Role, store.NonNil(g.WorkspaceIDs),
		g.ExpiresAtMs, g.GrantedBy, g.Origin, g.InvitationID, g.CreatedAtMs)
	return err
}

func (r grantRepo) Update(ctx context.Context, tenantID, grantID string, patch saas.GrantPatch, nowMs int64) (bool, error) {
	var workspaces []string
	if patch.WorkspaceIDs != nil {
		workspaces = store.NonNil(*patch.WorkspaceIDs)
	}
	role := ""
	if patch.Role != nil {
		role = *patch.Role
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE source_grants SET
		   role = CASE WHEN $3 <> '' THEN $3 ELSE role END,
		   workspace_ids = CASE WHEN $4 THEN $5::text[] ELSE workspace_ids END,
		   expires_at_ms = CASE WHEN $6 THEN NULL WHEN $7::bigint IS NOT NULL THEN $7::bigint ELSE expires_at_ms END,
		   updated_at_ms = $8
		 WHERE tenant_id = $1 AND grant_id = $2 AND revoked_at_ms IS NULL`,
		tenantID, grantID, role, patch.WorkspaceIDs != nil, workspaces, patch.ClearExpiry, patch.ExpiresAtMs, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r grantRepo) Revoke(ctx context.Context, tenantID, grantID, revokedBy string, nowMs int64) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`UPDATE source_grants SET revoked_at_ms = $3, revoked_by = $4, updated_at_ms = $3
		 WHERE tenant_id = $1 AND grant_id = $2 AND revoked_at_ms IS NULL`, tenantID, grantID, nowMs, revokedBy)
	return tag.RowsAffected() == 1, err
}

func (r grantRepo) RevokeAccount(ctx context.Context, tenantID, sourceID, accountID, revokedBy string, nowMs int64) ([]string, error) {
	return list(ctx, r.q, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `UPDATE source_grants SET revoked_at_ms = $4, revoked_by = $5, updated_at_ms = $4
	    WHERE tenant_id = $1 AND source_id = $2 AND subject_kind IN ('member', 'guest') AND subject_id = $3 AND revoked_at_ms IS NULL
	    RETURNING grant_id`, tenantID, sourceID, accountID, nowMs, revokedBy)
}
