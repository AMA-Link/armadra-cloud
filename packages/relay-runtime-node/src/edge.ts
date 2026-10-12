/**
 * 客户端边缘的 Node 外壳（中继设计 §4）。判定（地址、限流、令牌、开流）全在内核的
 * {@link EdgePolicy}；这里只管字节进出。0.5.0 起只转端到端通道（cloud-api §20）：
 *
 * - HTTP：源地址上一律 `404 not_found`（中继不转明文的应用请求）。
 * - 端到端通道（`_relay/e2e`）：中继自己完成升级（选 `armadra-e2e.1`），开
 *   `OPEN kind: "e2e"`，二进制消息与流字节两边透传（`e2e-pipe.ts`），不解析。被拒时先
 *   完成升级再以关闭码结束（浏览器读不到升级失败的状态码）。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import { E2E_SUBPROTOCOL } from "@armadra/platform-protocol/e2e";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { LIMITS } from "@armadra/platform-protocol/tunnel";
import type { AuthContext } from "@armadra/relay-kernel/auth/context";
import type { ControlPlane } from "@armadra/relay-kernel/control/types";
import type { Addressing } from "@armadra/relay-kernel/edge/addressing";
import {
  EdgePolicy,
  wsCloseCode,
  type EdgeConnectionFilter,
  type EdgeRequest,
  type UpgradeDecision,
  type TrafficIdentity,
} from "@armadra/relay-kernel/edge/policy";
import type { RelayLog } from "@armadra/relay-kernel/log";
import type { RelayMetrics } from "@armadra/relay-kernel/metrics";
import type { TrafficMeter } from "@armadra/relay-kernel/traffic";
import type { Tunnel } from "@armadra/relay-kernel/tunnel/tunnel";
import { WebSocketServer } from "ws";

import { createClientIp, type ClientIp } from "./client-ip.ts";
import { pipeE2e } from "./e2e-pipe.ts";
import { rejectUpgrade, sendRelayError } from "./errors.ts";
import type { TunnelStream } from "./stream.ts";

export type { EdgeConnectionFilter };

export interface EdgeOptions {
  nodeId: string;
  addressing: Addressing;
  auth: AuthContext;
  control: ControlPlane;
  lookup(sourceId: string): Promise<Tunnel<TunnelStream> | null>;
  log: RelayLog;
  metrics: RelayMetrics;
  /**
   * 错误答复回显 CORS 的来源（原生 App 的两个来源总是回显，不用列）。答案不得随源在
   * 不在线、登没登记而变。缺省空。
   */
  corsOrigins?: (sourceId: string) => readonly string[];
  /** 对客户端 WS 的 ping 间隔与无响应断开时长（缺省 30 秒 / 60 秒）。 */
  wsPingMs?: number;
  wsIdleMs?: number;
  /** 客户端 IP 的取法（受信代理见 `client-ip.ts`）；缺省用直连对端地址。 */
  clientIp?: ClientIp;
  /** 流量统计（cloud-api §19）；不给就不统计。 */
  traffic?: TrafficMeter;
}

export class Edge {
  private readonly wss = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    // 只为发关闭码而完成升级：回选客户端给的第一个子协议，浏览器才会接受这次握手。
    handleProtocols: (protocols) => protocols.values().next().value ?? false,
  });

  /** 端到端通道：中继自己是 WS 的服务端，只选 `armadra-e2e.1`。 */
  private readonly e2eWss = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    maxPayload: LIMITS.maxFrameBytes,
    handleProtocols: (protocols) =>
      protocols.has(E2E_SUBPROTOCOL) ? E2E_SUBPROTOCOL : false,
  });

  private readonly options: EdgeOptions;
  private readonly policy: EdgePolicy<TunnelStream, Duplex>;
  private readonly clientIp: ClientIp;

  constructor(options: EdgeOptions) {
    this.options = options;
    this.policy = new EdgePolicy({
      nodeId: options.nodeId,
      addressing: options.addressing,
      auth: options.auth,
      control: options.control,
      lookup: options.lookup,
      metrics: options.metrics,
      corsOrigins: options.corsOrigins,
      traffic: options.traffic,
    });
    this.clientIp = options.clientIp ?? createClientIp();
  }

  private edgeRequest(req: IncomingMessage): EdgeRequest {
    return {
      method: req.method ?? "GET",
      url: req.url ?? "/",
      headers: req.headers,
      clientIp: this.clientIp(req),
    };
  }

  /** 源地址上的 HTTP：一律 `404 not_found`（中继不转明文的应用请求）。 */
  handleRequest(req: IncomingMessage, res: ServerResponse): void {
    const refusal = this.policy.decideRequest(this.edgeRequest(req));
    if (refusal.drain) req.resume();
    sendRelayError(res, refusal.code, {
      retryAfterMs: refusal.retryAfterMs,
      cors: refusal.cors,
    });
  }

  async handleUpgrade(
    req: IncomingMessage,
    socket: Duplex,
    head: Buffer,
  ): Promise<void> {
    socket.on("error", () => {});
    const edgeRequest = this.edgeRequest(req);
    const decision = await this.policy.decideUpgrade(edgeRequest);
    if (decision.kind === "refuse") {
      this.refuseUpgrade(
        req,
        edgeRequest,
        socket,
        head,
        decision.code,
        decision.sourceId,
        decision.reason,
      );
      return;
    }
    const traffic = {
      subject: decision.subject,
      deviceId: decision.device ?? "",
    };
    socket.once("close", () => this.policy.releaseUpgrade(decision.subject));
    if (socket.destroyed) return;
    this.e2e(req, socket, head, edgeRequest, decision, traffic);
  }

  /** 端到端通道：开 `OPEN kind: "e2e"`，完成升级，两边字节透传。 */
  private e2e(
    req: IncomingMessage,
    socket: Duplex,
    head: Buffer,
    edgeRequest: EdgeRequest,
    accepted: Extract<UpgradeDecision<TunnelStream>, { kind: "accept" }>,
    traffic: TrafficIdentity,
  ): void {
    const { target, tunnel, subject, remoteIp } = accepted;
    const opened = this.policy.openE2e(tunnel, edgeRequest, remoteIp, traffic);
    if (!opened.ok) {
      this.refuseUpgrade(
        req,
        edgeRequest,
        socket,
        head,
        opened.code,
        target.sourceId,
      );
      return;
    }
    const stream = opened.stream;
    stream.on("error", () => {
      // 由 pipeE2e 处理；这里只防止未处理的 error 事件。
    });
    this.policy.track(socket, target.sourceId, subject);
    try {
      this.e2eWss.handleUpgrade(req, socket, head, (ws) => {
        pipeE2e(ws, stream, {
          sourceId: target.sourceId,
          pingMs: this.options.wsPingMs ?? 30_000,
          idleMs: this.options.wsIdleMs ?? 60_000,
          log: this.options.log,
          done: () => this.policy.untrack(socket),
        });
      });
    } catch {
      stream.destroy();
      this.policy.untrack(socket);
      rejectUpgrade(
        socket,
        "bad_request",
        this.policy.errorCors(edgeRequest, target.sourceId),
      );
    }
  }

  /** 关闭时断开所有已升级的客户端连接（`server.close()` 不管升级后的 socket）。 */
  closeAll(): void {
    for (const socket of this.policy.takeAll()) socket.destroy();
  }

  /** 断开匹配的已升级连接（撤销源 / 访客），答断开了几条。 */
  disconnect(filter: EdgeConnectionFilter): number {
    const sockets = this.policy.take(filter);
    for (const socket of sockets) socket.destroy();
    return sockets.length;
  }

  private refuseUpgrade(
    req: IncomingMessage,
    edgeRequest: EdgeRequest,
    socket: Duplex,
    head: Buffer,
    code: ErrorCode,
    sourceId?: string,
    reason?: string,
  ): void {
    const closeCode = wsCloseCode(code);
    const cors = () => this.policy.errorCors(edgeRequest, sourceId);
    const isWebSocket =
      (req.headers.upgrade ?? "").toLowerCase() === "websocket" &&
      typeof req.headers["sec-websocket-key"] === "string";
    if (closeCode === undefined || !isWebSocket || socket.destroyed) {
      rejectUpgrade(socket, code, cors());
      return;
    }
    try {
      this.wss.handleUpgrade(req, socket, head, (ws) => {
        ws.close(closeCode, reason ?? code);
      });
    } catch {
      rejectUpgrade(socket, code, cors());
    }
  }
}
