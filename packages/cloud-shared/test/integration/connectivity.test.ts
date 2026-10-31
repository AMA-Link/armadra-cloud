/**
 * 集成：真连 PostgreSQL。CI 用服务容器，本地先 `pnpm dev:up`，再用
 * `.env.dev` 里的连接串跑（见 README）。
 */
import { describe, expect, it } from "vitest";

import { createDb, pingDb } from "../../src/db/client.ts";

const databaseUrl =
  process.env.ARMADRA_CLOUD_TEST_DATABASE_URL ??
  process.env.ARMADRA_CLOUD_DATABASE_URL;

describe("PostgreSQL 可达", () => {
  it("PostgreSQL 17", async () => {
    expect(databaseUrl, "ARMADRA_CLOUD_TEST_DATABASE_URL 未设置").toBeTruthy();
    const sql = createDb(databaseUrl ?? "", { max: 1 });
    try {
      await pingDb(sql);
      const [row] = await sql<
        { server_version: string }[]
      >`show server_version`;
      expect(Number.parseInt(row?.server_version ?? "0", 10)).toBe(17);
    } finally {
      await sql.end({ timeout: 5 });
    }
  });
});
