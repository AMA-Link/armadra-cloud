/**
 * PostgreSQL 迁移：读 `migrations/NNNN_*.sql`（打成单文件时在旁边的 `pg-migrations/`），对账
 * `cloud_migrations`，在一个事务里补跑没跑过的。事务开头拿 advisory lock，多个节点同时启动时
 * 只有一个真正执行，其余等它提交后对账为最新。
 *
 * 拒绝（{@link PostgresStoreError}），绝不自动清库或重建：
 * - 当前 schema 没有账本却已有表（不是本程序的库）；
 * - 账本与程序带的迁移对不上（名字或 SHA-384 不同、编号不连续）；
 * - 账本比程序多（库是更新的版本写的）。
 */
import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

import {
  checkAvailable,
  LedgerError,
  parseMigrationName,
  pendingMigrations,
  type MigrationFile,
} from "@armadra/control-store-sql/ledger";

import { transaction, type Sql } from "./db.ts";
import { PostgresStoreError } from "./errors.ts";

/** 账本的建表语句（迁移器在空 schema 上先建它）。 */
export const LEDGER_DDL =
  "CREATE TABLE cloud_migrations (id integer PRIMARY KEY, name text NOT NULL, sha384 text NOT NULL, applied_at_ms bigint NOT NULL)";

/**
 * 迁移锁的名字。键是 `hashtextextended(current_schema() || ':' || 名字, 0)`，与 Go 一侧
 * （`go/internal/store/postgres` 的 `schemaLock`）逐字相同：同一 schema 的两种实现互斥，同库不同 schema 互不排队。
 */
const MIGRATION_LOCK = "migrate";

const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

function hasMigrations(dir: string): boolean {
  return (
    existsSync(dir) && readdirSync(dir).some((name) => parseMigrationName(name))
  );
}

/** 随包迁移目录：单文件旁的 `pg-migrations/`，否则本包的 `migrations/`。 */
export function defaultMigrationsDir(): string {
  const bundled = fileURLToPath(new URL("./pg-migrations/", import.meta.url));
  if (hasMigrations(bundled)) return bundled;
  return fileURLToPath(new URL("../migrations/", import.meta.url));
}

/** 一个迁移文件（`0001_x.sql` + 字节）→ 账本要的记录；不是迁移文件名答 null。 */
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

/** 目录里的迁移文件名（`NNNN_name.sql`），按编号排序。 */
export function listMigrationFiles(dir = defaultMigrationsDir()): string[] {
  return readdirSync(dir)
    .filter((file) => parseMigrationName(file) !== null)
    .sort((left, right) => left.localeCompare(right));
}

/** 读迁移文件；编号不从 1 起连续是打包错误，直接拒绝。 */
export function loadMigrations(dir = defaultMigrationsDir()): MigrationFile[] {
  const files = listMigrationFiles(dir).map(
    (file) =>
      migrationFile(file, readFileSync(join(dir, file))) as MigrationFile,
  );
  try {
    checkAvailable(files);
  } catch (error) {
    throw new PostgresStoreError(
      "unknown",
      `${(error as Error).message}，拒绝执行`,
    );
  }
  return files;
}

/** 跑到最新；答这次跑了几个。 */
export async function migratePostgres(
  sql: Sql,
  migrations: readonly MigrationFile[] = loadMigrations(),
  now: () => number = Date.now,
): Promise<number> {
  try {
    return await transaction(sql, async (tx) => {
      await tx`SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':' || ${MIGRATION_LOCK}::text, 0))`;
      const [schema] = await tx`SELECT current_schema() AS name`;
      if (!schema?.name)
        throw new PostgresStoreError(
          "unknown",
          "search_path 指向的 schema 不存在，拒绝启动",
        );
      const ledger = await tx`
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = current_schema() AND table_name = 'cloud_migrations'`;
      if (ledger.length === 0) {
        const [tables] = await tx`
          SELECT count(*)::int AS n FROM information_schema.tables
          WHERE table_schema = current_schema()`;
        if (Number(tables?.n ?? 0) > 0)
          throw new PostgresStoreError(
            "unknown",
            `数据库没有迁移账本却已有表，拒绝启动：schema ${String(schema.name)}`,
          );
        await tx.unsafe(LEDGER_DDL);
      }
      const applied = (
        await tx`SELECT id, name, sha384 FROM cloud_migrations ORDER BY id`
      ).map((row) => ({
        id: Number(row.id),
        name: String(row.name),
        sha384: String(row.sha384),
      }));
      const pending = pendingMigrations(applied, migrations);
      for (const migration of pending) {
        await tx.unsafe(migration.sql).simple();
        await tx`
          INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms)
          VALUES (${migration.id}, ${migration.name}, ${migration.sha384}, ${now()})`;
      }
      return pending.length;
    });
  } catch (error) {
    if (error instanceof LedgerError)
      throw new PostgresStoreError(
        error.problem === "newer" ? "newer" : "unknown",
        `${error.message}，拒绝启动`,
      );
    throw error;
  }
}
