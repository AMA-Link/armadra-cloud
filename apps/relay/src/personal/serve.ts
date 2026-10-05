/**
 * personal 模式装配：数据目录 → HTTP。**不碰 PostgreSQL / Redis**（AGENTS.md；
 * `boundaries.test.ts` 扫 import 守住）。账号、签名、源目录、分享与 TLS 在 R2 落地；
 * 现在只有健康检查与就绪检查。
 */
import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { createLogger } from "@armadra/cloud-shared/log";
import { constants } from "node:fs";
import { access, mkdir } from "node:fs/promises";

import { createRelayRouter } from "../http/server.ts";
import { RELAY_VERSION } from "../version.ts";
import {
  isLoopback,
  SUPPORTED_TLS_MODES,
  UsageError,
  type PersonalServeOptions,
} from "./args.ts";

export interface RunningPersonalRelay {
  url: string;
  close(): Promise<void>;
}

export async function startPersonalRelay(
  options: PersonalServeOptions,
): Promise<RunningPersonalRelay> {
  if (!SUPPORTED_TLS_MODES.includes(options.tls)) {
    throw new UsageError(
      `--tls ${options.tls} 尚未实现，当前只支持 --tls plain`,
    );
  }
  const log = createLogger({ name: "relay", level: options.logLevel });
  await mkdir(options.dataDir, { recursive: true, mode: 0o700 });
  if (options.tls === "plain" && !isLoopback(options.listen.host)) {
    log.warn(
      "明文监听非回环地址，只应放在终止 TLS 的反向代理或容器端口映射之后",
      {
        listen: `${options.listen.host}:${options.listen.port}`,
      },
    );
  }
  const router = createRelayRouter({
    mode: "personal",
    nodeId: options.host,
    version: RELAY_VERSION,
    log,
    counts: () => ({ tunnels: 0, streams: 0 }),
    probes: {
      dataDir: () => access(options.dataDir, constants.R_OK | constants.W_OK),
    },
  });
  const server: RunningServer = await listen(options.listen, (req, res) =>
    router.handle(req, res),
  );
  log.info("个人中转已启动", {
    url: server.url,
    host: options.host,
    tls: options.tls,
  });
  return {
    url: server.url,
    async close() {
      await server.close();
      log.info("个人中转已停止");
    },
  };
}
