/** Redis 客户端（node-redis 5）。断线由客户端自己重连；探活用 PING。 */
import { createClient } from "redis";

export type Redis = ReturnType<typeof createClient>;

export function createRedis(url: string): Redis {
  const client = createClient({
    url,
    socket: {
      connectTimeout: 5_000,
      reconnectStrategy: (retries) => Math.min(retries * 200, 5_000),
    },
  });
  // 没有监听器时 node-redis 会把连接错误当未处理异常抛出；探活负责报告不可达。
  client.on("error", () => {});
  return client;
}

export async function pingRedis(client: Redis): Promise<void> {
  const reply = await client.ping();
  if (reply !== "PONG") throw new Error("unexpected PING reply");
}
