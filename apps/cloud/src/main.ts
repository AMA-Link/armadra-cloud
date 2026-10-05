/** 装配：配置 → PostgreSQL / Redis 客户端 → HTTP。依赖不可达不阻止启动，只让 /ready 答 503。 */
import { createDb, pingDb } from "@armadra/cloud-shared/db";
import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { createRedis, pingRedis } from "@armadra/cloud-shared/redis";

import type { CloudConfig } from "./config.ts";
import { createCloudRouter } from "./http/server.ts";
import { CLOUD_VERSION } from "./version.ts";

export interface RunningCloud {
  url: string;
  close(): Promise<void>;
}

export async function startCloud(config: CloudConfig): Promise<RunningCloud> {
  const log = createLogger({ name: "cloud", level: config.logLevel });
  const sql = createDb(config.databaseUrl);
  const redis = createRedis(config.redisUrl);
  redis.connect().catch(() => {
    log.warn("Redis 暂不可达，后台重连");
  });

  const router = createCloudRouter({
    version: CLOUD_VERSION,
    log,
    probes: {
      postgres: () => pingDb(sql),
      redis: async () => {
        if (!redis.isReady) throw new Error("not ready");
        await pingRedis(redis);
      },
    },
  });
  let server: RunningServer;
  try {
    server = await listen(config.listen, (req, res) => router.handle(req, res));
  } catch (error) {
    redis.destroy();
    await sql.end({ timeout: 1 });
    throw error;
  }
  log.info("控制面已启动", { url: server.url, version: CLOUD_VERSION });
  return {
    url: server.url,
    async close() {
      await server.close();
      redis.destroy();
      await sql.end({ timeout: 5 });
      log.info("控制面已停止");
    },
  };
}
