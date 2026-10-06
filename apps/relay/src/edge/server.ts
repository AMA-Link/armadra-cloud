/**
 * 客户端边缘（中继设计 §4）：HTTP 请求与 WS 升级 → 解析源 → 限流 → 校验中继令牌 →
 * 找隧道 → 开流转发。对模式零分支：地址写法由 {@link Addressing} 决定，控制面由
 * `ControlPlane` 决定。
 *
 * - HTTP：经 `http.request({ createConnection: () => 流 })` 把请求写成 HTTP/1.1 字节进流，
 *   源的响应由 Node 解析后写回客户端（客户端侧保持 keep-alive；流上一条流一个请求）。
 * - WS：升级请求序列化进流，之后两个方向字节透传（`ws-downstream.ts` 只跟踪帧边界）。
 *
 * CORS 预检（`OPTIONS` + `Origin` + `Access-Control-Request-Method`）由边缘自己回答，
 * 不验令牌、不找隧道、不开流（见 `cors.ts`）：按 IP 限流后，来源在名单里答 204，不在答
 * 403 `forbidden`（不带 CORS 头）。其余请求（含不是预检的 `OPTIONS`）照常要令牌。
 *
 * 拒绝：地址不匹配 → 404（WS 也答 404，不升级）；之后的拒绝 HTTP 答 `{ code, message }`，
 * WS 先完成升级再以关闭码结束（浏览器读不到升级失败的状态码）：令牌 4401 / 4403、
 * 限流 4429、源离线 4404。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import { request, type IncomingMessage, type ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { CLOSE, LIMITS } from "@armadra/platform-protocol/tunnel";
import { WebSocketServer } from "ws";

import type { AuthContext } from "../auth/context.ts";
import {
  relayTokenFromHeaders,
  splitRelaySubprotocol,
  verifyRelayToken,
} from "../auth/relay-token.ts";
import type { ControlPlane } from "../control/types.ts";
import {
  rawErrorResponse,
  rejectUpgrade,
  sendRelayError,
} from "../http/errors.ts";
import { errorName, type RelayLog } from "../log.ts";
import type { RelayMetrics } from "../metrics.ts";
import { TunnelUnavailableError, type Tunnel } from "../tunnel/tunnel.ts";
import type { Addressing, AddressTarget } from "./addressing.ts";
import {
  errorCorsHeaders,
  isPreflight,
  originAllowed,
  preflightHeaders,
} from "./cors.ts";
import {
  flatten,
  forwardRequestHeaders,
  forwardResponseHeaders,
  headersBytes,
  serializeRequestHead,
  type HeaderPair,
} from "./forward.ts";
import { WsDownstream } from "./ws-downstream.ts";

export interface EdgeOptions {
  nodeId: string;
  addressing: Addressing;
  auth: AuthContext;
  control: ControlPlane;
  lookup(sourceId: string): Promise<Tunnel | null>;
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
}

const WS_CLOSE: Partial<Record<ErrorCode, number>> = {
  relay_token_invalid: CLOSE.unauthenticated,
  relay_source_mismatch: CLOSE.forbidden,
  rate_limited: CLOSE.limitReached,
  limit_reached: CLOSE.limitReached,
  source_offline: CLOSE.sourceOffline,
  bad_request: CLOSE.badFrame,
};

type Admission =
  | {
      ok: true;
      target: AddressTarget;
      tunnel: Tunnel;
      subject: string;
      remoteIp: string;
    }
  | {
      ok: false;
      code: ErrorCode;
      retryAfterMs?: number;
      /** 地址解析出的源（名单按源取；地址不匹配时没有）。 */
      sourceId?: string;
    };

function remoteIpOf(req: IncomingMessage): string {
  return req.socket.remoteAddress ?? "";
}

function originOf(req: IncomingMessage): string | null {
  const origin = req.headers.origin;
  return typeof origin === "string" ? origin : null;
}

/** {@link Edge.disconnect} 的条件：都给时要同时满足。 */
export interface EdgeConnectionFilter {
  sourceId?: string;
  /** 中继令牌的 `sub` 以它开头（访客：`guest:<linkId>:`）。 */
  subjectPrefix?: string;
}

interface EdgeConnection {
  sourceId: string;
  subject: string;
}

export class Edge {
  private readonly wss = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    // 只为发关闭码而完成升级：回选客户端给的第一个子协议，浏览器才会接受这次握手。
    handleProtocols: (protocols) => protocols.values().next().value ?? false,
  });

  private readonly options: EdgeOptions;
  private readonly sockets = new Map<Duplex, EdgeConnection>();

  constructor(options: EdgeOptions) {
    this.options = options;
  }

  async handleRequest(
    req: IncomingMessage,
    res: ServerResponse,
  ): Promise<void> {
    if (isPreflight(req)) return this.answerPreflight(req, res);
    const admission = await this.admit(req, relayTokenFromHeaders(req.headers));
    if (!admission.ok) {
      this.options.metrics.edgeConnection(admission.code);
      sendRelayError(res, admission.code, {
        retryAfterMs: admission.retryAfterMs,
        cors: this.errorCors(req, admission.sourceId),
      });
      return;
    }
    const { target, tunnel, remoteIp } = admission;
    const headers = forwardRequestHeaders(req.rawHeaders, { remoteIp });
    const stream = this.open(tunnel, "http", req, target, headers, remoteIp);
    if (!stream.ok) {
      sendRelayError(res, stream.code, {
        cors: this.errorCors(req, target.sourceId),
      });
      return;
    }
    this.options.metrics.edgeConnection("ok");
    const upstream = request({
      createConnection: () => stream.stream,
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
          cors: this.errorCors(req, target.sourceId),
        });
      else res.destroy();
      this.options.log.debug("边缘流结束", {
        sourceId: target.sourceId,
        streamId: stream.stream.streamId,
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
    const { token, rest } = splitRelaySubprotocol(
      req.headers["sec-websocket-protocol"],
    );
    const admission = await this.admit(req, token);
    if (!admission.ok) {
      this.options.metrics.edgeConnection(admission.code);
      this.refuseUpgrade(req, socket, head, admission.code, admission.sourceId);
      return;
    }
    const { target, tunnel, subject, remoteIp } = admission;
    const { control } = this.options;
    const concurrent = await control.limits.hit(
      "edge.concurrent.acct",
      subject,
    );
    if (!concurrent.allowed) {
      this.options.metrics.edgeConnection("limit_reached");
      this.refuseUpgrade(req, socket, head, "limit_reached", target.sourceId);
      return;
    }
    socket.once("close", () => {
      void control.limits.release?.("edge.concurrent.acct", subject);
    });
    if (socket.destroyed) return;
    const headers = forwardRequestHeaders(req.rawHeaders, {
      remoteIp,
      upgrade: { subprotocols: rest },
    });
    const opened = this.open(tunnel, "ws", req, target, headers, remoteIp);
    if (!opened.ok) {
      this.refuseUpgrade(req, socket, head, opened.code, target.sourceId);
      return;
    }
    this.options.metrics.edgeConnection("ok");
    const stream = opened.stream;
    stream.write(
      serializeRequestHead(req.method ?? "GET", target.path, headers),
    );
    if (head.length > 0) stream.write(head);
    // 先接上 pipe 再建 WsDownstream（它也监听 data，先监听会让 socket 提前流动丢字节）。
    socket.pipe(stream);
    const downstream = new WsDownstream(socket, {
      pingMs: this.options.wsPingMs ?? 30_000,
      idleMs: this.options.wsIdleMs ?? 60_000,
      offlineResponse: rawErrorResponse(
        "source_offline",
        this.errorCors(req, target.sourceId),
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
    this.sockets.set(socket, { sourceId: target.sourceId, subject });
    socket.once("close", () => {
      this.sockets.delete(socket);
      stream.destroy();
    });
  }

  /** 预检：解析源 → 按 IP 限流（预检专用桶）→ 来源名单；不验令牌、不碰隧道，请求体丢掉。 */
  private async answerPreflight(
    req: IncomingMessage,
    res: ServerResponse,
  ): Promise<void> {
    req.resume();
    const { addressing, control, corsOrigins } = this.options;
    const target = addressing.resolve({
      url: req.url ?? "/",
      host: req.headers.host,
    });
    if (!target) {
      sendRelayError(res, "not_found", { cors: this.errorCors(req) });
      return;
    }
    const byIp = await control.limits.hit("edge.preflight.ip", remoteIpOf(req));
    if (!byIp.allowed) {
      this.options.metrics.edgeConnection("rate_limited");
      sendRelayError(res, "rate_limited", {
        retryAfterMs: byIp.retryAfterMs,
        cors: this.errorCors(req, target.sourceId),
      });
      return;
    }
    const origin = originOf(req) as string;
    if (!originAllowed(origin, corsOrigins?.(target.sourceId) ?? [])) {
      this.options.metrics.edgeConnection("forbidden");
      sendRelayError(res, "forbidden");
      return;
    }
    this.options.metrics.edgeConnection("preflight");
    res.writeHead(204, preflightHeaders(origin));
    res.end();
  }

  /** 错误答复的 CORS 头：名单与预检同一份（地址不匹配时只有原生 App 来源）。 */
  private errorCors(
    req: IncomingMessage,
    sourceId?: string,
  ): Record<string, string> | undefined {
    const trusted =
      sourceId === undefined
        ? []
        : (this.options.corsOrigins?.(sourceId) ?? []);
    return errorCorsHeaders(originOf(req), trusted);
  }

  /** 关闭时断开所有已升级的客户端连接（`server.close()` 不管升级后的 socket）。 */
  closeAll(): void {
    for (const socket of this.sockets.keys()) socket.destroy();
    this.sockets.clear();
  }

  /** 断开匹配的已升级连接（撤销源 / 访客），答断开了几条。 */
  disconnect(filter: EdgeConnectionFilter): number {
    let count = 0;
    for (const [socket, connection] of this.sockets) {
      if (
        filter.sourceId !== undefined &&
        connection.sourceId !== filter.sourceId
      )
        continue;
      if (
        filter.subjectPrefix !== undefined &&
        !connection.subject.startsWith(filter.subjectPrefix)
      )
        continue;
      socket.destroy();
      this.sockets.delete(socket);
      count += 1;
    }
    return count;
  }

  /** 解析源 → 中继令牌（无效按 IP 限流；有效按令牌与账号限流）→ 找隧道。 */
  private async admit(
    req: IncomingMessage,
    token: string | undefined,
  ): Promise<Admission> {
    const { addressing, control, auth, lookup } = this.options;
    const target = addressing.resolve({
      url: req.url ?? "/",
      host: req.headers.host,
    });
    if (!target) return { ok: false, code: "not_found" };
    const { sourceId } = target;
    const remoteIp = remoteIpOf(req);
    const verified = await verifyRelayToken(auth, token, target.sourceId);
    if (!verified.ok) {
      // 无令牌 / 令牌无效 / 源不符：按 IP 严格限流（令牌校验是本地验签，先验后计）。
      const byIp = await control.limits.hit("edge.connect.ip", remoteIp);
      if (!byIp.allowed)
        return {
          ok: false,
          code: "rate_limited",
          retryAfterMs: byIp.retryAfterMs,
          sourceId,
        };
      return { ok: false, code: verified.error, sourceId };
    }
    const subject = verified.claims.sub;
    // 有效令牌：先按令牌、再按账号计数；与 IP、预检的桶互不相干。
    for (const [kind, key] of [
      ["edge.connect.token", verified.claims.jti],
      ["edge.connect.acct", subject],
    ] as const) {
      const hit = await control.limits.hit(kind, key);
      if (!hit.allowed)
        return {
          ok: false,
          code: "rate_limited",
          retryAfterMs: hit.retryAfterMs,
          sourceId,
        };
    }
    const tunnel = await lookup(target.sourceId);
    if (!tunnel) return { ok: false, code: "source_offline", sourceId };
    return { ok: true, target, tunnel, subject, remoteIp };
  }

  private open(
    tunnel: Tunnel,
    kind: "http" | "ws",
    req: IncomingMessage,
    target: AddressTarget,
    headers: HeaderPair[],
    remoteIp: string,
  ):
    | { ok: true; stream: ReturnType<Tunnel["openStream"]> }
    | { ok: false; code: ErrorCode } {
    if (headersBytes(headers) > LIMITS.openHeadersMaxBytes)
      return { ok: false, code: "bad_request" };
    try {
      const stream = tunnel.openStream({
        kind,
        method: req.method ?? "GET",
        path: target.path,
        headers,
        clientOrigin: originOf(req),
        remoteIp,
        relayNode: this.options.nodeId,
      });
      stream.on("error", () => {
        // 由上层（http.request / WsDownstream）处理；这里只防止未处理的 error 事件。
      });
      return { ok: true, stream };
    } catch (error) {
      if (error instanceof TunnelUnavailableError)
        return {
          ok: false,
          code:
            error.reason === "max_streams" ? "limit_reached" : "source_offline",
        };
      throw error;
    }
  }

  private refuseUpgrade(
    req: IncomingMessage,
    socket: Duplex,
    head: Buffer,
    code: ErrorCode,
    sourceId?: string,
  ): void {
    const closeCode = WS_CLOSE[code];
    const isWebSocket =
      (req.headers.upgrade ?? "").toLowerCase() === "websocket" &&
      typeof req.headers["sec-websocket-key"] === "string";
    if (closeCode === undefined || !isWebSocket || socket.destroyed) {
      rejectUpgrade(socket, code, this.errorCors(req, sourceId));
      return;
    }
    try {
      this.wss.handleUpgrade(req, socket, head, (ws) => {
        ws.close(closeCode, code);
      });
    } catch {
      rejectUpgrade(socket, code, this.errorCors(req, sourceId));
    }
  }
}
