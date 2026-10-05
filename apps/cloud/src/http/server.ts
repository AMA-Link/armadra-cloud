/**
 * 控制面 HTTP 面。现在只有存活与就绪两个端点是真的；`/v1/*` 与 `/.well-known/*`
 * 的业务路由在 `reserved.ts` 里预留，统一答 501 `not_implemented`（总计划 §12.2），
 * 落地时经 `http/rpc.ts`（oRPC 门面）挂上来。
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

import { answerReserved, createReservedMatcher } from "./reserved.ts";

export interface CloudHttpOptions {
  version: string;
  probes: Record<string, Probe>;
  log: Logger;
}

export function createCloudRouter(options: CloudHttpOptions): Router {
  const headers = { "armadra-protocol": PROTOCOL_HEADER_VALUE };
  const reserved = createReservedMatcher();
  return createRouter({
    onError: (error) =>
      options.log.error("请求处理失败", { error: (error as Error).name }),
    fallback: (req, res, context) =>
      answerReserved(reserved, req, res, context),
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
