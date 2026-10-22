package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

func TestMapSaaSError(t *testing.T) {
	pg := func(code, constraint string) error {
		return fmt.Errorf("包了一层：%w", &pgconn.PgError{Code: code, ConstraintName: constraint})
	}
	for _, c := range []struct {
		name string
		in   error
		want gen.ErrorCode
	}{
		{"团队 slug", pg("23505", "teams_slug"), gen.ErrTeamSlugTaken},
		{"已是成员", pg("23505", "team_members_pkey"), gen.ErrMemberExists},
		{"邮箱", pg("23505", "accounts_email"), gen.ErrEmailTaken},
		{"别的唯一冲突", pg("23505", "source_grants_active"), gen.ErrConflict},
		{"最后一个 owner", pg("23514", "team_has_owner"), gen.ErrTeamLastOwner},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got *saas.Error
			if !errors.As(mapSaaSError(c.in), &got) || got.Code != c.want {
				t.Fatalf("want %s, got %v", c.want, mapSaaSError(c.in))
			}
		})
	}
	if err := mapSaaSError(pg("23514", "other_check")); errors.As(err, new(*saas.Error)) {
		t.Fatalf("别的 CHECK 不该映射：%v", err)
	}
	plain := errors.New("x")
	if mapSaaSError(plain) != plain || mapSaaSError(nil) != nil {
		t.Fatal("非数据库错误与 nil 原样返回")
	}
	if !errors.Is(mapSaaSError(pg("23505", "teams_slug")), saas.ErrTeamSlugTaken) || errors.Is(mapSaaSError(pg("23505", "teams_slug")), saas.ErrConflict) {
		t.Fatal("errors.Is 按错误码比较")
	}
}

func openSaaSStore(t *testing.T) (*DB, saas.Store) {
	t.Helper()
	url, schema := pgtest.Schema(t)
	db, err := Open(context.Background(), Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(context.Background(), "cloud", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://c.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	return db, db.SaaS()
}

func TestSaaSTxAfterCommit(t *testing.T) {
	ctx := context.Background()
	_, s := openSaaSStore(t)
	if err := s.Read().Ping(ctx); err != nil {
		t.Fatal(err)
	}

	var order []string
	err := s.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Ping(ctx); err != nil {
			return err
		}
		tx.AfterCommit(func(context.Context) { order = append(order, "a") })
		tx.AfterCommit(func(context.Context) { order = append(order, "b") })
		if len(order) != 0 {
			t.Fatal("提交前不该执行回调")
		}
		return nil
	})
	if err != nil || fmt.Sprint(order) != "[a b]" {
		t.Fatalf("提交后按序执行：%v %v", order, err)
	}

	order = nil
	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx saas.Tx) error {
		tx.AfterCommit(func(context.Context) { order = append(order, "x") })
		return boom
	})
	if !errors.Is(err, boom) || len(order) != 0 {
		t.Fatalf("回滚不执行回调：%v %v", order, err)
	}
}

func TestSaaSTxRetriesDeadlockAndDropsCallbacks(t *testing.T) {
	ctx := context.Background()
	_, s := openSaaSStore(t)
	runs, fired := 0, 0
	err := s.Tx(ctx, func(tx saas.Tx) error {
		runs++
		tx.AfterCommit(func(context.Context) { fired++ })
		if runs < 3 {
			return &pgconn.PgError{Code: "40P01"}
		}
		return nil
	})
	if err != nil || runs != 3 || fired != 1 {
		t.Fatalf("死锁应重试到成功，只执行成功那次的回调：runs=%d fired=%d err=%v", runs, fired, err)
	}

	runs = 0
	err = s.Tx(ctx, func(saas.Tx) error { runs++; return &pgconn.PgError{Code: "40001"} })
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || runs != 4 {
		t.Fatalf("最多重试 3 次（共 4 次）后返回原错误：runs=%d err=%v", runs, err)
	}

	runs = 0
	_ = s.Tx(ctx, func(saas.Tx) error { runs++; return &pgconn.PgError{Code: "23505"} })
	if runs != 1 {
		t.Fatalf("别的错误不重试：%d", runs)
	}
}

func TestSaaSTxMapsConstraintErrors(t *testing.T) {
	ctx := context.Background()
	db, s := openSaaSStore(t)
	pool := db.Pool()
	for _, id := range []string{"a1", "a2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
		  last_failure_at_ms, created_at_ms, updated_at_ms, email) VALUES ('cloud', $1, $1, '{}', 0, 0, 0, 1, 1, $1 || '@x.test')`, id); err != nil {
			t.Fatal(err)
		}
	}
	// 骨架阶段仓储还没有写方法：借内部的事务助手直接执行 SQL，验证提交时的错误映射。
	run := func(sql string, args ...any) error {
		return mapSaaSError(db.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		}))
	}
	team := func(id, slug, owner string) error {
		return mapSaaSError(db.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms)
			  VALUES ('cloud', $1, $2, 'n', '{}', 'x', 1, 1)`, id, slug); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ('cloud', $1, $2, 'owner', 1)`, id, owner)
			return err
		}))
	}
	if err := team("t1", "one", "a1"); err != nil {
		t.Fatal(err)
	}
	if err := team("t2", "one", "a2"); !errors.Is(err, saas.ErrTeamSlugTaken) {
		t.Fatalf("slug 冲突：%v", err)
	}
	if err := run("UPDATE accounts SET email = 'a1@x.test' WHERE account_id = 'a2'"); !errors.Is(err, saas.ErrEmailTaken) {
		t.Fatalf("邮箱冲突：%v", err)
	}
	if err := run("DELETE FROM team_members WHERE account_id = 'a1'"); !errors.Is(err, saas.ErrTeamLastOwner) {
		t.Fatalf("最后一个 owner：%v", err)
	}

	// 仓储：LockTeam / LockAccount。
	err := s.Tx(ctx, func(tx saas.Tx) error {
		if ok, err := tx.Teams().LockTeam(ctx, "cloud", "t1"); err != nil || !ok {
			return fmt.Errorf("LockTeam t1：%v %v", ok, err)
		}
		if ok, err := tx.Teams().LockTeam(ctx, "cloud", "nope"); err != nil || ok {
			return fmt.Errorf("LockTeam nope：%v %v", ok, err)
		}
		if ok, err := tx.Accounts().LockAccount(ctx, "cloud", "a1"); err != nil || !ok {
			return fmt.Errorf("LockAccount a1：%v %v", ok, err)
		}
		if ok, err := tx.Accounts().LockAccount(ctx, "other", "a1"); err != nil || ok {
			return fmt.Errorf("别的租户不该锁到：%v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTeamScopeAccessors(t *testing.T) {
	var zero saas.TeamScope
	if !zero.IsZero() {
		t.Fatal("零值应被识别")
	}
	s := teamscope.Issue("cloud", "t1", teamscope.Admin)
	if s.IsZero() || s.TenantID() != "cloud" || s.TeamID() != "t1" || s.Role() != saas.TeamRole("admin") || !s.Role().Valid() {
		t.Fatalf("%+v", s)
	}
	if teamscope.Role("root").Valid() {
		t.Fatal("未知角色不合法")
	}
}
