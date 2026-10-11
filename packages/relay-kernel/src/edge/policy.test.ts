/** 边缘判定（运行时无关）：预检、无令牌、关闭码与已升级连接表。端到端行为见 Node 运行时的集成测试。 */
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { fakeTunnel } from "../../test/fake-transport.ts";
import { relayToken, SOURCE_ID, testControlPlane } from "../../test/tokens.ts";
import { createAuthContext } from "../auth/context.ts";
import { RelayMetrics } from "../metrics.ts";
import type { Tunnel } from "../tunnel/tunnel.ts";
import type { WebTunnelStream } from "../tunnel/web-stream.ts";
import { pathAddressing } from "./addressing.ts";
import {
  EdgePolicy,
  isE2ePath,
  wsCloseCode,
  type EdgeRequest,
} from "./policy.ts";

function policy(tunnel: Tunnel<WebTunnelStream> | null = null) {
  const control = testControlPlane();
  const metrics = new RelayMetrics(() => ({ tunnels: 0, streams: 0 }));
  const edge = new EdgePolicy<WebTunnelStream, string>({
    nodeId: "relay-1",
    addressing: pathAddressing(),
    auth: createAuthContext(control),
    control,
    lookup: async () => tunnel,
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

  it("_relay/e2e：只认 WS 升级，普通 HTTP 答 404（不进隧道）", async () => {
    const { edge } = policy();
    expect(isE2ePath("/_relay/e2e")).toBe(true);
    expect(isE2ePath("/_relay/e2e?v=1")).toBe(true);
    expect(isE2ePath("/_relay/e2ex")).toBe(false);
    expect(
      await edge.decideRequest(request({ url: `/s/${SOURCE_ID}/_relay/e2e` })),
    ).toMatchObject({ kind: "error", code: "not_found", drain: true });
  });

  it("_relay/e2e 升级：令牌先验；缺版本子协议 4409；源没报 e2e 4404（原因 e2e_unsupported）；报了才接受，带 dev", async () => {
    const { tunnel } = fakeTunnel();
    const { edge, metrics } = policy(tunnel);
    const upgrade = (protocols: string) =>
      edge.decideUpgrade(
        request({
          url: `/s/${SOURCE_ID}/_relay/e2e`,
          headers: {
            host: "relay",
            upgrade: "websocket",
            "sec-websocket-protocol": protocols,
          },
        }),
      );
    const token = relayToken({ dev: "dev_1" });
    expect(await upgrade("armadra-e2e.1")).toMatchObject({
      kind: "refuse",
      code: "relay_token_invalid",
    });
    expect(await upgrade(`armadra-relay.${token}`)).toMatchObject({
      kind: "refuse",
      code: "protocol_unsupported",
    });
    expect(wsCloseCode("protocol_unsupported")).toBe(CLOSE.protocolUnsupported);
    expect(await upgrade(`armadra-e2e.1, armadra-relay.${token}`)).toEqual({
      kind: "refuse",
      code: "source_offline",
      sourceId: SOURCE_ID,
      reason: "e2e_unsupported",
    });
    tunnel.capabilities = new Set(["e2e"]);
    const accepted = await upgrade(`armadra-e2e.1, armadra-relay.${token}`);
    expect(accepted).toMatchObject({
      kind: "accept",
      mode: "e2e",
      subject: "acct_test",
      subprotocols: ["armadra-e2e.1"],
      device: "dev_1",
    });
    if (accepted.kind !== "accept") throw new Error("应当接受");
    const opened = edge.openE2e(
      tunnel,
      request({ headers: { host: "relay", origin: "https://app.example" } }),
      "203.0.113.7",
    );
    expect(opened.ok).toBe(true);
    expect(
      tunnel.snapshot()?.capabilities,
      "能力随快照存下（休眠后还原）",
    ).toEqual(["e2e"]);
    expect(metrics.render()).toContain(
      'relay_edge_streams_total{kind="e2e"} 1',
    );
    edge.releaseUpgrade(accepted.subject);
    tunnel.close(CLOSE.normal, "done");
  });

  it("普通 WS 升级：mode ws，子协议原样转给源；老令牌没有 dev", async () => {
    const { tunnel } = fakeTunnel();
    const { edge } = policy(tunnel);
    const decision = await edge.decideUpgrade(
      request({
        url: `/s/${SOURCE_ID}/api/ws`,
        headers: {
          host: "relay",
          upgrade: "websocket",
          "sec-websocket-protocol": `armadra-ticket.t, armadra-relay.${relayToken()}`,
        },
      }),
    );
    expect(decision).toMatchObject({
      kind: "accept",
      mode: "ws",
      subprotocols: ["armadra-ticket.t"],
    });
    expect(decision).not.toHaveProperty("device");
    tunnel.close(CLOSE.normal, "done");
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
