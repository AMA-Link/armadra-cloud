import { expect, it } from "vitest";

import {
  errorCodeSchema,
  ERRORS,
  errors,
  errorStatus,
  isErrorCode,
  limitReachedDetailsSchema,
  teamHostsOnlineDetailsSchema,
  QUOTAS,
  wireErrorSchema,
} from "../src/errors/index.ts";
import { readJsonFixture } from "../src/fixtures/index.ts";

it("码全是 snake_case，状态码是 4xx / 5xx", () => {
  for (const [code, { status }] of Object.entries(ERRORS)) {
    expect(code).toMatch(/^[a-z]+(_[a-z]+)*$/);
    expect(status).toBeGreaterThanOrEqual(400);
    expect(status).toBeLessThan(600);
  }
});

it("errors.json 夹具就是注册表", () => {
  expect(readJsonFixture("errors.json")).toEqual(ERRORS);
});

it("pick 只收登记过的码，带上状态码", () => {
  expect(errors.pick("not_found", "rate_limited")).toEqual({
    not_found: { status: 404 },
    rate_limited: { status: 429 },
  });
  expect(() => errors.pick("nope" as never)).toThrow(/未登记/);
  expect(errorStatus("source_offline")).toBe(503);
  expect(isErrorCode("toString")).toBe(false);
});

it("线上错误形状 { code, message, requestId?, details? }", () => {
  expect(
    wireErrorSchema.parse({ code: "not_found", message: "x", requestId: "r" }),
  ).toEqual({
    code: "not_found",
    message: "x",
    requestId: "r",
  });
  expect(wireErrorSchema.safeParse({ message: "x" }).success).toBe(false);
  expect(errorCodeSchema.safeParse("source_taken").success).toBe(true);
  expect(errorCodeSchema.safeParse("SOURCE_TAKEN").success).toBe(false);
});

it("limit_reached 的 details 是 { quota, used, limit }，quota 取固定枚举", () => {
  expect(QUOTAS).toEqual([
    "teams",
    "hosts",
    "members",
    "guests",
    "concurrent",
    "traffic",
  ]);
  expect(
    limitReachedDetailsSchema.parse({ quota: "hosts", used: 3, limit: 3 }),
  ).toEqual({ quota: "hosts", used: 3, limit: 3 });
  expect(
    limitReachedDetailsSchema.safeParse({ quota: "cpu", used: 1, limit: 1 })
      .success,
  ).toBe(false);
  expect(teamHostsOnlineDetailsSchema.parse({ onlineHosts: 2 })).toEqual({
    onlineHosts: 2,
  });
  expect(
    teamHostsOnlineDetailsSchema.safeParse({ onlineHosts: 0 }).success,
  ).toBe(false);
});
