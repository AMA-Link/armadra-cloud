/**
 * 客户端边缘的 Node 外壳（中继设计 §4）。判定（地址、限流、令牌、媒体票、开流）
 * 全在内核的 {@link EdgePolicy}；这里只管字节进出：
 *
 * - HTTP：经 `http.request({ createConnection: () => 流 })` 把请求写成 HTTP/1.1 字节进流，
 *   源的响应由 Node 解析后写回客户端（客户端侧保持 keep-alive；流上一条流一个请求）。
 * - WS：升级请求序列化进流，之后两个方向字节透传（`ws-downstream.ts` 只跟踪帧边界）。
 *   被拒时先完成升级再以关闭码结束（浏览器读不到升级失败的状态码）。
 * - 端到端通道（`_relay/e2e`，cloud-api §20）：中继自己完成升级（选 `armadra-e2e.1`），开
 *   `OPEN kind: "e2e"`，二进制消息与流字节两边透传（`e2e-pipe.ts`），不解析。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import { request, type IncomingMessage, type ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import { E2E_SUBPROTOCOL } from "@armadra/platform-protocol/e2e";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { LIMITS } from "@armadra/platform-protocol/tunnel";
import type { AuthContext } from "@armadra/relay-kernel/auth/context";
import type { ControlPlane } from "@armadra/relay-kernel/control/types";
import type { Addressing } from "@armadra/relay-kernel/edge/addressing";
import {
  flatten,
  forwardRequestHeaders,
  forwardResponseHeaders,
  serializeRequestHead,
} from "@armadra/relay-kernel/edge/forward";
import { MEDIA_ISSUE_BODY_MAX } from "@armadra/relay-kernel/edge/media-tickets";
import {
  EdgePolicy,
  wsCloseCode,
  type EdgeConnectionFilter,
  type EdgeRefusal,
  type EdgeRequest,
  type UpgradeDecision,
  type RequestDecision,
  type TrafficIdentity,
} from "@armadra/relay-kernel/edge/policy";
import { errorName, type RelayLog } from "@armadra/relay-kernel/log";
import type { RelayMetrics } from "@armadra/relay-kernel/metrics";
import type { TrafficMeter } from "@armadra/relay-kernel/traffic";
import type { Tunnel } from "@armadra/relay-kernel/tunnel/tunnel";
import { WebSocketServer } from "ws";

import { createClientIp, type ClientIp } from "./client-ip.ts";
import { pipeE2e } from "./e2e-pipe.ts";
import { rawErrorBytes, rejectUpgrade, sendRelayError } from "./errors.ts";
import type { TunnelStream } from "./stream.ts";
import { WsDownstream } from "./ws-downstream.ts";

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
   * 预检放行的来源（原生 App 的两个来源总是放行，不用列）。答案不得随源在不在线、
   * 登没登记而变，否则预检就成了探测源的手段。缺省空。
   */
  corsOrigins?: (sourceId: string) => readonly string[];
  /** 对客户端 WS 的 ping 间隔与无响应断开时长（缺省 30 秒 / 60 秒）。 */
  wsPingMs?: number;
  wsIdleMs?: number;
  /** 客户端 IP 的取法（受信代理见 `client-ip.ts`）；缺省用直连对端地址。 */
  clientIp?: ClientIp;
  /** 媒体票的时钟（测试用）。 */
  now?: () => number;
  /** 流量统计（cloud-api §19）；不给就不统计。 */
  traffic?: TrafficMeter;
}

/** 读一个小 JSON 体；超长、不是 JSON 答 `undefined`。 */
async function readSmallJson(
  req: IncomingMessage,
  limit: number,
): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    size += (chunk as Buffer).length;
    if (size > limit) {
      req.resume();
      return undefined;
    }
    chunks.push(chunk as Buffer);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8")) as unknown;
  } catch {
    return undefined;
  }
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
      now: options.now,
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

  async handleRequest(
    req: IncomingMessage,
    res: ServerResponse,
  ): Promise<void> {
    const edgeRequest = this.edgeRequest(req);
    const decision = await this.policy.decideRequest(edgeRequest);
    switch (decision.kind) {
      case "error":
        this.refuse(req, res, decision);
        return;
      case "preflight":
        req.resume();
        res.writeHead(204, decision.headers);
        res.end();
        return;
      case "issue":
        return this.issueMedia(req, res, edgeRequest, decision.admission);
      case "forward":
        this.forward(req, res, edgeRequest, decision);
        return;
    }
  }

  private refuse(
    req: IncomingMessage,
    res: ServerResponse,
    refusal: EdgeRefusal,
  ): void {
    if (refusal.drain) req.resume();
    sendRelayError(res, refusal.code, {
      retryAfterMs: refusal.retryAfterMs,
      cors: refusal.cors,
    });
  }

  /** 把一个 HTTP 请求开流转给源，源的响应原样写回（`path` 是转给源的路径）。 */
  private forward(
    req: IncomingMessage,
    res: ServerResponse,
    edgeRequest: EdgeRequest,
    decision: Extract<RequestDecision<TunnelStream>, { kind: "forward" }>,
  ): void {
    const { target, tunnel, remoteIp } = decision;
    const headers = forwardRequestHeaders(req.rawHeaders, { remoteIp });
    const opened = this.policy.open(
      tunnel,
      "http",
      edgeRequest,
      target,
      headers,
      remoteIp,
      decision.traffic,
    );
    if (!opened.ok) {
      sendRelayError(res, opened.code, {
        cors: this.policy.errorCors(edgeRequest, target.sourceId),
      });
      return;
    }
    const stream = opened.stream;
    stream.on("error", () => {
      // 由 http.request 处理；这里只防止未处理的 error 事件。
    });
    const upstream = request({
      createConnection: () => stream,
      method: req.method,
      path: target.path,
      headers: flatten(headers),
      setHost: false,
    });
    upstream.on("response", (response) => {
      res.writeHead(
        response.statusCode ?? 502,
        response.statusMessage,
        flatten(forwardResponseHeaders(response.rawHeaders)),
      );
      response.pipe(res);
      response.on("error", () => res.destroy());
    });
    upstream.on("error", (error) => {
      if (!res.headersSent)
        sendRelayError(res, "source_offline", {
          cors: this.policy.errorCors(edgeRequest, target.sourceId),
        });
      else res.destroy();
      this.options.log.debug("边缘流结束", {
        sourceId: target.sourceId,
        streamId: stream.streamId,
        error: errorName(error),
      });
    });
    res.on("close", () => {
      if (!res.writableFinished) upstream.destroy();
    });
    req.pipe(upstream);
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
    const { target, tunnel, subject, remoteIp } = decision;
    const traffic = { subject, deviceId: decision.device ?? "" };
    socket.once("close", () => this.policy.releaseUpgrade(subject));
    if (socket.destroyed) return;
    if (decision.mode === "e2e") {
      this.e2e(req, socket, head, edgeRequest, decision, traffic);
      return;
    }
    const headers = forwardRequestHeaders(req.rawHeaders, {
      remoteIp,
      upgrade: { subprotocols: decision.subprotocols },
    });
    const opened = this.policy.open(
      tunnel,
      "ws",
      edgeRequest,
      target,
      headers,
      remoteIp,
      traffic,
    );
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
      // 由 WsDownstream 处理；这里只防止未处理的 error 事件。
    });
    stream.write(
      serializeRequestHead(edgeRequest.method, target.path, headers),
    );
    if (head.length > 0) stream.write(head);
    // 先接上 pipe 再建 WsDownstream（它也监听 data，先监听会让 socket 提前流动丢字节）。
    socket.pipe(stream);
    const downstream = new WsDownstream(socket, {
      pingMs: this.options.wsPingMs ?? 30_000,
      idleMs: this.options.wsIdleMs ?? 60_000,
      offlineResponse: rawErrorBytes(
        "source_offline",
        this.policy.errorCors(edgeRequest, target.sourceId),
      ),
    });
    stream.pipe(downstream);
    stream.on("error", () => {
      stream.unpipe(downstream);
      downstream.sourceGone();
    });
    stream.once("close", () => {
      // 流没了：客户端后面发来的字节丢掉（不然 socket 停在暂停态，读不到 FIN）。
      socket.unpipe(stream);
      socket.resume();
    });
    this.policy.track(socket, target.sourceId, subject);
    socket.once("close", () => {
      this.policy.untrack(socket);
      stream.destroy();
    });
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

  /**
   * 签媒体票：令牌已验过。只认 POST；体 `{ path }` 交给内核签，答
   * `{ ticket, path: "/_relay/m/<票>", expiresAt }`。
   */
  private async issueMedia(
    req: IncomingMessage,
    res: ServerResponse,
    edgeRequest: EdgeRequest,
    admission: Parameters<EdgePolicy<TunnelStream>["issueMedia"]>[0],
  ): Promise<void> {
    const cors = this.policy.errorCors(edgeRequest, admission.target.sourceId);
    if (req.method !== "POST") {
      req.resume();
      sendRelayError(res, "bad_request", { cors });
      return;
    }
    const body = await readSmallJson(req, MEDIA_ISSUE_BODY_MAX);
    const issued = this.policy.issueMedia(admission, body);
    if (!issued.ok) {
      sendRelayError(res, issued.code, { cors });
      return;
    }
    for (const [name, value] of Object.entries(cors ?? {}))
      res.setHeader(name, value);
    res.writeHead(200, {
      "content-type": "application/json",
      "cache-control": "no-store",
    });
    res.end(issued.json);
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
