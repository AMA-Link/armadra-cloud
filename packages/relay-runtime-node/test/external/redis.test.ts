/** `--control redis`：两个节点（各自一个客户端，同一个随机前缀）经键与 pub/sub 同步。 */
import { randomBytes } from "node:crypto";

import { createRedis } from "@armadra/cloud-shared/redis";
import { afterAll, afterEach } from "vitest";

import { createRedisControlPlane } from "../../src/control/redis.ts";
import { clusterContract, LIMITED } from "./cluster.ts";

const url = process.env.REDIS_URL;
if (!url) throw new Error("REDIS_URL 未设置");
const prefix = `relay-test:${randomBytes(6).toString("hex")}:`;
const cleanups: (() => Promise<void>)[] = [];
let round = 0;

afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

afterAll(async () => {
  // 只删本文件前缀下的键。
  const client = createRedis(url);
  await client.connect();
  try {
    for await (const batch of client.scanIterator({ MATCH: `${prefix}*` }))
      if (batch.length > 0) await client.del(batch);
  } finally {
    client.destroy();
  }
});

async function node(nodeId: string, keyPrefix: string) {
  const redis = createRedis(url as string);
  await redis.connect();
  const control = await createRedisControlPlane({
    redis,
    prefix: keyPrefix,
    issuer: "https://relay.example.test",
    jwks: () => ({ keys: [] }),
    nodeId,
    limits: LIMITED,
  });
  cleanups.push(async () => {
    await control.close();
    redis.destroy();
  });
  return control;
}

clusterContract("Redis 控制面", async () => {
  round += 1;
  const keyPrefix = `${prefix}${round}:`;
  const [a, b] = await Promise.all([
    node("node-a", keyPrefix),
    node("node-b", keyPrefix),
  ]);
  return { a, b };
});
