/**
 * `--control postgres`：两个节点（各自一个连接池，同一个临时 schema）经表与 LISTEN/NOTIFY 同步；
 * 监听连接被断开后重连，断线期间的撤销全量重读回来。
 */
import { randomBytes } from "node:crypto";

import { connectPostgres } from "@armadra/control-store-postgres/open";
import {
  tempSchema,
  type TempSchema,
} from "@armadra/control-store-postgres/testing/pg";
import { afterEach, expect, it, vi } from "vitest";

import {
  createPostgresControlPlane,
  type PostgresControlPlane,
} from "../../src/control/postgres.ts";
import { clusterContract, LIMITED } from "./cluster.ts";

const JWKS = () => ({ keys: [] });
const cleanups: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

async function node(
  schema: TempSchema,
  nodeId: string,
): Promise<{ control: PostgresControlPlane; applicationName: string }> {
  const applicationName = `relay-test-${randomBytes(4).toString("hex")}`;
  const sql = connectPostgres({
    url: schema.url,
    schema: schema.name,
    max: 4,
    applicationName,
  });
  const errors: unknown[] = [];
  const control = await createPostgresControlPlane({
    sql,
    issuer: "https://relay.example.test",
    jwks: JWKS,
    nodeId,
    limits: LIMITED,
    onError: (error) => errors.push(error),
  });
  cleanups.push(async () => {
    await control.close();
    await sql.end({ timeout: 5 });
  });
  return { control, applicationName };
}

async function migrated(): Promise<TempSchema> {
  const schema = tempSchema("s2_control");
  cleanups.push(() => schema.drop());
  await schema.open({ max: 1 });
  return schema;
}

clusterContract("PostgreSQL 控制面", async () => {
  const schema = await migrated();
  const [a, b] = await Promise.all([
    node(schema, "node-a"),
    node(schema, "node-b"),
  ]);
  return { a: a.control, b: b.control };
});

it("监听连接断开后重连：断线期间写进表的撤销全量重读回来", async () => {
  const schema = await migrated();
  const b = await node(schema, "node-b");
  const admin = connectPostgres({
    url: schema.url,
    schema: schema.name,
    max: 1,
  });
  cleanups.push(() => admin.end({ timeout: 5 }));
  const listening = async () =>
    admin`
      SELECT pid FROM pg_stat_activity
      WHERE application_name = ${b.applicationName} AND query ILIKE 'listen%'`;
  await vi.waitFor(async () => expect((await listening()).length).toBe(1));
  const [listener] = await listening();
  await admin`SELECT pg_terminate_backend(${listener?.pid as number})`;
  // 不发 NOTIFY，只写表：只有重连后的全量重读能让 b 知道。
  await admin`
    INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('jti-missed', ${Date.now() + 60_000})`;
  await vi.waitFor(
    async () =>
      expect(await b.control.tokens.isRevoked("jti-missed")).toBe(true),
    { timeout: 15_000, interval: 100 },
  );
  await vi.waitFor(async () => {
    const rows = await listening();
    expect(rows.length).toBe(1);
    expect(rows[0]?.pid).not.toBe(listener?.pid);
  });
});

it("没迁移过的库：拒绝启动，提示先迁移", async () => {
  const schema = tempSchema("s2_control");
  cleanups.push(() => schema.drop());
  await schema.create();
  const sql = connectPostgres({ url: schema.url, schema: schema.name, max: 1 });
  cleanups.push(() => sql.end({ timeout: 5 }));
  await expect(
    createPostgresControlPlane({
      sql,
      issuer: "https://relay.example.test",
      jwks: JWKS,
      nodeId: "node-a",
    }),
  ).rejects.toThrow(/先运行迁移/);
});
