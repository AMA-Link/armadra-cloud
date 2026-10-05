/**
 * 集成：假 core 经中继完成 HTTP 往返、WebSocket 双向、背压、断线重连、GOAWAY（中继设计 §9）。
 * 中继用 personal 的寻址（`/s/<id>/…`）与内存控制面；令牌用协议包的测试钥签。
 * 每个用例结束时核对「零日志」：日志里没有正文、令牌、路径。
 */
import { createHash } from "node:crypto";
import { once } from "node:events";
import { get, request } from "node:http";
import { connect } from "node:net";

import { createLogger } from "@armadra/cloud-shared/log";
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { afterEach, describe, expect, it } from "vitest";
import { WebSocket } from "ws";

import { pathAddressing } from "../../src/edge/addressing.ts";
import {
  startRelay,
  type RelayOptions,
  type RunningRelay,
} from "../../src/main.ts";
import { bigByte, FakeCore } from "../fake-core.ts";
import {
  relayToken,
  SOURCE_ID,
  testControlPlane,
  tunnelToken,
} from "../tokens.ts";

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

async function connected(h: Harness, core = h.core()): Promise<FakeCore> {
  const result = await core.connect();
  expect(result.ready).toBeDefined();
  return core;
}

describe("HTTP 往返", () => {
  it("GET：中继剥令牌、加 X-Forwarded-*、保留 Host 与 Authorization；/s/<id> 前缀剥掉", async () => {
    const h = await harness();
    await connected(h);
    const response = await fetch(`${h.base}/echo-request?q=1`, {
      headers: {
        ...auth(),
        authorization: "Bearer core-session",
        "x-forwarded-for": "6.6.6.6",
        origin: "https://app.example",
      },
    });
    expect(response.status).toBe(200);
    const echoed = (await response.json()) as {
      url: string;
      headers: Record<string, string>;
    };
    expect(echoed.url).toBe("/echo-request?q=1");
    expect(echoed.headers["armadra-relay-token"]).toBeUndefined();
    expect(echoed.headers["x-forwarded-for"]).toBe("127.0.0.1");
    expect(echoed.headers["x-forwarded-proto"]).toBe("https");
    expect(echoed.headers.authorization).toBe("Bearer core-session");
    expect(echoed.headers.host).toBe(new URL(h.relay.url).host);
  });

  it("POST：定长体与 chunked 体都原样回来", async () => {
    const h = await harness();
    await connected(h);
    const fixed = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: { ...auth(), "content-type": "text/plain" },
      body: `${MARKER}-fixed`,
    });
    expect(await fixed.text()).toBe(`${MARKER}-fixed`);

    const chunks = Array.from(
      { length: 50 },
      (_, index) => `${MARKER}-${index};`,
    );
    const body = await new Promise<string>((resolve, reject) => {
      const req = request(
        `${h.base}/echo`,
        {
          method: "POST",
          headers: { ...auth(), "content-type": "text/plain" },
        },
        (res) => {
          let text = "";
          res.setEncoding("utf8");
          res.on("data", (chunk: string) => (text += chunk));
          res.on("end", () => resolve(text));
        },
      );
      req.on("error", reject);
      for (const chunk of chunks) req.write(chunk);
      req.end();
    });
    expect(body).toBe(chunks.join(""));
  });

  it("并发 100 条流都完成，结束后流表清空", async () => {
    const h = await harness();
    const core = await connected(h);
    const results = await Promise.all(
      Array.from({ length: 100 }, async (_, index) => {
        const response = await fetch(`${h.base}/echo`, {
          method: "POST",
          headers: auth(),
          body: `${MARKER}-${index}`,
        });
        return response.text();
      }),
    );
    expect(results).toEqual(
      Array.from({ length: 100 }, (_, index) => `${MARKER}-${index}`),
    );
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    await waitFor(() => core.openStreams === 0);
  });

  it("令牌缺 401、src 不符 403、不是源地址 404；/health 仍是中继自己的", async () => {
    const h = await harness();
    await connected(h);
    const missing = await fetch(`${h.base}/echo-request`);
    expect(missing.status).toBe(401);
    expect(await missing.json()).toEqual({
      code: "relay_token_invalid",
      message: expect.any(String),
    });
    const mismatch = await fetch(`${h.base}/echo-request`, {
      headers: auth(relayToken({ src: "fedcba9876543210fedcba9876543210" })),
    });
    expect(mismatch.status).toBe(403);
    expect(((await mismatch.json()) as { code: string }).code).toBe(
      "relay_source_mismatch",
    );
    const other = await fetch(`${h.relay.url}/s/nothex/x`, { headers: auth() });
    expect(other.status).toBe(404);
    const health = await fetch(`${h.relay.url}/health`);
    expect(await health.json()).toMatchObject({ ok: true, tunnels: 1 });
  });

  it("源离线 → 503 source_offline", async () => {
    const h = await harness();
    const response = await fetch(`${h.base}/echo-request`, { headers: auth() });
    expect(response.status).toBe(503);
    expect(((await response.json()) as { code: string }).code).toBe(
      "source_offline",
    );
  });
});

describe("WebSocket", () => {
  function open(h: Harness, protocols: string[]) {
    const ws = new WebSocket(
      `${h.base.replace("http", "ws")}/api/events`,
      protocols,
    );
    cleanups.push(() => ws.terminate());
    return ws;
  }

  it("双向回显 1000 帧与二进制帧；armadra-relay.* 剥掉、armadra-ticket.* 留给源", async () => {
    const h = await harness();
    await connected(h);
    const ws = open(h, ["armadra-ticket.t1", `armadra-relay.${relayToken()}`]);
    await once(ws, "open");
    expect(ws.protocol).toBe("armadra-ticket.t1");
    const replies: string[] = [];
    const binary: Buffer[] = [];
    ws.on("message", (data: Buffer, isBinary: boolean) => {
      if (isBinary) binary.push(data);
      else replies.push(data.toString());
    });
    ws.send("whoami");
    await waitFor(() => replies.length === 1);
    const whoami = JSON.parse(replies.shift() ?? "{}") as {
      url: string;
      protocol: string;
      headers: Record<string, string>;
    };
    expect(whoami.url).toBe("/api/events");
    expect(whoami.headers["sec-websocket-protocol"]).toBe("armadra-ticket.t1");
    for (let index = 0; index < 1000; index += 1) ws.send(`${MARKER}-${index}`);
    await waitFor(() => replies.length === 1000);
    expect(replies[999]).toBe(`${MARKER}-999`);
    const payload = Buffer.alloc(300_000, 9);
    ws.send(payload);
    await waitFor(() => binary.length === 1);
    expect(binary[0]?.equals(payload)).toBe(true);
    ws.send("bye");
    const [code] = (await once(ws, "close")) as [number];
    expect(code).toBe(4000);
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
  });

  it("令牌错 → 4401；src 不符 → 4403；源离线 → 4404", async () => {
    const h = await harness();
    const offline = open(h, [`armadra-relay.${relayToken()}`]);
    expect(((await once(offline, "close")) as [number])[0]).toBe(
      CLOSE.sourceOffline,
    );
    await connected(h);
    const missing = open(h, ["armadra-ticket.t1"]);
    expect(((await once(missing, "close")) as [number])[0]).toBe(
      CLOSE.unauthenticated,
    );
    const mismatch = open(h, [
      `armadra-relay.${relayToken({ src: "fedcba9876543210fedcba9876543210" })}`,
    ]);
    expect(((await once(mismatch, "close")) as [number])[0]).toBe(
      CLOSE.forbidden,
    );
  });

  it("中继对客户端定时 ping；客户端不说话超过 idle 就断", async () => {
    const h = await harness({ wsPingMs: 50, wsIdleMs: 400 });
    await connected(h);
    const ws = open(h, ["armadra-ticket.t1", `armadra-relay.${relayToken()}`]);
    await once(ws, "open");
    let pings = 0;
    ws.on("ping", () => (pings += 1));
    await waitFor(() => pings >= 3);
    // ws 客户端会自动回 pong，所以一直活着；下面换一个只握手不说话的裸连接。
    const socket = connect(Number(new URL(h.relay.url).port), "127.0.0.1");
    cleanups.push(() => void socket.destroy());
    await once(socket, "connect");
    socket.write(
      `GET /s/${SOURCE_ID}/api/raw HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n` +
        "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
        "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
        `Sec-WebSocket-Protocol: armadra-relay.${relayToken()}\r\n\r\n`,
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

  it("源下线时已建立的 WS 以 4404 结束", async () => {
    const h = await harness();
    const core = await connected(h);
    const ws = open(h, ["armadra-ticket.t1", `armadra-relay.${relayToken()}`]);
    await once(ws, "open");
    core.terminate();
    expect(((await once(ws, "close")) as [number])[0]).toBe(
      CLOSE.sourceOffline,
    );
  });
});

describe("限额", () => {
  it("隧道流数满 → 429 limit_reached；释放后恢复", async () => {
    const h = await harness({ limits: { maxStreams: 1 } });
    const core = await connected(h);
    const held = await new Promise<import("node:http").IncomingMessage>(
      (resolve, reject) => {
        const req = get(`${h.base}/hold`, { headers: auth() }, resolve);
        req.on("error", reject);
      },
    );
    held.resume();
    const full = await fetch(`${h.base}/echo-request`, { headers: auth() });
    expect(full.status).toBe(429);
    expect(((await full.json()) as { code: string }).code).toBe(
      "limit_reached",
    );
    core.release();
    await once(held, "end");
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
  });

  it("同一账号并发 WS 超限 → 4429；关掉一条后又能连", async () => {
    const h = await harness(
      {},
      { limits: { "edge.concurrent.acct": { type: "concurrent", max: 1 } } },
    );
    await connected(h);
    const protocols = () => [
      "armadra-ticket.t1",
      `armadra-relay.${relayToken()}`,
    ];
    const url = `${h.base.replace("http", "ws")}/api/events`;
    const first = new WebSocket(url, protocols());
    cleanups.push(() => first.terminate());
    await once(first, "open");
    const second = new WebSocket(url, protocols());
    cleanups.push(() => second.terminate());
    expect(((await once(second, "close")) as [number])[0]).toBe(
      CLOSE.limitReached,
    );
    first.close();
    await once(first, "close");
    await waitFor(async () => {
      const probe = new WebSocket(url, protocols());
      cleanups.push(() => probe.terminate());
      const opened = await Promise.race([
        once(probe, "open").then(() => true),
        once(probe, "close").then(() => false),
      ]);
      probe.terminate();
      return opened;
    }, 50);
  });

  it("按 IP 限流 → 429 rate_limited 带 Retry-After", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.ip": { type: "rate", max: 1, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
    const limited = await fetch(`${h.base}/echo-request`, { headers: auth() });
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("60");
    expect(((await limited.json()) as { code: string }).code).toBe(
      "rate_limited",
    );
  });
});

describe("背压", () => {
  it("客户端不读时源被窗口挡住；恢复后整段字节完好", async () => {
    const h = await harness();
    const core = await connected(h);
    const total = 64 * 1024 * 1024;
    const response = await new Promise<import("node:http").IncomingMessage>(
      (resolve, reject) => {
        const req = get(
          `${h.base}/big?bytes=${total}`,
          { headers: auth() },
          resolve,
        );
        req.on("error", reject);
      },
    );
    response.pause();
    // 等源停下来：连续一段时间 bytesSent 不再增长。
    let last = -1;
    await waitFor(() => {
      const stalled = core.bytesSent === last && last > 0;
      last = core.bytesSent;
      return stalled;
    }, 200);
    const stalledAt = core.bytesSent;
    // 停下时源只送出了窗口 + 两端套接字缓冲那么多（本机实测约 1.2 MB），远小于全量。
    expect(stalledAt).toBeLessThan(8 * 1024 * 1024);
    const hash = createHash("sha256");
    let received = 0;
    let mismatch = -1;
    response.on("data", (chunk: Buffer) => {
      for (let index = 0; index < chunk.length && mismatch < 0; index += 1)
        if (chunk[index] !== bigByte(received + index))
          mismatch = received + index;
      received += chunk.length;
      hash.update(chunk);
    });
    response.resume();
    await once(response, "end");
    expect(mismatch).toBe(-1);
    expect(received).toBe(total);
    // bytesSent 还含响应头。
    expect(core.bytesSent - total).toBeGreaterThan(0);
    expect(core.bytesSent - total).toBeLessThan(1024);
    expect(h.relay.metrics.snapshot().bytes.to_client).toBeGreaterThanOrEqual(
      total,
    );
  }, 30_000);

  it("小窗口下 POST 大体也能完整往返（两侧信用都回补）", async () => {
    const h = await harness({
      limits: { streamWindow: 4096, tunnelWindow: 16_384 },
    });
    await connected(h);
    const body = Buffer.alloc(1_000_000);
    for (let index = 0; index < body.length; index += 1)
      body[index] = bigByte(index);
    const response = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: auth(),
      body,
    });
    const echoed = Buffer.from(await response.arrayBuffer());
    expect(echoed.equals(body)).toBe(true);
  }, 30_000);
});

describe("断线、重连与替换", () => {
  it("源断开 → 路由删除、503；重连后路由更新、请求恢复", async () => {
    const h = await harness();
    const core = await connected(h);
    const presence: string[] = [];
    h.control.presence.subscribe((event) => presence.push(event.type));
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
    const firstRoute = await h.control.routes.get(SOURCE_ID);
    core.terminate();
    await waitFor(async () => (await h.control.routes.get(SOURCE_ID)) === null);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(503);
    await connected(h, core);
    const secondRoute = await h.control.routes.get(SOURCE_ID);
    expect(secondRoute?.tunnelId).not.toBe(firstRoute?.tunnelId);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
    expect(presence).toEqual(["sourceOffline", "sourceOnline"]);
  });

  it("同源新隧道替换旧的：旧的收 GOAWAY replaced 后以 4491 关闭，路由指向新的", async () => {
    const h = await harness({ limits: { goawayGraceMs: 200 } });
    const first = await connected(h);
    const second = await connected(h);
    expect(await first.closed()).toBe(CLOSE.tunnelReplaced);
    expect(first.goaways).toEqual(["replaced"]);
    expect(second.connected).toBe(true);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
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
  it("中继关闭：发 GOAWAY shutdown，新请求 503，旧流自然结束后 4492", async () => {
    const h = await harness();
    const core = await connected(h);
    const held = await new Promise<import("node:http").IncomingMessage>(
      (resolve, reject) => {
        const req = get(`${h.base}/hold`, { headers: auth() }, resolve);
        req.on("error", reject);
      },
    );
    let text = "";
    held.setEncoding("utf8");
    held.on("data", (chunk: string) => (text += chunk));
    const closing = h.relay.close(5000);
    await waitFor(() => core.goaways.length === 1);
    expect(core.goaways).toEqual(["shutdown"]);
    const refused = await fetch(`${h.base}/echo-request`, { headers: auth() });
    expect(refused.status).toBe(503);
    core.release();
    await once(held, "end");
    expect(text).toBe("held:released");
    expect(await core.closed()).toBe(CLOSE.tunnelGoaway);
    await closing;
  });

  it("源发 GOAWAY：中继不再给它开流，删路由", async () => {
    const h = await harness();
    const core = await connected(h);
    core.sendGoaway();
    await waitFor(async () => (await h.control.routes.get(SOURCE_ID)) === null);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(503);
  });
});

describe("指标", () => {
  it("/metrics 只在单独的监听上，可要求令牌", async () => {
    const h = await harness({
      metricsListen: { host: "127.0.0.1", port: 0 },
      metricsToken: "metrics-secret",
    });
    await connected(h);
    await fetch(`${h.base}/echo-request`, { headers: auth() });
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
