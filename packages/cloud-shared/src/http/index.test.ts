import { request, Agent } from "node:http";
import type { Socket } from "node:net";

import { afterEach, expect, it } from "vitest";

import {
  createRouter,
  HTTP_TIMEOUTS,
  listen,
  sendJson,
  type RunningServer,
} from "./index.ts";

let running: RunningServer | undefined;
afterEach(async () => {
  await running?.close();
  running = undefined;
});

it("精确路径分派，HEAD 走 GET，未命中答 404 not_found", async () => {
  const router = createRouter().get("/health", (_req, res) =>
    sendJson(res, 200, { ok: true }),
  );
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) =>
    router.handle(req, res),
  );

  const ok = await fetch(`${running.url}/health`);
  expect(ok.status).toBe(200);
  expect(ok.headers.get("cache-control")).toBe("no-store");
  expect(await ok.json()).toEqual({ ok: true });

  expect(
    (await fetch(`${running.url}/health`, { method: "HEAD" })).status,
  ).toBe(200);

  const missing = await fetch(`${running.url}/nope`);
  expect(missing.status).toBe(404);
  const body = (await missing.json()) as { code: string; requestId: string };
  expect(body.code).toBe("not_found");
  expect(body.requestId).toMatch(/^[0-9a-f-]{36}$/);
});

it("处理器抛错答 500 internal，不回显错误原文", async () => {
  const errors: unknown[] = [];
  const router = createRouter({ onError: (error) => errors.push(error) }).get(
    "/boom",
    () => {
      throw new Error("secret detail");
    },
  );
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) =>
    router.handle(req, res),
  );
  const response = await fetch(`${running.url}/boom`);
  expect(response.status).toBe(500);
  const text = await response.text();
  expect(text).toContain('"code":"internal"');
  expect(text).not.toContain("secret detail");
  expect(errors).toHaveLength(1);
});

it("监听的空闲 keep-alive 75 秒、请求头超时略大于它、整条请求 5 分钟", async () => {
  running = await listen({ host: "127.0.0.1", port: 0 }, (_req, res) =>
    sendJson(res, 200, {}),
  );
  const { server } = running;
  expect(server.keepAliveTimeout).toBe(75_000);
  expect(server.headersTimeout).toBe(76_000);
  expect(server.requestTimeout).toBe(300_000);
  expect(HTTP_TIMEOUTS.headersTimeoutMs).toBeGreaterThan(
    HTTP_TIMEOUTS.keepAliveTimeoutMs,
  );
  expect(HTTP_TIMEOUTS.requestTimeoutMs).toBeGreaterThanOrEqual(
    HTTP_TIMEOUTS.headersTimeoutMs,
  );
});

it("空闲 6 秒后同一条 keep-alive 连接还在，POST 照样成功（Node 缺省 5 秒会先关）", async () => {
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) => {
    req.resume();
    req.on("end", () => sendJson(res, 200, { method: req.method }));
  });
  const agent = new Agent({ keepAlive: true, maxSockets: 1 });
  const sockets = new Set<Socket>();
  let closedByServer = false;
  const send = (method: string) =>
    new Promise<number>((resolve, reject) => {
      const req = request(
        `${running!.url}/`,
        { method, agent, headers: { "content-type": "application/json" } },
        (res) => {
          res.resume();
          res.on("end", () => resolve(res.statusCode ?? 0));
        },
      );
      req.on("socket", (socket: Socket) => {
        if (sockets.has(socket)) return;
        sockets.add(socket);
        socket.once("close", () => (closedByServer = true));
      });
      req.on("error", reject);
      req.end(method === "POST" ? "{}" : undefined);
    });
  try {
    expect(await send("GET")).toBe(200);
    await new Promise((resolve) => setTimeout(resolve, 6_000));
    expect(closedByServer).toBe(false);
    expect(await send("POST")).toBe(200);
    expect(sockets.size).toBe(1);
  } finally {
    agent.destroy();
  }
}, 15_000);
