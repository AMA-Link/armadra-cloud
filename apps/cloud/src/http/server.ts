/**
 * 控制面 HTTP 面。C0-1 只有存活与就绪两个端点；`/v1/*` 与 `/.well-known/*` 由
 * C2-1 经 `http/rpc.ts`（oRPC 门面）挂上来。
 *
 * - `GET /health`：进程存活，`{ ok: true, version, mode }`（契约 `platform.health`）。
 * - `GET /ready`：依赖就绪，逐项报 PostgreSQL 与 Redis，任一失败答 503。
 */
import { runProbes, type Probe } from "@armadra/cloud-shared/health";
import {
  createRouter,
  PROTOCOL_HEADER_VALUE,
  sendJson,
  type Router,
} from "@armadra/cloud-shared/http";
import type { Logger } from "@armadra/cloud-shared/log";

export interface CloudHttpOptions {
  version: string;
  probes: Record<string, Probe>;
  log: Logger;
}

export function createCloudRouter(options: CloudHttpOptions): Router {
  const headers = { "armadra-protocol": PROTOCOL_HEADER_VALUE };
  return createRouter({
    onError: (error) =>
      options.log.error("请求处理失败", { error: (error as Error).name }),
  })
    .get("/health", (_req, res) => {
      sendJson(
        res,
        200,
        { ok: true, version: options.version, mode: "saas" },
        headers,
      );
    })
    .get("/ready", async (_req, res) => {
      const result = await runProbes(options.probes);
      sendJson(res, result.ok ? 200 : 503, result, headers);
    });
}
