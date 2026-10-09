/**
 * 隧道终端 `/t/v1`（中继设计 §3.1）：WS 升级 → 握手 → {@link Tunnel}；本节点的隧道表
 * （每源一条，新的替换旧的）；路由与在线事件写进 `ControlPlane`。
 *
 * 替换：同一源在本节点已有隧道 → 旧的发 `GOAWAY replaced`，`goawayGraceMs` 后关 4491；
 * 路由 `set` 覆盖。关闭：只有「当前」隧道（没被替换）才删路由（CAS）并发 `sourceOffline`。
 */
import { randomBytes } from "node:crypto";
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import { CLOSE, LIMITS, type Ready } from "@armadra/platform-protocol/tunnel";
import { WebSocketServer, type RawData, type WebSocket } from "ws";

import type { AuthContext } from "../auth/context.ts";
import { ROUTE_TTL_MS, type ControlPlane } from "../control/types.ts";
import { errorName, type RelayLog } from "../log.ts";
import type { RelayMetrics } from "../metrics.ts";
import { Handshake, type HandshakeStep } from "./handshake.ts";
import { Tunnel, type TunnelLimitsConfig } from "./tunnel.ts";

export interface TunnelServerOptions {
  control: ControlPlane;
  auth: AuthContext;
  nodeId: string;
  limits: TunnelLimitsConfig;
  handshakeTimeoutMs: number;
  routeTtlMs?: number;
  log: RelayLog;
  metrics: RelayMetrics;
  nonce?: () => string;
}

function toBytes(data: RawData): Buffer {
  if (Buffer.isBuffer(data)) return data;
  if (Array.isArray(data)) return Buffer.concat(data);
  return Buffer.from(data);
}

export class TunnelServer {
  private readonly wss = new WebSocketServer({
    noServer: true,
    maxPayload: LIMITS.maxFrameBytes + 16,
    perMessageDeflate: false,
  });
  /** sourceId → 当前隧道。 */
  private readonly current = new Map<string, Tunnel>();
  /** 所有未关闭的隧道（含排空中的旧隧道）。 */
  private readonly all = new Set<Tunnel>();
  private readonly unsubscribe: (() => void) | undefined;
  private shuttingDown = false;
  private readonly options: TunnelServerOptions;

  constructor(options: TunnelServerOptions) {
    this.options = options;
    this.unsubscribe = options.control.nodeCommands?.subscribe((command) => {
      const tunnel = this.current.get(command.sourceId);
      if (!tunnel) return;
      if (command.type === "kick") tunnel.goaway("replaced");
      else tunnel.close(CLOSE.tunnelGoaway, "closed_by_control");
    });
  }

  counts(): { tunnels: number; streams: number } {
    let streams = 0;
    for (const tunnel of this.all) streams += tunnel.streamCount;
    return { tunnels: this.all.size, streams };
  }

  /** 边缘用：本节点上持有这个源、且路由指向它、还能开流的隧道。 */
  async lookup(sourceId: string): Promise<Tunnel | null> {
    const route = await this.options.control.routes.get(sourceId);
    if (!route || route.node !== this.options.nodeId) return null;
    const tunnel = this.current.get(sourceId);
    if (!tunnel || tunnel.tunnelId !== route.tunnelId || !tunnel.accepting)
      return null;
    return tunnel;
  }

  handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): void {
    if (this.shuttingDown) {
      socket.destroy();
      return;
    }
    this.wss.handleUpgrade(req, socket, head, (ws) => this.accept(ws));
  }

  /** 所有隧道发 GOAWAY，等它们排空（或 grace 到）。 */
  async shutdown(graceMs: number): Promise<void> {
    this.shuttingDown = true;
    this.unsubscribe?.();
    const tunnels = [...this.all];
    for (const tunnel of tunnels) tunnel.goaway("shutdown", graceMs);
    await Promise.all(tunnels.map((tunnel) => tunnel.closed()));
    for (const client of this.wss.clients) client.terminate();
    await new Promise<void>((resolve) => this.wss.close(() => resolve()));
  }

  private accept(ws: WebSocket): void {
    const { options } = this;
    const startedAt = performance.now();
    const handshake = new Handshake({
      auth: options.auth,
      limits: options.control.limits,
      relayNode: options.nodeId,
      nonce: options.nonce,
    });
    let tunnel: Tunnel | undefined;
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
      ws.close(CLOSE.badFrame, "handshake_timeout");
    }, options.handshakeTimeoutMs);

    const act = async (step: HandshakeStep) => {
      switch (step.kind) {
        case "send":
          ws.send(JSON.stringify(step.message));
          return;
        case "reject":
          settle(step.message.code);
          ws.send(JSON.stringify(step.message));
          ws.close(step.closeCode, step.message.code);
          return;
        case "fail":
          settle(step.reason);
          ws.close(step.closeCode, step.reason);
          return;
        case "ready":
          tunnel = await this.ready(ws, step);
          if (tunnel) settle("ok");
          return;
      }
    };

    ws.on("message", (data: RawData, isBinary: boolean) => {
      if (tunnel) {
        if (isBinary) tunnel.receive(toBytes(data));
        else tunnel.close(CLOSE.badFrame, "text_after_ready");
        return;
      }
      chain = chain.then(async () => {
        if (tunnel) {
          if (isBinary) tunnel.receive(toBytes(data));
          return;
        }
        if (settled || ws.readyState !== ws.OPEN) return;
        if (isBinary) {
          settle("binary_before_ready");
          ws.close(CLOSE.badFrame, "binary_before_ready");
          return;
        }
        await act(await handshake.onText(toBytes(data).toString("utf8")));
      });
      chain.catch((error: unknown) => {
        options.log.error("隧道握手异常", { error: errorName(error) });
        ws.terminate();
      });
    });
    ws.on("close", (code: number) => {
      clearTimeout(timer);
      settle("aborted");
      tunnel?.transportClosed(code);
    });
    ws.on("error", () => {
      // close 随后到达。
    });
  }

  private async ready(
    ws: WebSocket,
    step: Extract<HandshakeStep, { kind: "ready" }>,
  ): Promise<Tunnel | undefined> {
    const { options } = this;
    const { sourceId, coreVersion } = step.hello;
    const tunnelId = randomBytes(16).toString("hex");
    const tunnel: Tunnel = new Tunnel(
      tunnelId,
      sourceId,
      coreVersion,
      {
        send: (bytes) => ws.send(bytes, { binary: true }),
        close: (code, reason) => ws.close(code, reason),
        terminate: () => ws.terminate(),
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
    if (ws.readyState !== ws.OPEN) {
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
    ws.send(JSON.stringify(ready));
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
    tunnel: Tunnel,
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
