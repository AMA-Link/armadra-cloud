/**
 * 打开控制面的 PostgreSQL 库：建连接池 → 探活 →（给了 schema 就建它）→ 迁移到最新（`migrate.ts`）。连不上、库不认识、
 * 账本对不上、库比程序新——一律拒绝启动（{@link PostgresStoreError}），绝不自动清库或重建。
 *
 * 连接串里有口令：错误消息、日志都不带它（只报驱动的错误码）。
 */
import { createDb, pingDb } from "@armadra/cloud-shared/db";
import type { MigrationFile } from "@armadra/control-store-sql/ledger";

import type { Sql } from "./db.ts";
import { PostgresStoreError } from "./errors.ts";
import { loadMigrations, migratePostgres } from "./migrate.ts";

export interface OpenPostgresOptions {
  /** `postgres://…`（含口令，不进日志与错误）。 */
  url: string;
  /** 用哪个 schema（`search_path`）；缺省是连接的缺省 schema（通常 `public`）。 */
  schema?: string;
  /** 连接池上限；缺省 10。 */
  max?: number;
  /** 连接名（`application_name`，`pg_stat_activity` 里看得到）。 */
  applicationName?: string;
  /** 迁移目录；缺省随包的。 */
  migrationsDir?: string;
  migrations?: readonly MigrationFile[];
  now?: () => number;
}

export interface PostgresDatabase {
  readonly sql: Sql;
  readonly schema: string | undefined;
  close(): Promise<void>;
}

const SCHEMA_NAME = /^[a-z_][a-z0-9_]{0,62}$/;

/** 驱动错误 → 不带连接串的短句。 */
function unreachable(error: unknown): PostgresStoreError {
  const code =
    error && typeof error === "object" && "code" in error
      ? String((error as { code: unknown }).code)
      : "unknown";
  return new PostgresStoreError("unreachable", `PostgreSQL 连不上：${code}`);
}

/**
 * 显式给了 schema 时先建它（`CREATE SCHEMA IF NOT EXISTS`，空 schema，不动已有的任何东西）；
 * 之后的迁移照样对账，已有表却没有账本的 schema 一律拒绝。
 */
export async function ensureSchema(
  sql: Sql,
  schema: string | undefined,
): Promise<void> {
  if (schema === undefined) return;
  if (!SCHEMA_NAME.test(schema))
    throw new PostgresStoreError(
      "unknown",
      `schema 名只能是小写字母、数字与下划线：${schema}`,
    );
  await sql.unsafe(`CREATE SCHEMA IF NOT EXISTS ${schema}`);
}

/** 只建连接池（不探活、不迁移）：多节点控制面的监听连接与测试用。 */
export function connectPostgres(
  options: Pick<
    OpenPostgresOptions,
    "url" | "schema" | "max" | "applicationName"
  >,
): Sql {
  if (options.schema !== undefined && !SCHEMA_NAME.test(options.schema))
    throw new PostgresStoreError(
      "unknown",
      `schema 名只能是小写字母、数字与下划线：${options.schema}`,
    );
  const connection: Record<string, string> = {};
  if (options.schema) connection.search_path = options.schema;
  if (options.applicationName)
    connection.application_name = options.applicationName;
  return createDb(options.url, {
    max: options.max ?? 10,
    ...(Object.keys(connection).length > 0 ? { connection } : {}),
  });
}

export async function openPostgres(
  options: OpenPostgresOptions,
): Promise<PostgresDatabase> {
  const migrations =
    options.migrations ?? loadMigrations(options.migrationsDir);
  const sql = connectPostgres(options);
  try {
    try {
      await pingDb(sql);
    } catch (error) {
      throw unreachable(error);
    }
    await ensureSchema(sql, options.schema);
    await migratePostgres(sql, migrations, options.now);
  } catch (error) {
    await sql.end({ timeout: 5 });
    throw error;
  }
  return {
    sql,
    schema: options.schema,
    close: () => sql.end({ timeout: 5 }),
  };
}
