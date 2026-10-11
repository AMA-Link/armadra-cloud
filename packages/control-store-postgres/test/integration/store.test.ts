/**
 * 迁移的拒绝条件、租户导入导出、签名钥轮换与并发写：只用临时 schema，用完删掉。
 */
import {
  DEFAULT_TENANT,
  type AccountRecord,
} from "@armadra/control-core/ports";
import { generateSigningKey } from "@armadra/control-core/signer";
import { afterEach, describe, expect, it } from "vitest";

import { PostgresStoreError } from "../../src/errors.ts";
import { loadMigrations, migratePostgres } from "../../src/migrate.ts";
import { connectPostgres, openPostgres } from "../../src/open.ts";
import { createPostgresPorts } from "../../src/ports.ts";
import { PostgresKeyCache, rotatePostgresKey } from "../../src/keys.ts";
import {
  exportTenant,
  importTenant,
  readRevision,
  revokedIds,
  updateTenant,
  type TenantSnapshot,
} from "../../src/tenants.ts";
import { tempSchema, type TempSchema } from "../pg.ts";

const NOW = 1_800_000_000_000;
const schemas: TempSchema[] = [];
afterEach(async () => {
  for (const schema of schemas.splice(0)) await schema.drop();
});
const fresh = () => {
  const schema = tempSchema();
  schemas.push(schema);
  return schema;
};

const account: AccountRecord = {
  accountId: "acct:owner",
  name: "owner",
  password: {
    kdf: "scrypt",
    cost: 16384,
    block: 8,
    parallel: 1,
    length: 32,
    salt: "c2FsdA",
    hash: "aGFzaA",
  },
  createdAtMs: NOW,
  updatedAtMs: NOW,
  failures: 0,
  lockedUntilMs: 0,
  lastFailureAtMs: 0,
};

function snapshot(): TenantSnapshot {
  return {
    tenant: {
      issuer: "https://relay.example.test",
      allowGuests: true,
      trustedOrigins: ["https://app.example.test"],
      rateLimits: { anonymous: 30 },
      trustedProxy: { proxies: ["127.0.0.1/32"], prefer: "real-ip" },
      displayName: "家里",
      revision: 7,
    },
    accounts: [account],
    signingKeys: [generateSigningKey("k1", NOW)],
    sources: [
      {
        sourceId: "51".repeat(16),
        name: "mac",
        kind: "desktop",
        publicJwk: { kty: "OKP", crv: "Ed25519", x: "x".repeat(43) },
        coreVersion: "0.3.0",
        capabilities: ["terminal"],
        registeredAtMs: NOW,
        lastSeenAtMs: null,
        revokedAtMs: NOW + 1,
        trustedOrigins: [],
      },
    ],
    registrations: [
      {
        tokenHash: "ab".repeat(32),
        expiresAtMs: NOW + 60_000,
        accountId: account.accountId,
      },
    ],
    devices: [],
    links: [
      {
        linkId: "1".repeat(16),
        sourceId: "51".repeat(16),
        invitationId: "inv-1",
        label: "访客",
        role: "viewer",
        secretHash: "cd".repeat(32),
        maxUses: 3,
        uses: 1,
        createdAtMs: NOW,
        expiresAtMs: NOW + 86_400_000,
        revokedAtMs: null,
        scope: { workspaceId: "w1", readOnly: true },
      },
    ],
  };
}

describe("迁移", () => {
  it("空 schema 跑到最新；再开一次不重跑；并发打开只有一个真正执行", async () => {
    const schema = fresh();
    await schema.create();
    const sql = connectPostgres({
      url: schema.url,
      schema: schema.name,
      max: 4,
    });
    try {
      const counts = await Promise.all([
        migratePostgres(sql),
        migratePostgres(sql),
        migratePostgres(sql),
      ]);
      expect(counts.sort()).toEqual([0, 0, 3]);
      expect(await migratePostgres(sql)).toBe(0);
      const ledger =
        await sql`SELECT id, name FROM cloud_migrations ORDER BY id`;
      expect(ledger.map((row) => row.name)).toEqual([
        "0001_control_init",
        "0002_relay_control",
        "0003_traffic",
      ]);
    } finally {
      await sql.end({ timeout: 5 });
    }
  });

  it("没有账本却已有表、账本对不上、库比程序新：拒绝，什么也不删", async () => {
    const schema = fresh();
    await schema.create();
    const sql = connectPostgres({
      url: schema.url,
      schema: schema.name,
      max: 1,
    });
    try {
      await sql`CREATE TABLE someone_elses (id int)`;
      await expect(migratePostgres(sql)).rejects.toMatchObject({
        code: "unknown",
      });
      expect(
        (await sql`SELECT count(*)::int AS n FROM someone_elses`)[0]?.n,
      ).toBe(0);
      await sql`DROP TABLE someone_elses`;

      const migrations = loadMigrations();
      expect(await migratePostgres(sql, migrations.slice(0, 1))).toBe(1);
      // 账本前缀：补跑新的。
      expect(await migratePostgres(sql, migrations)).toBe(1);
      // 库比程序新。
      const error = await migratePostgres(sql, migrations.slice(0, 1)).catch(
        (caught: unknown) => caught,
      );
      expect(error).toBeInstanceOf(PostgresStoreError);
      expect(error).toMatchObject({ code: "newer" });
      // 已跑的迁移被改了字节。
      const tampered = migrations.map((row, index) =>
        index === 0 ? { ...row, sha384: "0".repeat(96) } : row,
      );
      await expect(migratePostgres(sql, tampered)).rejects.toMatchObject({
        code: "unknown",
      });
      expect(
        (await sql`SELECT count(*)::int AS n FROM cloud_migrations`)[0]?.n,
      ).toBe(2);
    } finally {
      await sql.end({ timeout: 5 });
    }
  });

  it("连不上：错误里不带连接串与口令", async () => {
    const error = await openPostgres({
      url: "postgres://someone:hunter2@127.0.0.1:1/nothing",
    }).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(PostgresStoreError);
    expect(error).toMatchObject({ code: "unreachable" });
    expect(String((error as Error).message)).not.toMatch(/hunter2|someone/);
  });

  it("给了不存在的 schema：建一个空的再迁移（不碰别的 schema）", async () => {
    const schema = fresh();
    const db = await openPostgres({
      url: schema.url,
      schema: schema.name,
      max: 1,
    });
    try {
      const [row] = await db.sql`SELECT current_schema() AS name`;
      expect(row?.name).toBe(schema.name);
      expect(
        (await db.sql`SELECT count(*)::int AS n FROM cloud_migrations`)[0]?.n,
      ).toBe(2);
    } finally {
      await db.close();
      // 不是经 tempSchema 建的：手动删。
      const admin = connectPostgres({ url: schema.url, max: 1 });
      await admin.unsafe(`DROP SCHEMA ${schema.name} CASCADE`);
      await admin.end({ timeout: 5 });
    }
  });

  it("schema 名只许小写字母、数字与下划线", () => {
    expect(() =>
      connectPostgres({ url: "postgres://x@127.0.0.1/x", schema: "a; drop" }),
    ).toThrow(PostgresStoreError);
  });
});

describe("租户与签名钥", () => {
  it("导入后导出逐字段一致（修订号照快照）；租户已存在拒绝", async () => {
    const db = await fresh().open();
    const input = snapshot();
    await importTenant(db.sql, DEFAULT_TENANT, input);
    expect(await exportTenant(db.sql, DEFAULT_TENANT)).toEqual(input);
    await expect(
      importTenant(db.sql, DEFAULT_TENANT, input),
    ).rejects.toMatchObject({ code: "exists" });
    expect(await revokedIds(db.sql, DEFAULT_TENANT)).toEqual({
      sources: ["51".repeat(16)],
      links: [],
    });
  });

  it("轮换：旧钥一小时后退役，缓存 refresh 后看得到；修订号加一", async () => {
    const db = await fresh().open();
    await importTenant(db.sql, DEFAULT_TENANT, snapshot());
    const keys = await PostgresKeyCache.load(db.sql, DEFAULT_TENANT);
    expect(keys.issuer()).toBe("https://relay.example.test");
    expect(keys.signingKeys().map((key) => key.kid)).toEqual(["k1"]);
    const kid = await rotatePostgresKey(db.sql, DEFAULT_TENANT, NOW + 10);
    expect(kid).toBe("k2");
    expect(keys.signingKeys().map((key) => key.kid)).toEqual(["k1"]);
    await keys.refresh();
    expect(keys.signingKeys().map((key) => [key.kid, key.retiredAtMs])).toEqual(
      [
        ["k1", NOW + 10 + 3_600_000],
        ["k2", null],
      ],
    );
    expect(await readRevision(db.sql, DEFAULT_TENANT)).toBe(8);
    await updateTenant(db.sql, DEFAULT_TENANT, (draft) => {
      draft.issuer = "https://relay.example.test";
    });
    expect(await readRevision(db.sql, DEFAULT_TENANT)).toBe(8);
  });

  it("并发的读-改-写不丢更新；fn 抛错什么也不写", async () => {
    const db = await fresh().open({ max: 8 });
    await importTenant(db.sql, DEFAULT_TENANT, snapshot());
    const writes: number[] = [];
    const ports = createPostgresPorts(db.sql, {
      onWrite: (_tenant, revision) => writes.push(revision),
    });
    await Promise.all(
      Array.from({ length: 20 }, () =>
        ports.accounts.update(DEFAULT_TENANT, account.accountId, (draft) => {
          if (draft) draft.failures += 1;
        }),
      ),
    );
    expect(
      (await ports.accounts.get(DEFAULT_TENANT, account.accountId))?.failures,
    ).toBe(20);
    expect(writes.sort((a, b) => a - b)).toEqual(
      Array.from({ length: 20 }, (_, index) => 8 + index),
    );
    await expect(
      ports.accounts.update(DEFAULT_TENANT, account.accountId, (draft) => {
        if (draft) draft.failures = 99;
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");
    expect(
      (await ports.accounts.get(DEFAULT_TENANT, account.accountId))?.failures,
    ).toBe(20);
    expect(writes).toHaveLength(20);
  });
});
