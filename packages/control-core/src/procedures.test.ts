import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";
import { expect, it } from "vitest";

import { createCloudRouter } from "./router.ts";
import { PERSONAL_CAPABILITIES, PERSONAL_PROCEDURES } from "./procedures.ts";
import { PLATFORM_CAPABILITIES } from "@armadra/platform-protocol/cloud-api";

it("personal 实现的 procedure 都在契约里（拼错一个就会悄悄变成 404）", () => {
  const keys = listProcedures(cloudContract).map((entry) => entry.key);
  for (const key of PERSONAL_PROCEDURES) expect(keys, key).toContain(key);
  expect(new Set(PERSONAL_PROCEDURES).size).toBe(PERSONAL_PROCEDURES.length);
});

it("报的能力是契约能力表的子集", () => {
  for (const capability of PERSONAL_CAPABILITIES)
    expect(PLATFORM_CAPABILITIES).toContain(capability);
});

it("协议 1.4 的 SaaS 各组在 personal 里都是预留的（路由命中但没有处理器，装配方答 404）", () => {
  const router = createCloudRouter(
    Object.fromEntries(PERSONAL_PROCEDURES.map((key) => [key, () => ({})])),
  );
  const saas =
    /^(teams|invitations|groups|grants|accessRequests|trust|status|audit|billing|apiTokens|admin)\./;
  const reserved = router.reserved.map((route) => route.key);
  const added = listProcedures(cloudContract)
    .map((entry) => entry.key)
    .filter((key) => saas.test(key));
  expect(added.length).toBeGreaterThanOrEqual(50);
  for (const key of added) {
    expect(PERSONAL_PROCEDURES as readonly string[], key).not.toContain(key);
    expect(reserved, key).toContain(key);
  }
  expect(router.match("POST", "/v1/teams")?.handler).toBeNull();
  expect(router.match("GET", "/v1/billing/plan")?.handler).toBeNull();
  expect(router.match("GET", "/v1/admin/accounts")?.handler).toBeNull();
});
