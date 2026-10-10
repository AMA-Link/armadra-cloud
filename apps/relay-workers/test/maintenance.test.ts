/** 维护页 Worker：拉闸后所有请求答 503 `unavailable`（带 Retry-After 与跨源头），WS 升级也拒。 */
import { expect, it } from "vitest";

import maintenance, { maintenanceResponse } from "../src/maintenance.ts";

const ORIGIN = "https://relay.armadra.com";

it("任何路径答 503 unavailable，带 Retry-After、不缓存、任意来源可读", async () => {
  for (const path of ["/v1/auth/login", "/health", "/s/abc/api/x", "/app/"]) {
    const response = maintenanceResponse(
      new Request(`${ORIGIN}${path}`, { method: "POST" }),
    );
    expect(response.status).toBe(503);
    expect(response.headers.get("retry-after")).toBe("300");
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("access-control-allow-origin")).toBe("*");
    expect(await response.json()).toMatchObject({
      code: "unavailable",
      details: { retryAfterMs: 300_000 },
    });
  }
});

it("预检 204；WS 升级与 HEAD 503 无体；默认导出走同一个处理", async () => {
  expect(
    maintenanceResponse(new Request(ORIGIN, { method: "OPTIONS" })).status,
  ).toBe(204);
  const upgrade = maintenanceResponse(
    new Request(`${ORIGIN}/t/v1`, { headers: { upgrade: "websocket" } }),
  );
  expect(upgrade.status).toBe(503);
  expect(upgrade.body).toBeNull();
  expect(
    maintenanceResponse(new Request(ORIGIN, { method: "HEAD" })).body,
  ).toBeNull();
  const viaDefault = await maintenance.fetch(new Request(`${ORIGIN}/ready`));
  expect(viaDefault.status).toBe(503);
  await viaDefault.arrayBuffer();
});
