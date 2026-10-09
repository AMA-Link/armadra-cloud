/**
 * 中继 HTTP 面：按路径分派（中继设计 §1）。
 *
 * - `GET /health`：`{ ok, mode, node, version, tunnels, streams }`（§4）。
 * - `GET /ready`：逐项探针（saas 报 Redis），失败答 503。
 * - 指向某个源的请求与升级（saas 的 `<id>.src.<域>`、personal 的 `/s/<id>/…`）先于一切
 *   交给边缘——源自己也有 `/health`，不能被中继的同名端点截走。
 * - 升级 `/t/v1` → 隧道终端；模式自己的升级（personal `/v1/me/stream`）→ extra；其余升级
 *   → 边缘（答 404）。
 * - 其余请求：精确路由 → `extra`（personal 的 `/v1`、托管页面等）→ 404。
 * - `/metrics` 不在这里：只在单独的 `RELAY_METRICS_LISTEN` 上（{@link createMetricsHandler}）。
 */
import { createHash, timingSafeEqual } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import { runProbes, type Probe } from "@armadra/cloud-shared/health";
import {
  createRouter,
  PROTOCOL_HEADER_VALUE,
  sendJson,
  type Router,
} from "@armadra/cloud-shared/http";
import type { Logger } from "@armadra/cloud-shared/log";
import { TUNNEL_PATH } from "@armadra/platform-protocol";

import { sendRelayError } from "./errors.ts";

export interface RelayHttpOptions {
  mode: "saas" | "personal";
  nodeId: string;
  version: string;
  probes: Record<string, Probe>;
  counts: () => { tunnels: number; streams: number };
  log: Logger;
  /** 精确路由都没命中时调用（认领了返回 true），否则 404。 */
  fallback?: (req: IncomingMessage, res: ServerResponse) => boolean;
}

export function createRelayRouter(options: RelayHttpOptions): Router {
  const headers = { "armadra-protocol": PROTOCOL_HEADER_VALUE };
  return createRouter({
    onError: (error) =>
      options.log.error("请求处理失败", { error: (error as Error).name }),
    fallback: options.fallback
      ? (req, res) => options.fallback?.(req, res) ?? false
      : undefined,
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

export interface UpgradeTargets {
  /** 这个请求指向某个源（交给边缘）。 */
  addressed(req: IncomingMessage): boolean;
  /** 模式自己的升级（personal `/v1/me/stream`）：认领了返回 true。 */
  extra?: (req: IncomingMessage, socket: Duplex, head: Buffer) => boolean;
  tunnel(req: IncomingMessage, socket: Duplex, head: Buffer): void;
  edge(req: IncomingMessage, socket: Duplex, head: Buffer): void;
}

/** `upgrade` 事件分派：指向源 → 边缘；`/t/v1` → 隧道；模式自己的 → extra；其余 → 边缘（404）。 */
export function dispatchUpgrade(targets: UpgradeTargets) {
  return (req: IncomingMessage, socket: Duplex, head: Buffer): void => {
    if (targets.addressed(req)) {
      targets.edge(req, socket, head);
      return;
    }
    const path = (req.url ?? "/").split("?")[0];
    if (path === TUNNEL_PATH) targets.tunnel(req, socket, head);
    else if (!targets.extra?.(req, socket, head))
      targets.edge(req, socket, head);
  };
}

/** `/metrics`：文本格式；配置了令牌时要 `Authorization: Bearer <令牌>`。 */
export function createMetricsHandler(
  render: () => string,
  token: string | undefined,
) {
  return (req: IncomingMessage, res: ServerResponse): void => {
    const path = (req.url ?? "/").split("?")[0];
    if (req.method !== "GET" || path !== "/metrics") {
      sendRelayError(res, "not_found");
      return;
    }
    if (token && !sameSecret(req.headers.authorization, `Bearer ${token}`)) {
      sendRelayError(res, "unauthenticated");
      return;
    }
    const body = render();
    res.writeHead(200, {
      "content-type": "text/plain; version=0.0.4; charset=utf-8",
      "content-length": Buffer.byteLength(body),
      "cache-control": "no-store",
    });
    res.end(body);
  };
}

function sameSecret(given: string | undefined, expected: string): boolean {
  const digest = (value: string) => createHash("sha256").update(value).digest();
  return timingSafeEqual(digest(given ?? ""), digest(expected));
}
