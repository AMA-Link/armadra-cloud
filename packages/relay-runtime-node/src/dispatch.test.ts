import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { afterEach, expect, it } from "vitest";

import { createRelayRouter } from "./dispatch.ts";

let running: RunningServer | undefined;
afterEach(async () => {
  await running?.close();
  running = undefined;
});

async function start(probe: () => Promise<unknown>) {
  const router = createRelayRouter({
    mode: "saas",
    nodeId: "node-a",
    version: "0.1.0",
    probes: { redis: probe },
    counts: () => ({ tunnels: 2, streams: 5 }),
    log: createLogger({ name: "test", write: () => {} }),
  });
  running = await listen({ host: "127.0.0.1", port: 0 }, (req, res) =>
    router.handle(req, res),
  );
  return running.url;
}

it("/health 报模式、节点与计数", async () => {
  const url = await start(() => Promise.reject(new Error("down")));
  const response = await fetch(`${url}/health`);
  expect(response.status).toBe(200);
  expect(await response.json()).toEqual({
    ok: true,
    mode: "saas",
    node: "node-a",
    version: "0.1.0",
    tunnels: 2,
    streams: 5,
  });
});

it("/ready 跟随 Redis", async () => {
  expect(
    (await fetch(`${await start(() => Promise.reject(new Error("x")))}/ready`))
      .status,
  ).toBe(503);
  await running?.close();
  running = undefined;
  expect(
    (await fetch(`${await start(() => Promise.resolve())}/ready`)).status,
  ).toBe(200);
});
