import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { afterEach, expect, it } from "vitest";

import { createCloudRouter } from "./server.ts";

let running: RunningServer | undefined;
afterEach(async () => {
  await running?.close();
  running = undefined;
});

async function start(probes: Record<string, () => Promise<unknown>>) {
  const router = createCloudRouter({
    version: "0.1.0",
    probes,
    log: createLogger({ name: "test", write: () => {} }),
  });
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) =>
    router.handle(req, res),
  );
  return running.url;
}

it("/health 答 200 与协议头，不依赖数据库", async () => {
  const url = await start({
    postgres: () => Promise.reject(new Error("down")),
  });
  const response = await fetch(`${url}/health`);
  expect(response.status).toBe(200);
  expect(response.headers.get("armadra-protocol")).toBe("1.4");
  expect(await response.json()).toEqual({
    ok: true,
    version: "0.1.0",
    mode: "saas",
  });
});

it("/ready 在依赖不可达时答 503 并逐项报告", async () => {
  const url = await start({
    postgres: () => Promise.resolve(),
    redis: () => Promise.reject(new Error("down")),
  });
  const response = await fetch(`${url}/ready`);
  expect(response.status).toBe(503);
  const body = (await response.json()) as {
    ok: boolean;
    checks: Record<string, { ok: boolean }>;
  };
  expect(body.ok).toBe(false);
  expect(body.checks.postgres?.ok).toBe(true);
  expect(body.checks.redis?.ok).toBe(false);
});

it("/ready 依赖全部可达时答 200", async () => {
  const url = await start({
    postgres: () => Promise.resolve(),
    redis: () => Promise.resolve(),
  });
  expect((await fetch(`${url}/ready`)).status).toBe(200);
});
