/**
 * Workers 中继端到端（workerd 本地）：初始化 → 登录 → 登记源 → 隧道握手 → 经中继的端到端通道；
 * 0.5.0 起源地址上的明文 HTTP、普通 WS 与媒体票一律 404（cloud-api §20）。
 */
import { beforeAll, describe, expect, it } from "vitest";

import {
  assertion,
  call,
  channelOutcome,
  closed,
  heartbeatHint,
  login,
  openChannel,
  openClientWs,
  registerSource,
  viaRelay,
  type Session,
  type TestSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

let session: Session;
let source: TestSource;
let core: FakeCore;
let relayToken: string;

beforeAll(async () => {
  session = await login();
  source = await registerSource(session);
  const hint = await heartbeatHint(source);
  core = new FakeCore(source);
  const outcome = await core.connect(
    hint.nodes[0]?.url ?? "",
    hint.tunnelToken,
  );
  if (!outcome.ready) throw new Error(`没连上：${JSON.stringify(outcome)}`);
  relayToken = (await assertion(session, source.sourceId)).relayToken;
});

describe("隧道", () => {
  it("节点地址带 ?src=<sourceId>；ready 下发 45 秒心跳，中继不主动 PING、源的 PING 立即回 PONG", async () => {
    const hint = await heartbeatHint(source);
    expect(hint.nodes).toEqual([
      {
        url: `wss://relay.armadra.test/t/v1?src=${source.sourceId}`,
        weight: 1,
      },
    ]);
    expect(core.ready?.heartbeatMs).toBe(45_000);
    expect(core.ready?.protocol).toEqual({ major: 1, minor: 3 });
    await core.ping();
  });

  it("源在线：assertion 报 online", async () => {
    expect((await assertion(session, source.sourceId)).online).toBe(true);
  });
});

describe("明文路径已删除", () => {
  it("源地址上的 HTTP（带令牌、预检、媒体票）一律 404，不进隧道；地址不是源也 404", async () => {
    const before = core.requests.length;
    const opens = core.opens.length;
    for (const [path, init] of [
      ["/echo-request", { token: relayToken }],
      ["/echo", { token: relayToken, method: "POST", body: "x" }],
      [
        "/api/x",
        {
          method: "OPTIONS",
          headers: {
            origin: "https://app.armadra.test",
            "access-control-request-method": "POST",
          },
        },
      ],
      [
        "/_relay/media-tickets",
        {
          token: relayToken,
          method: "POST",
          body: JSON.stringify({ path: "/media" }),
        },
      ],
      ["/_relay/m/ticket", {}],
    ] as const) {
      const response = await viaRelay(source.sourceId, path, init);
      expect(response.status, path).toBe(404);
      await response.arrayBuffer();
    }
    expect(core.requests.length).toBe(before);
    expect(core.opens.length).toBe(opens);
    expect((await call("/s/nothex/echo")).status).toBe(404);
  });

  it("别的路径上的 WS 升级（明文 WS）：404，不升级、不进隧道", async () => {
    const opens = core.opens.length;
    const { response, ws } = await openClientWs(
      source.sourceId,
      "/ws",
      relayToken,
    );
    expect(response.status).toBe(404);
    expect(ws).toBeNull();
    expect(core.opens.length).toBe(opens);
  });
});

describe("通道", () => {
  it("令牌无效：先完成升级再以 4401 关闭；别的源的令牌 4403；源离线 4404", async () => {
    const { response, ws } = await openChannel(source.sourceId, "bad");
    expect(response.status).toBe(101);
    if (!ws) throw new Error("没有 WS");
    expect((await closed(ws)).code).toBe(4401);
    const other = await registerSource(session);
    const otherToken = (await assertion(session, other.sourceId)).relayToken;
    expect(await channelOutcome(source.sourceId, otherToken)).toBe(4403);
    expect(await channelOutcome(other.sourceId, otherToken)).toBe(4404);
  });

  it("客户端单条消息超过 1 MiB：以 1009 关闭", async () => {
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    const done = closed(ws);
    ws.send(new Uint8Array(1024 * 1024 + 1));
    expect((await done).code).toBe(1009);
  });
});
