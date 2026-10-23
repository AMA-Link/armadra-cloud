package postgres

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// txAttempts 是死锁与序列化失败时 Tx 的最多尝试次数（首次加重试 3 次）。
const txAttempts = 4

// SaaS 返回 SaaS 的存储端口，与核心端口共用同一个连接池。
func (d *DB) SaaS() saas.Store { return &saasStore{d: d} }

type saasStore struct{ d *DB }

var _ saas.Store = (*saasStore)(nil)

func (s *saasStore) Read() saas.Reader { return reader{q: s.d.pool} }

func (s *saasStore) Queue() saas.JobQueue { return queue{d: s.d} }

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
	t := &saasTx{d: s.d}
	err := s.d.tx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		t.q, t.pgx = tx, tx
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
		case pgErr.ConstraintName == saas.ConstraintTeamMembersPK:
			return &saas.Error{Code: saas.ErrMemberExists.Code, Constraint: pgErr.ConstraintName}
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
	d      *DB
	pgx    pgx.Tx
	after  []func(ctx context.Context)
	locked lockState
}

// lockState 记本事务已拿过哪类锁、同类里最后锁的 id，用来强制统一的加锁顺序：
// 团队 → 账号 → 其余；同类多行按 id 升序（批量用 LockTeams / LockAccounts，它们自己排序）。
type lockState struct {
	account     bool
	lastAccount string
	lastTeam    string
}

var _ saas.Tx = (*saasTx)(nil)

func (t *saasTx) AfterCommit(fn func(ctx context.Context)) { t.after = append(t.after, fn) }

func (t *saasTx) Accounts() saas.AccountRepo       { return accountRepo{q: t.q, locked: &t.locked} }
func (t *saasTx) Teams() saas.TeamRepo             { return teamRepo{q: t.q, locked: &t.locked} }
func (t *saasTx) DeviceCodes() saas.DeviceCodeRepo { return deviceCodeRepo{q: t.q} }
func (t *saasTx) Invitations() saas.InvitationRepo { return invitationRepo{q: t.q} }
func (t *saasTx) Hosts() saas.HostRepo             { return hostRepo{q: t.q} }
func (t *saasTx) Grants() saas.GrantRepo           { return grantRepo{q: t.q} }
func (t *saasTx) Trust() saas.TrustRepo            { return trustRepo{q: t.q} }
func (t *saasTx) Audit() saas.AuditRepo            { return auditRepo{q: t.q} }
func (t *saasTx) Jobs() saas.JobRepo               { return jobRepo{q: t.q} }
func (t *saasTx) Settings() saas.SettingsRepo      { return struct{}{} }
func (t *saasTx) Issued() saas.IssuedRepo          { return issuedRepo{q: t.q} }

// Ports 把核心端口绑到本事务（TxPorts）。
func (t *saasTx) Ports() *store.Ports { return portsOf(ports{d: t.d, tx: t.pgx}) }

// TxPorts 是 tx.Ports() 的另一种写法：把核心端口绑到 SaaS 事务 tx 上。tx 必须来自本库的 SaaS().Tx。
func (d *DB) TxPorts(tx saas.Tx) *store.Ports { return tx.Ports() }

type accountRepo struct {
	q      querier
	locked *lockState
}

func (r accountRepo) LockAccount(ctx context.Context, tenantID, accountID string) (bool, error) {
	if accountID < r.locked.lastAccount {
		return false, saas.ErrLockOrder
	}
	found, err := lockRow(ctx, r.q, "SELECT 1 FROM accounts WHERE tenant_id = $1 AND account_id = $2 FOR UPDATE", tenantID, accountID)
	if err != nil {
		// 没锁上就不记：不能让失败的尝试改变后面的顺序判断。
		return false, err
	}
	r.locked.account, r.locked.lastAccount = true, accountID
	return found, nil
}

func (r accountRepo) LockAccounts(ctx context.Context, tenantID string, accountIDs ...string) error {
	for _, id := range sortedUnique(accountIDs) {
		if _, err := r.LockAccount(ctx, tenantID, id); err != nil {
			return err
		}
	}
	return nil
}

type teamRepo struct {
	q      querier
	locked *lockState
}

func (r teamRepo) LockTeam(ctx context.Context, tenantID, teamID string) (bool, error) {
	if r.locked.account || teamID < r.locked.lastTeam {
		return false, saas.ErrLockOrder
	}
	found, err := lockRow(ctx, r.q, "SELECT 1 FROM teams WHERE tenant_id = $1 AND team_id = $2 FOR UPDATE", tenantID, teamID)
	if err != nil {
		return false, err
	}
	r.locked.lastTeam = teamID
	return found, nil
}

func (r teamRepo) LockTeams(ctx context.Context, tenantID string, teamIDs ...string) error {
	for _, id := range sortedUnique(teamIDs) {
		if _, err := r.LockTeam(ctx, tenantID, id); err != nil {
			return err
		}
	}
	return nil
}

// sortedUnique 是升序去重的副本：批量加锁的固定顺序。
func sortedUnique(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return slices.Compact(out)
}

func lockRow(ctx context.Context, q querier, query string, args ...any) (bool, error) {
	var one int
	err := q.QueryRow(ctx, query, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
