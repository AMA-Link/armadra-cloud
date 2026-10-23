package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 邀请的 PostgreSQL 实现（迁移 0007 invitations；设计 §4.4）。令牌只存哈希。

const invitationColumns = `invitation_id, kind, email, COALESCE(team_id, ''), COALESCE(team_role, ''), COALESCE(source_id, ''),
	COALESCE(host_role, ''), workspace_ids, COALESCE(token_hash, ''), state, invited_by, COALESCE(decided_by, ''),
	created_at_ms, expires_at_ms, grant_expires_at_ms, decided_at_ms, sent_at_ms, resend_count`

func scanInvitation(row scanner) (saas.Invitation, error) {
	var (
		i        saas.Invitation
		teamRole string
	)
	err := row.Scan(&i.InvitationID, &i.Kind, &i.Email, &i.TeamID, &teamRole, &i.SourceID, &i.HostRole, &i.WorkspaceIDs,
		&i.TokenHash, &i.State, &i.InvitedBy, &i.DecidedBy, &i.CreatedAtMs, &i.ExpiresAtMs, &i.GrantExpiresAtMs,
		&i.DecidedAtMs, &i.SentAtMs, &i.ResendCount)
	i.TeamRole = saas.TeamRole(teamRole)
	i.WorkspaceIDs = store.NonNil(i.WorkspaceIDs)
	return i, err
}

func (r reader) Invitation(ctx context.Context, tenantID, invitationID string) (*saas.Invitation, error) {
	i, err := scanInvitation(r.q.QueryRow(ctx,
		"SELECT "+invitationColumns+" FROM invitations WHERE tenant_id = $1 AND invitation_id = $2", tenantID, invitationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

func (r reader) ReceivedInvitations(ctx context.Context, tenantID, email string, nowMs int64) ([]saas.Invitation, error) {
	return list(ctx, r.q, scanInvitation,
		"SELECT "+invitationColumns+` FROM invitations WHERE tenant_id = $1 AND email = lower($2) AND state = 'pending'
		 AND expires_at_ms > $3 ORDER BY created_at_ms DESC, invitation_id`, tenantID, email, nowMs)
}

type invitationRepo struct{ q querier }

func (r invitationRepo) Lock(ctx context.Context, tenantID, invitationID string) (*saas.Invitation, error) {
	i, err := scanInvitation(r.q.QueryRow(ctx,
		"SELECT "+invitationColumns+" FROM invitations WHERE tenant_id = $1 AND invitation_id = $2 FOR UPDATE", tenantID, invitationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

func (r invitationRepo) ExpireStale(ctx context.Context, scope saas.TeamScope, email string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		`UPDATE invitations SET state = 'expired', decided_at_ms = $1
		 WHERE tenant_id = $2 AND kind = 'team' AND team_id = $3 AND email = lower($4) AND state = 'pending' AND expires_at_ms <= $1`,
		nowMs, scope.TenantID(), scope.TeamID(), email)
	return err
}

func (r invitationRepo) Insert(ctx context.Context, scope saas.TeamScope, i saas.Invitation) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO invitations (tenant_id, invitation_id, kind, email, team_id, team_role, state, invited_by, created_at_ms, expires_at_ms)
		 VALUES ($1, $2, 'team', lower($3), $4, $5, 'pending', $6, $7, $8)`,
		scope.TenantID(), i.InvitationID, i.Email, scope.TeamID(), string(i.TeamRole), i.InvitedBy, i.CreatedAtMs, i.ExpiresAtMs)
	return err
}

func (r invitationRepo) List(ctx context.Context, scope saas.TeamScope) ([]saas.Invitation, error) {
	return list(ctx, r.q, scanInvitation,
		"SELECT "+invitationColumns+" FROM invitations WHERE tenant_id = $1 AND team_id = $2 AND kind = 'team' ORDER BY created_at_ms DESC, invitation_id",
		scope.TenantID(), scope.TeamID())
}

func (r invitationRepo) PendingSeats(ctx context.Context, scope saas.TeamScope, nowMs int64) (saas.Seats, error) {
	var s saas.Seats
	err := r.q.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE team_role <> 'guest'), count(*) FILTER (WHERE team_role = 'guest')
		 FROM invitations WHERE tenant_id = $1 AND team_id = $2 AND kind = 'team' AND state = 'pending' AND expires_at_ms > $3`,
		scope.TenantID(), scope.TeamID(), nowMs).Scan(&s.Members, &s.Guests)
	return s, err
}

func (r invitationRepo) Decide(ctx context.Context, tenantID, invitationID, state, decidedBy string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		"UPDATE invitations SET state = $1, decided_by = NULLIF($2, ''), decided_at_ms = $3 WHERE tenant_id = $4 AND invitation_id = $5",
		state, decidedBy, nowMs, tenantID, invitationID)
	return err
}

func (r invitationRepo) Reissue(ctx context.Context, tenantID, invitationID string, expiresAtMs int64) error {
	_, err := r.q.Exec(ctx,
		`UPDATE invitations SET expires_at_ms = $1, token_hash = NULL, sent_at_ms = NULL, resend_count = resend_count + 1
		 WHERE tenant_id = $2 AND invitation_id = $3`, expiresAtMs, tenantID, invitationID)
	return err
}

func (r invitationRepo) SetToken(ctx context.Context, tenantID, invitationID, tokenHash string, nowMs int64) error {
	_, err := r.q.Exec(ctx, "UPDATE invitations SET token_hash = $1, sent_at_ms = $2 WHERE tenant_id = $3 AND invitation_id = $4",
		tokenHash, nowMs, tenantID, invitationID)
	return err
}

func (r invitationRepo) ExpireDue(ctx context.Context, tenantID string, nowMs int64) ([]saas.Invitation, error) {
	return list(ctx, r.q, scanInvitation,
		`UPDATE invitations SET state = 'expired', decided_at_ms = $1
		 WHERE tenant_id = $2 AND state = 'pending' AND expires_at_ms <= $1 RETURNING `+invitationColumns, nowMs, tenantID)
}
