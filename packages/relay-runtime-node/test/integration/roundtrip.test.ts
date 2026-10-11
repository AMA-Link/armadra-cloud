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

describe("CORS 预检", () => {
  const TRUSTED = "https://app.example";
  const preflight = (
    url: string,
    origin: string,
    extra: Record<string, string> = {},
  ) =>
    fetch(url, {
      method: "OPTIONS",
      headers: {
        origin,
        "access-control-request-method": "POST",
        "access-control-request-headers": "armadra-relay-token, content-type",
        ...extra,
      },
    });

  it("名单里的来源（原生 App、可信来源）不带令牌也答 204，按 Origin 回显；不进隧道", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    const core = await connected(h);
    for (const origin of [
      "https://localhost",
      "capacitor://localhost",
      TRUSTED,
    ]) {
      const answer = await preflight(
        `${h.base}/api/identity/cloud/login`,
        origin,
      );
      expect(answer.status).toBe(204);
      expect(answer.headers.get("access-control-allow-origin")).toBe(origin);
      expect(answer.headers.get("vary")).toBe("origin");
      expect(answer.headers.get("access-control-allow-credentials")).toBeNull();
      expect(answer.headers.get("access-control-allow-methods")).toBe(
        "GET,POST,PUT,PATCH,DELETE,OPTIONS",
      );
      expect(
        answer.headers.get("access-control-allow-headers")?.split(", "),
      ).toEqual(
        expect.arrayContaining([
          "armadra-relay-token",
          "authorization",
          "content-type",
          "x-armadra-csrf",
        ]),
      );
    }
    expect(core.requests).toBe(0);
    expect(h.relay.tunnels.counts().streams).toBe(0);
    // 预检之后的真请求照常带令牌转进隧道。
    const real = await fetch(`${h.base}/echo-request`, {
      headers: { ...auth(), origin: "https://localhost" },
    });
    expect(real.status).toBe(200);
    expect(core.requests).toBe(1);
  });

  it("答案与源在不在线、登没登记无关（借预检探不出源）", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    const offline = await preflight(`${h.base}/api/x`, TRUSTED);
    const unknown = await preflight(
      `${h.relay.url}/s/${"f".repeat(32)}/api/x`,
      TRUSTED,
    );
    for (const answer of [offline, unknown]) {
      expect(answer.status).toBe(204);
      expect(answer.headers.get("access-control-allow-origin")).toBe(TRUSTED);
    }
    const badOffline = await preflight(
      `${h.base}/api/x`,
      "https://evil.example",
    );
    expect(badOffline.status).toBe(403);
  });

  it("名单外的来源答 403 forbidden，不带 CORS 头；没配名单时只认原生 App", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    const core = await connected(h);
    for (const origin of [
      "https://evil.example",
      "https://app.example:8443",
      "null",
    ]) {
      const answer = await preflight(`${h.base}/api/x`, origin);
      expect(answer.status).toBe(403);
      expect(answer.headers.get("access-control-allow-origin")).toBeNull();
      expect(((await answer.json()) as { code: string }).code).toBe(
        "forbidden",
      );
    }
    expect(core.requests).toBe(0);
    const bare = await harness();
    expect((await preflight(`${bare.base}/api/x`, TRUSTED)).status).toBe(403);
    expect(
      (await preflight(`${bare.base}/api/x`, "https://localhost")).status,
    ).toBe(204);
  });

  it("不是预检的请求仍要令牌：带来源的 POST、缺 Request-Method 的 OPTIONS 都 401", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    const core = await connected(h);
    const post = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: { origin: TRUSTED },
      body: MARKER,
    });
    expect(post.status).toBe(401);
    expect(((await post.json()) as { code: string }).code).toBe(
      "relay_token_invalid",
    );
    const options = await fetch(`${h.base}/echo-request`, {
      method: "OPTIONS",
      headers: { origin: TRUSTED },
    });
    expect(options.status).toBe(401);
    const get = await fetch(`${h.base}/echo-request`, {
      headers: { origin: "https://localhost" },
    });
    expect(get.status).toBe(401);
    expect(core.requests).toBe(0);
  });

  it("预检按 IP 限流（单独的桶）：超了答 429 rate_limited 带 Retry-After", async () => {
    const h = await harness(
      { corsOrigins: () => [TRUSTED] },
      {
        limits: {
          "edge.preflight.ip": { type: "rate", max: 2, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    expect((await preflight(`${h.base}/api/x`, TRUSTED)).status).toBe(204);
    expect((await preflight(`${h.base}/api/x`, TRUSTED)).status).toBe(204);
    const limited = await preflight(`${h.base}/api/x`, TRUSTED);
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("60");
    // 名单内来源的 429 也带 CORS 头，页面读得到 retry-after。
    expect(limited.headers.get("access-control-allow-origin")).toBe(TRUSTED);
    expect(limited.headers.get("access-control-expose-headers")).toBe(
      "retry-after",
    );
    // 预检突发不影响已鉴权的请求，也不影响无令牌请求的 IP 桶。
    for (let index = 0; index < 5; index += 1)
      expect(
        (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
      ).toBe(200);
    expect((await fetch(`${h.base}/echo-request`)).status).toBe(401);
  });
});

describe("错误答复的 CORS", () => {
  const TRUSTED = "https://app.example";
  const OUTSIDER = "https://evil.example";
  type Seen = {
    status: number;
    code: string;
    headers: Headers;
  };
  const seen = async (url: string, init: RequestInit): Promise<Seen> => {
    const response = await fetch(url, init);
    const body = (await response.json()) as { code: string; message: string };
    expect(Object.keys(body).sort()).toEqual(["code", "message"]);
    return {
      status: response.status,
      code: body.code,
      headers: response.headers,
    };
  };
  const echoed = (answer: Seen, origin: string) => {
    expect(answer.headers.get("access-control-allow-origin")).toBe(origin);
    expect(answer.headers.get("vary")?.toLowerCase()).toContain("origin");
    expect(answer.headers.get("access-control-expose-headers")).toBe(
      "retry-after",
    );
    expect(answer.headers.get("access-control-allow-credentials")).toBeNull();
  };
  const bare = (answer: Seen) => {
    expect(answer.headers.get("access-control-allow-origin")).toBeNull();
    expect(answer.headers.get("access-control-expose-headers")).toBeNull();
  };

  it("401 / 403 令牌类：名单内来源（可信、原生 App）回显并暴露 retry-after，名单外与无 Origin 不加", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    await connected(h);
    const wrongSource = auth(
      relayToken({ src: "fedcba9876543210fedcba9876543210" }),
    );
    for (const origin of [
      TRUSTED,
      "https://localhost",
      "capacitor://localhost",
    ]) {
      const missing = await seen(`${h.base}/echo-request`, {
        headers: { origin },
      });
      expect([missing.status, missing.code]).toEqual([
        401,
        "relay_token_invalid",
      ]);
      echoed(missing, origin);
      const mismatch = await seen(`${h.base}/echo-request`, {
        headers: { origin, ...wrongSource },
      });
      expect([mismatch.status, mismatch.code]).toEqual([
        403,
        "relay_source_mismatch",
      ]);
      echoed(mismatch, origin);
    }
    bare(
      await seen(`${h.base}/echo-request`, { headers: { origin: OUTSIDER } }),
    );
    bare(await seen(`${h.base}/echo-request`, {}));
    bare(
      await seen(`${h.base}/echo-request`, {
        headers: { origin: "null", ...wrongSource },
      }),
    );
  });

  it("503 source_offline：名单内来源带 CORS 头，名单外不带", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    const inside = await seen(`${h.base}/echo-request`, {
      headers: { origin: TRUSTED, ...auth() },
    });
    expect([inside.status, inside.code]).toEqual([503, "source_offline"]);
    echoed(inside, TRUSTED);
    bare(
      await seen(`${h.base}/echo-request`, {
        headers: { origin: OUTSIDER, ...auth() },
      }),
    );
  });

  it("429 rate_limited：带 retry-after 且它被暴露；名单外只有 retry-after", async () => {
    const h = await harness(
      { corsOrigins: () => [TRUSTED] },
      {
        limits: {
          "edge.connect.token": { type: "rate", max: 1, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    const fixed = auth();
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: fixed })).status,
    ).toBe(200);
    const inside = await seen(`${h.base}/echo-request`, {
      headers: { origin: TRUSTED, ...fixed },
    });
    expect([inside.status, inside.code]).toEqual([429, "rate_limited"]);
    expect(inside.headers.get("retry-after")).toBe("60");
    echoed(inside, TRUSTED);
    const outside = await seen(`${h.base}/echo-request`, {
      headers: { origin: OUTSIDER, ...fixed },
    });
    expect(outside.status).toBe(429);
    expect(outside.headers.get("retry-after")).toBe("60");
    bare(outside);
  });

  it("名单按源取：别的源的可信来源不算", async () => {
    const OTHER = "fedcba9876543210fedcba9876543210";
    const h = await harness({
      corsOrigins: (sourceId) => (sourceId === SOURCE_ID ? [TRUSTED] : []),
    });
    const own = await seen(`${h.base}/echo-request`, {
      headers: { origin: TRUSTED },
    });
    echoed(own, TRUSTED);
    const other = await seen(`${h.relay.url}/s/${OTHER}/echo-request`, {
      headers: { origin: TRUSTED },
    });
    expect([other.status, other.code]).toEqual([401, "relay_token_invalid"]);
    bare(other);
    echoed(
      await seen(`${h.relay.url}/s/${OTHER}/echo-request`, {
        headers: { origin: "https://localhost" },
      }),
      "https://localhost",
    );
  });

  it("升级被拒的 HTTP 答复同样带名单内来源的头；鉴权判定不变", async () => {
    const h = await harness({ corsOrigins: () => [TRUSTED] });
    await connected(h);
    const attempt = (origin: string) =>
      new Promise<{ status: number; headers: Record<string, unknown> }>(
        (resolve, reject) => {
          const req = request(`${h.base}/api/events`, {
            headers: { connection: "Upgrade", upgrade: "custom", origin },
          });
          req.on("response", (res) => {
            res.resume();
            resolve({ status: res.statusCode ?? 0, headers: res.headers });
          });
          req.on("upgrade", () => reject(new Error("不该升级")));
          req.on("error", reject);
          req.end();
        },
      );
    const inside = await attempt(TRUSTED);
    expect(inside.status).toBe(401);
    expect(inside.headers["access-control-allow-origin"]).toBe(TRUSTED);
    expect(inside.headers["access-control-expose-headers"]).toBe("retry-after");
    expect(inside.headers.vary).toBe("origin");
    const outside = await attempt(OUTSIDER);
    expect(outside.status).toBe(401);
    expect(outside.headers["access-control-allow-origin"]).toBeUndefined();
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

  it("有效令牌按令牌限流 → 429 rate_limited 带 Retry-After；别的令牌各自独立", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.token": { type: "rate", max: 1, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    const fixed = auth();
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: fixed })).status,
    ).toBe(200);
    const limited = await fetch(`${h.base}/echo-request`, { headers: fixed });
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("60");
    expect(((await limited.json()) as { code: string }).code).toBe(
      "rate_limited",
    );
    // 同一个 IP、另一个令牌：不受影响。
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
  });

  it("同一 IP 下多个令牌各自计数，不被 IP 桶挡住", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.ip": { type: "rate", max: 1, windowMs: 60_000 },
          "edge.connect.token": { type: "rate", max: 3, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    const tokens = [auth(), auth()];
    for (const headers of tokens)
      for (let index = 0; index < 3; index += 1)
        expect(
          (await fetch(`${h.base}/echo-request`, { headers })).status,
        ).toBe(200);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: tokens[0] })).status,
    ).toBe(429);
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
      expect(
        (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
      ).toBe(200);
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(429);
    expect(
      (
        await fetch(`${h.base}/echo-request`, {
          headers: auth(relayToken({ sub: "acct_other" })),
        })
      ).status,
    ).toBe(200);
  });

  it("无令牌与无效令牌仍按 IP 严格限流，不挤占有效令牌", async () => {
    const h = await harness(
      {},
      {
        limits: {
          "edge.connect.ip": { type: "rate", max: 2, windowMs: 60_000 },
        },
      },
    );
    await connected(h);
    expect((await fetch(`${h.base}/echo-request`)).status).toBe(401);
    expect(
      (
        await fetch(`${h.base}/echo-request`, {
          headers: { "armadra-relay-token": "garbage" },
        })
      ).status,
    ).toBe(401);
    const limited = await fetch(`${h.base}/echo-request`);
    expect(limited.status).toBe(429);
    expect(limited.headers.get("retry-after")).toBe("60");
    // 带有效令牌的请求照常通过。
    expect(
      (await fetch(`${h.base}/echo-request`, { headers: auth() })).status,
    ).toBe(200);
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

describe("媒体票（cloud-api §12）", () => {
  const issue = (h: Harness, path: string, token = relayToken()) =>
    fetch(`${h.base}/_relay/media-tickets`, {
      method: "POST",
      headers: { ...auth(token), "content-type": "application/json" },
      body: JSON.stringify({ path }),
    });

  it("带令牌签票，不带头按票取到源上那一条路径；Range 原样转过去", async () => {
    const h = await harness();
    await connected(h);
    const issued = await issue(h, "/echo-request?media=1");
    expect(issued.status).toBe(200);
    const body = (await issued.json()) as {
      ticket: string;
      path: string;
      expiresAt: string;
    };
    expect(body.path).toBe(`/_relay/m/${body.ticket}`);
    expect(body.path).not.toContain("echo");
    expect(Date.parse(body.expiresAt)).toBeGreaterThan(Date.now());

    for (let round = 0; round < 2; round += 1) {
      const response = await fetch(`${h.base}${body.path}`, {
        headers: { range: "bytes=0-9" },
      });
      expect(response.status).toBe(200);
      const echoed = (await response.json()) as {
        method: string;
        url: string;
        headers: Record<string, string>;
      };
      expect(echoed.url).toBe("/echo-request?media=1");
      expect(echoed.headers.range).toBe("bytes=0-9");
      expect(echoed.headers["armadra-relay-token"]).toBeUndefined();
    }
    const head = await fetch(`${h.base}${body.path}`, { method: "HEAD" });
    expect(head.status).toBe(200);
  });

  it("没令牌不签；坏路径 400；不认识的票 404；只认 GET 与 HEAD", async () => {
    const h = await harness();
    await connected(h);
    const anonymous = await fetch(`${h.base}/_relay/media-tickets`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ path: "/echo-request" }),
    });
    expect(anonymous.status).toBe(401);
    expect((await issue(h, "/_relay/m/x")).status).toBe(400);
    expect((await issue(h, "relative")).status).toBe(400);
    const unknown = await fetch(`${h.base}/_relay/m/${"A".repeat(43)}`);
    expect(unknown.status).toBe(404);
    expect(((await unknown.json()) as { code: string }).code).toBe("not_found");
    const { path } = (await (await issue(h, "/echo")).json()) as {
      path: string;
    };
    const post = await fetch(`${h.base}${path}`, {
      method: "POST",
      body: MARKER,
    });
    expect(post.status).toBe(400);
  });

  it("票绑源：换个源地址兑不出来；签票令牌被撤销后作废", async () => {
    const h = await harness();
    await connected(h);
    const jti = "media-revoke-jti";
    const token = relayToken({ jti });
    const { path } = (await (
      await issue(h, "/echo-request", token)
    ).json()) as {
      path: string;
    };
    const other = `${h.relay.url}/s/${"f".repeat(32)}${path}`;
    expect((await fetch(other)).status).toBe(404);
    expect((await fetch(`${h.base}${path}`)).status).toBe(200);
    await h.control.tokens.revoke(jti, Date.now() + 60_000);
    expect((await fetch(`${h.base}${path}`)).status).toBe(404);
  });
});

describe("只读中继令牌（ro，协议 1.1）", () => {
  const readOnly = () => relayToken({ ro: true });

  it("边缘不按方法过滤：写方法与 RPC 照常转给源，读写由 core 判定", async () => {
    const h = await harness();
    const core = await connected(h);
    const token = readOnly();
    const before = core.requests;
    const posted = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: { ...auth(token), "content-type": "text/plain" },
      body: MARKER,
    });
    expect(await posted.text()).toBe(MARKER);
    for (const method of ["PUT", "PATCH", "DELETE"]) {
      const echoed = await fetch(`${h.base}/echo-request`, {
        method,
        headers: auth(token),
      });
      expect(((await echoed.json()) as { method: string }).method).toBe(method);
    }
    // core 的 RPC 一律 POST；假 core 不认识这条路径，答 404：说明边缘放行了。
    const rpc = await fetch(`${h.base}/api/rpc/system/hello`, {
      method: "POST",
      headers: { ...auth(token), "content-type": "application/json" },
      body: "{}",
    });
    expect(rpc.status).toBe(404);
    expect(core.requests).toBe(before + 5);
  });

  it("不带 ro 的令牌写方法照常；ro 令牌的 WS 升级与媒体票照常", async () => {
    const h = await harness();
    await connected(h);
    const writable = await fetch(`${h.base}/echo`, {
      method: "POST",
      headers: auth(relayToken({ ro: false })),
      body: MARKER,
    });
    expect(await writable.text()).toBe(MARKER);
    const token = readOnly();
    const ws = new WebSocket(`${h.base.replace("http", "ws")}/api/events`, [
      "armadra-ticket.t1",
      `armadra-relay.${token}`,
    ]);
    cleanups.push(() => ws.terminate());
    await once(ws, "open");
    ws.send("bye");
    expect(((await once(ws, "close")) as [number])[0]).toBe(4000);
    const issued = await fetch(`${h.base}/_relay/media-tickets`, {
      method: "POST",
      headers: { ...auth(token), "content-type": "application/json" },
      body: JSON.stringify({ path: "/echo-request" }),
    });
    expect(issued.status).toBe(200);
  });
});

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
    const core = await connected(h);
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

  it("HTTP、下载与 WebSocket 按 sub × 类别记连接与字节，总数与 relay_bytes_total 相等；不记路径", async () => {
    const record = recording();
    const h = await harness({ traffic: record.traffic });
    await connected(h);
    const alice = relayToken({ sub: "acct:alice" });
    const guest = relayToken({ sub: "guest:0123456789abcdef:aa" });
    for (let index = 0; index < 5; index += 1) {
      const response = await fetch(`${h.base}/echo`, {
        method: "POST",
        headers: auth(alice),
        body: `${MARKER}-${index}`,
      });
      await response.text();
    }
    const big = await fetch(`${h.base}/big?bytes=1000000`, {
      headers: auth(guest),
    });
    expect((await big.arrayBuffer()).byteLength).toBe(1_000_000);
    const ws = new WebSocket(`${h.base.replace("http", "ws")}/api/events`, [
      "armadra-ticket.t1",
      `armadra-relay.${alice}`,
    ]);
    cleanups.push(() => ws.terminate());
    await once(ws, "open");
    let replies = 0;
    ws.on("message", () => (replies += 1));
    for (let index = 0; index < 20; index += 1) ws.send(`${MARKER}-${index}`);
    await waitFor(() => replies === 20);
    ws.close();
    await once(ws, "close");
    await waitFor(() => h.relay.tunnels.counts().streams === 0);
    await h.relay.traffic?.flush();

    const of = (subject: string, kind: string) =>
      record.rows.filter((row) => row.subject === subject && row.kind === kind);
    expect(sum(of("acct:alice", "http"), "connections")).toBe(5);
    expect(sum(of("acct:alice", "ws"), "connections")).toBe(1);
    expect(sum(of("guest:0123456789abcdef:aa", "http"), "connections")).toBe(1);
    expect(
      sum(of("guest:0123456789abcdef:aa", "http"), "bytesToClient"),
    ).toBeGreaterThan(1_000_000);
    for (const row of record.rows)
      expect(row).toMatchObject({
        node: "relay-test",
        sourceId: SOURCE_ID,
        deviceId: "",
      });
    const metered = h.relay.metrics.snapshot().bytes;
    expect(sum(record.rows, "bytesToSource")).toBe(metered.to_source);
    expect(sum(record.rows, "bytesToClient")).toBe(metered.to_client);
    const text = JSON.stringify(record.rows);
    expect(text).not.toContain("/echo");
    expect(text).not.toContain("127.0.0.1");
    expect(text).not.toContain(MARKER);
  });

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
    await (await fetch(`${h.base}/echo-request`, { headers: auth() })).text();
    await waitFor(() => record.rows.length === 1);
    await (await fetch(`${h.base}/echo-request`, { headers: auth() })).text();
    await h.relay.close(0);
    expect(sum(record.rows, "connections")).toBe(2);

    const off = await harness();
    await connected(off);
    await (await fetch(`${off.base}/echo-request`, { headers: auth() })).text();
    expect(off.relay.traffic).toBeUndefined();
  });

  it("开销：开不开统计，往返延迟与下载耗时没有明显差别", async () => {
    const measure = async (h: Harness) => {
      const token = relayToken();
      const latencies: number[] = [];
      for (let index = 0; index < 100; index += 1) {
        const started = performance.now();
        await (
          await fetch(`${h.base}/echo-request`, { headers: auth(token) })
        ).text();
        latencies.push(performance.now() - started);
      }
      latencies.sort((a, b) => a - b);
      const started = performance.now();
      await (
        await fetch(`${h.base}/big?bytes=5000000`, { headers: auth(token) })
      ).arrayBuffer();
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
