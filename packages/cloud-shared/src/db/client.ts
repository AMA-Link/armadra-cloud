/**
 * PostgreSQL 客户端（postgres.js）。只负责建连接池与探活；迁移运行器在
 * C2-1 落地（`db/migrate.ts`），在那之前 `armadra-cloud migrate` 只确认库可达。
 */
import postgres from "postgres";

export type Sql = postgres.Sql;

export function createDb(url: string, options: { max?: number } = {}): Sql {
  return postgres(url, {
    max: options.max ?? 10,
    idle_timeout: 30,
    connect_timeout: 10,
    // 不把服务端 NOTICE 打到 stdout（它可能带 SQL 片段）。
    onnotice: () => {},
  });
}

export async function pingDb(sql: Sql): Promise<void> {
  await sql`select 1`;
}
