/**
 * 集成：假 core 经中继完成端到端通道往返、背压、限额、断线重连、GOAWAY（中继设计 §9）。
 * 0.5.0 起中继只转端到端通道：源地址上的明文 HTTP、普通 WS 与媒体票一律 404，不进隧道。
 * 中继用 personal 的寻址（`/s/<id>/…`）与内存控制面；令牌用协议包的测试钥签。
 * 每个用例结束时核对「零日志」：日志里没有正文、令牌、路径。
 */
import { createHash } from "node:crypto";
import { once } from "node:events";
import { connect } from "node:net";

import { createLogger } from "@armadra/cloud-shared/log";
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { afterEach, describe, expect, it } from "vitest";
import { WebSocket } from "ws";

import { pathAddressing } from "@armadra/relay-kernel/edge/addressing";
import type { TrafficRow } from "@armadra/relay-kernel/traffic";
import {
  relayToken,
  SOURCE_ID,
  testControlPlane,
  tunnelToken,
} from "@armadra/relay-kernel/testing/tokens";

import {
  startRelay,
  type RelayOptions,
  type RunningRelay,
} from "../../src/start-relay.ts";
import { bigByte, FakeCore } from "../fake-core.ts";

const MARKER = "PAYLOAD-MARKER-7f3a";

interface Harness {
  relay: RunningRelay;
  base: string;
  tunnelUrl: string;
  logs: string[];
  control: ReturnType<typeof testControlPlane>;
  core(options?: Partial<ConstructorParameters<typeof FakeCore>[0]>): FakeCore;
}

const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

async function harness(
  overrides: Partial<RelayOptions> = {},
  controlOptions: Parameters<typeof testControlPlane>[0] = {},
): Promise<Harness> {
  const logs: string[] = [];
  const control = testControlPlane(controlOptions);
  const relay = await startRelay({
    mode: "personal",
    nodeId: "relay-test",
    version: "0.0.0",
    listen: { host: "127.0.0.1", port: 0 },
    control,
    addressing: pathAddressing(),
    log: createLogger({
      name: "relay",
      level: "debug",
      write: (line) => logs.push(line),
    }),
    shutdownGraceMs: 1000,
    ...overrides,
  });
  cleanups.push(() => relay.close(0));
  const tunnelUrl = `${relay.url.replace("http", "ws")}/t/v1`;
  const tokenValues: string[] = [];
  cleanups.push(() => {
    // 零日志：正文、令牌、路径都不该出现在日志里。
    const text = logs.join("\n");
    expect(text).not.toContain(MARKER);
    expect(text).not.toContain("/echo");
    for (const token of tokenValues)
      expect(text).not.toContain(token.slice(-20));
  });
  return {
    relay,
    base: `${relay.url}/s/${SOURCE_ID}`,
    tunnelUrl,
    logs,
    control,
    core(options = {}) {
      const core = new FakeCore({ tunnelUrl, ...options });
      cleanups.push(() => core.stop());
      return core;
    },
  };
}

const auth = (token = relayToken()) => ({ "armadra-relay-token": token });

/** 开一条端到端通道（缺省带有效令牌与版本子协议）。 */
function channel(h: Harness, token: string | null = relayToken()): WebSocket {
  const ws = new WebSocket(
    `${h.base.replace("http", "ws")}/_relay/e2e`,
    token === null
      ? ["armadra-e2e.1"]
      : ["armadra-e2e.1", `armadra-relay.${token}`],
  );
  cleanups.push(() => ws.terminate());
  return ws;
}

/**
 * 通道开成答 `null`，被拒答关闭码。中继拒绝时也先完成升级再以 44xx 关：开了之后等一小会儿
 * 看它是不是马上被关。
 */
async function outcome(ws: WebSocket): Promise<number | null> {
  const closed = once(ws, "close").then(([code]) => code as number);
  const opened = await Promise.race([
    once(ws, "open").then(() => true),
    closed.then(() => false),
  ]);
  if (!opened) return closed;
  return Promise.race([
    closed,
    new Promise<null>((resolve) => setTimeout(() => resolve(null), 300)),
  ]);
}

/** 经通道回显一段字节（假 core 把流接回自己）。 */
async function echo(ws: WebSocket, payload: Buffer): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let size = 0;
  const done = new Promise<void>((resolve) => {
    const onMessage = (data: Buffer) => {
      chunks.push(data);
      size += data.length;
      if (size >= payload.length) {
        ws.off("message", onMessage);
        resolve();
      }
    };
    ws.on("message", onMessage);
  });
  // 边缘一条消息的上限是协议的帧上限：大的切开发。
  for (let offset = 0; offset < payload.length; offset += 60_000)
    ws.send(payload.subarray(offset, offset + 60_000));
  await done;
  return Buffer.concat(chunks);
}

async function connected(
  h: Harness,
  core = h.core({ capabilities: ["e2e"] }),
): Promise<FakeCore> {
  const result = await core.connect();
  expect(result.ready).toBeDefined();
  return core;
}

describe("明文路径已删除（cloud-api §20）", () => {
  it("源地址上的 HTTP（带令牌、预检、媒体票）一律 404，不进隧道；/health 仍是中继自己的", async () => {
    const h = await harness({ corsOrigins: () => ["https://app.example"] });
    const core = await connected(h);
    const get = await fetch(`${h.base}/echo-request`, {
      headers: { ...auth(), origin: "https://app.example" },
    });
    expect(get.status).toBe(404);
    expect(((await get.json()) as { code: string }).code).toBe("not_found");
    expect(get.headers.get("access-control-allow-origin")).toBe(
      "https://app.example",
    );
    const post = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: auth(),
      body: MARKER,
    });
    expect(post.status).toBe(404);
    const preflight = await fetch(`${h.base}/api/x`, {
      method: "OPTIONS",
      headers: {
        origin: "https://app.example",
        "access-control-request-method": "POST",
      },
    });
    expect(preflight.status).toBe(404);
    const issue = await fetch(`${h.base}/_relay/media-tickets`, {
      method: "POST",
      headers: { ...auth(), "content-type": "application/json" },
      body: JSON.stringify({ path: "/api/media/x" }),
    });
    expect(issue.status).toBe(404);
    expect((await fetch(`${h.base}/_relay/m/ticket`)).status).toBe(404);
    expect((await fetch(`${h.relay.url}/s/abc/echo`)).status).toBe(404);
    expect((await fetch(`${h.relay.url}/health`)).status).toBe(200);
    expect(core.requests).toBe(0);
    expect(h.relay.tunnels.counts().streams).toBe(0);
  });

  it("别的路径上的 WS 升级（明文 WS）：404，不升级、不进隧道", async () => {
    const h = await harness();
    const core = await connected(h);
    const ws = new WebSocket(`${h.base.replace("http", "ws")}/api/events`, [
      "armadra-ticket.t1",
      `armadra-relay.${relayToken()}`,
    ]);
    ws.on("error", () => {});
    const [, response] = (await once(ws, "unexpected-response")) as [
      unknown,
      import("node:http").IncomingMessage,
    ];
    expect(response.statusCode).toBe(404);
    expect(core.requests).toBe(0);
  });
});

describe("通道", () => {
  it("1000 条消息与 300 KB 二进制往返完好；源下线以 4404 结束", async () => {
    const h = await harness();
    const core = await connected(h);
    const ws = channel(h);
    expect(await outcome(ws)).toBeNull();
    for (let index = 0; index < 1000; index += 1) {
      const payload = Buffer.from(`${MARKER}-${index}`);
      expect((await echo(ws, payload)).equals(payload)).toBe(true);
    }
    const big = Buffer.alloc(300_000, 9);
    expect((await echo(ws, big)).equals(big)).toBe(true);
    core.terminate();
    expect(((await once(ws, "close")) as [number])[0]).toBe(
      CLOSE.sourceOffline,
    );
  });

  it("中继对客户端定时 ping；客户端不说话超过 idle 就断", async () => {
    const h = await harness({ wsPingMs: 50, wsIdleMs: 400 });
    await connected(h);
    const ws = channel(h);
    await once(ws, "open");
    let pings = 0;
    ws.on("ping", () => (pings += 1));
    await waitFor(() => pings >= 3);
    // ws 客户端会自动回 pong，所以一直活着；下面换一个只握手不说话的裸连接。
    const socket = connect(Number(new URL(h.relay.url).port), "127.0.0.1");
    cleanups.push(() => void socket.destroy());
    await once(socket, "connect");
    socket.write(
      `GET /s/${SOURCE_ID}/_relay/e2e HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n` +
        "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
        "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
        `Sec-WebSocket-Protocol: armadra-e2e.1, armadra-relay.${relayToken()}\r\n\r\n`,
    );
    const started = Date.now();
    let received = "";
    socket.on(
      "data",
      (chunk: Buffer) => (received += chunk.toString("latin1")),
    );
    await once(socket, "close");
    expect(received.startsWith("HTTP/1.1 101")).toBe(true);
    expect(Date.now() - started).toBeGreaterThanOrEqual(350);
    expect(ws.readyState).toBe(WebSocket.OPEN);
  });
});

describe("限额", () => {
  it("隧道流数满 → 4429 limit_reached；释放后恢复", async () => {
    const h = await harness({ limits: { maxStreams: 1 } });
    await connected(h);
    const held = channel(h);
    expect(await outcome(held)).toBeNull();
    expect(await outcome(channel(h))).toBe(CLOSE.limitReached);
    held.close();
    await once(held, "close");
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    expect(await outcome(channel(h))).toBeNull();
  });

  it("同一账号并发通道超限 → 4429；关掉一条后又能连", async () => {
    const h = await harness(
      {},
      { limits: { "edge.concurrent.acct": { type: "concurrent", max: 1 } } },
    );
    await connected(h);
    const first = channel(h);
    expect(await outcome(first)).toBeNull();
    expect(await outcome(channel(h))).toBe(CLOSE.limitReached);
    first.close();
    await once(first, "close");
    await waitFor(async () => {
      const probe = channel(h);
      const opened = (await outcome(probe)) === null;
      probe.terminate();
      return opened;
    }, 50);
  });

  it("有效令牌按令牌限流 → 4429；别的令牌各自独立", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.token": { type: "rate", max: 1, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    const fixed = relayToken();
    expect(await outcome(channel(h, fixed))).toBeNull();
    expect(await outcome(channel(h, fixed))).toBe(CLOSE.limitReached);
    // 同一个 IP、另一个令牌：不受影响。
    expect(await outcome(channel(h))).toBeNull();
  });

  it("按账号限流：同一账号的多个令牌共用", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.acct": { type: "rate", max: 2, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    for (let index = 0; index < 2; index += 1)
      expect(await outcome(channel(h))).toBeNull();
    expect(await outcome(channel(h))).toBe(CLOSE.limitReached);
    expect(
      await outcome(channel(h, relayToken({ sub: "acct_other" }))),
    ).toBeNull();
  });

  it("无令牌与无效令牌按 IP 严格限流，不挤占有效令牌", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.ip": { type: "rate", max: 2, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    expect(await outcome(channel(h, null))).toBe(CLOSE.unauthenticated);
    expect(await outcome(channel(h, "garbage"))).toBe(CLOSE.unauthenticated);
    expect(await outcome(channel(h, null))).toBe(CLOSE.limitReached);
    // 带有效令牌的照常通过。
    expect(await outcome(channel(h))).toBeNull();
  });
});

describe("背压", () => {
  it("客户端不读时源被窗口挡住；恢复后整段字节完好", async () => {
    const total = 64 * 1024 * 1024;
    const h = await harness();
    const core = await connected(
      h,
      h.core({ capabilities: ["e2e"], e2eSend: total }),
    );
    const ws = channel(h);
    const chunks: Buffer[] = [];
    let received = 0;
    ws.on("message", (data: Buffer) => {
      chunks.push(data);
      received += data.length;
    });
    await once(ws, "open");
    const raw = (ws as unknown as { _socket: import("node:net").Socket })
      ._socket;
    raw.pause();
    // 等源停下来：连续一段时间 bytesSent 不再增长。
    let last = -1;
    await waitFor(() => {
      const stalled = core.bytesSent === last && last > 0;
      last = core.bytesSent;
      return stalled;
    }, 200);
    // 停下时源只送出了窗口 + 两端套接字缓冲那么多，远小于全量。
    expect(core.bytesSent).toBeLessThan(16 * 1024 * 1024);
    raw.resume();
    await waitFor(() => received >= total, 20, 30_000);
    const all = Buffer.concat(chunks);
    expect(all.length).toBe(total);
    let mismatch = -1;
    for (let index = 0; index < all.length && mismatch < 0; index += 1)
      if (all[index] !== bigByte(index)) mismatch = index;
    expect(mismatch).toBe(-1);
  }, 60_000);

  it("小窗口下 1 MB 也能完整往返（两侧信用都回补）", async () => {
    const h = await harness({
      limits: { streamWindow: 4096, tunnelWindow: 16_384 },
    });
    await connected(h);
    const ws = channel(h);
    await once(ws, "open");
    const body = Buffer.alloc(1_000_000);
    for (let index = 0; index < body.length; index += 1)
      body[index] = bigByte(index);
    expect((await echo(ws, body)).equals(body)).toBe(true);
  }, 30_000);
});

describe("断线、重连与替换", () => {
  it("源断开 → 路由删除、4404；重连后路由更新、通道恢复", async () => {
    const h = await harness();
    const core = await connected(h);
    const presence: string[] = [];
    h.control.presence.subscribe((event) => presence.push(event.type));
    expect(await outcome(channel(h))).toBeNull();
    const firstRoute = await h.control.routes.get(SOURCE_ID);
    core.terminate();
    await waitFor(async () => (await h.control.routes.get(SOURCE_ID)) === null);
    expect(await outcome(channel(h))).toBe(CLOSE.sourceOffline);
    await connected(h, core);
    const secondRoute = await h.control.routes.get(SOURCE_ID);
    expect(secondRoute?.tunnelId).not.toBe(firstRoute?.tunnelId);
    expect(await outcome(channel(h))).toBeNull();
    expect(presence).toEqual(["sourceOffline", "sourceOnline"]);
  });

  it("同源新隧道替换旧的：旧的收 GOAWAY replaced 后以 4491 关闭，路由指向新的", async () => {
    const h = await harness({ limits: { goawayGraceMs: 200 } });
    const first = await connected(h);
    const second = await connected(h);
    expect(await first.closed()).toBe(CLOSE.tunnelReplaced);
    expect(first.goaways).toEqual(["replaced"]);
    expect(second.connected).toBe(true);
    expect(await outcome(channel(h))).toBeNull();
    expect(h.relay.tunnels.counts().tunnels).toBe(1);
  });

  it("心跳两次未答：中继断开隧道、删路由、计数", async () => {
    const h = await harness({ limits: { heartbeatMs: 50 } });
    const core = await connected(h);
    core.answerPings = false;
    await core.closed();
    await waitFor(async () => (await h.control.routes.get(SOURCE_ID)) === null);
    expect(h.relay.metrics.snapshot().heartbeatMisses).toBe(1);
  });

  it("握手：坏令牌被拒（4490）、10 秒内不发 hello 关 4400", async () => {
    const h = await harness({ handshakeTimeoutMs: 100 });
    const bad = h.core({
      token: () => tunnelToken({ sub: "fedcba9876543210fedcba9876543210" }),
    });
    const result = await bad.connect();
    expect(result.reject?.code).toBe("tunnel_token_invalid");
    expect(result.closeCode).toBe(CLOSE.tunnelAuthFailed);
    const idle = new WebSocket(h.tunnelUrl);
    cleanups.push(() => idle.terminate());
    expect(((await once(idle, "close")) as [number])[0]).toBe(CLOSE.badFrame);
    expect(h.relay.metrics.snapshot().handshakes).toMatchObject({
      tunnel_token_invalid: 1,
      timeout: 1,
    });
  });
});

describe("GOAWAY", () => {
  it("中继关闭：发 GOAWAY shutdown，新通道 4404，旧通道自然结束后 4492", async () => {
    const h = await harness();
    const core = await connected(h);
    const held = channel(h);
    expect(await outcome(held)).toBeNull();
    const closing = h.relay.close(5000);
    await waitFor(() => core.goaways.length === 1);
    expect(core.goaways).toEqual(["shutdown"]);
    expect(await outcome(channel(h))).not.toBeNull();
    const payload = Buffer.from(MARKER);
    expect((await echo(held, payload)).equals(payload)).toBe(true);
    held.close();
    expect(await core.closed()).toBe(CLOSE.tunnelGoaway);
    await closing;
  });

  it("源发 GOAWAY：中继不再给它开流，删路由", async () => {
    const h = await harness();
    const core = await connected(h);
    core.sendGoaway();
    await waitFor(async () => (await h.control.routes.get(SOURCE_ID)) === null);
    expect(await outcome(channel(h))).toBe(CLOSE.sourceOffline);
  });
});

describe("指标", () => {
  it("/metrics 只在单独的监听上，可要求令牌", async () => {
    const h = await harness({
      metricsListen: { host: "127.0.0.1", port: 0 },
      metricsToken: "metrics-secret",
    });
    await connected(h);
    expect(await outcome(channel(h))).toBeNull();
    expect((await fetch(`${h.relay.url}/metrics`)).status).toBe(404);
    expect((await fetch(`${h.relay.metricsUrl}/metrics`)).status).toBe(401);
    const text = await (
      await fetch(`${h.relay.metricsUrl}/metrics`, {
        headers: { authorization: "Bearer metrics-secret" },
      })
    ).text();
    expect(text).toContain("relay_tunnels 1");
    expect(text).toContain('relay_edge_connections_total{result="ok"} 1');
    expect(text).toContain('relay_handshake_total{result="ok"} 1');
  });
});

async function waitFor(
  condition: () => boolean | Promise<boolean>,
  intervalMs = 10,
  timeoutMs = 10_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error("等待超时");
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}

describe("端到端通道 _relay/e2e（cloud-api §20）", () => {
  const e2eUrl = (h: Harness) => `${h.base.replace("http", "ws")}/_relay/e2e`;
  function open(h: Harness, protocols: string[]) {
    const ws = new WebSocket(e2eUrl(h), protocols);
    cleanups.push(() => ws.terminate());
    return ws;
  }
  const closeCode = async (ws: WebSocket) =>
    ((await once(ws, "close")) as [number, Buffer]).map((value) =>
      Buffer.isBuffer(value) ? value.toString() : value,
    );

  it("源报 e2e：OPEN kind e2e 不带头；随机字节双向 10 MB 原样回来；指标 kind=e2e；日志没有内容", async () => {
    const h = await harness({ metricsListen: { host: "127.0.0.1", port: 0 } });
    const core = await connected(h, h.core({ capabilities: ["e2e"] }));
    const ws = open(h, [
      "armadra-e2e.1",
      `armadra-relay.${relayToken({ dev: "dev_1" })}`,
    ]);
    await once(ws, "open");
    expect(ws.protocol).toBe("armadra-e2e.1");
    await waitFor(() => core.e2eOpens.length === 1);
    expect(core.e2eOpens[0]).toEqual({
      kind: "e2e",
      method: "GET",
      path: "/_relay/e2e",
      headers: [],
      clientOrigin: null,
      remoteIp: "127.0.0.1",
      relayNode: "relay-test",
    });
    const total = 10 * 1024 * 1024;
    const sent = createHash("sha256");
    const got = createHash("sha256");
    let received = 0;
    ws.on("message", (data: Buffer) => {
      got.update(data);
      received += data.length;
    });
    // 中继可以合并或切分消息：只比字节流。
    let seed = 1;
    for (let offset = 0; offset < total; ) {
      const size = Math.min(total - offset, 1 + (seed % 70_000));
      const chunk = Buffer.alloc(size);
      for (let index = 0; index < size; index += 1) {
        seed = (seed * 1_103_515_245 + 12_345) & 0x7fffffff;
        chunk[index] = seed & 0xff;
      }
      sent.update(chunk);
      ws.send(chunk);
      offset += size;
      if (ws.bufferedAmount > 4 * 1024 * 1024)
        await waitFor(() => ws.bufferedAmount < 1024 * 1024);
    }
    await waitFor(() => received >= total, 20, 30_000);
    expect(received).toBe(total);
    expect(got.digest("hex")).toBe(sent.digest("hex"));
    const text = await (await fetch(`${h.relay.metricsUrl}/metrics`)).text();
    expect(text).toContain('relay_edge_streams_total{kind="e2e"} 1');
    ws.close(1000);
    await once(ws, "close");
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    await waitFor(() => h.logs.some((line) => line.includes("端到端通道结束")));
    const line = h.logs.find((entry) => entry.includes("端到端通道结束")) ?? "";
    expect(line).toContain('"kind":"e2e"');
    expect(line).not.toContain("dev_1");
    expect(line).not.toContain("acct_test");
  }, 60_000);

  it("拒绝：令牌缺 4401、src 不符 4403、缺版本子协议 4409、源没报 e2e 4404 e2e_unsupported、源离线 4404；HTTP 404", async () => {
    const h = await harness();
    expect(
      await closeCode(
        open(h, ["armadra-e2e.1", `armadra-relay.${relayToken()}`]),
      ),
    ).toEqual([CLOSE.sourceOffline, "source_offline"]);
    const core = await connected(h, h.core());
    expect(await closeCode(open(h, ["armadra-e2e.1"]))).toEqual([
      CLOSE.unauthenticated,
      "relay_token_invalid",
    ]);
    expect(
      await closeCode(
        open(h, [
          "armadra-e2e.1",
          `armadra-relay.${relayToken({ src: "fedcba9876543210fedcba9876543210" })}`,
        ]),
      ),
    ).toEqual([CLOSE.forbidden, "relay_source_mismatch"]);
    expect(
      await closeCode(
        open(h, [`armadra-relay.${relayToken()}`, "armadra-ticket.t"]),
      ),
    ).toEqual([CLOSE.protocolUnsupported, "protocol_unsupported"]);
    expect(
      await closeCode(
        open(h, ["armadra-e2e.1", `armadra-relay.${relayToken()}`]),
      ),
    ).toEqual([CLOSE.sourceOffline, "e2e_unsupported"]);
    expect(core.e2eOpens).toHaveLength(0);
    const plain = await fetch(`${h.base}/_relay/e2e`, { headers: auth() });
    expect(plain.status).toBe(404);
    expect(core.requests).toBe(0);
  });

  it("文本消息以 4400 关闭；源下线以 4404 结束；客户端关闭后流收尾", async () => {
    const h = await harness();
    const core = await connected(h, h.core({ capabilities: ["e2e"] }));
    const text = open(h, ["armadra-e2e.1", `armadra-relay.${relayToken()}`]);
    await once(text, "open");
    text.send("plain");
    expect((await closeCode(text))[0]).toBe(CLOSE.badFrame);
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    const ws = open(h, ["armadra-e2e.1", `armadra-relay.${relayToken()}`]);
    await once(ws, "open");
    const echoed = new Promise<Buffer>((resolve) =>
      ws.once("message", (data: Buffer) => resolve(data)),
    );
    ws.send(Buffer.from([1, 2, 3]));
    expect([...(await echoed)]).toEqual([1, 2, 3]);
    core.terminate();
    expect(await closeCode(ws)).toEqual([
      CLOSE.sourceOffline,
      "source_offline",
    ]);
  });

  it("撤销源时 e2e 连接一起断开", async () => {
    const h = await harness();
    await connected(h, h.core({ capabilities: ["e2e"] }));
    const ws = open(h, ["armadra-e2e.1", `armadra-relay.${relayToken()}`]);
    await once(ws, "open");
    expect(h.relay.disconnect({ sourceId: SOURCE_ID })).toBe(1);
    await once(ws, "close");
  });
});

describe("流量统计（cloud-api §19）", () => {
  function recording() {
    const rows: TrafficRow[] = [];
    return {
      rows,
      traffic: {
        write: async (batch: TrafficRow[]) => {
          rows.push(...batch.map((row) => ({ ...row })));
        },
        // 用例里手动 flush。
        flushMs: 3_600_000,
      },
    };
  }

  const sum = (rows: TrafficRow[], field: keyof TrafficRow) =>
    rows.reduce((total, row) => total + Number(row[field]), 0);

  it("端到端通道记为 e2e：字节是边缘 WS 载荷之和，设备取令牌的 dev", async () => {
    const record = recording();
    const h = await harness({ traffic: record.traffic });
    await connected(h, h.core({ capabilities: ["e2e"] }));
    const ws = new WebSocket(`${h.base.replace("http", "ws")}/_relay/e2e`, [
      "armadra-e2e.1",
      `armadra-relay.${relayToken({ sub: "acct:alice", dev: "dev_1" })}`,
    ]);
    cleanups.push(() => ws.terminate());
    await once(ws, "open");
    let received = 0;
    ws.on("message", (data: Buffer) => (received += data.length));
    for (let index = 0; index < 10; index += 1) ws.send(Buffer.alloc(1000, 7));
    await waitFor(() => received >= 10_000);
    ws.close();
    await once(ws, "close");
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    await h.relay.traffic?.flush();
    expect(record.rows).toEqual([
      expect.objectContaining({
        subject: "acct:alice",
        deviceId: "dev_1",
        kind: "e2e",
        connections: 1,
        bytesToSource: 10_000,
        bytesToClient: 10_000,
      }),
    ]);
  });

  it("每 flushMs 自动落库，关闭时把剩下的写掉；没开统计不写", async () => {
    const record = recording();
    const h = await harness({
      traffic: { ...record.traffic, flushMs: 50 },
    });
    await connected(h);
    const first = channel(h);
    await once(first, "open");
    await waitFor(() => record.rows.length === 1);
    const second = channel(h);
    await once(second, "open");
    await h.relay.close(0);
    expect(sum(record.rows, "connections")).toBe(2);

    const off = await harness();
    await connected(off);
    expect(await outcome(channel(off))).toBeNull();
    expect(off.relay.traffic).toBeUndefined();
  });

  it("开销：开不开统计，通道往返延迟与 5 MB 回显耗时没有明显差别", async () => {
    const measure = async (h: Harness) => {
      const ws = channel(h);
      await once(ws, "open");
      const latencies: number[] = [];
      const small = Buffer.from("x");
      for (let index = 0; index < 100; index += 1) {
        const started = performance.now();
        await echo(ws, small);
        latencies.push(performance.now() - started);
      }
      latencies.sort((a, b) => a - b);
      const started = performance.now();
      await echo(ws, Buffer.alloc(5_000_000, 3));
      ws.close();
      return {
        medianMs: latencies[50] ?? 0,
        downloadMs: performance.now() - started,
      };
    };
    const plain = await harness();
    await connected(plain);
    const metered = await harness({ traffic: recording().traffic });
    await connected(metered);
    // 预热一轮，再各测一轮。
    await measure(plain);
    await measure(metered);
    const without = await measure(plain);
    const withTraffic = await measure(metered);
    // 宽松的门槛（CI 带覆盖率插桩、机器嘈杂）：只防退化成按帧写库这类数量级的开销。
    expect(withTraffic.medianMs).toBeLessThan(without.medianMs * 2 + 2);
    expect(withTraffic.downloadMs).toBeLessThan(without.downloadMs * 2 + 200);
  }, 60_000);
});
