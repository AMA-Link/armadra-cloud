package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 主机的 PostgreSQL 实现（迁移 0006 source_spaces、registration_tokens.team_id；设计 §4.5）。

// hostColumns 是目录查询的列：源记录（s.）+ 空间行（p.）+ 所有者的显示名 + 查询者在团队里的角色。
const hostColumns = `s.source_id, s.name, s.kind, s.public_jwk_json, s.core_version, s.capabilities_json,
	s.registered_at_ms, s.last_seen_at_ms, s.revoked_at_ms, s.trusted_origins_json,
	COALESCE(p.team_id, ''), p.owner_account_id, o.display_name, p.tags, p.settings, p.created_at_ms, p.updated_at_ms,
	COALESCE(vm.role, ''), COALESCE(t.settings->>'defaultHostRole', '')`

func scanHost(row scanner) (saas.Host, error) {
	var (
		h                          saas.Host
		jwk, capabilities, origins string
		settings                   []byte
		role                       string
	)
	if err := row.Scan(&h.SourceID, &h.Name, &h.Kind, &jwk, &h.CoreVersion, &capabilities,
		&h.RegisteredAtMs, &h.LastSeenAtMs, &h.RevokedAtMs, &origins,
		&h.TeamID, &h.OwnerAccountID, &h.OwnerName, &h.Tags, &settings, &h.CreatedAtMs, &h.UpdatedAtMs, &role, &h.TeamDefaultRole); err != nil {
		return saas.Host{}, err
	}
	if err := decodeJSON(jwk, &h.PublicJWK); err != nil {
		return saas.Host{}, err
	}
	if err := decodeJSON(capabilities, &h.Capabilities); err != nil {
		return saas.Host{}, err
	}
	if err := decodeJSON(origins, &h.TrustedOrigins); err != nil {
		return saas.Host{}, err
	}
	h.Capabilities = store.NonNil(h.Capabilities)
	h.TrustedOrigins = store.NonNil(h.TrustedOrigins)
	h.Tags = store.NonNil(h.Tags)
	h.Settings = settings
	h.ViewerRole = saas.TeamRole(role)
	return h, nil
}

// likeEscaper 把模糊词里的 LIKE 通配符转义（配 ESCAPE '\'）。
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// visibleSQL 是目录可见性（设计 §4.6）：$2 是查询者，$3 是时钟。
//
//   - 个人空间：所有者，以及被授予 guest 的账号。
//   - 团队主机（团队未进恢复期）：团队 owner / admin 看到全部（他们管得了）；member 看到自己登记的，以及
//     member / team 授予到的；guest 角色的成员只看 guest 授予到的；billing 与外人什么都看不到。
//   - 授予只算没撤销、没到期的。与 grants 包的有效权限合成保持一致（表驱动测试逐格对照）。
const visibleSQL = `(
  (p.team_id IS NULL AND (p.owner_account_id = $2 OR EXISTS (
     SELECT 1 FROM source_grants g WHERE g.tenant_id = p.tenant_id AND g.source_id = p.source_id
       AND g.subject_kind = 'guest' AND g.subject_id = $2 AND g.revoked_at_ms IS NULL
       AND (g.expires_at_ms IS NULL OR g.expires_at_ms > $3))))
  OR (p.team_id IS NOT NULL AND vm.role IS NOT NULL AND t.deleted_at_ms IS NULL AND (
     vm.role IN ('owner', 'admin')
     OR (vm.role = 'member' AND p.owner_account_id = $2)
     OR EXISTS (
       SELECT 1 FROM source_grants g WHERE g.tenant_id = p.tenant_id AND g.source_id = p.source_id
         AND g.revoked_at_ms IS NULL AND (g.expires_at_ms IS NULL OR g.expires_at_ms > $3)
         AND ((vm.role IN ('owner', 'admin', 'member')
               AND ((g.subject_kind = 'member' AND g.subject_id = $2) OR (g.subject_kind = 'team' AND g.subject_id = p.team_id)))
              OR (vm.role = 'guest' AND g.subject_kind = 'guest' AND g.subject_id = $2))))))`

// ListHosts 列出 ViewerID 此刻能看到的主机。可见性写在这条查询里（visibleSQL），不在调用方。
func (r reader) ListHosts(ctx context.Context, tenantID string, f saas.HostFilter) ([]saas.Host, error) {
	args := []any{tenantID, f.ViewerID, f.NowMs}
	where := []string{"p.tenant_id = $1", visibleSQL}
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.SourceID != "" {
		where = append(where, "p.source_id = "+arg(f.SourceID))
	}
	switch f.SpaceKind {
	case saas.SpacePersonal:
		where = append(where, "p.team_id IS NULL")
	case saas.SpaceTeam:
		where = append(where, "p.team_id = "+arg(f.TeamID))
	}
	if f.Revoked {
		where = append(where, "s.revoked_at_ms IS NOT NULL")
	} else {
		where = append(where, "s.revoked_at_ms IS NULL")
	}
	if len(f.Tags) > 0 {
		where = append(where, "p.tags @> "+arg(f.Tags)+"::text[]")
	}
	if f.Query != "" {
		where = append(where, "s.name ILIKE "+arg("%"+likeEscaper.Replace(f.Query)+"%")+` ESCAPE '\'`)
	}
	if f.After != nil {
		where = append(where, "(s.registered_at_ms, s.source_id) > ("+arg(f.After.RegisteredAtMs)+", "+arg(f.After.SourceID)+")")
	}
	query := "SELECT " + hostColumns + `
		FROM source_spaces p
		JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		JOIN accounts o ON o.tenant_id = p.tenant_id AND o.account_id = p.owner_account_id
		LEFT JOIN teams t ON t.tenant_id = p.tenant_id AND t.team_id = p.team_id
		LEFT JOIN team_members vm ON vm.tenant_id = p.tenant_id AND vm.team_id = p.team_id AND vm.account_id = $2
		WHERE ` + strings.Join(where, " AND ") + " ORDER BY s.registered_at_ms, s.source_id"
	if f.Limit > 0 {
		query += " LIMIT " + arg(f.Limit)
	}
	return list(ctx, r.q, scanHost, query, args...)
}

type hostRepo struct{ q querier }

func (r hostRepo) LockSource(ctx context.Context, tenantID, sourceID string) error {
	_, err := r.q.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "host:"+tenantID+":"+sourceID)
	return err
}

func (r reader) SpaceOf(ctx context.Context, tenantID, sourceID string) (*saas.HostSpace, error) {
	var (
		space        saas.HostSpace
		settings     *string
		teamSettings string
	)
	err := r.q.QueryRow(ctx,
		`SELECT p.source_id, COALESCE(p.team_id, ''), p.owner_account_id, s.revoked_at_ms IS NOT NULL, p.settings::text,
		        COALESCE(t.settings::text, ''), COALESCE(t.status, ''), COALESCE(t.deleted_at_ms IS NOT NULL, false)
		 FROM source_spaces p LEFT JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 LEFT JOIN teams t ON t.tenant_id = p.tenant_id AND t.team_id = p.team_id
		 WHERE p.tenant_id = $1 AND p.source_id = $2`, tenantID, sourceID).
		Scan(&space.SourceID, &space.TeamID, &space.OwnerAccountID, &space.Revoked, &settings, &teamSettings, &space.TeamStatus, &space.TeamDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if settings != nil {
		space.Settings = []byte(*settings)
	}
	if teamSettings != "" {
		space.TeamSettings = []byte(teamSettings)
	}
	return &space, nil
}

func (r hostRepo) CountPersonal(ctx context.Context, tenantID, accountID, excludeSourceID string) (int, error) {
	var n int
	err := r.q.QueryRow(ctx,
		`SELECT count(*)::int FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 WHERE p.tenant_id = $1 AND p.team_id IS NULL AND p.owner_account_id = $2 AND s.revoked_at_ms IS NULL AND p.source_id <> $3`,
		tenantID, accountID, excludeSourceID).Scan(&n)
	return n, err
}

func (r hostRepo) CountTeam(ctx context.Context, scope saas.TeamScope, excludeSourceID string) (int, error) {
	var n int
	err := r.q.QueryRow(ctx,
		`SELECT count(*)::int FROM source_spaces p JOIN sources s ON s.tenant_id = p.tenant_id AND s.source_id = p.source_id
		 WHERE p.tenant_id = $1 AND p.team_id = $2 AND s.revoked_at_ms IS NULL AND p.source_id <> $3`,
		scope.TenantID(), scope.TeamID(), excludeSourceID).Scan(&n)
	return n, err
}

func (r hostRepo) UpsertSpace(ctx context.Context, tenantID string, space saas.HostSpace, nowMs int64) error {
	// 同一空间的重登记保留标签与设置；换了空间（撤销后重新登记到别处）则重置。
	_, err := r.q.Exec(ctx,
		`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms)
		 VALUES ($1, $2, NULLIF($3, ''), $4, $5, $5)
		 ON CONFLICT (tenant_id, source_id) DO UPDATE SET
		   tags = CASE WHEN source_spaces.team_id IS NOT DISTINCT FROM EXCLUDED.team_id THEN source_spaces.tags ELSE '{}' END,
		   settings = CASE WHEN source_spaces.team_id IS NOT DISTINCT FROM EXCLUDED.team_id THEN source_spaces.settings ELSE NULL END,
		   team_id = EXCLUDED.team_id, owner_account_id = EXCLUDED.owner_account_id, updated_at_ms = EXCLUDED.updated_at_ms`,
		tenantID, space.SourceID, space.TeamID, space.OwnerAccountID, nowMs)
	return err
}

func (r hostRepo) Patch(ctx context.Context, tenantID, sourceID, teamID string, patch saas.HostPatch, nowMs int64) (bool, error) {
	var tags []string
	if patch.Tags != nil {
		tags = store.NonNil(*patch.Tags)
	}
	var settings []byte
	if patch.Settings != nil {
		settings = patch.Settings
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE source_spaces SET
		   tags = CASE WHEN $4 THEN $5::text[] ELSE tags END,
		   settings = CASE WHEN $6 THEN $7::jsonb ELSE settings END,
		   updated_at_ms = $8
		 WHERE tenant_id = $1 AND source_id = $2 AND team_id IS NOT DISTINCT FROM NULLIF($3, '')`,
		tenantID, sourceID, teamID, patch.Tags != nil, tags, patch.Settings != nil, settings, nowMs)
	return tag.RowsAffected() == 1, err
}

func (r hostRepo) AddRegistration(ctx context.Context, tenantID string, token saas.RegistrationToken, nowMs int64, max int) (bool, error) {
	// 先清掉这个账号已过期的，再数还有效的：同时有效的令牌按账号计，不是整个租户。
	if _, err := r.q.Exec(ctx,
		"DELETE FROM registration_tokens WHERE tenant_id = $1 AND account_id = $2 AND expires_at_ms <= $3",
		tenantID, token.AccountID, nowMs); err != nil {
		return false, err
	}
	var live int
	if err := r.q.QueryRow(ctx,
		"SELECT count(*)::int FROM registration_tokens WHERE tenant_id = $1 AND account_id = $2", tenantID, token.AccountID).Scan(&live); err != nil {
		return false, err
	}
	if live >= max {
		return false, nil
	}
	_, err := r.q.Exec(ctx,
		"INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms, team_id) VALUES ($1, $2, $3, $4, NULLIF($5, ''))",
		tenantID, token.TokenHash, token.AccountID, token.ExpiresAtMs, token.TeamID)
	return err == nil, err
}

func (r hostRepo) PeekRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*saas.RegistrationToken, error) {
	var out saas.RegistrationToken
	err := r.q.QueryRow(ctx,
		`SELECT token_hash, account_id, COALESCE(team_id, ''), expires_at_ms FROM registration_tokens
		 WHERE tenant_id = $1 AND token_hash = $2 AND expires_at_ms > $3`, tenantID, tokenHash, nowMs).
		Scan(&out.TokenHash, &out.AccountID, &out.TeamID, &out.ExpiresAtMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !store.SameHex(out.TokenHash, tokenHash) {
		return nil, nil
	}
	return &out, nil
}

func (r hostRepo) ConsumeRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*saas.RegistrationToken, error) {
	// 哈希是 256 位随机令牌的 SHA-256，按它查不会泄露原文；命中后仍用定时安全比较复核。
	var out saas.RegistrationToken
	err := r.q.QueryRow(ctx,
		`DELETE FROM registration_tokens WHERE tenant_id = $1 AND token_hash = $2 AND expires_at_ms > $3
		 RETURNING token_hash, account_id, COALESCE(team_id, ''), expires_at_ms`, tenantID, tokenHash, nowMs).
		Scan(&out.TokenHash, &out.AccountID, &out.TeamID, &out.ExpiresAtMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !store.SameHex(out.TokenHash, tokenHash) {
		return nil, nil
	}
	return &out, nil
}
