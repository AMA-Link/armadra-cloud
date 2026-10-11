import { describe, expect, it } from "vitest";
import type { z } from "zod";

import {
  cloudContract,
  listProcedures,
  PLATFORM_CAPABILITIES,
  sectionOf,
  SCOPES,
  TRAFFIC_KINDS,
} from "../src/cloud-api/index.ts";
import {
  meStreamClientMessageSchema,
  meStreamEventSchema,
} from "../src/cloud-api/events.ts";
import { errorStatus, isErrorCode } from "../src/errors/registry.ts";
import { PROTOCOL_VERSION } from "../src/version.ts";
import { listFixtures, readJsonFixture } from "../src/fixtures/index.ts";

const procedures = listProcedures(cloudContract);

describe("契约结构", () => {
  it("覆盖设计 §5 的七个域与节点回报（§17）", () => {
    expect(Object.keys(cloudContract)).toEqual([
      "platform",
      "auth",
      "me",
      "sources",
      "links",
      "orgs",
      "billing",
      "relay",
    ]);
    expect(procedures.length).toBeGreaterThanOrEqual(50);
  });

  it("「方法 + 路径」唯一", () => {
    const seen = new Map<string, string>();
    for (const entry of procedures) {
      const route = `${entry.method} ${entry.path}`;
      expect(
        seen.get(route),
        `${entry.key} 与 ${seen.get(route)} 重复 ${route}`,
      ).toBeUndefined();
      seen.set(route, entry.key);
    }
  });

  it("每条都有方法、路径、scope、since 与 cloud-api.md 的节号", () => {
    for (const entry of procedures) {
      expect(entry.path, entry.key).toMatch(/^\/(v1\/|\.well-known\/|health$)/);
      expect(["GET", "POST", "PATCH", "PUT", "DELETE"], entry.key).toContain(
        entry.method,
      );
      expect(
        entry.meta.scope.length,
        `${entry.key} 没有 scope`,
      ).toBeGreaterThan(0);
      for (const scope of entry.meta.scope)
        expect(SCOPES, entry.key).toContain(scope);
      expect(entry.meta.since, entry.key).toMatch(/^\d+\.\d+\.\d+$/);
      expect(sectionOf(entry.key), entry.key).toMatch(/^§\d+$/);
    }
  });

  it("路径参数都在 input schema 里", () => {
    for (const entry of procedures) {
      const params = [...entry.path.matchAll(/\{(\w+)\}/g)].map(
        (match) => match[1],
      );
      const input = entry.procedure["~orpc"].inputSchema as
        | z.ZodObject
        | undefined;
      for (const param of params)
        expect(
          Object.keys(input?.shape ?? {}),
          `${entry.key} 缺 ${param}`,
        ).toContain(param);
    }
  });

  it("所有 errors 都在注册表里，状态码一致", () => {
    for (const entry of procedures) {
      const errorMap = entry.procedure["~orpc"].errorMap as Record<
        string,
        { status?: number }
      >;
      for (const code of entry.errorCodes) {
        expect(isErrorCode(code), `${entry.key}: ${code}`).toBe(true);
        expect(errorMap[code]?.status).toBeGreaterThanOrEqual(400);
      }
    }
  });
});

describe("cloud-api 夹具", () => {
  const names = listFixtures("cloud-api");

  it("每条 procedure 正好一份夹具", () => {
    expect(
      names.map((name) => name.replace(/^cloud-api\/|\.json$/g, "")).sort(),
    ).toEqual(procedures.map((entry) => entry.key).sort());
  });

  it.each(procedures.map((entry) => [entry.key, entry] as const))(
    "%s 的 input / output 通过各自 schema",
    (key, entry) => {
      const sample = readJsonFixture<{
        input: unknown;
        output?: unknown;
        outputs?: unknown[];
      }>(`cloud-api/${key}.json`);
      const def = entry.procedure["~orpc"];
      const input = def.inputSchema as z.ZodType;
      const output = def.outputSchema as z.ZodType;
      expect(input.safeParse(sample.input).error).toBeUndefined();
      const outputs = sample.outputs ?? [sample.output];
      expect(outputs.length).toBeGreaterThan(0);
      for (const value of outputs)
        expect(output.safeParse(value).error).toBeUndefined();
    },
  );
});

describe("me.stream 事件", () => {
  it("七种事件按 type 区分", () => {
    const sourceId = "0123456789abcdef0123456789abcdef";
    const events = [
      { type: "sourceOnline", sourceId, sinceMs: 1, relayOrigin: "https://r" },
      { type: "sourceOffline", sourceId, atMs: 2 },
      { type: "sourceRevoked", sourceId },
      { type: "accessRevoked", sourceId },
      { type: "linkUsed", linkId: "l", sourceId, by: { guest: true }, atMs: 3 },
      { type: "ping", ts: 4 },
      { type: "pong", ts: 4 },
    ];
    for (const event of events)
      expect(meStreamEventSchema.parse(event)).toEqual(event);
    expect(meStreamEventSchema.safeParse({ type: "unknown" }).success).toBe(
      false,
    );
    expect(meStreamClientMessageSchema.parse({ type: "ping" })).toEqual({
      type: "ping",
    });
  });
});

describe("挑战（§16）与节点回报（§17）", () => {
  const input = (key: string) =>
    procedures.find((entry) => entry.key === key)?.procedure["~orpc"]
      .inputSchema as z.ZodType;
  const output = (key: string) =>
    procedures.find((entry) => entry.key === key)?.procedure["~orpc"]
      .outputSchema as z.ZodType;
  const device = { platform: "desktop", name: "Laptop" };

  it("协议 1.2：挑战与节点回报的错误码登记在注册表里", () => {
    expect(PROTOCOL_VERSION).toEqual({ major: 1, minor: 3 });
    expect(errorStatus("challenge_required")).toBe(400);
    expect(errorStatus("challenge_invalid")).toBe(400);
    expect(errorStatus("storage_unavailable")).toBe(503);
    expect(errorStatus("not_initialized")).toBe(503);
  });

  it("auth.login 与 links.accept 的 challenge 可选，只认 turnstile", () => {
    const login = input("auth.login");
    const base = { account: "owner", password: "pw", device };
    expect(login.safeParse(base).success).toBe(true);
    expect(
      login.safeParse({
        ...base,
        challenge: { provider: "turnstile", token: "t" },
      }).success,
    ).toBe(true);
    expect(
      login.safeParse({ ...base, challenge: { provider: "other", token: "t" } })
        .success,
    ).toBe(false);
    expect(
      login.safeParse({
        ...base,
        challenge: { provider: "turnstile", token: "" },
      }).success,
    ).toBe(false);
    const accept = input("links.accept");
    expect(
      accept.safeParse({
        linkId: "0123456789abcdef",
        challenge: { provider: "turnstile", token: "x".repeat(2048) },
      }).success,
    ).toBe(true);
    expect(
      accept.safeParse({
        linkId: "0123456789abcdef",
        challenge: { provider: "turnstile", token: "x".repeat(2049) },
      }).success,
    ).toBe(false);
  });

  it("auth.changePassword（0.3.10）：会话、旧口令 + 新口令、可选挑战，答当前设备的会话", () => {
    const entry = listProcedures().find(
      (one) => one.key === "auth.changePassword",
    );
    expect(entry).toMatchObject({
      method: "POST",
      path: "/v1/auth/password",
      meta: { scope: ["session"], since: "0.3.10" },
    });
    expect(entry?.errorCodes).toEqual(
      expect.arrayContaining([
        "credentials_invalid",
        "account_locked",
        "password_too_short",
        "challenge_required",
      ]),
    );
    expect(sectionOf("auth.changePassword")).toBe("§18");
    const change = input("auth.changePassword");
    expect(change.safeParse({ password: "a", newPassword: "b" }).success).toBe(
      true,
    );
    expect(change.safeParse({ newPassword: "b" }).success).toBe(false);
    expect(
      change.safeParse({
        password: "a",
        newPassword: "b",
        challenge: { provider: "turnstile", token: "t" },
      }).success,
    ).toBe(true);
    expect(PLATFORM_CAPABILITIES).toContain("auth.password-change");
  });

  it("me.traffic（0.4.0）：会话、query 里的毫秒按数字收、粒度缺省按小时", () => {
    const entry = listProcedures().find((one) => one.key === "me.traffic");
    expect(entry).toMatchObject({
      method: "GET",
      path: "/v1/me/traffic",
      meta: { scope: ["session"], since: "0.4.0" },
    });
    expect(entry?.errorCodes).toEqual(["bad_request"]);
    expect(sectionOf("me.traffic")).toBe("§19");
    const query = input("me.traffic");
    expect(query.parse({ fromMs: "0", toMs: "3600000" })).toEqual({
      fromMs: 0,
      toMs: 3_600_000,
      granularity: "hour",
    });
    expect(
      query.safeParse({ fromMs: "1", toMs: "2", granularity: "week" }).success,
    ).toBe(false);
    expect(query.safeParse({ fromMs: "-1", toMs: "2" }).success).toBe(false);
    expect(
      query.safeParse({ fromMs: "1", toMs: "2", sourceId: "nope" }).success,
    ).toBe(false);
    const bucket = output("me.traffic");
    expect(
      bucket.safeParse({
        buckets: [
          {
            bucketStartMs: 0,
            node: "n",
            sourceId: "s",
            subject: "acct:a",
            deviceId: "",
            kind: "telnet",
            connections: 0,
            bytesToSource: 0,
            bytesToClient: 0,
          },
        ],
      }).success,
    ).toBe(false);
    expect(TRAFFIC_KINDS).toEqual(["e2e", "http", "ws", "media"]);
    expect(PLATFORM_CAPABILITIES).toContain("me.traffic");
  });

  it("platform.info.challenge 可选，scope 至少一条", () => {
    const info = output("platform.info");
    const base = {
      mode: "personal",
      issuer: "https://relay.armadra.test",
      protocol: { major: 1, minor: 2 },
      capabilities: ["challenge.turnstile"],
      relay: { addressing: "path" },
      webApp: null,
    };
    expect(info.safeParse(base).success).toBe(true);
    expect(
      info.safeParse({
        ...base,
        challenge: {
          provider: "turnstile",
          siteKey: "k",
          scope: ["auth.login"],
        },
      }).success,
    ).toBe(true);
    expect(
      info.safeParse({
        ...base,
        challenge: { provider: "turnstile", siteKey: "k", scope: [] },
      }).success,
    ).toBe(false);
  });

  it("节点地址可带 query；节点回报的事件 1–256 条、ttlMs 10 秒到 10 分钟", () => {
    const hint = output("sources.heartbeatHint");
    const limits = {
      maxStreams: 256,
      streamWindow: 262_144,
      tunnelWindow: 8_388_608,
      maxFrameBytes: 1_048_576,
    };
    expect(
      hint.safeParse({
        tunnelToken: "t",
        expiresAtMs: 1,
        nodes: [
          {
            url: "wss://relay.armadra.test/t/v1?src=0123456789abcdef0123456789abcdef",
            weight: 1,
          },
        ],
        limits,
      }).success,
    ).toBe(true);
    const presence = input("relay.nodePresence");
    const event = {
      type: "sourceOffline",
      sourceId: "0123456789abcdef0123456789abcdef",
      tunnelId: "fedcba9876543210fedcba9876543210",
      atMs: 1,
    };
    expect(
      presence.safeParse({ node: "cf", ttlMs: 120_000, events: [event] })
        .success,
    ).toBe(true);
    expect(
      presence.safeParse({ node: "cf", ttlMs: 120_000, events: [] }).success,
    ).toBe(false);
    expect(
      presence.safeParse({ node: "cf", ttlMs: 5_000, events: [event] }).success,
    ).toBe(false);
    expect(
      presence.safeParse({ node: "CF", ttlMs: 120_000, events: [event] })
        .success,
    ).toBe(false);
  });
});
