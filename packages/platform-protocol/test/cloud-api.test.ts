import { describe, expect, it } from "vitest";
import type { z } from "zod";

import {
  cloudContract,
  listProcedures,
  sectionOf,
  SCOPES,
} from "../src/cloud-api/index.ts";
import {
  meStreamClientMessageSchema,
  meStreamEventSchema,
} from "../src/cloud-api/events.ts";
import { isErrorCode } from "../src/errors/registry.ts";
import { listFixtures, readJsonFixture } from "../src/fixtures/index.ts";

const procedures = listProcedures(cloudContract);

describe("契约结构", () => {
  it("覆盖设计 §5 的七个域", () => {
    expect(Object.keys(cloudContract)).toEqual([
      "platform",
      "auth",
      "me",
      "sources",
      "links",
      "orgs",
      "billing",
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
