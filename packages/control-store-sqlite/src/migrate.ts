/**
 * 迁移：读 `@armadra/control-store-sql/migrations/NNNN_*.sql`，对账 `relay_migrations`，在一个
 * `BEGIN IMMEDIATE` 事务里补跑没跑过的（多进程同时打开时只有一个真正执行）。
 *
 * 迁移目录的位置：打成单文件时在 `out/migrations/`（构建脚本拷过去），源码运行时在
 * `packages/control-store-sql/migrations/`。
 */
import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  LEDGER_DDL,
  LedgerError,
  parseMigrationName,
  pendingMigrations,
  type AppliedMigration,
  type MigrationFile,
} from "@armadra/control-store-sql/ledger";

import { SqliteStoreError } from "./errors.ts";
import type { Database } from "./open.ts";

function hasMigrations(dir: string): boolean {
  return (
    existsSync(dir) && readdirSync(dir).some((name) => parseMigrationName(name))
  );
}

/** 随包迁移目录：单文件旁的 `migrations/`，否则 `@armadra/control-store-sql` 包里的。 */
export function defaultMigrationsDir(): string {
  const bundled = fileURLToPath(new URL("./migrations/", import.meta.url));
  if (hasMigrations(bundled)) return bundled;
  const manifest = createRequire(import.meta.url).resolve(
    "@armadra/control-store-sql/package.json",
  );
  return join(dirname(manifest), "migrations");
}

export function loadMigrations(dir = defaultMigrationsDir()): MigrationFile[] {
  const files = readdirSync(dir)
    .map((file) => ({ file, parsed: parseMigrationName(file) }))
    .filter((entry) => entry.parsed !== null)
    .sort((left, right) => left.file.localeCompare(right.file));
  return files.map(({ file, parsed }) => {
    const bytes = readFileSync(join(dir, file));
    return {
      id: parsed?.id ?? 0,
      name: parsed?.name ?? file,
      sha384: createHash("sha384").update(bytes).digest("hex"),
      sql: bytes.toString("utf8"),
    };
  });
}

function hasTable(db: Database, name: string): boolean {
  return (
    db.get(
      "SELECT 1 AS found FROM sqlite_master WHERE type = 'table' AND name = ?",
      name,
    ) !== undefined
  );
}

function applied(db: Database): AppliedMigration[] {
  return db
    .all("SELECT id, name, sha384 FROM relay_migrations ORDER BY id")
    .map((row) => ({
      id: Number(row.id),
      name: String(row.name),
      sha384: String(row.sha384),
    }));
}

/** 跑到最新；答这次跑了几个。 */
export function migrate(
  db: Database,
  migrations: readonly MigrationFile[],
  now: () => number = Date.now,
): number {
  try {
    return db.transaction(() => {
      if (!hasTable(db, "relay_migrations")) {
        const tables = db.get(
          "SELECT count(*) AS n FROM sqlite_master WHERE type = 'table'",
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
