/** 边缘判定（运行时无关）：预检、无令牌、关闭码与已升级连接表。端到端行为见 Node 运行时的集成测试。 */
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { SOURCE_ID, testControlPlane } from "../../test/tokens.ts";
import { createAuthContext } from "../auth/context.ts";
import { RelayMetrics } from "../metrics.ts";
import { pathAddressing } from "./addressing.ts";
import { EdgePolicy, wsCloseCode, type EdgeRequest } from "./policy.ts";

function policy() {
  const control = testControlPlane();
  const metrics = new RelayMetrics(() => ({ tunnels: 0, streams: 0 }));
  const edge = new EdgePolicy<never, string>({
    nodeId: "relay-1",
    addressing: pathAddressing(),
    auth: createAuthContext(control),
    control,
    lookup: async () => null,
    metrics,
    corsOrigins: () => ["https://app.example"],
  });
  return { edge, metrics };
}

const request = (overrides: Partial<EdgeRequest> = {}): EdgeRequest => ({
  method: "GET",
  url: `/s/${SOURCE_ID}/api/x`,
  headers: { host: "relay" },
  clientIp: "203.0.113.7",
  ...overrides,
});

describe("EdgePolicy", () => {
  it("预检：名单内答 204 头，名单外 403 不带 CORS，地址不匹配 404", async () => {
    const { edge } = policy();
    const preflight = (origin: string, url = `/s/${SOURCE_ID}/api/x`) =>
      edge.decideRequest(
        request({
          method: "OPTIONS",
          url,
          headers: {
            host: "relay",
            origin,
            "access-control-request-method": "POST",
          },
        }),
      );
    expect(await preflight("https://app.example")).toMatchObject({
      kind: "preflight",
      headers: { "access-control-allow-origin": "https://app.example" },
    });
    expect(await preflight("https://evil.example")).toEqual({
      kind: "error",
      code: "forbidden",
      drain: true,
    });
    expect(await preflight("https://app.example", "/nope")).toMatchObject({
      kind: "error",
      code: "not_found",
      drain: true,
    });
  });

  it("无令牌：401 relay_token_invalid，名单内来源带 CORS 头；计入指标", async () => {
    const { edge, metrics } = policy();
    const decision = await edge.decideRequest(
      request({ headers: { host: "relay", origin: "https://app.example" } }),
    );
    expect(decision).toMatchObject({
      kind: "error",
      code: "relay_token_invalid",
      drain: false,
      cors: { "access-control-allow-origin": "https://app.example" },
    });
    expect(metrics.render()).toContain('result="relay_token_invalid"');
  });

  it("WS 拒绝的关闭码", () => {
    expect(wsCloseCode("relay_token_invalid")).toBe(CLOSE.unauthenticated);
    expect(wsCloseCode("source_offline")).toBe(CLOSE.sourceOffline);
    expect(wsCloseCode("not_found")).toBeUndefined();
  });

  it("已升级连接：按源 / 账号前缀摘出", () => {
    const { edge } = policy();
    edge.track("a", "s1", "acct_1");
    edge.track("b", "s1", "guest:l1:x");
    edge.track("c", "s2", "guest:l1:y");
    expect(edge.take({ sourceId: "s1", subjectPrefix: "guest:l1:" })).toEqual([
      "b",
    ]);
    expect(edge.take({ subjectPrefix: "guest:" })).toEqual(["c"]);
    edge.untrack("a");
    expect(edge.takeAll()).toEqual([]);
  });
});
