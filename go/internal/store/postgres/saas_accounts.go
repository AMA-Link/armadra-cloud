package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 账号的 SaaS 读写。核心表 accounts 的 SaaS 列（迁移 0004）只在这里读写；核心端口的 UPDATE 不碰它们。

const saasAccountColumns = accountColumns + `, email, email_verified_at_ms, display_name, locale, status, status_reason, scopes`

func toSaaSAccount(row scanner) (*saas.Account, error) {
	var (
		a        saas.Account
		password string
		failures int32
		email    *string
	)
	if err := row.Scan(&a.AccountID, &a.Name, &password, &failures, &a.LockedUntilMs, &a.LastFailureAtMs,
		&a.CreatedAtMs, &a.UpdatedAtMs, &email, &a.EmailVerifiedAtMs, &a.DisplayName, &a.Locale, &a.Status,
		&a.StatusReason, &a.Scopes); err != nil {
		return nil, err
	}
	a.Failures = int64(failures)
	if email != nil {
		a.Email = *email
	}
	a.Scopes = store.NonNil(a.Scopes)
	if err := decodeJSON(password, &a.Password); err != nil {
		return nil, err
	}
	return &a, nil
}

func (r reader) Account(ctx context.Context, tenantID, accountID string) (*saas.Account, error) {
	return one(toSaaSAccount(r.q.QueryRow(ctx,
		"SELECT "+saasAccountColumns+" FROM accounts WHERE tenant_id = $1 AND account_id = $2", tenantID, accountID)))
}

func (r reader) AccountByEmail(ctx context.Context, tenantID, email string) (*saas.Account, error) {
	return one(toSaaSAccount(r.q.QueryRow(ctx,
		"SELECT "+saasAccountColumns+" FROM accounts WHERE tenant_id = $1 AND email = $2", tenantID, strings.ToLower(email))))
}

func (r reader) Memberships(ctx context.Context, tenantID, accountID string) ([]saas.Membership, error) {
	return list(ctx, r.q, func(s scanner) (saas.Membership, error) {
		var m saas.Membership
		var role string
		err := s.Scan(&m.TeamID, &m.Slug, &m.Name, &role)
		m.Role = saas.TeamRole(role)
		return m, err
	}, `SELECT t.team_id, t.slug, t.name, m.role
	    FROM team_members m JOIN teams t ON t.tenant_id = m.tenant_id AND t.team_id = m.team_id
	    WHERE m.tenant_id = $1 AND m.account_id = $2 AND t.deleted_at_ms IS NULL
	    ORDER BY m.joined_at_ms, t.team_id`, tenantID, accountID)
}

func (r reader) Incident(ctx context.Context, tenantID string) (saas.Incident, error) {
	var raw []byte
	err := r.q.QueryRow(ctx, "SELECT value FROM platform_settings WHERE tenant_id = $1 AND key = 'incident'", tenantID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return saas.Incident{}, nil
	}
	if err != nil {
		return saas.Incident{}, err
	}
	var incident saas.Incident
	if err := json.Unmarshal(raw, &incident); err != nil {
		return saas.Incident{}, store.Errorf(store.KindCorrupt, "platform_settings.incident 不合格式，拒绝启动")
	}
	return incident, nil
}

func (r accountRepo) Create(ctx context.Context, tenantID string, a saas.Account) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
		   last_failure_at_ms, created_at_ms, updated_at_ms, email, email_verified_at_ms, display_name, locale,
		   status, status_reason, scopes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		tenantID, a.AccountID, a.Name, encodeJSON(a.Password), a.Failures, a.LockedUntilMs, a.LastFailureAtMs,
		a.CreatedAtMs, a.UpdatedAtMs, strings.ToLower(a.Email), a.EmailVerifiedAtMs, a.DisplayName,
		orDefault(a.Locale, "zh-CN"), orDefault(a.Status, saas.AccountActive), a.StatusReason, store.NonNil(a.Scopes))
	return err
}

func (r accountRepo) SetDisplayName(ctx context.Context, tenantID, accountID, displayName string, nowMs int64) error {
	_, err := r.q.Exec(ctx,
		"UPDATE accounts SET display_name = $1, updated_at_ms = $2 WHERE tenant_id = $3 AND account_id = $4",
		displayName, nowMs, tenantID, accountID)
	return err
}
