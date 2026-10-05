/**
 * 中继 HTTP 面。C0-1 只有存活与就绪；`/t/v1`、边缘、`/metrics` 在 R1 挂上来。
 *
 * - `GET /health`：`{ ok, mode, node, tunnels, streams }`（中继设计 §4）。
 * - `GET /ready`：saas 逐项报 Redis，失败答 503。
 */
import { runProbes, type Probe } from "@armadra/cloud-shared/health";
import {
  createRouter,
  PROTOCOL_HEADER_VALUE,
  sendJson,
  type Router,
} from "@armadra/cloud-shared/http";
import type { Logger } from "@armadra/cloud-shared/log";

export interface RelayHttpOptions {
  mode: "saas" | "personal";
  nodeId: string;
  version: string;
  probes: Record<string, Probe>;
  counts: () => { tunnels: number; streams: number };
  log: Logger;
}

export function createRelayRouter(options: RelayHttpOptions): Router {
  const headers = { "armadra-protocol": PROTOCOL_HEADER_VALUE };
  return createRouter({
    onError: (error) =>
      options.log.error("请求处理失败", { error: (error as Error).name }),
  })
    .get("/health", (_req, res) => {
      sendJson(
        res,
        200,
        {
          ok: true,
          mode: options.mode,
          node: options.nodeId,
          version: options.version,
          ...options.counts(),
        },
        headers,
      );
    })
    .get("/ready", async (_req, res) => {
      const result = await runProbes(options.probes);
      sendJson(res, result.ok ? 200 : 503, result, headers);
    });
}
