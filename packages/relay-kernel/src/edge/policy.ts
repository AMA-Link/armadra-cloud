/**
 * 客户端边缘的判定（中继设计 §4），与运行时无关：解析源 → 限流 → 校验中继令牌 → 找隧道
 * → 决定转发还是拒绝。字节怎么进出由运行时负责（Node 在 `@armadra/relay-runtime-node`
 * 的 `edge.ts`），这里只看 {@link EdgeRequest}，答一个「下一步做什么」。对模式零分支：地址
 * 写法由 {@link Addressing} 决定，控制面由 `ControlPlane` 决定。
 *
 * CORS 预检（`OPTIONS` + `Origin` + `Access-Control-Request-Method`）由边缘自己回答，
 * 不验令牌、不找隧道、不开流（见 `cors.ts`）：按 IP 限流后，来源在名单里答 204，不在答
 * 403 `forbidden`（不带 CORS 头）。其余请求（含不是预检的 `OPTIONS`）照常要令牌。
 *
 * 拒绝：地址不匹配 → 404（WS 也答 404，不升级）；之后的拒绝 HTTP 答 `{ code, message }`，
 * WS 先完成升级再以关闭码结束（浏览器读不到升级失败的状态码）：令牌 4401 / 4403、
 * 限流 4429、源离线 4404（{@link wsCloseCode}）。
 *
 * 只读中继令牌（`ro: true`，协议 1.1）：只放行安全方法与身份域路径（协议包
 * `isReadOnlyAllowed`），其余 HTTP 答 403 `forbidden`、WS 以 4403 关闭；媒体票的签票与兑票
 * 是读，照常放行。纵深防御，core 仍按自己的授予判定。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import { isReadOnlyAllowed } from "@armadra/platform-protocol/assertion";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { CLOSE, LIMITS } from "@armadra/platform-protocol/tunnel";

import type { AuthContext } from "../auth/context.ts";
import {
  relayTokenFromHeaders,
  splitRelaySubprotocol,
  verifyRelayToken,
} from "../auth/relay-token.ts";
import type { ControlPlane } from "../control/types.ts";
import type { RelayMetrics } from "../metrics.ts";
import type { StreamEndpoint } from "../tunnel/stream.ts";
import { TunnelUnavailableError, type Tunnel } from "../tunnel/tunnel.ts";
import type { Addressing, AddressTarget } from "./addressing.ts";
import {
  errorCorsHeaders,
  isPreflight,
  originAllowed,
  preflightHeaders,
} from "./cors.ts";
import { headersBytes, type HeaderBag, type HeaderPair } from "./forward.ts";
import {
  isIssuePath,
  MEDIA_REDEEM_PREFIX,
  MediaTickets,
  mediaTargetPath,
  redeemTicketOf,
  type MediaTicketStore,
} from "./media-tickets.ts";

/** 运行时交给边缘的一个请求（HTTP 或 WS 升级）的元数据；不含正文。 */
export interface EdgeRequest {
  method: string;
  /** 请求目标（路径 + query）。 */
  url: string;
  /** 按小写名取值的头（`host`、`origin`、令牌头、子协议）。 */
  headers: HeaderBag;
  /** 客户端 IP（运行时按自己的规则取：Node 直连或受信代理，Workers `CF-Connecting-IP`）。 */
  clientIp: string;
}

export interface EdgePolicyOptions<S extends StreamEndpoint> {
  nodeId: string;
  addressing: Addressing;
  auth: AuthContext;
  control: ControlPlane;
  lookup(sourceId: string): Promise<Tunnel<S> | null>;
  metrics: RelayMetrics;
  /**
   * 预检放行的来源（原生 App 的两个来源总是放行，不用列）。答案不得随源在不在线、
   * 登没登记而变，否则预检就成了探测源的手段。缺省空。
   */
  corsOrigins?: (sourceId: string) => readonly string[];
  /** 媒体票的时钟（测试用）。 */
  now?: () => number;
  /** 媒体票的存放；缺省进程内存（Workers 的 SourceDO 给 DO 存储版）。 */
  mediaTickets?: MediaTicketStore;
}

const WS_CLOSE: Partial<Record<ErrorCode, number>> = {
  relay_token_invalid: CLOSE.unauthenticated,
  relay_source_mismatch: CLOSE.forbidden,
  forbidden: CLOSE.forbidden,
  rate_limited: CLOSE.limitReached,
  limit_reached: CLOSE.limitReached,
  source_offline: CLOSE.sourceOffline,
  bad_request: CLOSE.badFrame,
};

/** 升级被拒时用来关 WS 的关闭码；没有对应码（如 `not_found`）答 undefined，改答 HTTP。 */
export function wsCloseCode(code: ErrorCode): number | undefined {
  return WS_CLOSE[code];
}

export type Admission<S extends StreamEndpoint> =
  | {
      ok: true;
      target: AddressTarget;
      tunnel: Tunnel<S>;
      subject: string;
      jti: string;
      remoteIp: string;
      /** 中继令牌带 `ro: true`。 */
      readOnly: boolean;
    }
  | {
      ok: false;
      code: ErrorCode;
      retryAfterMs?: number;
      /** 地址解析出的源（名单按源取；地址不匹配时没有）。 */
      sourceId?: string;
    };

/** 拒绝：答 `{ code, message }`；`drain` = 先把请求体读掉丢弃。 */
export interface EdgeRefusal {
  kind: "error";
  code: ErrorCode;
  retryAfterMs?: number;
  cors?: Record<string, string>;
  drain: boolean;
}

/** HTTP 请求的下一步。 */
export type RequestDecision<S extends StreamEndpoint> =
  | EdgeRefusal
  /** 预检通过：答 204（请求体丢弃）。 */
  | { kind: "preflight"; headers: Record<string, string> }
  /** 签媒体票：运行时读小 JSON 体后调 {@link EdgePolicy.issueMedia}。 */
  | { kind: "issue"; admission: Extract<Admission<S>, { ok: true }> }
  /** 开流转发（`target.path` 是转给源的路径）。 */
  | {
      kind: "forward";
      target: AddressTarget;
      tunnel: Tunnel<S>;
      remoteIp: string;
    };

/** WS 升级的下一步。 */
export type UpgradeDecision<S extends StreamEndpoint> =
  | { kind: "refuse"; code: ErrorCode; sourceId?: string }
  | {
      kind: "accept";
      target: AddressTarget;
      tunnel: Tunnel<S>;
      subject: string;
      remoteIp: string;
      /** 剥掉中继令牌后的子协议（转给源）。 */
      subprotocols: string[];
    };

/** {@link EdgePolicy.disconnect} 的条件：都给时要同时满足。 */
export interface EdgeConnectionFilter {
  sourceId?: string;
  /** 中继令牌的 `sub` 以它开头（访客：`guest:<linkId>:`）。 */
  subjectPrefix?: string;
}

interface EdgeConnection {
  sourceId: string;
  subject: string;
}

function originOf(req: EdgeRequest): string | null {
  const origin = req.headers.origin;
  return typeof origin === "string" ? origin : null;
}

function single(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value.join(", ") : value;
}

/** `C` 是运行时里一条已升级连接的句柄（Node 是 socket）。 */
export class EdgePolicy<S extends StreamEndpoint, C = unknown> {
  private readonly options: EdgePolicyOptions<S>;
  private readonly media: MediaTicketStore;
  private readonly connections = new Map<C, EdgeConnection>();

  constructor(options: EdgePolicyOptions<S>) {
    this.options = options;
    this.media = options.mediaTickets ?? new MediaTickets(options.now);
  }

  resolve(req: EdgeRequest): AddressTarget | null {
    return this.options.addressing.resolve({
      url: req.url,
      host: single(req.headers.host),
    });
  }

  /** HTTP 请求：预检、兑票、签票、只读门与转发的判定（指标在这里计）。 */
  async decideRequest(req: EdgeRequest): Promise<RequestDecision<S>> {
    if (isPreflight(req)) return this.preflight(req);
    const addressed = this.resolve(req);
    if (addressed?.path.startsWith(MEDIA_REDEEM_PREFIX))
      return this.redeemMedia(req, addressed, redeemTicketOf(addressed.path));
    const admission = await this.admit(req, relayTokenFromHeaders(req.headers));
    if (!admission.ok) {
      this.options.metrics.edgeConnection(admission.code);
      return {
        kind: "error",
        code: admission.code,
        retryAfterMs: admission.retryAfterMs,
        cors: this.errorCors(req, admission.sourceId),
        drain: false,
      };
    }
    if (isIssuePath(admission.target.path)) return { kind: "issue", admission };
    if (this.readOnlyRefused(req, admission)) {
      this.options.metrics.edgeConnection("forbidden");
      return {
        kind: "error",
        code: "forbidden",
        cors: this.errorCors(req, admission.target.sourceId),
        drain: true,
      };
    }
    return {
      kind: "forward",
      target: admission.target,
      tunnel: admission.tunnel,
      remoteIp: admission.remoteIp,
    };
  }

  /** WS 升级：令牌（子协议）、只读门、按账号的并发上限。 */
  async decideUpgrade(req: EdgeRequest): Promise<UpgradeDecision<S>> {
    const { token, rest } = splitRelaySubprotocol(
      single(req.headers["sec-websocket-protocol"]),
    );
    const admission = await this.admit(req, token);
    if (!admission.ok) {
      this.options.metrics.edgeConnection(admission.code);
      return {
        kind: "refuse",
        code: admission.code,
        sourceId: admission.sourceId,
      };
    }
    if (this.readOnlyRefused(req, admission)) {
      this.options.metrics.edgeConnection("forbidden");
      return {
        kind: "refuse",
        code: "forbidden",
        sourceId: admission.target.sourceId,
      };
    }
    const { target, tunnel, subject, remoteIp } = admission;
    const concurrent = await this.options.control.limits.hit(
      "edge.concurrent.acct",
      subject,
    );
    if (!concurrent.allowed) {
      this.options.metrics.edgeConnection("limit_reached");
      return {
        kind: "refuse",
        code: "limit_reached",
        sourceId: target.sourceId,
      };
    }
    return {
      kind: "accept",
      target,
      tunnel,
      subject,
      remoteIp,
      subprotocols: rest,
    };
  }

  /** 已接受的升级结束：还回 `edge.concurrent.acct`。 */
  releaseUpgrade(subject: string): void {
    void this.options.control.limits.release?.("edge.concurrent.acct", subject);
  }

  /**
   * 签媒体票：令牌已验过（{@link admit}）。体 `{ path }`，答
   * `{ ticket, path: "/_relay/m/<票>", expiresAt }`——`path` 相对源地址。
   */
  issueMedia(
    admission: Extract<Admission<S>, { ok: true }>,
    body: unknown,
  ): { ok: false; code: ErrorCode } | { ok: true; json: string } {
    const path = mediaTargetPath(body);
    if (path === null) return { ok: false, code: "bad_request" };
    const issued = this.media.issue({
      sourceId: admission.target.sourceId,
      path,
      subject: admission.subject,
      jti: admission.jti,
    });
    this.options.metrics.edgeConnection("ok");
    return {
      ok: true,
      json: JSON.stringify({
        ticket: issued.ticket,
        path: `${MEDIA_REDEEM_PREFIX}${issued.ticket}`,
        expiresAt: new Date(issued.expiresAtMs).toISOString(),
      }),
    };
  }

  /**
   * 兑媒体票：不要令牌，票就是凭据。票不认识、过期、换了源或签票令牌被撤销按 IP
   * 严格限流后答 404；认得的按签票令牌与账号计数（与带头的请求同一组桶），再按票里
   * 的路径转给源。只认 GET / HEAD。
   */
  private async redeemMedia(
    req: EdgeRequest,
    target: AddressTarget,
    ticket: string | null,
  ): Promise<RequestDecision<S>> {
    const { control, lookup } = this.options;
    const cors = this.errorCors(req, target.sourceId);
    const remoteIp = req.clientIp;
    const refuse = (code: ErrorCode): EdgeRefusal => {
      this.options.metrics.edgeConnection(code);
      return { kind: "error", code, cors, drain: true };
    };
    const limited = (retryAfterMs: number | undefined): EdgeRefusal => {
      this.options.metrics.edgeConnection("rate_limited");
      return {
        kind: "error",
        code: "rate_limited",
        retryAfterMs,
        cors,
        drain: false,
      };
    };
    if (req.method !== "GET" && req.method !== "HEAD")
      return refuse("bad_request");
    const grant = ticket === null ? undefined : this.media.use(ticket);
    const revoked =
      grant !== undefined && (await control.tokens.isRevoked(grant.jti));
    if (revoked && ticket !== null) this.media.forget(ticket);
    if (grant === undefined || revoked || grant.sourceId !== target.sourceId) {
      const byIp = await control.limits.hit("edge.connect.ip", remoteIp);
      if (!byIp.allowed) return limited(byIp.retryAfterMs);
      return refuse("not_found");
    }
    for (const [kind, key] of [
      ["edge.connect.token", grant.jti],
      ["edge.connect.acct", grant.subject],
    ] as const) {
      const hit = await control.limits.hit(kind, key);
      if (!hit.allowed) return limited(hit.retryAfterMs);
    }
    const tunnel = await lookup(grant.sourceId);
    if (!tunnel) return refuse("source_offline");
    return {
      kind: "forward",
      target: { sourceId: grant.sourceId, path: grant.path },
      tunnel,
      remoteIp,
    };
  }

  /** 预检：解析源 → 按 IP 限流（预检专用桶）→ 来源名单；不验令牌、不碰隧道，请求体丢掉。 */
  private async preflight(req: EdgeRequest): Promise<RequestDecision<S>> {
    const { control, corsOrigins } = this.options;
    const target = this.resolve(req);
    if (!target)
      return {
        kind: "error",
        code: "not_found",
        cors: this.errorCors(req),
        drain: true,
      };
    const byIp = await control.limits.hit("edge.preflight.ip", req.clientIp);
    if (!byIp.allowed) {
      this.options.metrics.edgeConnection("rate_limited");
      return {
        kind: "error",
        code: "rate_limited",
        retryAfterMs: byIp.retryAfterMs,
        cors: this.errorCors(req, target.sourceId),
        drain: true,
      };
    }
    const origin = originOf(req) as string;
    if (!originAllowed(origin, corsOrigins?.(target.sourceId) ?? [])) {
      this.options.metrics.edgeConnection("forbidden");
      return { kind: "error", code: "forbidden", drain: true };
    }
    this.options.metrics.edgeConnection("preflight");
    return { kind: "preflight", headers: preflightHeaders(origin) };
  }

  /** 错误答复的 CORS 头：名单与预检同一份（地址不匹配时只有原生 App 来源）。 */
  errorCors(
    req: EdgeRequest,
    sourceId?: string,
  ): Record<string, string> | undefined {
    const trusted =
      sourceId === undefined
        ? []
        : (this.options.corsOrigins?.(sourceId) ?? []);
    return errorCorsHeaders(originOf(req), trusted);
  }

  /** 只读令牌下不放行的请求（方法不安全且不在身份域）。 */
  private readOnlyRefused(
    req: EdgeRequest,
    admission: Extract<Admission<S>, { ok: true }>,
  ): boolean {
    return (
      admission.readOnly &&
      !isReadOnlyAllowed(req.method, admission.target.path)
    );
  }

  /** 解析源 → 中继令牌（无效按 IP 限流；有效按令牌与账号限流）→ 找隧道。 */
  private async admit(
    req: EdgeRequest,
    token: string | undefined,
  ): Promise<Admission<S>> {
    const { control, auth, lookup } = this.options;
    const target = this.resolve(req);
    if (!target) return { ok: false, code: "not_found" };
    const { sourceId } = target;
    const remoteIp = req.clientIp;
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
    return {
      ok: true,
      target,
      tunnel,
      subject,
      jti: verified.claims.jti,
      remoteIp,
      readOnly: verified.claims.ro === true,
    };
  }

  /**
   * 为一个请求开流（发 OPEN）。头超过 `openHeadersMaxBytes` 答 `bad_request`；隧道排空 /
   * 关闭答 `source_offline`，满流答 `limit_reached`。开成了计一次 `ok`。
   */
  open(
    tunnel: Tunnel<S>,
    kind: "http" | "ws",
    req: EdgeRequest,
    target: AddressTarget,
    headers: HeaderPair[],
    remoteIp: string,
  ): { ok: true; stream: S } | { ok: false; code: ErrorCode } {
    if (headersBytes(headers) > LIMITS.openHeadersMaxBytes)
      return { ok: false, code: "bad_request" };
    let stream: S;
    try {
      stream = tunnel.openStream({
        kind,
        method: req.method,
        path: target.path,
        headers,
        clientOrigin: originOf(req),
        remoteIp,
        relayNode: this.options.nodeId,
      });
    } catch (error) {
      if (error instanceof TunnelUnavailableError)
        return {
          ok: false,
          code:
            error.reason === "max_streams" ? "limit_reached" : "source_offline",
        };
      throw error;
    }
    this.options.metrics.edgeConnection("ok");
    return { ok: true, stream };
  }

  // ------------------------------------------------------------ 已升级的连接

  track(connection: C, sourceId: string, subject: string): void {
    this.connections.set(connection, { sourceId, subject });
  }

  untrack(connection: C): void {
    this.connections.delete(connection);
  }

  /** 摘掉并交出全部已升级连接（关闭时断开）。 */
  takeAll(): C[] {
    const all = [...this.connections.keys()];
    this.connections.clear();
    return all;
  }

  /** 摘掉并交出匹配的已升级连接（撤销源 / 访客）。 */
  take(filter: EdgeConnectionFilter): C[] {
    const taken: C[] = [];
    for (const [handle, connection] of this.connections) {
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
      this.connections.delete(handle);
      taken.push(handle);
    }
    return taken;
  }
}
