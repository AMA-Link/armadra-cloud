import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "@armadra/cloud-shared/log";
import { DEFAULT_LIMIT_RULES } from "@armadra/relay-kernel/control/types";
import { afterAll, expect, it } from "vitest";

import {
  databaseFor,
  PASSWORD,
  TEST_STORE_KINDS,
} from "../../test/personal.ts";
import type { PersonalServeOptions } from "./args.ts";
import { runRotateKey, runTraffic } from "./commands.ts";
import { edgeLimitRules, startPersonalRelay } from "./serve.ts";
import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { openDatabase } from "@armadra/control-store-sqlite/open";
import { updateTenant } from "@armadra/control-store-sqlite/tenants";
import { connectPostgres } from "@armadra/control-store-postgres/open";
import { updateTenant as updatePostgresTenant } from "@armadra/control-store-postgres/tenants";

import { StateError, StateStore } from "./state.ts";
import { openPersonalStore } from "./store.ts";
import { RELAY_VERSION } from "../version.ts";

const root = mkdtempSync(join(tmpdir(), "relay-personal-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));
const passwordFile = join(root, "pw");
writeFileSync(passwordFile, PASSWORD);

const base: Omit<PersonalServeOptions, "dataDir"> = {
  host: "127.0.0.1",
  port: 8443,
  listen: { host: "127.0.0.1", port: 0 },
  tls: "plain",
  logLevel: "error",
  account: "me",
  passwordFile,
};

const quiet = {
  log: createLogger({ name: "relay", level: "error", write: () => {} }),
};

it("首次自动初始化：新装缺省 SQLite（relay.db 0600）、数据目录 0700；/health 与 /ready 200", async () => {
  const dataDir = join(root, "a", "data");
  const relay = await startPersonalRelay({ ...base, dataDir }, quiet);
  try {
    expect(relay.store.kind).toBe("sqlite");
    expect(existsSync(join(dataDir, "relay.db"))).toBe(true);
    expect(existsSync(join(dataDir, "state.json"))).toBe(false);
    if (process.platform !== "win32") {
      expect(statSync(dataDir).mode & 0o777).toBe(0o700);
      expect(statSync(join(dataDir, "relay.db")).mode & 0o777).toBe(0o600);
    }
    const health = await fetch(`${relay.url}/health`);
    expect(await health.json()).toEqual({
      ok: true,
      mode: "personal",
      node: "127.0.0.1",
      version: RELAY_VERSION,
      tunnels: 0,
      streams: 0,
    });
    expect((await fetch(`${relay.url}/ready`)).status).toBe(200);
    // 明文模式（反向代理之后）：issuer 仍是 https，没有要钉的指纹。
    expect(relay.issuer).toBe("https://127.0.0.1:8443");
    expect(relay.fingerprint).toBeNull();
  } finally {
    await relay.close();
  }
});

it("没初始化又没给账号口令：拒绝启动", async () => {
  await expect(
    startPersonalRelay(
      {
        ...base,
        dataDir: join(root, "b"),
        account: undefined,
        passwordFile: undefined,
      },
      quiet,
    ),
  ).rejects.toThrow(StateError);
});

it("--store file 自动初始化写状态文件；之后不写 --store 照旧用它；--store 与目录里的不符就拒绝", async () => {
  const dataDir = join(root, "f");
  const first = await startPersonalRelay(
    { ...base, dataDir, store: "file" },
    quiet,
  );
  await first.close();
  expect(existsSync(join(dataDir, "state.json"))).toBe(true);
  expect(existsSync(join(dataDir, "relay.db"))).toBe(false);
  const again = await startPersonalRelay({ ...base, dataDir }, quiet);
  try {
    expect(again.store.kind).toBe("file");
  } finally {
    await again.close();
  }
  await expect(
    startPersonalRelay({ ...base, dataDir, store: "sqlite" }, quiet),
  ).rejects.toThrow(/migrate --to sqlite/);
  await expect(
    startPersonalRelay(
      { ...base, dataDir: join(root, "a", "data"), store: "file" },
      quiet,
    ),
  ).rejects.toThrow(/migrate --to file/);
  // 两种存储都在：拒绝启动，不替人挑。
  writeFileSync(join(dataDir, "relay.db"), "");
  await expect(startPersonalRelay({ ...base, dataDir }, quiet)).rejects.toThrow(
    /同时有/,
  );
});

it.each(TEST_STORE_KINDS)(
  "--public-origin 变了：issuer 跟着改并写回存储（%s）",
  async (store) => {
    const dataDir = join(root, `c-${store}`);
    const pg = await databaseFor(store);
    const first = await startPersonalRelay(
      { ...base, dataDir, store, ...pg.options },
      quiet,
    );
    await first.close();
    const second = await startPersonalRelay(
      {
        ...base,
        dataDir,
        publicOrigin: "https://relay.example",
        ...pg.options,
      },
      quiet,
    );
    try {
      expect(second.issuer).toBe("https://relay.example");
      const reopened = await openPersonalStore(
        dataDir,
        undefined,
        pg.options.database,
      );
      expect(reopened.issuer()).toBe("https://relay.example");
      await reopened.close();
      const info = (await (
        await fetch(`${second.url}/.well-known/armadra-platform`)
      ).json()) as { issuer: string; webApp: string | null };
      expect(info).toMatchObject({
        issuer: "https://relay.example",
        webApp: null,
      });
    } finally {
      await second.close();
      await pg.drop();
    }
  },
);

it("--web-root 有产物：/app/ 托管，platform.info 报 webApp", async () => {
  const web = join(root, "web");
  mkdirSync(web, { recursive: true });
  writeFileSync(
    join(web, "index.html"),
    "<!doctype html><title>armadra</title>",
  );
  const relay = await startPersonalRelay(
    { ...base, dataDir: join(root, "d"), webRoot: web },
    quiet,
  );
  try {
    expect(await (await fetch(`${relay.url}/app/`)).text()).toContain(
      "armadra",
    );
    const info = (await (
      await fetch(`${relay.url}/.well-known/armadra-platform`)
    ).json()) as { webApp: string | null };
    expect(info.webApp).toBe("https://127.0.0.1:8443/app/");
  } finally {
    await relay.close();
  }
});

it.each(TEST_STORE_KINDS)(
  "platform.info 报服务端名称：--name 优先，其次存储里的 displayName，缺省 --host（%s）",
  async (store) => {
    const dataDir = join(root, `name-${store}`);
    const pg = await databaseFor(store);
    const nameOf = async (url: string) =>
      (
        (await (await fetch(`${url}/.well-known/armadra-platform`)).json()) as {
          name?: string;
        }
      ).name;
    const first = await startPersonalRelay(
      { ...base, dataDir, store, ...pg.options },
      quiet,
    );
    try {
      expect(await nameOf(first.url)).toBe("127.0.0.1");
    } finally {
      await first.close();
    }
    if (store === "file")
      StateStore.open(dataDir).update((draft) => {
        draft.settings.displayName = "家里的中转";
      });
    else if (store === "postgres") {
      const database = pg.options.database;
      const sql = connectPostgres({
        url: database?.url ?? "",
        ...(database?.schema ? { schema: database.schema } : {}),
        max: 1,
      });
      await updatePostgresTenant(sql, DEFAULT_TENANT, (draft) => {
        draft.displayName = "家里的中转";
      });
      await sql.end({ timeout: 5 });
    } else {
      const db = openDatabase(join(dataDir, "relay.db"));
      updateTenant(db, DEFAULT_TENANT, (draft) => {
        draft.displayName = "家里的中转";
      });
      db.close();
    }
    const second = await startPersonalRelay(
      { ...base, dataDir, ...pg.options },
      quiet,
    );
    try {
      expect(await nameOf(second.url)).toBe("家里的中转");
    } finally {
      await second.close();
    }
    const third = await startPersonalRelay(
      { ...base, dataDir, name: "书房", ...pg.options },
      quiet,
    );
    try {
      expect(await nameOf(third.url)).toBe("书房");
    } finally {
      await third.close();
      await pg.drop();
    }
  },
);

it.each(TEST_STORE_KINDS)(
  "运行中另一个进程轮换签名钥：一秒内 JWKS 就有新钥（%s）",
  async (store) => {
    const dataDir = join(root, `e-${store}`);
    const pg = await databaseFor(store);
    const relay = await startPersonalRelay(
      { ...base, dataDir, store, ...pg.options },
      quiet,
    );
    try {
      await runRotateKey(dataDir, () => {}, pg.options.database);
      const deadline = Date.now() + 5_000;
      let kids: string[] = [];
      while (Date.now() < deadline) {
        const jwks = (await (
          await fetch(`${relay.url}/.well-known/jwks.json`)
        ).json()) as { keys: { kid: string }[] };
        kids = jwks.keys.map((key) => key.kid);
        if (kids.includes("k2")) break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      expect(kids).toEqual(["k1", "k2"]);
    } finally {
      await relay.close();
      await pg.drop();
    }
  },
  10_000,
);

it("边缘限流额度：命令行 > 状态文件 > 缺省；登录与链接的限流不可配、不变", () => {
  expect(edgeLimitRules(undefined, undefined)).toEqual({});
  expect(edgeLimitRules({ token: 5 }, { token: 9, preflight: 7 })).toEqual({
    "edge.connect.token": { type: "rate", max: 5, windowMs: 60_000 },
    "edge.preflight.ip": { type: "rate", max: 7, windowMs: 60_000 },
  });
  const rules = edgeLimitRules({
    anonymous: 1,
    preflight: 1,
    token: 1,
    account: 1,
  });
  expect(Object.keys(rules).sort()).toEqual([
    "edge.connect.acct",
    "edge.connect.ip",
    "edge.connect.token",
    "edge.preflight.ip",
  ]);
  expect(DEFAULT_LIMIT_RULES["login.ip"]).toEqual({
    type: "rate",
    max: 20,
    windowMs: 60_000,
  });
  expect(DEFAULT_LIMIT_RULES["login.acct"]).toEqual({
    type: "rate",
    max: 20,
    windowMs: 60_000,
  });
  expect(DEFAULT_LIMIT_RULES["link.accept.ip"].type).toBe("rate");
});

it("边缘缺省额度按真实使用：iOS 全部跨源多次预检、同出口 IP 多设备、终端节点多条 WS", () => {
  const max = (kind: keyof typeof DEFAULT_LIMIT_RULES) =>
    DEFAULT_LIMIT_RULES[kind].max;
  expect(max("edge.preflight.ip")).toBe(1200);
  expect(max("edge.connect.token")).toBe(1200);
  expect(max("edge.connect.acct")).toBe(3600);
  expect(DEFAULT_LIMIT_RULES["edge.concurrent.acct"]).toEqual({
    type: "concurrent",
    max: 256,
  });
  // 一个账号的全部令牌加起来至少容得下三台设备各跑满自己的令牌额度。
  expect(max("edge.connect.acct")).toBeGreaterThanOrEqual(
    3 * max("edge.connect.token"),
  );
  // 无令牌的请求仍然严格。
  expect(max("edge.connect.ip")).toBe(60);
});

/** 预检打到不存在的源也会先过 `edge.preflight.ip`：限额 2，第 3 次起 429。 */
async function preflights(url: string, forwardedFor?: string) {
  const out: number[] = [];
  for (let i = 0; i < 3; i += 1) {
    const response = await fetch(
      `${url}/s/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/x`,
      {
        method: "OPTIONS",
        headers: {
          origin: "https://a.example",
          "access-control-request-method": "GET",
          ...(forwardedFor ? { "x-forwarded-for": forwardedFor } : {}),
        },
      },
    );
    out.push(response.status);
    await response.arrayBuffer();
  }
  return out;
}

it("受信代理：回环对端在列表内时按 XFF 的客户端 IP 分桶；未配置时 XFF 无效", async () => {
  const trusted = await startPersonalRelay(
    {
      ...base,
      dataDir: join(root, "tp1"),
      rateLimits: { preflight: 2 },
      trustedProxy: { proxies: ["127.0.0.1/32", "::1/128"] },
    },
    quiet,
  );
  try {
    const a = await preflights(trusted.url, "198.51.100.1");
    const b = await preflights(trusted.url, "198.51.100.2");
    expect(a[2]).toBe(429);
    expect(b[0]).not.toBe(429); // 另一个客户端有自己的桶
    expect(a.slice(0, 2)).not.toContain(429);
  } finally {
    await trusted.close();
  }

  const plain = await startPersonalRelay(
    { ...base, dataDir: join(root, "tp2"), rateLimits: { preflight: 2 } },
    quiet,
  );
  try {
    await preflights(plain.url, "198.51.100.1");
    // 没配置：换一个 XFF 也是同一个桶（伪造不能绕过限流）
    expect((await preflights(plain.url, "198.51.100.2"))[0]).toBe(429);
  } finally {
    await plain.close();
  }

  const elsewhere = await startPersonalRelay(
    {
      ...base,
      dataDir: join(root, "tp3"),
      rateLimits: { preflight: 2 },
      trustedProxy: { proxies: ["10.9.9.9/32"] },
    },
    quiet,
  );
  try {
    await preflights(elsewhere.url, "198.51.100.1");
    expect((await preflights(elsewhere.url, "198.51.100.2"))[0]).toBe(429);
  } finally {
    await elsewhere.close();
  }
});

it.each(TEST_STORE_KINDS)(
  "流量统计：启动时清掉保留期外的桶，关闭时把内存里的写进存储，personal traffic 读得出（%s）",
  async (store) => {
    const dataDir = join(root, `traffic-${store}`);
    const pg = await databaseFor(store);
    const nowMs = Date.UTC(2026, 9, 12, 9, 30);
    const hour = (ms: number) => ms - (ms % 3_600_000);
    const row = (bucketStartMs: number, subject: string) => ({
      bucketStartMs,
      node: "127.0.0.1",
      sourceId: "a".repeat(32),
      subject,
      deviceId: "",
      kind: "http" as const,
      connections: 1,
      bytesToSource: 100,
      bytesToClient: 2048,
    });
    const first = await startPersonalRelay(
      { ...base, dataDir, store, ...pg.options },
      quiet,
    );
    await first.close();
    if (store !== "file") {
      const seeded = await openPersonalStore(
        dataDir,
        undefined,
        pg.options.database,
      );
      await seeded.ports.traffic.upsert(DEFAULT_TENANT, [
        row(hour(nowMs) - 31 * 86_400_000, "acct:old"),
        row(hour(nowMs) - 86_400_000, "acct:me"),
      ]);
      await seeded.close();
    }
    const relay = await startPersonalRelay(
      { ...base, dataDir, trafficRetentionDays: 30, ...pg.options },
      { ...quiet, now: () => nowMs },
    );
    const meter = relay.relay.traffic;
    expect(meter).toBeDefined();
    meter?.add({
      sourceId: "a".repeat(32),
      subject: "acct:me",
      deviceId: "",
      kind: "ws",
      connections: 1,
      bytesToClient: 5000,
    });
    const ports = relay.store.ports;
    await relay.close();
    if (store === "file") {
      // 状态文件：只在本进程内存里，CLI 读不到。
      expect(
        (
          await ports.traffic.query(DEFAULT_TENANT, {
            fromMs: 0,
            toMs: nowMs + 1,
            granularity: "hour",
            limit: 10,
          })
        ).map((one) => one.kind),
      ).toEqual(["ws"]);
      await expect(
        runTraffic({ dataDir, sinceMs: 86_400_000 }, () => {}, nowMs),
      ).rejects.toThrow(/migrate --to sqlite/);
      await pg.drop();
      return;
    }
    const lines: string[] = [];
    await runTraffic(
      { dataDir, sinceMs: 7 * 86_400_000 },
      (line) => lines.push(line),
      nowMs,
      pg.options.database,
    );
    try {
      expect(lines[0]).toMatch(
        /^源\s+账号\s+设备\s+类别\s+连接\s+上行\s+下行$/,
      );
      expect(lines.slice(1, -1)).toEqual([
        expect.stringMatching(/acct:me\s+—\s+ws\s+1\s+0 B\s+4\.9 KiB$/),
        expect.stringMatching(/acct:me\s+—\s+http\s+1\s+100 B\s+2\.0 KiB$/),
      ]);
      expect(lines.at(-1)).toMatch(/^合计\s+2\s+100 B\s+6\.9 KiB$/);
      expect(lines.join("\n")).not.toContain("acct:old");
    } finally {
      await pg.drop();
    }
  },
);

it("保留天数为 0：不统计", async () => {
  const relay = await startPersonalRelay(
    { ...base, dataDir: join(root, "traffic-off"), trafficRetentionDays: 0 },
    quiet,
  );
  try {
    expect(relay.relay.traffic).toBeUndefined();
  } finally {
    await relay.close();
  }
});
