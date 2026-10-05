import { afterEach, expect, it } from "vitest";

import { createRouter, listen, sendJson, type RunningServer } from "./index.ts";

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
