/**
 * 打进 Worker 的 SQL 迁移：与 Node 中继同一份 `packages/control-store-sql/migrations/`（wrangler 按
 * `Text` 规则把 `.sql` 当字符串打包），SHA-384 按 UTF-8 字节算，与 `migrations.lock` 一致。新增迁移
 * 时在这里加一行（`migrations.test.ts` 核对与目录一一对应）。
 */
import type { MigrationFile } from "@armadra/control-store-sql/ledger";
import { migrationFile } from "@armadra/control-store-sqlite/apply";
import init from "@armadra/control-store-sql/migrations/0001_init.sql";
import traffic from "@armadra/control-store-sql/migrations/0002_traffic.sql";

const encoder = new TextEncoder();

const BUNDLED: readonly [string, string][] = [
  ["0001_init.sql", init],
  ["0002_traffic.sql", traffic],
];

export function bundledMigrations(): MigrationFile[] {
  return BUNDLED.map(
    ([file, text]) =>
      migrationFile(file, encoder.encode(text)) as MigrationFile,
  );
}

/** 打进来的迁移文件名（测试核对用）。 */
export const BUNDLED_MIGRATION_FILES: readonly string[] = BUNDLED.map(
  ([file]) => file,
);
