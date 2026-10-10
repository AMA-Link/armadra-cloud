/**
 * 隧道终端 `/t/v1` 的运行时无关部分（中继设计 §3.1）：握手 → {@link Tunnel}；本节点的
 * 隧道表（每源一条，新的替换旧的）；路由与在线事件写进 `ControlPlane`。WebSocket 本身
 * 由运行时终止（Node 用 `ws`），这里只看 {@link TunnelSocket}。
 *
 * 替换：同一源在本节点已有隧道 → 旧的发 `GOAWAY replaced`，`goawayGraceMs` 后关 4491；
 * 路由 `set` 覆盖。关闭：只有「当前」隧道（没被替换）才删路由（CAS）并发 `sourceOffline`。
 */
import { randomBytes } from "node:crypto";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import { CLOSE, LIMITS, type Ready } from "@armadra/platform-protocol/tunnel";

import type { AuthContext } from "../auth/context.ts";
import { ROUTE_TTL_MS, type ControlPlane } from "../control/types.ts";
import { errorName, type RelayLog } from "../log.ts";
import type { RelayMetrics } from "../metrics.ts";
import { Handshake, type HandshakeStep } from "./handshake.ts";
import type { StreamEndpoint, StreamFactory } from "./stream.ts";
import { Tunnel, type TunnelLimitsConfig } from "./tunnel.ts";

/** 运行时交给内核的一条隧道 WebSocket（已完成升级）。 */
export interface TunnelSocket {
  sendText(text: string): void;
  sendBinary(bytes: Uint8Array): void;
  /** 正常关闭（发关闭帧）。 */
  close(code: number, reason: string): void;
  /** 立即断开。 */
  terminate(): void;
  /** 还能收发（WebSocket `OPEN`）。 */
  readonly isOpen: boolean;
}

/** {@link TunnelHub.accept} 的回执：运行时把这条 WS 上的事件交回来。 */
export interface TunnelConnection {
  message(data: Uint8Array, isBinary: boolean): void;
  closed(code: number): void;
}

export interface TunnelHubOptions<S extends StreamEndpoint> {
  control: ControlPlane;
  auth: AuthContext;
  nodeId: string;
  limits: TunnelLimitsConfig;
  handshakeTimeoutMs: number;
  routeTtlMs?: number;
  log: RelayLog;
  metrics: RelayMetrics;
  /** 每条流的适配（Node：`stream.Duplex`）。 */
  streams: StreamFactory<S>;
  nonce?: () => string;
}

/** 与 `Buffer#toString("utf8")` 同义：坏字节换 U+FFFD，不吞 BOM。 */
const utf8 = new TextDecoder("utf-8", { ignoreBOM: true });

export class TunnelHub<S extends StreamEndpoint> {
  /** sourceId → 当前隧道。 */
  private readonly current = new Map<string, Tunnel<S>>();
  /** 所有未关闭的隧道（含排空中的旧隧道）。 */
  private readonly all = new Set<Tunnel<S>>();
  private readonly unsubscribe: (() => void) | undefined;
  private shuttingDown = false;
  private readonly options: TunnelHubOptions<S>;

  constructor(options: TunnelHubOptions<S>) {
    this.options = options;
    this.unsubscribe = options.control.nodeCommands?.subscribe((command) => {
      const tunnel = this.current.get(command.sourceId);
      if (!tunnel) return;
      if (command.type === "kick") tunnel.goaway("replaced");
      else tunnel.close(CLOSE.tunnelGoaway, "closed_by_control");
    });
  }

  /** 还收新隧道（没在关闭）。 */
  get accepting(): boolean {
    return !this.shuttingDown;
  }

  counts(): { tunnels: number; streams: number } {
    let streams = 0;
    for (const tunnel of this.all) streams += tunnel.streamCount;
    return { tunnels: this.all.size, streams };
  }

  /** 边缘用：本节点上持有这个源、且路由指向它、还能开流的隧道。 */
  async lookup(sourceId: string): Promise<Tunnel<S> | null> {
    const route = await this.options.control.routes.get(sourceId);
    if (!route || route.node !== this.options.nodeId) return null;
    const tunnel = this.current.get(sourceId);
    if (!tunnel || tunnel.tunnelId !== route.tunnelId || !tunnel.accepting)
      return null;
    return tunnel;
  }

  /** 所有隧道发 GOAWAY，等它们排空（或 grace 到）。 */
  async shutdown(graceMs: number): Promise<void> {
    this.shuttingDown = true;
    this.unsubscribe?.();
    const tunnels = [...this.all];
    for (const tunnel of tunnels) tunnel.goaway("shutdown", graceMs);
    await Promise.all(tunnels.map((tunnel) => tunnel.closed()));
  }

  /** 接一条刚升级好的隧道 WebSocket：握手、就绪后转交 {@link Tunnel}。 */
  accept(socket: TunnelSocket): TunnelConnection {
    const { options } = this;
    const startedAt = performance.now();
    const handshake = new Handshake({
      auth: options.auth,
      limits: options.control.limits,
      relayNode: options.nodeId,
      nonce: options.nonce,
    });
    let tunnel: Tunnel<S> | undefined;
    let settled = false;
    let chain = Promise.resolve();
    const settle = (result: string) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      options.metrics.handshake(
        result,
        result === "ok" ? (performance.now() - startedAt) / 1000 : undefined,
      );
      if (result !== "ok")
        options.log.warn("隧道握手失败", {
          result,
          sourceId: handshake.sourceId,
        });
    };
    const timer = setTimeout(() => {
      settle("timeout");
      socket.close(CLOSE.badFrame, "handshake_timeout");
    }, options.handshakeTimeoutMs);

    const act = async (step: HandshakeStep) => {
      switch (step.kind) {
        case "send":
          socket.sendText(JSON.stringify(step.message));
          return;
        case "reject":
          settle(step.message.code);
          socket.sendText(JSON.stringify(step.message));
          socket.close(step.closeCode, step.message.code);
          return;
        case "fail":
          settle(step.reason);
          socket.close(step.closeCode, step.reason);
          return;
        case "ready":
          tunnel = await this.ready(socket, step);
          if (tunnel) settle("ok");
          return;
      }
    };

    return {
      message: (data, isBinary) => {
        if (tunnel) {
          if (isBinary) tunnel.receive(data);
          else tunnel.close(CLOSE.badFrame, "text_after_ready");
          return;
        }
        chain = chain.then(async () => {
          if (tunnel) {
            if (isBinary) tunnel.receive(data);
            return;
          }
          if (settled || !socket.isOpen) return;
          if (isBinary) {
            settle("binary_before_ready");
            socket.close(CLOSE.badFrame, "binary_before_ready");
            return;
          }
          await act(await handshake.onText(utf8.decode(data)));
        });
        chain.catch((error: unknown) => {
          options.log.error("隧道握手异常", { error: errorName(error) });
          socket.terminate();
        });
      },
      closed: (code) => {
        clearTimeout(timer);
        settle("aborted");
        tunnel?.transportClosed(code);
      },
    };
  }

  private async ready(
    socket: TunnelSocket,
    step: Extract<HandshakeStep, { kind: "ready" }>,
  ): Promise<Tunnel<S> | undefined> {
    const { options } = this;
    const { sourceId, coreVersion } = step.hello;
    const tunnelId = randomBytes(16).toString("hex");
    const tunnel: Tunnel<S> = new Tunnel(
      tunnelId,
      sourceId,
      coreVersion,
      {
        send: (bytes) => socket.sendBinary(bytes),
        close: (code, reason) => socket.close(code, reason),
        terminate: () => socket.terminate(),
      },
      options.limits,
      {
        onPong: () => {
          if (this.current.get(sourceId) === tunnel)
            void options.control.routes.touch(
              sourceId,
              options.routeTtlMs ?? ROUTE_TTL_MS,
            );
        },
        onPeerGoaway: () => {
          if (this.current.get(sourceId) === tunnel)
            void options.control.routes.delete(sourceId, tunnelId);
        },
        onHeartbeatMiss: () => options.metrics.heartbeatMiss(),
        onBytes: (direction, bytes) =>
          options.metrics.addBytes(direction, bytes),
        onClose: (info) => this.closed(tunnel, info),
      },
      options.streams,
    );
    const previous = this.current.get(sourceId);
    this.current.set(sourceId, tunnel);
    this.all.add(tunnel);
    previous?.goaway("replaced");
    await options.control.routes.set(
      sourceId,
      {
        node: options.nodeId,
        tunnelId,
        sinceMs: Date.now(),
        coreVersion,
      },
      options.routeTtlMs ?? ROUTE_TTL_MS,
    );
    if (!socket.isOpen) {
      tunnel.transportClosed(CLOSE.goingAway, "closed_during_handshake");
      return undefined;
    }
    const ready: Ready = {
      type: "ready",
      tunnelId,
      limits: {
        maxStreams: options.limits.maxStreams,
        streamWindow: options.limits.streamWindow,
        tunnelWindow: options.limits.tunnelWindow,
        maxFrameBytes: LIMITS.maxFrameBytes,
      },
      heartbeatMs: options.limits.heartbeatMs,
      protocol: { ...PROTOCOL_VERSION },
    };
    socket.sendText(JSON.stringify(ready));
    tunnel.start();
    void options.control.presence.publish({
      type: "sourceOnline",
      sourceId,
      node: options.nodeId,
      tunnelId,
      coreVersion,
      atMs: Date.now(),
    });
    options.log.info("隧道已就绪", { sourceId, tunnelId });
    return tunnel;
  }

  private closed(
    tunnel: Tunnel<S>,
    info: { code: number; reason: string; replaced: boolean },
  ): void {
    const { options } = this;
    this.all.delete(tunnel);
    options.log.info("隧道已关闭", {
      sourceId: tunnel.sourceId,
      tunnelId: tunnel.tunnelId,
      code: info.code,
      reason: info.reason,
    });
    if (this.current.get(tunnel.sourceId) !== tunnel) return;
    this.current.delete(tunnel.sourceId);
    if (info.replaced) return;
    void options.control.routes.delete(tunnel.sourceId, tunnel.tunnelId);
    void options.control.presence.publish({
      type: "sourceOffline",
      sourceId: tunnel.sourceId,
      node: options.nodeId,
      tunnelId: tunnel.tunnelId,
      atMs: Date.now(),
    });
  }
}
