/**
 * 端口实现对库的全部要求：postgres.js 的连接池（`Sql`）或它的事务句柄。读走池，写都在
 * `sql.begin` 事务里；同一租户的写先锁租户行（`SELECT … FOR UPDATE`），读-改-写之间别人插不进来
 * ——与 SQLite 的 `BEGIN IMMEDIATE` 同一个语义。
 */
import type { Sql } from "@armadra/cloud-shared/db";
import type postgres from "postgres";

export type { Sql };
export type Tx = postgres.TransactionSql;
export type Queryable = Sql | Tx;
export type Row = Record<string, unknown>;

/** 写事务：`fn` 抛错就回滚，原样抛出。 */
export function transaction<T>(
  sql: Sql,
  fn: (tx: Tx) => Promise<T>,
): Promise<T> {
  return sql.begin((tx) => fn(tx)) as Promise<T>;
}

/** 先锁租户行：同一租户的写串行（没有这个租户时什么也不锁）。 */
export async function lockTenant(tx: Tx, tenantId: string): Promise<void> {
  await tx`SELECT 1 FROM tenants WHERE tenant_id = ${tenantId} FOR UPDATE`;
}

/** 租户的修订加一（每个改了东西的写事务一次），答新修订号；没有这个租户答 null。 */
export async function bumpRevision(
  tx: Tx,
  tenantId: string,
): Promise<number | null> {
  const rows = await tx`
    UPDATE tenants SET revision = revision + 1 WHERE tenant_id = ${tenantId} RETURNING revision`;
  const row = rows[0];
  return row ? Number(row.revision) : null;
}
