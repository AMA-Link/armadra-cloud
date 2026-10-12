/**
 * 中继的 Node 装配（中继设计 §1）：内核（控制面 → 令牌校验 → 指标 → 隧道表，
 * `@armadra/relay-kernel/main`）→ `ws` 隧道终端 + 边缘 → HTTP 服务器（+ 可选的指标监听）。
 * 两种模式都走这里，差别只在调用方给的 `control`、`addressing` 与 `extra` / `extraUpgrade`
 * （personal 的 `/v1`、`/v1/me/stream`、托管页面）。TLS（自签 CA / ACME / 文件证书）经
 * `tls` 监听 HTTPS，`setTls` 热换证书。
 */
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import type { ListenAddress } from "@armadra/cloud-shared/config";
import type { Probe } from "@armadra/cloud-shared/health";
import {
  listen,
  type RunningServer,
  type TlsPair,
} from "@armadra/cloud-shared/http";
import type { Logger } from "@armadra/cloud-shared/log";
import { LIMITS } from "@armadra/platform-protocol/tunnel";
import type { ControlPlane } from "@armadra/relay-kernel/control/types";
import type { Addressing } from "@armadra/relay-kernel/edge/addressing";
import { errorName } from "@armadra/relay-kernel/log";
import { createRelayCore } from "@armadra/relay-kernel/main";
import type { RelayMetrics } from "@armadra/relay-kernel/metrics";
import {
  TRAFFIC_FLUSH_MS,
  TrafficMeter,
  type TrafficWriter,
} from "@armadra/relay-kernel/traffic";
import type { TunnelLimitsConfig } from "@armadra/relay-kernel/tunnel/tunnel";

import type { ClientIp } from "./client-ip.ts";
import {
  createMetricsHandler,
  createRelayRouter,
  dispatchUpgrade,
} from "./dispatch.ts";
import { Edge, type EdgeConnectionFilter } from "./edge.ts";
import { nodeStreams } from "./stream.ts";
import { TunnelServer } from "./ws-server.ts";

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
  /** 模式自己的 WS 升级（personal `/v1/me/stream`）：认领了返回 true。 */
  extraUpgrade?: (
    req: IncomingMessage,
    socket: Duplex,
    head: Buffer,
  ) => boolean;
  /** 源地址上错误答复回显 CORS 的来源（见 `edge/cors.ts`）；原生 App 的来源总是回显。 */
  corsOrigins?: (sourceId: string) => readonly string[];
  /** 客户端 IP 的取法；缺省直连对端地址（不信任任何代理）。 */
  clientIp?: ClientIp;
  /** 给了就监听 HTTPS（personal 的自签 CA / ACME / 文件证书）。 */
  tls?: TlsPair;
  now?: () => number;
  /**
   * 流量统计（cloud-api §19）：在内存里累加，每 `flushMs`（缺省 60 秒）与关闭时把攒下的行交给
   * `write`（存储的批量 upsert）。不给就不统计。
   */
  traffic?: {
    write: TrafficWriter;
    flushMs?: number;
    /** 写失败（`dropped` 是因积压超限丢掉的行数）。 */
    onError?: (error: unknown, dropped: number) => void;
  };
}

export interface RunningRelay {
  url: string;
  metricsUrl?: string;
  tunnels: TunnelServer;
  metrics: RelayMetrics;
  /** 流量统计的累加器（没开时为 undefined）；`flush()` 立即落库。 */
  traffic?: TrafficMeter;
  /** 换证书（叶证书续期、文件热加载）；明文监听上是空操作。 */
  setTls(tls: TlsPair): void;
  /** 断开匹配的客户端 WS（撤销源、撤销访客时用）；HTTP 请求逐个验令牌，不需要断。 */
  disconnect(filter: EdgeConnectionFilter): number;
  close(graceMs?: number): Promise<void>;
}

export async function startRelay(options: RelayOptions): Promise<RunningRelay> {
  const { log, auth, metrics, tunnels } = createRelayCore({
    nodeId: options.nodeId,
    control: options.control,
    log: options.log,
    limits: options.limits,
    handshakeTimeoutMs: options.handshakeTimeoutMs,
    routeTtlMs: options.routeTtlMs,
    streams: nodeStreams,
    now: options.now,
  });
  const counts = (): { tunnels: number; streams: number } => tunnels.counts();
  const traffic = options.traffic
    ? new TrafficMeter({
        node: options.nodeId,
        write: options.traffic.write,
        onError: options.traffic.onError,
        ...(options.now ? { now: options.now } : {}),
      })
    : undefined;
  const tunnelServer = new TunnelServer(tunnels);
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
    corsOrigins: options.corsOrigins,
    clientIp: options.clientIp,
    traffic,
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
  const server: RunningServer = await listen(
    options.listen,
    (req, res) => {
      if (!addressed(req)) return router.handle(req, res);
      edge.handleRequest(req, res);
    },
    options.tls ? { tls: options.tls } : {},
  );
  server.server.on(
    "upgrade",
    dispatchUpgrade({
      addressed,
      extra: options.extraUpgrade,
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
  const trafficTimer = traffic
    ? setInterval(
        () => void traffic.flush(),
        options.traffic?.flushMs ?? TRAFFIC_FLUSH_MS,
      )
    : undefined;
  trafficTimer?.unref();
  let closing: Promise<void> | undefined;
  return {
    url: server.url,
    metricsUrl: metricsServer?.url,
    tunnels: tunnelServer,
    metrics,
    ...(traffic ? { traffic } : {}),
    setTls(tls) {
      server.setTls?.(tls);
    },
    disconnect: (filter) => edge.disconnect(filter),
    close(graceMs = options.shutdownGraceMs ?? LIMITS.goawayGraceMs) {
      closing ??= (async () => {
        await tunnelServer.shutdown(graceMs);
        edge.closeAll();
        await server.close();
        await metricsServer?.close();
        clearInterval(trafficTimer);
        await traffic?.flush();
      })();
      return closing;
    },
  };
}
