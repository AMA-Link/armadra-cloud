/**
 * `--store postgres` 与 `--control postgres|redis`：存储迁移往返（file → sqlite → postgres → sqlite →
 * file 逐字段等价）、拒绝条件，以及两个节点共用一个库起服务。每个用例一个临时 schema，用完删掉。
 */
import { randomBytes } from "node:crypto";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { isDeepStrictEqual } from "node:util";

import { createLogger } from "@armadra/cloud-shared/log";
import { afterAll, afterEach, describe, expect, it } from "vitest";

import { PASSWORD, testDatabase } from "../personal.ts";
import type { PersonalServeOptions } from "../../src/personal/args.ts";
import { runLinksRevoke, runRotateKey } from "../../src/personal/commands.ts";
import { createState } from "../../src/personal/init.ts";
import { migrateStore } from "../../src/personal/migrate.ts";
import { startPersonalRelay } from "../../src/personal/serve.ts";
import {
  StateError,
  StateStore,
  type State,
} from "../../src/personal/state.ts";
import {
  openPersonalStore,
  resolveStoreKind,
} from "../../src/personal/store.ts";
import type { DatabaseOptions } from "../../src/personal/store-types.ts";

const root = mkdtempSync(join(tmpdir(), "relay-pg-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));
const drops: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const drop of drops.splice(0)) await drop();
});

const NOW = Date.UTC(2026, 9, 10, 12);
const SOURCE = "0123456789abcdef0123456789abcdef";
const LINK = "0123456789abcdef";

async function database(): Promise<DatabaseOptions> {
  const db = await testDatabase();
  drops.push(db.drop);
  return db.database;
}

let counter = 0;
/** 一个只有状态文件的数据目录，每张表、每个可选字段都放一点。 */
async function legacyDir(): Promise<{ dir: string; state: State }> {
  counter += 1;
  const dir = join(root, `legacy-${counter}`);
  const store = await createState({
    dataDir: dir,
    account: "me",
    password: PASSWORD,
    issuer: "https://relay.test:8443",
    store: "file",
    nowMs: NOW,
  });
  await store.rotateKey(NOW + 1);
  await store.close();
  StateStore.open(dir).update((draft) => {
    draft.sources.push({
      sourceId: SOURCE,
      name: "我的 MacBook",
      kind: "desktop",
      publicJwk: { kty: "OKP", crv: "Ed25519", x: "x".repeat(43) },
      coreVersion: "0.3.0",
      capabilities: ["terminal", "links.scope"],
      registeredAtMs: NOW,
      lastSeenAtMs: NOW + 5,
      revokedAtMs: null,
      trustedOrigins: ["https://app.example.test"],
    });
    draft.registrationTokens.push({
      tokenHash: "a".repeat(64),
      expiresAtMs: NOW + 60_000,
    });
    draft.devices.push({
      deviceId: "d".repeat(32),
      subject: `guest:${LINK}:${"e".repeat(16)}`,
      platform: "ios",
      name: "访客的手机",
      refreshHash: "b".repeat(64),
      previousRefreshHash: "c".repeat(64),
      rotation: 3,
      createdAtMs: NOW,
      expiresAtMs: NOW + 86_400_000,
      revokedAtMs: null,
      lastSeenAtMs: NOW + 7,
    });
    draft.links.push({
      linkId: LINK,
      sourceId: SOURCE,
      invitationId: "inv-1",
      label: "给同事",
      role: "viewer",
      secretHash: "f".repeat(64),
      maxUses: 2,
      uses: 1,
      createdAtMs: NOW,
      expiresAtMs: NOW + 86_400_000,
      revokedAtMs: null,
      scope: { workspaceId: "ws_1", readOnly: true },
    });
    draft.settings.displayName = "家里的中转";
    draft.settings.rateLimits = { anonymous: 30 };
    draft.settings.trustedProxy = {
      proxies: ["127.0.0.1/32"],
      prefer: "real-ip",
    };
  });
  return { dir, state: StateStore.open(dir).current };
}

const codeOf = async (run: () => Promise<unknown>) => {
  try {
    await run();
  } catch (error) {
    return error instanceof StateError ? error.code : (error as Error).message;
  }
  return "ok";
};

async function snapshotOf(dir: string, db?: DatabaseOptions): Promise<State> {
  const store = await openPersonalStore(dir, undefined, db);
  try {
    return await store.snapshot();
  } finally {
    await store.close();
  }
}

describe("迁移往返", () => {
  it("file → sqlite → postgres → sqlite → file：每一步逐字段等价，原存储改名留作回退，库里的数据不删", async () => {
    const db = await database();
    const { dir, state } = await legacyDir();

    expect((await migrateStore(dir, "sqlite", { nowMs: NOW })).from).toBe(
      "file",
    );
    expect(isDeepStrictEqual(await snapshotOf(dir), state)).toBe(true);

    const toPostgres = await migrateStore(dir, "postgres", {
      nowMs: NOW + 1,
      database: db,
    });
    expect(toPostgres.from).toBe("sqlite");
    expect(existsSync(join(dir, "relay.db"))).toBe(false);
    expect(existsSync(join(dir, "postgres.json"))).toBe(true);
    expect(resolveStoreKind(dir)).toEqual({ kind: "postgres", legacy: false });
    expect(await snapshotOf(dir, db)).toEqual(state);
    // 标记里不带连接串。
    expect(
      readdirSync(dir).filter((name) => name.startsWith("postgres")),
    ).toEqual(["postgres.json"]);

    // 在 PostgreSQL 上改一点：迁回去时改动都在。
    await runLinksRevoke(dir, LINK, () => {}, db);
    const changed = await snapshotOf(dir, db);
    expect(changed.revision).toBe(state.revision + 1);
    expect(changed.links[0]?.revokedAtMs).not.toBeNull();
    expect(changed.devices[0]?.revokedAtMs).not.toBeNull();

    const toSqlite = await migrateStore(dir, "sqlite", {
      nowMs: NOW + 2,
      database: db,
    });
    expect(toSqlite.from).toBe("postgres");
    expect(existsSync(join(dir, "postgres.json"))).toBe(false);
    expect(existsSync(toSqlite.kept)).toBe(true);
    expect(await snapshotOf(dir)).toEqual(changed);

    await migrateStore(dir, "file", { nowMs: NOW + 3 });
    expect(StateStore.open(dir).current).toEqual(changed);

    // 迁走之后 PostgreSQL 里的租户原样还在：再迁回去被拒绝（已有个人中转），不覆盖。
    expect(
      await codeOf(() => migrateStore(dir, "postgres", { database: db })),
    ).toBe("exists");
    expect(existsSync(join(dir, "postgres.json"))).toBe(false);
    expect(existsSync(join(dir, "state.json"))).toBe(true);
  });

  it("file → postgres 直接迁；之后不写 --store 也按 PostgreSQL 开，忘了连接串拒绝启动", async () => {
    const db = await database();
    const { dir, state } = await legacyDir();
    await migrateStore(dir, "postgres", { nowMs: NOW, database: db });
    expect(existsSync(join(dir, "state.json"))).toBe(false);
    expect(await snapshotOf(dir, db)).toEqual(state);
    await expect(openPersonalStore(dir)).rejects.toThrow(/--database-url/);
    expect(await codeOf(() => openPersonalStore(dir, "sqlite"))).toBe(
      "invalid",
    );
  });
});

describe("两个节点", () => {
  const quiet = {
    log: createLogger({ name: "relay", level: "error", write: () => {} }),
  };
  const passwordFile = join(root, "pw");
  writeFileSync(passwordFile, PASSWORD);
  const base = (
    dataDir: string,
    db: DatabaseOptions,
  ): PersonalServeOptions => ({
    dataDir,
    host: "127.0.0.1",
    port: 8443,
    listen: { host: "127.0.0.1", port: 0 },
    tls: "plain",
    logLevel: "error",
    account: "me",
    passwordFile,
    store: "postgres",
    database: db,
  });

  it("--store postgres --control postgres：第二个节点打开第一个建好的库；轮换后两边的 JWKS 都有新钥；/ready 探库", async () => {
    const db = await database();
    const a = await startPersonalRelay(
      {
        ...base(join(root, `node-a-${counter}`), db),
        control: "postgres",
        nodeId: "node-a",
      },
      quiet,
    );
    const b = await startPersonalRelay(
      {
        ...base(join(root, `node-b-${counter}`), db),
        control: "postgres",
        nodeId: "node-b",
      },
      quiet,
    );
    try {
      const kids = async (url: string) =>
        (
          (await (await fetch(`${url}/.well-known/jwks.json`)).json()) as {
            keys: { kid: string }[];
          }
        ).keys.map((key) => key.kid);
      expect(await kids(a.url)).toEqual(["k1"]);
      expect(await kids(b.url)).toEqual(["k1"]);
      await runRotateKey(b.store.dataDir, () => {}, db);
      const deadline = Date.now() + 5_000;
      while (Date.now() < deadline) {
        if (
          (await kids(a.url)).includes("k2") &&
          (await kids(b.url)).includes("k2")
        )
          break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      expect(await kids(a.url)).toEqual(["k1", "k2"]);
      expect(await kids(b.url)).toEqual(["k1", "k2"]);
      const ready = await fetch(`${a.url}/ready`);
      expect(ready.status).toBe(200);
      expect(JSON.stringify(await ready.json())).toContain("database");
    } finally {
      await a.close();
      await b.close();
    }
  });

  it("--control redis：起得来，/ready 探 Redis；连不上的 Redis 拒绝启动", async () => {
    const url = process.env.REDIS_URL;
    if (!url) throw new Error("REDIS_URL 未设置");
    const db = await database();
    const relay = await startPersonalRelay(
      {
        ...base(join(root, `redis-${randomBytes(4).toString("hex")}`), db),
        control: "redis",
        redisUrl: url,
        nodeId: "node-r",
      },
      quiet,
    );
    try {
      const ready = await fetch(`${relay.url}/ready`);
      expect(ready.status).toBe(200);
      expect(JSON.stringify(await ready.json())).toContain("redis");
    } finally {
      await relay.close();
    }
    await expect(
      startPersonalRelay(
        {
          ...base(join(root, `redis-bad-${counter}`), db),
          control: "redis",
          redisUrl: "redis://127.0.0.1:1",
          nodeId: "node-r",
        },
        quiet,
      ),
    ).rejects.toThrow(/Redis 连不上/);
  });
});
