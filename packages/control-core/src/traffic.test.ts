/**
 * `me.traffic` 的服务：可见范围（账号本人 / 访客）、参数校验与行数上限；桶与合并的工具函数。
 * 存储语义（累加、范围、排序、按天合并、保留期）在端口合同里，对每个存储实现各跑一遍。
 */
import { TRAFFIC_MAX_ROWS } from "@armadra/platform-protocol/cloud-api";
import { describe, expect, it } from "vitest";

import { ControlError } from "./errors.ts";
import type { TrafficRow } from "./ports.ts";
import type { Caller } from "./sessions.ts";
import { memoryHarness } from "./testing/memory-ports.ts";
import {
  bucketStart,
  createTraffic,
  DAY_MS,
  HOUR_MS,
  rollUp,
} from "./traffic.ts";

const S1 = "51".repeat(16);
const S2 = "52".repeat(16);
const T0 = Date.UTC(2026, 9, 10, 0);
const OWNER: Caller = {
  subject: "acct:alice",
  deviceId: "d-owner",
  role: "owner",
  linkId: null,
};
const GUEST: Caller = {
  subject: "guest:0123456789abcdef:aa",
  deviceId: "d-guest",
  role: "guest",
  linkId: "0123456789abcdef",
};

const row = (overrides: Partial<TrafficRow> = {}): TrafficRow => ({
  bucketStartMs: T0,
  node: "n1",
  sourceId: S1,
  subject: OWNER.subject,
  deviceId: "",
  kind: "http",
  connections: 1,
  bytesToSource: 10,
  bytesToClient: 20,
  ...overrides,
});

const codeOf = async (run: () => Promise<unknown>) =>
  run().then(
    () => "ok",
    (error: unknown) =>
      error instanceof ControlError ? error.code : String(error),
  );

async function seeded() {
  const h = memoryHarness();
  await h.ports.traffic.upsert(h.context.tenantId, [
    row(),
    row({ sourceId: S2 }),
    row({ subject: GUEST.subject, kind: "ws" }),
  ]);
  return { h, traffic: createTraffic(h.context) };
}

describe("me.traffic", () => {
  const range = { fromMs: T0, toMs: T0 + DAY_MS, granularity: "hour" as const };

  it("账号本人看全部源与全部 sub；按源过滤", async () => {
    const { traffic } = await seeded();
    expect((await traffic.summary(OWNER, range)).buckets).toHaveLength(3);
    expect(
      (await traffic.summary(OWNER, { ...range, sourceId: S2 })).buckets,
    ).toEqual([row({ sourceId: S2 })]);
  });

  it("访客只看自己的 sub", async () => {
    const { traffic } = await seeded();
    expect((await traffic.summary(GUEST, range)).buckets).toEqual([
      row({ subject: GUEST.subject, kind: "ws" }),
    ]);
    expect(
      (await traffic.summary(GUEST, { ...range, sourceId: S2 })).buckets,
    ).toEqual([]);
  });

  it("fromMs ≥ toMs 答 bad_request", async () => {
    const { traffic } = await seeded();
    expect(
      await codeOf(() => traffic.summary(OWNER, { ...range, toMs: T0 })),
    ).toBe("bad_request");
  });

  it(`超过 ${TRAFFIC_MAX_ROWS} 行答 bad_request，正好 ${TRAFFIC_MAX_ROWS} 行照常答`, async () => {
    const h = memoryHarness();
    const rows = Array.from({ length: TRAFFIC_MAX_ROWS }, (_, index) =>
      row({ deviceId: `d${index}` }),
    );
    await h.ports.traffic.upsert(h.context.tenantId, rows);
    const traffic = createTraffic(h.context);
    expect((await traffic.summary(OWNER, range)).buckets).toHaveLength(
      TRAFFIC_MAX_ROWS,
    );
    await h.ports.traffic.upsert(h.context.tenantId, [row({ deviceId: "x" })]);
    expect(await codeOf(() => traffic.summary(OWNER, range))).toBe(
      "bad_request",
    );
  });
});

describe("桶与合并", () => {
  it("桶起点向下取整到 UTC 整小时 / 整天", () => {
    expect(bucketStart(T0 + HOUR_MS + 1)).toBe(T0 + HOUR_MS);
    expect(bucketStart(T0 + HOUR_MS - 1)).toBe(T0);
    expect(bucketStart(T0 + 25 * HOUR_MS, DAY_MS)).toBe(T0 + DAY_MS);
  });

  it("rollUp 按粒度相加并排序", () => {
    expect(
      rollUp(
        [
          row({ bucketStartMs: T0 + 3 * HOUR_MS }),
          row({ bucketStartMs: T0 + HOUR_MS }),
        ],
        "day",
      ),
    ).toEqual([row({ connections: 2, bytesToSource: 20, bytesToClient: 40 })]);
    expect(
      rollUp(
        [row({ bucketStartMs: T0 + HOUR_MS }), row({ kind: "e2e" })],
        "hour",
      ).map((one) => one.kind),
    ).toEqual(["e2e", "http"]);
  });
});
