/** saas 模式装配：配置 → Redis → HTTP。saas 控制面适配（Redis 路由、限流、撤销）预留到 R3（总计划 §12.2）。中继不碰数据库。 */
import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { createRedis, pingRedis } from "@armadra/cloud-shared/redis";

import type { SaasRelayConfig } from "../config.ts";
import { createRelayRouter } from "../http/server.ts";
import { RELAY_VERSION } from "../version.ts";

export interface RunningRelay {
  url: string;
  close(): Promise<void>;
}

export async function startSaasRelay(
  config: SaasRelayConfig,
): Promise<RunningRelay> {
  const log = createLogger({ name: "relay", level: config.logLevel });
  const redis = createRedis(config.redisUrl);
  redis.connect().catch(() => {
    log.warn("Redis 暂不可达，后台重连");
  });
  const router = createRelayRouter({
    mode: "saas",
    nodeId: config.nodeId,
    version: RELAY_VERSION,
    log,
    counts: () => ({ tunnels: 0, streams: 0 }),
    probes: {
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
    throw error;
  }
  log.info("中继已启动", {
    url: server.url,
    mode: "saas",
    node: config.nodeId,
  });
  return {
    url: server.url,
    async close() {
      await server.close();
      redis.destroy();
      log.info("中继已停止");
    },
  };
}
