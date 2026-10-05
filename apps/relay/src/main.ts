/**
 * 中继内核装配（中继设计 §1）：控制面 → 令牌校验 → 隧道终端 + 边缘 → HTTP 服务器
 * （+ 可选的指标监听）。两种模式都走这里，差别只在调用方给的 `control`、`addressing`
 * 与 `extra`（personal 的 `/v1`、托管页面在 R2 挂进来）。
 */
import type { IncomingMessage, ServerResponse } from "node:http";

import type { ListenAddress } from "@armadra/cloud-shared/config";
import type { Probe } from "@armadra/cloud-shared/health";
import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import type { Logger } from "@armadra/cloud-shared/log";
import { LIMITS } from "@armadra/platform-protocol/tunnel";

import { createAuthContext } from "./auth/context.ts";
import type { ControlPlane } from "./control/types.ts";
import type { Addressing } from "./edge/addressing.ts";
import { Edge } from "./edge/server.ts";
import { sendRelayError } from "./http/errors.ts";
import {
  createMetricsHandler,
  createRelayRouter,
  dispatchUpgrade,
} from "./http/server.ts";
import { errorName, relayLog } from "./log.ts";
import { RelayMetrics } from "./metrics.ts";
import { TunnelServer } from "./tunnel/server.ts";
import type { TunnelLimitsConfig } from "./tunnel/tunnel.ts";

export interface RelayOptions {
  mode: "saas" | "personal";
  nodeId: string;
  version: string;
  listen: ListenAddress;
  control: ControlPlane;
  addressing: Addressing;
  log: Logger;
  probes?: Record<string, Probe>;
  /** 指标单独监听（`RELAY_METRICS_LISTEN`）；不给就不开。 */
  metricsListen?: ListenAddress;
  metricsToken?: string;
  /** 覆盖协议包 LIMITS（测试用小窗口、短心跳）。 */
  limits?: Partial<TunnelLimitsConfig>;
  handshakeTimeoutMs?: number;
  routeTtlMs?: number;
  wsPingMs?: number;
  wsIdleMs?: number;
  /** 关闭时给隧道的排空时间（缺省 LIMITS.goawayGraceMs）。 */
  shutdownGraceMs?: number;
  /** 模式自己的路由（personal `/v1` 等）：认领了返回 true。 */
  extra?: (req: IncomingMessage, res: ServerResponse) => boolean;
  now?: () => number;
}

export interface RunningRelay {
  url: string;
  metricsUrl?: string;
  tunnels: TunnelServer;
  metrics: RelayMetrics;
  close(graceMs?: number): Promise<void>;
}

export const DEFAULT_TUNNEL_LIMITS: TunnelLimitsConfig = {
  maxStreams: LIMITS.maxStreams,
  streamWindow: LIMITS.streamWindow,
  tunnelWindow: LIMITS.tunnelWindow,
  maxDataChunk: LIMITS.maxDataChunk,
  heartbeatMs: LIMITS.heartbeatMs,
  heartbeatMisses: LIMITS.heartbeatMisses,
  goawayGraceMs: LIMITS.goawayGraceMs,
};

export async function startRelay(options: RelayOptions): Promise<RunningRelay> {
  const log = relayLog(options.log);
  const auth = createAuthContext(options.control, options.now);
  // 指标的读数回调到隧道表（闭包里引用，调用时已建好）。
  const counts = (): { tunnels: number; streams: number } =>
    tunnelServer.counts();
  const metrics: RelayMetrics = new RelayMetrics(counts);
  const tunnelServer: TunnelServer = new TunnelServer({
    control: options.control,
    auth,
    nodeId: options.nodeId,
    limits: { ...DEFAULT_TUNNEL_LIMITS, ...options.limits },
    handshakeTimeoutMs: options.handshakeTimeoutMs ?? LIMITS.handshakeTimeoutMs,
    routeTtlMs: options.routeTtlMs,
    log,
    metrics,
  });
  const edge = new Edge({
    nodeId: options.nodeId,
    addressing: options.addressing,
    auth,
    control: options.control,
    lookup: (sourceId) => tunnelServer.lookup(sourceId),
    log,
    metrics,
    wsPingMs: options.wsPingMs,
    wsIdleMs: options.wsIdleMs,
  });
  const router = createRelayRouter({
    mode: options.mode,
    nodeId: options.nodeId,
    version: options.version,
    probes: options.probes ?? {},
    counts,
    log: options.log,
    fallback: options.extra,
  });
  const addressed = (req: IncomingMessage) =>
    options.addressing.resolve({
      url: req.url ?? "/",
      host: req.headers.host,
    }) !== null;
  const server: RunningServer = await listen(options.listen, (req, res) => {
    if (!addressed(req)) return router.handle(req, res);
    return edge.handleRequest(req, res).catch((error: unknown) => {
      log.error("边缘请求异常", { error: errorName(error) });
      if (!res.headersSent) sendRelayError(res, "internal");
      else res.destroy();
    });
  });
  server.server.on(
    "upgrade",
    dispatchUpgrade({
      addressed,
      tunnel: (req, socket, head) =>
        tunnelServer.handleUpgrade(req, socket, head),
      edge: (req, socket, head) => {
        edge.handleUpgrade(req, socket, head).catch((error: unknown) => {
          log.error("边缘升级异常", { error: errorName(error) });
          socket.destroy();
        });
      },
    }),
  );
  let metricsServer: RunningServer | undefined;
  if (options.metricsListen) {
    try {
      metricsServer = await listen(
        options.metricsListen,
        createMetricsHandler(() => metrics.render(), options.metricsToken),
      );
    } catch (error) {
      await server.close();
      throw error;
    }
  }
  let closing: Promise<void> | undefined;
  return {
    url: server.url,
    metricsUrl: metricsServer?.url,
    tunnels: tunnelServer,
    metrics,
    close(graceMs = options.shutdownGraceMs ?? LIMITS.goawayGraceMs) {
      closing ??= (async () => {
        await tunnelServer.shutdown(graceMs);
        edge.closeAll();
        await server.close();
        await metricsServer?.close();
      })();
      return closing;
    },
  };
}
