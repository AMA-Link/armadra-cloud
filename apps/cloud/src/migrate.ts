/**
 * `armadra-cloud migrate`：PostgreSQL 迁移（`@armadra/control-store-postgres` 的迁移器——账本
 * `cloud_migrations`、SHA-384、advisory lock）。库不认识、账本对不上、库比程序新一律拒绝，绝不跳过、
 * 清库或重建。连接串含口令：错误只报驱动的错误码。
 */
import {
  listMigrationFiles,
  loadMigrations,
  migratePostgres,
} from "@armadra/control-store-postgres/migrate";
import { connectPostgres } from "@armadra/control-store-postgres/open";
import { PostgresStoreError } from "@armadra/control-store-postgres/errors";

import type { MigrateConfig } from "./config.ts";

export { listMigrationFiles as listMigrations };

export async function migrate(
  config: MigrateConfig,
): Promise<{ applied: number; pending: number }> {
  const migrations = loadMigrations(config.migrationsDir);
  const sql = connectPostgres({ url: config.databaseUrl, max: 1 });
  try {
    try {
      await sql`SELECT 1`;
    } catch (error) {
      throw new PostgresStoreError(
        "unreachable",
        `PostgreSQL 连不上：${(error as { code?: string }).code ?? "unknown"}`,
      );
    }
    const applied = await migratePostgres(sql, migrations);
    return { applied, pending: 0 };
  } finally {
    await sql.end({ timeout: 5 });
  }
}
