package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func nowMs() int64 { return time.Now().UnixMilli() }

// ledgerDDL 是账本的建表语句（迁移器在空库上先建它）；与 TypeScript 一侧逐字相同。
const ledgerDDL = "CREATE TABLE relay_migrations (id INTEGER PRIMARY KEY, name TEXT NOT NULL, sha384 TEXT NOT NULL, applied_at_ms INTEGER NOT NULL) STRICT"

// migrate 对账 relay_migrations 并在一个 BEGIN IMMEDIATE 事务里补跑没跑过的
// （多进程同时打开时只有一个真正执行）。答这次跑了几个。没有账本却已经有业务表的库不认。
func (d *DB) migrate(ctx context.Context, migrations []store.Migration, now func() int64) (int, error) {
	count := 0
	err := d.write(ctx, func(q *sql.Conn) error {
		var found int
		err := q.QueryRowContext(ctx,
			"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'relay_migrations'").Scan(&found)
		hasLedger := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !hasLedger {
			var tables int
			if err := q.QueryRowContext(ctx,
				`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE '\_cf\_%' ESCAPE '\'`).Scan(&tables); err != nil {
				return err
			}
			if tables > 0 {
				return store.Errorf(store.KindUnknown, "数据库没有迁移账本，拒绝启动：%s", d.path)
			}
			if _, err := q.ExecContext(ctx, ledgerDDL); err != nil {
				return err
			}
		}
		applied, err := readLedger(ctx, q)
		if err != nil {
			return err
		}
		pending, err := store.PendingMigrations(applied, migrations)
		if err != nil {
			var ledger *store.LedgerError
			if errors.As(err, &ledger) {
				return store.LedgerToStoreError(ledger, "："+d.path)
			}
			return err
		}
		for _, migration := range pending {
			if _, err := q.ExecContext(ctx, migration.SQL); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx,
				"INSERT INTO relay_migrations (id, name, sha384, applied_at_ms) VALUES (?, ?, ?, ?)",
				migration.ID, migration.Name, migration.SHA384, now()); err != nil {
				return err
			}
		}
		count = len(pending)
		return nil
	})
	return count, err
}

func readLedger(ctx context.Context, q *sql.Conn) ([]store.Applied, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, name, sha384 FROM relay_migrations ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Applied
	for rows.Next() {
		var row store.Applied
		if err := rows.Scan(&row.ID, &row.Name, &row.SHA384); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
