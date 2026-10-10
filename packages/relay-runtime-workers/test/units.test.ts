/** 与对象无关的小件：配置、日配额、闸的归类、媒体票的过期、saas 的外部往来、没有产物时的页面。 */
import { env, runInDurableObject } from "cloudflare:test";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BudgetLedger, BudgetMeter, dayOf } from "../src/budget.ts";
import {
  ConfigError,
  DEFAULT_HEARTBEAT_MS,
  readConfig,
  tunnelNodeUrl,
  type RelayEnv,
} from "../src/env.ts";
import { apiGate, edgeGate } from "../src/gates.ts";
import { KvMediaTickets } from "../src/media-tickets.ts";
import { servePages } from "../src/pages.ts";
import { cloudJwks, reportPresence } from "../src/saas.ts";

const base = { RELAY_ISSUER: "https://relay.armadra.test" } as RelayEnv;

describe("配置", () => {
  it("缺省：personal、45 秒心跳、60 秒检查、apac；issuer 去尾斜杠", () => {
    const config = readConfig({
      ...base,
      RELAY_ISSUER: "https://relay.armadra.test/",
    });
    expect(config).toMatchObject({
      mode: "personal",
      issuer: "https://relay.armadra.test",
      heartbeatMs: DEFAULT_HEARTBEAT_MS,
      alarmMs: 60_000,
      clientIdleMs: 120_000,
      controlLocation: "apac",
      dailyRequestBudget: 2_000_000,
    });
  });

  it("心跳可配 10–60 秒（用户可改成 20 秒）；越界、不是整数、坏地址都拒绝", () => {
    expect(
      readConfig({ ...base, RELAY_HEARTBEAT_MS: "20000" }).heartbeatMs,
    ).toBe(20_000);
    for (const bad of [
      { RELAY_HEARTBEAT_MS: "5000" },
      { RELAY_HEARTBEAT_MS: "61000" },
      { RELAY_HEARTBEAT_MS: "2e4" },
      { RELAY_ISSUER: "https://relay.armadra.test/path" },
      { RELAY_ISSUER: undefined },
      { RELAY_MODE: "team" },
      { RELAY_MODE: "saas" },
      { RELAY_CONTROL_LOCATION: "mars" },
      { RELAY_TRUSTED_ORIGINS: "not a url" },
    ])
      expect(() => readConfig({ ...base, ...bad } as RelayEnv)).toThrow(
        ConfigError,
      );
    expect(
      readConfig({
        ...base,
        RELAY_MODE: "saas",
        RELAY_CLOUD_ISSUER: "https://app.armadra.test",
        RELAY_DOMAIN: "Armadra.com",
      }),
    ).toMatchObject({ mode: "saas", domain: "armadra.com" });
  });

  it("隧道节点地址：wss + /t/v1?src=", () => {
    expect(tunnelNodeUrl("https://relay.armadra.com", "ab".repeat(16))).toBe(
      `wss://relay.armadra.com/t/v1?src=${"ab".repeat(16)}`,
    );
  });
});

describe("日配额", () => {
  it("SourceDO 攒够 100 次才报；报不上放回去下次再报；拿回「超了」", async () => {
    const reports: { requests: number; bytes: number }[] = [];
    let fail = true;
    const meter = new BudgetMeter(async (delta) => {
      if (fail) throw new Error("down");
      reports.push(delta);
      return true;
    });
    for (let index = 0; index < 99; index += 1) meter.request();
    await meter.flush();
    // 报不上：放回去。
    expect(reports).toEqual([]);
    fail = false;
    meter.request();
    for (let index = 0; index < 40; index += 1) meter.message();
    meter.bytes(10);
    await meter.flush();
    await meter.flush();
    const total = reports.reduce(
      (sum, delta) => ({
        requests: sum.requests + delta.requests,
        bytes: sum.bytes + delta.bytes,
      }),
      { requests: 0, bytes: 0 },
    );
    expect(total).toEqual({ requests: 102, bytes: 10 });
    expect(meter.exceeded).toBe(true);
  });

  it("ControlDO：按 UTC 自然日记，0 = 不限，换日清掉旧键", async () => {
    const stub = env.CONTROL.get(env.CONTROL.idFromName("budget-unit"));
    await runInDurableObject(stub, (_instance, state) => {
      let now = Date.UTC(2026, 9, 10, 23, 59);
      const ledger = new BudgetLedger(
        state.storage.kv,
        { requests: 3, bytes: 0 },
        () => now,
      );
      expect(ledger.add({ requests: 2, bytes: 5 }).exceeded).toBe(false);
      ledger.note();
      expect(ledger.exceeded()).toBe(true);
      expect(ledger.usage()).toEqual({
        day: "2026-10-10",
        requests: 3,
        bytes: 5,
      });
      now = Date.UTC(2026, 9, 11, 0, 1);
      expect(ledger.add({ requests: 1, bytes: 0 }).exceeded).toBe(false);
      expect(
        [...state.storage.kv.list({ prefix: "budget:" })].map(([key]) => key),
      ).toEqual(["budget:2026-10-11"]);
      const unlimited = new BudgetLedger(
        state.storage.kv,
        { requests: 0, bytes: 0 },
        () => now,
      );
      expect(
        unlimited.add({ requests: 10 ** 9, bytes: 10 ** 12 }).exceeded,
      ).toBe(false);
    });
    expect(dayOf(Date.UTC(2026, 0, 2))).toBe("2026-01-02");
  });
});

describe("闸的归类", () => {
  it("/v1：登录、接受链接、注册令牌与登记；其余不过闸", () => {
    expect(apiGate("POST", "/v1/auth/login")).toBe("LIMIT_LOGIN");
    expect(apiGate("POST", "/v1/links/0123456789abcdef/accept")).toBe(
      "LIMIT_LINK_ACCEPT",
    );
    expect(apiGate("POST", "/v1/sources/registration-tokens")).toBe(
      "LIMIT_REGISTER",
    );
    expect(apiGate("POST", "/v1/sources/register")).toBe("LIMIT_REGISTER");
    expect(apiGate("GET", "/v1/auth/login")).toBeNull();
    expect(apiGate("POST", "/v1/auth/refresh")).toBeNull();
  });

  it("源地址：预检、签票、没带令牌（含兑票）分开计；带令牌的不过闸", () => {
    const request = (headers: Record<string, string>, method = "GET") =>
      new Request("https://relay.armadra.test/s/x", { method, headers });
    expect(
      edgeGate(
        request(
          { origin: "https://a.test", "access-control-request-method": "GET" },
          "OPTIONS",
        ),
        "/x",
      ),
    ).toBe("LIMIT_PREFLIGHT");
    expect(
      edgeGate(
        request({ "armadra-relay-token": "t" }, "POST"),
        "/_relay/media-tickets",
      ),
    ).toBe("LIMIT_MEDIA");
    expect(edgeGate(request({}), "/_relay/m/abc")).toBe("LIMIT_ANONYMOUS");
    expect(
      edgeGate(request({ "armadra-relay-token": "t" }), "/api/x"),
    ).toBeNull();
    expect(
      edgeGate(
        request({
          "sec-websocket-protocol": "armadra-relay.t, armadra-ticket.x",
        }),
        "/ws",
      ),
    ).toBeNull();
  });
});

describe("媒体票（DO 存储版）", () => {
  it("存储里只有票的派生哈希与密文；闲置 5 分钟或签出 30 分钟作废；清扫", async () => {
    const stub = env.SOURCE.get(env.SOURCE.idFromName("tickets-unit"));
    await runInDurableObject(stub, (_instance, state) => {
      let now = 1_000_000;
      const tickets = new KvMediaTickets(state.storage.kv, () => now);
      const grant = {
        sourceId: "s",
        path: "/media",
        subject: "acct:o",
        jti: "j",
      };
      const { ticket, expiresAtMs } = tickets.issue(grant);
      expect(expiresAtMs).toBe(now + 5 * 60_000);
      const stored = [...state.storage.kv.list({ prefix: "mt:" })];
      expect(stored).toHaveLength(1);
      // 存储里既没有票、也没有签票时的路径与令牌标识。
      const dump = JSON.stringify(stored);
      for (const part of [ticket, "/media", "acct:o", '"j"'])
        expect(dump).not.toContain(part);
      now += 4 * 60_000;
      expect(tickets.use(ticket)).toEqual(grant);
      now += 4 * 60_000;
      expect(tickets.use(ticket)).toEqual(grant);
      now += 6 * 60_000;
      expect(tickets.use(ticket)).toBeUndefined();
      const second = tickets.issue(grant).ticket;
      for (let minute = 0; minute < 31; minute += 2) {
        now += 2 * 60_000;
        tickets.use(second);
      }
      expect(tickets.use(second)).toBeUndefined();
      tickets.issue(grant);
      now += 10 * 60_000;
      expect(tickets.sweep()).toBe(0);
      expect([...state.storage.kv.list({ prefix: "mt:" })]).toHaveLength(0);
    });
  });
});

describe("saas 的外部往来", () => {
  afterEach(() => vi.restoreAllMocks());
  const config = readConfig({
    ...base,
    RELAY_MODE: "saas",
    RELAY_CLOUD_ISSUER: "https://app.armadra.test",
  });
  const event = {
    type: "sourceOnline" as const,
    sourceId: "ab".repeat(16),
    node: "cf",
    tunnelId: "cd".repeat(16),
    coreVersion: "0.2.0",
    atMs: 1,
  };

  it("节点回报：Bearer 节点令牌、ttlMs = 3 个检查周期；没令牌不报", async () => {
    const calls: { url: string; init: RequestInit }[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      calls.push({ url: String(url), init: init ?? {} });
      return new Response('{"accepted":1}');
    });
    await reportPresence(config, undefined, event);
    expect(calls).toEqual([]);
    await reportPresence(config, "node-token", event);
    expect(calls[0]?.url).toBe(
      "https://app.armadra.test/v1/relay/nodes/cf/presence",
    );
    expect(new Headers(calls[0]?.init.headers).get("authorization")).toBe(
      "Bearer node-token",
    );
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual({
      node: "cf",
      ttlMs: 180_000,
      events: [
        {
          type: "sourceOnline",
          sourceId: event.sourceId,
          tunnelId: event.tunnelId,
          coreVersion: "0.2.0",
          atMs: 1,
        },
      ],
    });
  });

  it("JWKS 从 cloud 拉，形状不对就抛（内核的缓存继续用旧的）", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          keys: [
            {
              kty: "OKP",
              crv: "Ed25519",
              x: "A".repeat(43),
              kid: "k1",
              alg: "EdDSA",
              use: "sig",
            },
          ],
        }),
      ),
    );
    expect((await cloudJwks("https://app.armadra.test")).keys[0]?.kid).toBe(
      "k1",
    );
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response("nope", { status: 500 }),
    );
    await expect(cloudJwks("https://app.armadra.test")).rejects.toThrow();
  });
});

describe("没有页面产物", () => {
  it("/app/* 答 503 错误页、/j/<linkId> 答落地页（中英文案同一份，nonce CSP）", async () => {
    const app = await servePages(
      new Request("https://relay.armadra.test/app/x"),
      undefined,
    );
    expect(app?.status).toBe(503);
    expect(app?.headers.get("content-security-policy")).toContain(
      "script-src 'nonce-",
    );
    const join = await servePages(
      new Request("https://relay.armadra.test/j/0123456789abcdef", {
        headers: { "accept-language": "en" },
      }),
      undefined,
    );
    expect(join?.status).toBe(200);
    expect(await join?.text()).toContain("Open in Armadra");
    expect(
      await servePages(
        new Request("https://relay.armadra.test/other"),
        undefined,
      ),
    ).toBeNull();
  });
});

describe("托管页的 CSP（Turnstile）", () => {
  const assets = {
    fetch: async () =>
      new Response("<title>Armadra</title>", {
        headers: { "content-type": "text/html" },
      }),
  } as unknown as Fetcher;
  const csp = async (path: string, challenge?: { embedOrigins: string[] }) =>
    (
      await servePages(
        new Request(`https://relay.armadra.test${path}`),
        assets,
        challenge,
      )
    )?.headers.get("content-security-policy");

  it("没配 Turnstile：头不变，/app/challenge 不可内嵌", async () => {
    const plain = await csp("/app/");
    expect(plain).not.toContain("challenges.cloudflare.com");
    expect(plain).toContain("frame-ancestors 'none'");
    expect(await csp("/app/challenge")).toBe(plain);
  });

  it("配了：放行 Turnstile；只有 /app/challenge 列出受信来源", async () => {
    const challenge = { embedOrigins: ["https://relay.armadra.test"] };
    const page = await csp("/app/challenge", challenge);
    expect(page).toContain(
      "script-src 'self' https://challenges.cloudflare.com",
    );
    expect(page).toContain(
      "frame-ancestors 'self' https://localhost capacitor://localhost http://localhost http://localhost:* https://relay.armadra.test",
    );
    expect(await csp("/app/x", challenge)).toContain("frame-ancestors 'none'");
  });
});
