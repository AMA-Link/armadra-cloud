/**
 * 客户端边缘的判定（中继设计 §4），与运行时无关：解析源 → 限流 → 校验中继令牌 → 找隧道
 * → 决定开通道还是拒绝。字节怎么进出由运行时负责（Node 在 `@armadra/relay-runtime-node`
 * 的 `edge.ts`），这里只看 {@link EdgeRequest}，答一个「下一步做什么」。对模式零分支：地址
 * 写法由 {@link Addressing} 决定，控制面由 `ControlPlane` 决定。
 *
 * 只转端到端通道（0.5.0 起，cloud-api §20）：源地址上唯一的入口是 `_relay/e2e` 的 WS
 * 升级，要带子协议 `armadra-e2e.1`（缺了 4409），源要在 `hello.capabilities` 报过 `e2e`
 * （没报 4404，原因 `e2e_unsupported`）；开的是 `OPEN kind: "e2e"`，之后字节两边透传，
 * 中继不解析。明文的应用请求（HTTP、普通 WS 升级、CORS 预检）与媒体票一并删除，源地址
 * 上的其它请求一律 `404 not_found`。
 *
 * 拒绝：地址不匹配或不是通道 → 404（WS 也答 404，不升级）；之后 WS 先完成升级再以关闭码
 * 结束（浏览器读不到升级失败的状态码）：令牌 4401 / 4403、限流 4429、源离线 4404
 * （{@link wsCloseCode}）。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import {
  E2E_CAPABILITY,
  E2E_PATH,
  E2E_SUBPROTOCOL,
} from "@armadra/platform-protocol/e2e";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { CLOSE } from "@armadra/platform-protocol/tunnel";

import type { AuthContext } from "../auth/context.ts";
import {
  splitRelaySubprotocol,
  verifyRelayToken,
} from "../auth/relay-token.ts";
import type { ControlPlane } from "../control/types.ts";
import type { RelayMetrics } from "../metrics.ts";
import type { TrafficCounter, TrafficMeter } from "../traffic.ts";
import type { StreamEndpoint } from "../tunnel/stream.ts";
import { TunnelUnavailableError, type Tunnel } from "../tunnel/tunnel.ts";
import type { Addressing, AddressTarget } from "./addressing.ts";
import { errorCorsHeaders } from "./cors.ts";
import type { HeaderBag } from "./headers.ts";

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
   * 错误答复回显 CORS 的来源（原生 App 的两个来源总是回显，不用列）。答案不得随源在
   * 不在线、登没登记而变。缺省空。
   */
  corsOrigins?: (sourceId: string) => readonly string[];
  /** 流量统计（cloud-api §19）；不给就不统计（保留期为 0）。 */
  traffic?: TrafficMeter;
}

/** 一条通道记到谁名下（流量统计，类别 `e2e`）：中继令牌的 `sub` / `dev`。 */
export interface TrafficIdentity {
  subject: string;
  deviceId: string;
}

const WS_CLOSE: Partial<Record<ErrorCode, number>> = {
  relay_token_invalid: CLOSE.unauthenticated,
  relay_source_mismatch: CLOSE.forbidden,
  forbidden: CLOSE.forbidden,
  rate_limited: CLOSE.limitReached,
  limit_reached: CLOSE.limitReached,
  source_offline: CLOSE.sourceOffline,
  bad_request: CLOSE.badFrame,
  protocol_unsupported: CLOSE.protocolUnsupported,
};

/** 端到端通道的边缘路径（地址解析之后、转给源之前的路径；可以带 query）。 */
export function isE2ePath(path: string): boolean {
  return path === E2E_PATH || path.startsWith(`${E2E_PATH}?`);
}

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
      /** 中继令牌的 `dev`（1.3，可选；只计数）。 */
      device?: string;
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

/** HTTP 请求的下一步：只有拒绝（中继不转明文的应用请求）。 */
export type RequestDecision = EdgeRefusal;

/** WS 升级的下一步。 */
export type UpgradeDecision<S extends StreamEndpoint> =
  | {
      kind: "refuse";
      code: ErrorCode;
      sourceId?: string;
      /** 关闭原因（缺省就是 `code`）。 */
      reason?: string;
    }
  | {
      /** 端到端通道：中继自己终止 WS，开 `OPEN kind: "e2e"`。 */
      kind: "accept";
      target: AddressTarget;
      tunnel: Tunnel<S>;
      subject: string;
      remoteIp: string;
      /** 中继答复时选的子协议（`armadra-e2e.1`）。 */
      subprotocols: string[];
      /** 中继令牌的 `dev`（1.3，可选；只计数）。 */
      device?: string;
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
  private readonly connections = new Map<C, EdgeConnection>();

  constructor(options: EdgePolicyOptions<S>) {
    this.options = options;
  }

  resolve(req: EdgeRequest): AddressTarget | null {
    return this.options.addressing.resolve({
      url: req.url,
      host: single(req.headers.host),
    });
  }

  /**
   * HTTP 请求：0.5.0 起中继不转明文的应用请求（cloud-api §20），源地址上的 HTTP 一律
   * `404 not_found`（预检同样），不验令牌、不找隧道。端到端通道只认 WS 升级。
   */
  decideRequest(req: EdgeRequest): RequestDecision {
    const addressed = this.resolve(req);
    this.options.metrics.edgeConnection("not_found");
    return {
      kind: "error",
      code: "not_found",
      cors: this.errorCors(req, addressed?.sourceId),
      drain: true,
    };
  }

  /**
   * WS 升级：只认 `_relay/e2e`（别的路径 `404 not_found`，不升级）。令牌（子协议）、
   * 版本子协议与源的 `e2e` 能力（令牌先验，未授权的请求看不到源支不支持）、按账号的
   * 并发上限。
   */
  async decideUpgrade(req: EdgeRequest): Promise<UpgradeDecision<S>> {
    const addressed = this.resolve(req);
    if (addressed === null || !isE2ePath(addressed.path)) {
      this.options.metrics.edgeConnection("not_found");
      return {
        kind: "refuse",
        code: "not_found",
        ...(addressed === null ? {} : { sourceId: addressed.sourceId }),
      };
    }
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
    const { target, tunnel, subject, remoteIp, device } = admission;
    if (!rest.includes(E2E_SUBPROTOCOL)) {
      this.options.metrics.edgeConnection("protocol_unsupported");
      return {
        kind: "refuse",
        code: "protocol_unsupported",
        sourceId: target.sourceId,
      };
    }
    if (!tunnel.capabilities.has(E2E_CAPABILITY)) {
      this.options.metrics.edgeConnection("source_offline");
      return {
        kind: "refuse",
        code: "source_offline",
        sourceId: target.sourceId,
        reason: "e2e_unsupported",
      };
    }
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
      subprotocols: [E2E_SUBPROTOCOL],
      ...(device === undefined ? {} : { device }),
    };
  }

  /** 已接受的升级结束：还回 `edge.concurrent.acct`。 */
  releaseUpgrade(subject: string): void {
    void this.options.control.limits.release?.("edge.concurrent.acct", subject);
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
      ...(verified.claims.dev === undefined
        ? {}
        : { device: verified.claims.dev }),
    };
  }

  /**
   * 开端到端通道的流：`OPEN { kind: "e2e", method: "GET", path: "/_relay/e2e", headers: [] }`
   * （子协议、令牌、头一概不转；地址里的 query 也不转）。给了 `traffic` 就按 `e2e` 类别统计。
   */
  openE2e(
    tunnel: Tunnel<S>,
    req: EdgeRequest,
    remoteIp: string,
    traffic?: TrafficIdentity,
  ): { ok: true; stream: S } | { ok: false; code: ErrorCode } {
    const counter =
      traffic && this.options.traffic
        ? this.options.traffic.counter({
            sourceId: tunnel.sourceId,
            subject: traffic.subject,
            deviceId: traffic.deviceId,
            kind: "e2e",
          })
        : undefined;
    return this.openStream(
      tunnel,
      { clientOrigin: originOf(req), remoteIp },
      counter,
    );
  }

  private openStream(
    tunnel: Tunnel<S>,
    client: { clientOrigin: string | null; remoteIp: string },
    counter?: TrafficCounter,
  ): { ok: true; stream: S } | { ok: false; code: ErrorCode } {
    let stream: S;
    try {
      stream = tunnel.openStream(
        {
          kind: "e2e",
          method: "GET",
          path: E2E_PATH,
          headers: [],
          clientOrigin: client.clientOrigin,
          remoteIp: client.remoteIp,
          relayNode: this.options.nodeId,
        },
        counter,
      );
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
    this.options.metrics.edgeStream("e2e");
    counter?.connection();
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
