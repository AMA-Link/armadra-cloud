/**
 * 集成测试用的 PostgreSQL：连接串取 `ARMADRA_CLOUD_TEST_DATABASE_URL`（CI 服务容器；本地 `pnpm dev:up`
 * 后用一个单独的测试库），每个用例文件一个随机名的 schema，用完删掉——不碰开发数据。
 */
import { randomBytes } from "node:crypto";

import {
  connectPostgres,
  openPostgres,
  type PostgresDatabase,
} from "../src/open.ts";

export function testDatabaseUrl(): string {
  const url = process.env.ARMADRA_CLOUD_TEST_DATABASE_URL;
  if (!url) throw new Error("ARMADRA_CLOUD_TEST_DATABASE_URL 未设置");
  return url;
}

export interface TempSchema {
  readonly url: string;
  readonly name: string;
  /** 建 schema 并迁移到最新，答打开的库（`close()` 由 `drop()` 统一做）。 */
  open(options?: {
    max?: number;
    applicationName?: string;
  }): Promise<PostgresDatabase>;
  /** 只建空 schema（迁移的拒绝条件用）。 */
  create(): Promise<void>;
  drop(): Promise<void>;
}

export function tempSchema(prefix = "s2"): TempSchema {
  const url = testDatabaseUrl();
  const name = `${prefix}_${randomBytes(6).toString("hex")}`;
  const opened: PostgresDatabase[] = [];
  /** 还没完成的 open：drop 先等它们，不让晚到的 CREATE SCHEMA 漏删。 */
  const pending = new Set<Promise<unknown>>();
  let created = false;
  const admin = () => connectPostgres({ url, max: 1 });
  const create = async () => {
    if (created) return;
    const sql = admin();
    try {
      await sql.unsafe(`CREATE SCHEMA ${name}`);
      created = true;
    } finally {
      await sql.end({ timeout: 5 });
    }
  };
  return {
    url,
    name,
    create,
    open(options = {}) {
      const task = (async () => {
        await create();
        const db = await openPostgres({ url, schema: name, ...options });
        opened.push(db);
        return db;
      })();
      pending.add(task);
      void task.then(
        () => pending.delete(task),
        () => pending.delete(task),
      );
      return task;
    },
    async drop() {
      await Promise.allSettled([...pending]);
      for (const db of opened.splice(0)) await db.close();
      if (!created) return;
      const sql = admin();
      try {
        await sql.unsafe(`DROP SCHEMA ${name} CASCADE`);
      } finally {
        await sql.end({ timeout: 5 });
      }
    },
  };
}
