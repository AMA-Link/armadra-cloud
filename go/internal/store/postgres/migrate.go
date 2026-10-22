package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// ledgerDDL 是账本的建表语句（迁移器在空 schema 上先建它）；与 TypeScript 一侧逐字相同。
const ledgerDDL = "CREATE TABLE cloud_migrations (id integer PRIMARY KEY, name text NOT NULL, sha384 text NOT NULL, applied_at_ms bigint NOT NULL)"

// migrationLock 是 pg_advisory_xact_lock 的键：「ARMD」。
const migrationLock = 0x41524d44

// migrate 在一个事务里对账 cloud_migrations 并补跑没跑过的。事务开头拿 advisory lock，
// 多个节点同时启动时只有一个真正执行，其余等它提交后对账为最新。
//
// 拒绝（store.Error），绝不自动清库或重建：当前 schema 没有账本却已有表、账本与程序带的迁移
// 对不上（名字或 SHA-384 不同、编号不连续）、账本比程序多。
//
// apply 为假时只对账不动库（serve 用）：账本落后（含空库）答 KindBehind，别的问题与上面相同。
func (d *DB) migrate(ctx context.Context, migrations []store.Migration, now func() int64, apply bool) (int, error) {
	count := 0
	err := d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
			return err
		}
		var schema *string
		if err := tx.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
			return err
		}
		if schema == nil {
			return store.Errorf(store.KindUnknown, "search_path 指向的 schema 不存在，拒绝启动")
		}
		var ledger int
		err := tx.QueryRow(ctx, `SELECT 1 FROM information_schema.tables
		  WHERE table_schema = current_schema() AND table_name = 'cloud_migrations'`).Scan(&ledger)
		hasLedger := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !hasLedger {
			var tables int
			if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM information_schema.tables
			  WHERE table_schema = current_schema()`).Scan(&tables); err != nil {
				return err
			}
			if tables > 0 {
				return store.Errorf(store.KindUnknown, "数据库没有迁移账本却已有表，拒绝启动：schema %s", *schema)
			}
			if !apply {
				return store.Errorf(store.KindBehind, "数据库版本落后，先运行 armadra-cloud migrate")
			}
			if _, err := tx.Exec(ctx, ledgerDDL); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, "SELECT id, name, sha384 FROM cloud_migrations ORDER BY id")
		if err != nil {
			return err
		}
		var applied []store.Applied
		for rows.Next() {
			var row store.Applied
			var id int32
			if err := rows.Scan(&id, &row.Name, &row.SHA384); err != nil {
				rows.Close()
				return err
			}
			row.ID = int(id)
			applied = append(applied, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		pending, err := store.PendingMigrations(applied, migrations)
		if err != nil {
			var problem *store.LedgerError
			if errors.As(err, &problem) {
				return store.LedgerToStoreError(problem, "")
			}
			return err
		}
		if !apply {
			if len(pending) > 0 {
				return store.Errorf(store.KindBehind, "数据库版本落后，先运行 armadra-cloud migrate")
			}
			return nil
		}
		for _, migration := range pending {
			// 没有参数时 pgx 走简单协议，一个文件里可以有多条语句。
			if _, err := tx.Exec(ctx, migration.SQL); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				"INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms) VALUES ($1, $2, $3, $4)",
				int32(migration.ID), migration.Name, migration.SHA384, now()); err != nil {
				return err
			}
		}
		count = len(pending)
		return nil
	})
	return count, err
}
