package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// txAttempts 是死锁与序列化失败时 Tx 的最多尝试次数（首次加重试 3 次）。
const txAttempts = 4

// SaaS 返回 SaaS 的存储端口，与核心端口共用同一个连接池。
func (d *DB) SaaS() saas.Store { return &saasStore{d: d} }

type saasStore struct{ d *DB }

var _ saas.Store = (*saasStore)(nil)

func (s *saasStore) Read() saas.Reader { return reader{q: s.d.pool} }

func (s *saasStore) Tx(ctx context.Context, fn func(tx saas.Tx) error) error {
	var err error
	for attempt := 1; attempt <= txAttempts; attempt++ {
		err = s.once(ctx, fn)
		if err == nil || attempt == txAttempts || !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return mapSaaSError(err)
}

// once 跑一次事务；提交成功后才执行登记的回调。
func (s *saasStore) once(ctx context.Context, fn func(tx saas.Tx) error) error {
	t := &saasTx{reader: reader{}}
	err := s.d.tx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		t.q = tx
		return fn(t)
	})
	if err != nil {
		return err
	}
	for _, after := range t.after {
		after(context.WithoutCancel(ctx))
	}
	return nil
}

// retryable：死锁（40P01）与序列化失败（40001）。
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// mapSaaSError 把唯一冲突与 team_has_owner 换成业务错误；其余原样返回。
func mapSaaSError(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		switch {
		case pgErr.ConstraintName == saas.ConstraintTeamSlug:
			return &saas.Error{Code: saas.ErrTeamSlugTaken.Code, Constraint: pgErr.ConstraintName}
		case pgErr.ConstraintName == saas.ConstraintAccountsEmail:
			return &saas.Error{Code: saas.ErrEmailTaken.Code, Constraint: pgErr.ConstraintName}
		default:
			return &saas.Error{Code: saas.ErrConflict.Code, Constraint: pgErr.ConstraintName}
		}
	case "23514":
		if pgErr.ConstraintName == saas.ConstraintTeamHasOwner {
			return &saas.Error{Code: saas.ErrTeamLastOwner.Code, Constraint: pgErr.ConstraintName}
		}
	}
	return err
}

type reader struct{ q querier }

func (r reader) Ping(ctx context.Context) error {
	var one int
	return r.q.QueryRow(ctx, "SELECT 1").Scan(&one)
}

type saasTx struct {
	reader
	after []func(ctx context.Context)
}

var _ saas.Tx = (*saasTx)(nil)

func (t *saasTx) AfterCommit(fn func(ctx context.Context)) { t.after = append(t.after, fn) }

func (t *saasTx) Accounts() saas.AccountRepo       { return accountRepo{t.q} }
func (t *saasTx) Teams() saas.TeamRepo             { return teamRepo{t.q} }
func (t *saasTx) Invitations() saas.InvitationRepo { return struct{}{} }
func (t *saasTx) Hosts() saas.HostRepo             { return struct{}{} }
func (t *saasTx) Grants() saas.GrantRepo           { return struct{}{} }
func (t *saasTx) Trust() saas.TrustRepo            { return struct{}{} }
func (t *saasTx) Audit() saas.AuditRepo            { return struct{}{} }
func (t *saasTx) Jobs() saas.JobRepo               { return struct{}{} }
func (t *saasTx) Settings() saas.SettingsRepo      { return struct{}{} }
func (t *saasTx) Issued() saas.IssuedRepo          { return struct{}{} }

type accountRepo struct{ q querier }

func (r accountRepo) LockAccount(ctx context.Context, tenantID, accountID string) (bool, error) {
	return lockRow(ctx, r.q, "SELECT 1 FROM accounts WHERE tenant_id = $1 AND account_id = $2 FOR UPDATE", tenantID, accountID)
}

type teamRepo struct{ q querier }

func (r teamRepo) LockTeam(ctx context.Context, tenantID, teamID string) (bool, error) {
	return lockRow(ctx, r.q, "SELECT 1 FROM teams WHERE tenant_id = $1 AND team_id = $2 FOR UPDATE", tenantID, teamID)
}

func lockRow(ctx context.Context, q querier, query string, args ...any) (bool, error) {
	var one int
	err := q.QueryRow(ctx, query, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
