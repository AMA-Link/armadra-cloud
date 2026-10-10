/**
 * PostgreSQL 客户端（postgres.js）。只负责建连接池与探活；迁移与端口实现在
 * `@armadra/control-store-postgres`。
 */
import postgres from "postgres";

export type Sql = postgres.Sql;

export interface DbOptions {
  max?: number;
  /** 连接参数（`search_path`、`application_name`……），每条新连接都带上。 */
  connection?: Record<string, string>;
}

export function createDb(url: string, options: DbOptions = {}): Sql {
  return postgres(url, {
    max: options.max ?? 10,
    idle_timeout: 30,
    connect_timeout: 10,
    ...(options.connection ? { connection: options.connection } : {}),
    // 不把服务端 NOTICE 打到 stdout（它可能带 SQL 片段）。
    onnotice: () => {},
  });
}

export async function pingDb(sql: Sql): Promise<void> {
  await sql`select 1`;
}
