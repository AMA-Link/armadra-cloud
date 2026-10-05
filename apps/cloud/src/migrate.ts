/**
 * `armadra-cloud migrate`。迁移运行器（账本 `cloud_migrations`、SHA-384、advisory
 * lock、拒绝条件）在 C2-1 落地；在那之前迁移目录必须为空，这里只确认库可达。
 * 目录里已经有迁移文件时拒绝执行，绝不跳过。
 */
import { createDb, pingDb } from "@armadra/cloud-shared/db";
import { readdirSync } from "node:fs";

import type { MigrateConfig } from "./config.ts";

export class MigrationsNotSupportedError extends Error {
  constructor(count: number) {
    super(
      `迁移目录里有 ${count} 个迁移，但本版本还没有迁移运行器（C2-1），拒绝执行`,
    );
    this.name = "MigrationsNotSupportedError";
  }
}

export function listMigrations(directory: string): string[] {
  return readdirSync(directory)
    .filter((name) => /^\d{4}_[a-z0-9_]+\.sql$/.test(name))
    .sort();
}

export async function migrate(
  config: MigrateConfig,
): Promise<{ applied: number; pending: number }> {
  const migrations = listMigrations(config.migrationsDir);
  if (migrations.length > 0)
    throw new MigrationsNotSupportedError(migrations.length);
  const sql = createDb(config.databaseUrl, { max: 1 });
  try {
    await pingDb(sql);
  } finally {
    await sql.end({ timeout: 5 });
  }
  return { applied: 0, pending: 0 };
}
