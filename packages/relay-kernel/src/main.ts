/**
 * 中继内核装配（中继设计 §1）中与运行时无关的一段：控制面 → 令牌校验 → 指标 → 隧道表。
 * 运行时在外面接监听、WebSocket 与边缘的字节进出（Workers：`@armadra/relay-runtime-workers`）。
 * 两种模式都走这里，差别只在调用方给的 `control`。
 */
import type { Logger } from "@armadra/cloud-shared/log";
import { LIMITS } from "@armadra/platform-protocol/tunnel";

import { createAuthContext, type AuthContext } from "./auth/context.ts";
import type { ControlPlane } from "./control/types.ts";
import { relayLog, type RelayLog } from "./log.ts";
import { RelayMetrics } from "./metrics.ts";
import { TunnelHub } from "./tunnel/hub.ts";
import type { StreamEndpoint, StreamFactory } from "./tunnel/stream.ts";
import type { TunnelLimitsConfig } from "./tunnel/tunnel.ts";

export const DEFAULT_TUNNEL_LIMITS: TunnelLimitsConfig = {
  maxStreams: LIMITS.maxStreams,
  streamWindow: LIMITS.streamWindow,
  tunnelWindow: LIMITS.tunnelWindow,
  maxDataChunk: LIMITS.maxDataChunk,
  heartbeatMs: LIMITS.heartbeatMs,
  heartbeatMisses: LIMITS.heartbeatMisses,
  goawayGraceMs: LIMITS.goawayGraceMs,
};

export interface RelayCoreOptions<S extends StreamEndpoint> {
  nodeId: string;
  control: ControlPlane;
  log: Logger;
  /** 覆盖协议包 LIMITS（测试用小窗口、短心跳）。 */
  limits?: Partial<TunnelLimitsConfig>;
  handshakeTimeoutMs?: number;
  routeTtlMs?: number;
  /** 每条隧道流的运行时适配。 */
  streams: StreamFactory<S>;
  now?: () => number;
}

export interface RelayCore<S extends StreamEndpoint> {
  /** 只写白名单字段的中继日志。 */
  log: RelayLog;
  auth: AuthContext;
  metrics: RelayMetrics;
  tunnels: TunnelHub<S>;
}

export function createRelayCore<S extends StreamEndpoint>(
  options: RelayCoreOptions<S>,
): RelayCore<S> {
  const log = relayLog(options.log);
  const auth = createAuthContext(options.control, options.now);
  // 指标的读数回调到隧道表（闭包里引用，调用时已建好）。
  const metrics: RelayMetrics = new RelayMetrics(() => tunnels.counts());
  const tunnels: TunnelHub<S> = new TunnelHub({
    control: options.control,
    auth,
    nodeId: options.nodeId,
    limits: { ...DEFAULT_TUNNEL_LIMITS, ...options.limits },
    handshakeTimeoutMs: options.handshakeTimeoutMs ?? LIMITS.handshakeTimeoutMs,
    routeTtlMs: options.routeTtlMs,
    log,
    metrics,
    streams: options.streams,
  });
  return { log, auth, metrics, tunnels };
}
