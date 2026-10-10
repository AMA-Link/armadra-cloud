/**
 * 迁移的对账与执行（与运行时无关）：读到的迁移文件（Node 从目录读，Workers 打进包里）交给
 * {@link applyMigrations}，在一个写事务里对账 `relay_migrations` 并补跑没跑过的。未知 / 损坏 /
 * 对不上一律抛 {@link SqliteStoreError}，绝不自动清库或重建。
 */
import { createHash } from "node:crypto";

import {
  LEDGER_DDL,
  LedgerError,
  parseMigrationName,
  pendingMigrations,
  type AppliedMigration,
  type MigrationFile,
} from "@armadra/control-store-sql/ledger";

import type { SqlDatabase } from "./db.ts";
import { SqliteStoreError } from "./errors.ts";

const decoder = new TextDecoder("utf-8", { fatal: true });

/** 一个迁移文件（`0001_init.sql` + 字节）→ 账本要的记录；不是迁移文件名答 null。 */
export function migrationFile(
  file: string,
  bytes: Uint8Array,
): MigrationFile | null {
  const parsed = parseMigrationName(file);
  if (!parsed) return null;
  return {
    id: parsed.id,
    name: parsed.name,
    sha384: createHash("sha384").update(bytes).digest("hex"),
    sql: decoder.decode(bytes),
  };
}

function hasTable(db: SqlDatabase, name: string): boolean {
  return (
    db.get(
      "SELECT 1 AS found FROM sqlite_master WHERE type = 'table' AND name = ?",
      name,
    ) !== undefined
  );
}

function applied(db: SqlDatabase): AppliedMigration[] {
  return db
    .all("SELECT id, name, sha384 FROM relay_migrations ORDER BY id")
    .map((row) => ({
      id: Number(row.id),
      name: String(row.name),
      sha384: String(row.sha384),
    }));
}

/**
 * 跑到最新；答这次跑了几个。没有账本却已经有业务表的库不认（运行时自己的内部表
 * `sqlite_*` 与 DO 的 `_cf_*` 不算）。
 */
export function applyMigrations(
  db: SqlDatabase,
  migrations: readonly MigrationFile[],
  now: () => number = Date.now,
): number {
  try {
    return db.transaction(() => {
      if (!hasTable(db, "relay_migrations")) {
        const tables = db.get(
          "SELECT count(*) AS n FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' AND name NOT LIKE '\\_cf\\_%' ESCAPE '\\'",
        );
        if (Number(tables?.n ?? 0) > 0)
          throw new SqliteStoreError(
            "unknown",
            `数据库没有迁移账本，拒绝启动：${db.path}`,
          );
        db.exec(LEDGER_DDL);
      }
      const pending = pendingMigrations(applied(db), migrations);
      for (const migration of pending) {
        db.exec(migration.sql);
        db.run(
          "INSERT INTO relay_migrations (id, name, sha384, applied_at_ms) VALUES (?, ?, ?, ?)",
          migration.id,
          migration.name,
          migration.sha384,
          now(),
        );
      }
      return pending.length;
    });
  } catch (error) {
    if (error instanceof LedgerError)
      throw new SqliteStoreError(
        error.problem === "newer" ? "newer" : "unknown",
        `${error.message}，拒绝启动：${db.path}`,
      );
    throw error;
  }
}
