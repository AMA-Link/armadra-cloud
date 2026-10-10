/**
 * 迁移：读 `@armadra/control-store-sql/migrations/NNNN_*.sql`，对账 `relay_migrations`，在一个
 * `BEGIN IMMEDIATE` 事务里补跑没跑过的（多进程同时打开时只有一个真正执行）。对账与执行在
 * `apply.ts`（与运行时无关，Workers 的 DO SQLite 共用）；这里只管从目录读文件。
 *
 * 迁移目录的位置：打成单文件时在 `out/migrations/`（构建脚本拷过去），源码运行时在
 * `packages/control-store-sql/migrations/`。
 */
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  parseMigrationName,
  type MigrationFile,
} from "@armadra/control-store-sql/ledger";

import { applyMigrations, migrationFile } from "./apply.ts";
import type { SqlDatabase } from "./db.ts";

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
  return files.map(
    ({ file }) =>
      migrationFile(file, readFileSync(join(dir, file))) as MigrationFile,
  );
}

/** 跑到最新；答这次跑了几个（实现在 `apply.ts`，Workers 共用）。 */
export const migrate: (
  db: SqlDatabase,
  migrations: readonly MigrationFile[],
  now?: () => number,
) => number = applyMigrations;
