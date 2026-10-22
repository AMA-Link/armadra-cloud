import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";

import {
  createReservedMatcher,
  IMPLEMENTED,
  RESERVED_ROUTES,
} from "./reserved.ts";
import { createCloudRouter } from "./server.ts";

const SOURCE_ID = "0123456789abcdef0123456789abcdef";

function concrete(path: string): string {
  return path.replace(/\{sourceId\}/g, SOURCE_ID).replace(/\{(\w+)\}/g, "x_$1");
}

let running: RunningServer;
beforeAll(async () => {
  const router = createCloudRouter({
    version: "0.1.0",
    probes: {},
    log: createLogger({ name: "test", write: () => {} }),
  });
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) =>
    router.handle(req, res),
  );
});
afterAll(() => running.close());

describe("预留路由表", () => {
  it("契约里除已实现的之外全部预留，方法 + 路径唯一", () => {
    const contract = listProcedures(cloudContract).map((entry) => entry.key);
    expect(RESERVED_ROUTES.map((route) => route.key)).toEqual(
      contract.filter((key) => !IMPLEMENTED.has(key)),
    );
    expect(RESERVED_ROUTES).toHaveLength(117);
    const routes = RESERVED_ROUTES.map(
      (route) => `${route.method} ${route.path}`,
    );
    expect(new Set(routes).size).toBe(routes.length);
    expect(routes).not.toContain("GET /health");
  });

  it("静态段优先于参数段", () => {
    const matcher = createReservedMatcher();
    expect(matcher.match("GET", "/v1/sources/me/relay")?.key).toBe(
      "sources.heartbeatHint",
    );
    expect(matcher.match("GET", `/v1/sources/${SOURCE_ID}`)?.key).toBe(
      "sources.get",
    );
    expect(matcher.match("GET", "/v1/nope")).toBeNull();
    expect(matcher.match("PUT", "/v1/me")).toBeNull();
  });

  it.each(RESERVED_ROUTES.map((route) => [route.key, route] as const))(
    "%s 答 501 not_implemented",
    async (key, route) => {
      const response = await fetch(`${running.url}${concrete(route.path)}`, {
        method: route.method,
        headers:
          route.method === "GET" || route.method === "DELETE"
            ? {}
            : { "content-type": "application/json" },
        body:
          route.method === "GET" || route.method === "DELETE"
            ? undefined
            : "{}",
      });
      expect(response.status).toBe(501);
      const body = (await response.json()) as {
        code: string;
        message: string;
        requestId: string;
      };
      expect(body.code).toBe("not_implemented");
      expect(body.message).toContain(key);
      expect(body.requestId).toBeTruthy();
    },
  );

  it("/health 照常 200，不在表里的路径仍是 404", async () => {
    expect((await fetch(`${running.url}/health`)).status).toBe(200);
    const missing = await fetch(`${running.url}/v1/unknown`);
    expect(missing.status).toBe(404);
    expect(((await missing.json()) as { code: string }).code).toBe("not_found");
  });
});
