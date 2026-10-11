/**
 * `SourceDO`：一源一个 Durable Object（中继设计 §2.1–§2.5）。持有这个源当前的隧道 WS、经它的
 * 客户端 WS，自己做客户端 HTTP / WS 的转发；隧道与客户端都在同一个对象里，「边缘 → 隧道」不用
 * 跨节点查路由。
 *
 * - **隧道**：握手与帧处理照搬内核的 `Handshake` / `Tunnel`。心跳「源主动、中继被动」：`ready`
 *   下发 `heartbeatMs`（缺省 45 秒），源发 `PING`、中继立即回 `PONG`，自己不起定时器（会阻止休眠）；
 *   检查周期（alarm，缺省 60 秒）看 `lastHeardAtMs`，超过 `2 × heartbeatMs` 就关。同源新隧道就绪 →
 *   旧的 `GOAWAY replaced`。握手里的 `sourceId` 必须是这个对象的源（`?src=` 只是路由提示）。
 * - **客户端**：判定在内核的 `EdgePolicy`（地址、令牌、限流、媒体票、CORS 预检）；字节在
 *   `http-forward.ts` 与 `ws-bridge.ts`；端到端通道（`_relay/e2e`）在 `e2e-bridge.ts`，WS 消息与
 *   `DATA` 原样互转、不经 `http-forward`。媒体票落在 DO 的同步 KV 里，休眠后照样认。
 * - **休眠**：隧道与客户端 WS 都经 Hibernation API 接受；热状态写进 WS 附件（`hibernation.ts`），
 *   唤醒时在构造函数里还原。
 * - **控制面**：personal 的 JWKS、撤销判断、可信来源与在线事件都经 RPC 问同一个 `ControlDO`（结果
 *   短缓存，撤销 / 改设置时 `ControlDO` 调 {@link SourceDO.invalidate} 立即作废）；saas 的 JWKS 从
 *   cloud 拉，在线事件经 `relay.nodePresence` 报给 cloud（cloud-api §17）。
 * - **流量统计**（cloud-api §19）：内核的 `TrafficMeter` 在内存里累加，攒下新行后
 *   {@link TRAFFIC_FLUSH_DELAY_MS} 与检查周期各写一次进本对象的 DO SQLite（同一份迁移的
 *   `relay_traffic_buckets`）；定时器在，对象就不会带着没写的计数休眠。`ControlDO` 查询时经
 *   {@link SourceDO.trafficQuery} 逐源取回合并。
 *
 * 日志只记元数据（内核白名单）；不记路径、头、令牌、正文。
 */
import { randomBytes } from "node:crypto";

import { DurableObject } from "cloudflare:workers";

import {
  DEFAULT_TENANT,
  type TrafficQuery,
  type TrafficRow,
  type TrafficStore,
} from "@armadra/control-core/ports";
import { applyMigrations } from "@armadra/control-store-sqlite/apply";
import { createSqliteTraffic } from "@armadra/control-store-sqlite/traffic";

import { PROTOCOL_VERSION, TUNNEL_PATH } from "@armadra/platform-protocol";
import type { JwkSet } from "@armadra/platform-protocol/assertion";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { E2E_SUBPROTOCOL } from "@armadra/platform-protocol/e2e";
import { CLOSE, LIMITS, type Ready } from "@armadra/platform-protocol/tunnel";
import type { AuthContext } from "@armadra/relay-kernel/auth/context";
import { JwksCache } from "@armadra/relay-kernel/auth/jwks";
import {
  createMemoryControlPlane,
  type MemoryControlPlane,
} from "@armadra/relay-kernel/control/memory";
import type { PresenceEvent } from "@armadra/relay-kernel/control/types";
import {
  hostAddressing,
  pathAddressing,
  type Addressing,
} from "@armadra/relay-kernel/edge/addressing";
import {
  forwardRequestHeaders,
  serializeRequestHead,
  type HeaderBag,
} from "@armadra/relay-kernel/edge/forward";
import { MEDIA_ISSUE_BODY_MAX } from "@armadra/relay-kernel/edge/media-tickets";
import {
  EdgePolicy,
  wsCloseCode,
  type EdgeConnectionFilter,
  type EdgeRequest,
} from "@armadra/relay-kernel/edge/policy";
import { errorName, type RelayLog } from "@armadra/relay-kernel/log";
import { DEFAULT_TUNNEL_LIMITS } from "@armadra/relay-kernel/main";
import { RelayMetrics } from "@armadra/relay-kernel/metrics";
import { TrafficMeter } from "@armadra/relay-kernel/traffic";
import {
  Handshake,
  rejectStep,
  type HandshakeStep,
} from "@armadra/relay-kernel/tunnel/handshake";
import {
  Tunnel,
  type TunnelCloseInfo,
  type TunnelHooks,
  type TunnelLimitsConfig,
} from "@armadra/relay-kernel/tunnel/tunnel";

import { BudgetMeter } from "./budget.ts";
import { endpoints, TunnelEndpoint } from "./endpoint.ts";
import {
  CONTROL_OBJECT_NAME,
  ConfigError,
  readConfig,
  type RelayConfig,
  type RelayEnv,
} from "./env.ts";
import {
  attachmentOf,
  KeepAwake,
  type ClientAttachment,
  type TunnelAttachment,
} from "./hibernation.ts";
import {
  forwardHttp,
  rawHeadersOf,
  readResponse,
  responseHeaders,
} from "./http-forward.ts";
import { workersRelayLog } from "./log.ts";
import { KvMediaTickets } from "./media-tickets.ts";
import { isUpgrade, refuseWithClose, relayError } from "./responses.ts";
import { cloudJwks, reportPresence } from "./saas.ts";
import { E2eBridge } from "./e2e-bridge.ts";
import { DoSqlDatabase } from "./store-do-sqlite/database.ts";
import { bundledMigrations } from "./store-do-sqlite/migrations.ts";
import { WsBridge } from "./ws-bridge.ts";

/** 中继节点名：Workers 上没有「节点」概念，路由的 `node` 固定是 `cf`。 */
export const WORKERS_NODE = "cf";
/** 一个对象最多同时接这么多 WS（DO 硬上限 32,768，留点余量）。 */
export const MAX_OBJECT_SOCKETS = 32_000;
/** 握手阶段单条文本消息的上限。 */
const MAX_HANDSHAKE_TEXT = 64 * 1024;
/** 撤销判断的缓存：没撤销的结果最多信 10 秒（撤销时 `ControlDO` 会立即作废缓存）。 */
const REVOCATION_CACHE_MS = 10_000;
/** 可信来源的缓存。 */
const CORS_CACHE_MS = 60_000;
/** JWKS 的缓存（`kid` 不认识时立即重取）。 */
const JWKS_CACHE_MS = 60_000;
const SOURCE_ID = /^[0-9a-f]{32}$/;
const META_SOURCE = "meta:sourceId";
/** 攒下新的统计行之后多久写库（定时器在，对象不休眠，计数不会随休眠丢掉）。 */
export const TRAFFIC_FLUSH_DELAY_MS = 30_000;
const TRAFFIC_PRUNED = "traffic:prunedAtMs";
const DAY_MS = 86_400_000;

interface TunnelRecord {
  ws: WebSocket;
  tunnel: Tunnel<TunnelEndpoint>;
  attachment: TunnelAttachment;
}

interface PendingHandshake {
  handshake: Handshake;
  chain: Promise<void>;
  settled: boolean;
  timer: ReturnType<typeof setTimeout>;
  startedAt: number;
}

interface BridgeRecord {
  bridge: WsBridge | E2eBridge;
  attachment: ClientAttachment;
}

const utf8 = new TextDecoder("utf-8", { fatal: false, ignoreBOM: true });

function headerBag(headers: Headers): HeaderBag {
  const bag: Record<string, string> = {};
  for (const [name, value] of headers) bag[name] = value;
  return bag;
}

/** 读一个小 JSON 体；超长、不是 JSON 答 `undefined`。 */
async function readSmallJson(
  request: Request,
  limit: number,
): Promise<unknown> {
  if (!request.body) return undefined;
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    size += value.length;
    if (size > limit) {
      await reader.cancel().catch(() => {});
      return undefined;
    }
    chunks.push(value);
  }
  try {
    return JSON.parse(utf8.decode(Buffer.concat(chunks))) as unknown;
  } catch {
    return undefined;
  }
}

export class SourceDO extends DurableObject<RelayEnv> {
  private readonly config: RelayConfig | null;
  private readonly configError: ConfigError | null;
  private readonly log: RelayLog;
  private readonly metrics: RelayMetrics;
  private readonly control: MemoryControlPlane;
  private readonly auth: AuthContext;
  private readonly policy: EdgePolicy<TunnelEndpoint, WebSocket>;
  private readonly tickets: KvMediaTickets;
  private readonly addressing: Addressing;
  private readonly limits: TunnelLimitsConfig;
  private readonly budget: BudgetMeter;
  private readonly keepAwake = new KeepAwake();
  private sourceId: string | null;
  private current: TunnelRecord | null = null;
  private readonly tunnels = new Map<WebSocket, TunnelRecord>();
  private readonly handshakes = new Map<WebSocket, PendingHandshake>();
  private readonly bridges = new Map<WebSocket, BridgeRecord>();
  /** 流 → 它的桥（只把动过的桥的附件重写一遍）。 */
  private readonly bridgeOf = new Map<TunnelEndpoint, BridgeRecord>();
  private readonly dirty = new Set<BridgeRecord>();
  private readonly revocations = new Map<
    string,
    { revoked: boolean; atMs: number }
  >();
  private cors: { origins: string[]; atMs: number } | null = null;
  private persistQueued = false;
  private readonly traffic: TrafficMeter | undefined;
  /** 本对象的 DO SQLite 里的流量表；undefined = 还没开，null = 开不了（不统计，不影响转发）。 */
  private trafficStore: TrafficStore | null | undefined;
  private trafficTimer: ReturnType<typeof setTimeout> | undefined;

  constructor(ctx: DurableObjectState, env: RelayEnv) {
    super(ctx, env);
    let config: RelayConfig | null = null;
    let configError: ConfigError | null = null;
    try {
      config = readConfig(env);
    } catch (error) {
      configError =
        error instanceof ConfigError ? error : new ConfigError("配置不可读");
    }
    this.config = config;
    this.configError = configError;
    this.log = workersRelayLog("relay-source", config?.logLevel ?? "info");
    this.metrics = new RelayMetrics(() => this.counts());
    this.limits = {
      ...DEFAULT_TUNNEL_LIMITS,
      heartbeatMs: config?.heartbeatMs ?? DEFAULT_TUNNEL_LIMITS.heartbeatMs,
    };
    const saas = config?.mode === "saas";
    this.control = createMemoryControlPlane({
      mode: config?.mode ?? "personal",
      issuer: saas ? (config?.cloudIssuer ?? "") : (config?.issuer ?? ""),
      jwks: () => this.jwks(),
      revoked: (jti) => this.isRevoked(jti),
    });
    this.auth = {
      control: this.control,
      jwks: new JwksCache(() => this.control.jwks(), { ttlMs: JWKS_CACHE_MS }),
      now: Date.now,
    };
    this.tickets = new KvMediaTickets(ctx.storage.kv);
    this.traffic =
      config && config.trafficRetentionDays > 0
        ? new TrafficMeter({
            node: WORKERS_NODE,
            write: (rows) => this.writeTraffic(rows),
            onRow: () => this.scheduleTrafficFlush(),
            onError: (error, dropped) =>
              this.log.warn("流量统计写入失败", {
                error: errorName(error),
                result: dropped > 0 ? "dropped" : "retry",
              }),
          })
        : undefined;
    this.addressing =
      saas && config?.domain ? hostAddressing(config.domain) : pathAddressing();
    this.policy = new EdgePolicy<TunnelEndpoint, WebSocket>({
      nodeId: WORKERS_NODE,
      addressing: this.addressing,
      auth: this.auth,
      control: this.control,
      lookup: async (sourceId) => this.lookup(sourceId),
      metrics: this.metrics,
      corsOrigins: () => this.cors?.origins ?? [],
      mediaTickets: this.tickets,
      traffic: this.traffic,
    });
    this.budget = new BudgetMeter((delta) => this.flushBudget(delta));
    this.sourceId = ctx.storage.kv.get<string>(META_SOURCE) ?? null;
    this.restore();
  }

  // ------------------------------------------------------------ HTTP 入口

  async fetch(request: Request): Promise<Response> {
    if (!this.config) {
      this.log.error("配置不对，拒绝服务", {
        reason: this.configError?.message ?? "unknown",
      });
      return relayError("unavailable");
    }
    const url = new URL(request.url);
    try {
      if (url.pathname === TUNNEL_PATH)
        return await this.tunnelUpgrade(request, url);
      return await this.edge(request, url);
    } catch (error) {
      this.log.error("边缘请求异常", { error: errorName(error) });
      return relayError("internal");
    }
  }

  /** 认下这个对象的源（第一次请求时记进存储）；对不上答 false。 */
  private claim(sourceId: string): boolean {
    if (!SOURCE_ID.test(sourceId)) return false;
    const named = this.ctx.id.name;
    if (named !== undefined && named !== `src:${sourceId}`) return false;
    if (this.sourceId === null) {
      this.sourceId = sourceId;
      this.ctx.storage.kv.put(META_SOURCE, sourceId);
    }
    return this.sourceId === sourceId;
  }

  // ------------------------------------------------------------ 隧道

  private async tunnelUpgrade(request: Request, url: URL): Promise<Response> {
    const sourceId = url.searchParams.get("src") ?? "";
    if (!isUpgrade(request) || !this.claim(sourceId))
      return relayError("not_found");
    if (this.budget.exceeded) return relayError("limit_reached");
    if (this.ctx.getWebSockets().length >= MAX_OBJECT_SOCKETS)
      return relayError("limit_reached");
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server, ["tunnel"]);
    const attachment: TunnelAttachment = {
      kind: "tunnel",
      phase: "handshake",
      sourceId,
      lastHeardAtMs: Date.now(),
      snapshot: null,
    };
    server.serializeAttachment(attachment);
    this.startHandshake(server);
    return new Response(null, { status: 101, webSocket: client });
  }

  private startHandshake(ws: WebSocket): void {
    const pending: PendingHandshake = {
      handshake: new Handshake({
        auth: this.auth,
        limits: this.control.limits,
        relayNode: WORKERS_NODE,
      }),
      chain: Promise.resolve(),
      settled: false,
      startedAt: performance.now(),
      timer: setTimeout(() => {
        this.settleHandshake(ws, "timeout");
        this.closeSocket(ws, CLOSE.badFrame, "handshake_timeout");
      }, LIMITS.handshakeTimeoutMs),
    };
    this.handshakes.set(ws, pending);
  }

  private settleHandshake(ws: WebSocket, result: string): void {
    const pending = this.handshakes.get(ws);
    if (!pending || pending.settled) return;
    pending.settled = true;
    clearTimeout(pending.timer);
    this.handshakes.delete(ws);
    this.metrics.handshake(
      result,
      result === "ok"
        ? (performance.now() - pending.startedAt) / 1000
        : undefined,
    );
    if (result !== "ok")
      this.log.warn("隧道握手失败", {
        result,
        sourceId: pending.handshake.sourceId,
      });
  }

  private handshakeMessage(
    ws: WebSocket,
    pending: PendingHandshake,
    message: string | ArrayBuffer,
  ): void {
    if (typeof message !== "string") {
      this.settleHandshake(ws, "binary_before_ready");
      this.closeSocket(ws, CLOSE.badFrame, "binary_before_ready");
      return;
    }
    if (message.length > MAX_HANDSHAKE_TEXT) {
      this.settleHandshake(ws, "bad_hello");
      this.closeSocket(ws, CLOSE.badFrame, "bad_hello");
      return;
    }
    pending.chain = pending.chain
      .then(async () => {
        if (pending.settled) return;
        const step = await pending.handshake.onText(message);
        await this.handshakeStep(ws, step);
      })
      .catch((error: unknown) => {
        this.log.error("隧道握手异常", { error: errorName(error) });
        this.settleHandshake(ws, "internal");
        this.closeSocket(ws, 1011, "internal");
      });
  }

  private async handshakeStep(
    ws: WebSocket,
    step: HandshakeStep,
  ): Promise<void> {
    switch (step.kind) {
      case "send":
        ws.send(JSON.stringify(step.message));
        return;
      case "reject":
        this.settleHandshake(ws, step.message.code);
        ws.send(JSON.stringify(step.message));
        this.closeSocket(ws, step.closeCode, step.message.code);
        return;
      case "fail":
        this.settleHandshake(ws, step.reason);
        this.closeSocket(ws, step.closeCode, step.reason);
        return;
      case "ready": {
        // `?src=` 只是路由：身份以隧道令牌的 `sub`（= hello.sourceId）为准，必须是这个对象的源。
        if (step.hello.sourceId !== this.sourceId) {
          const reject = rejectStep("tunnel_token_invalid");
          if (reject.kind === "reject") {
            this.settleHandshake(ws, "source_mismatch");
            ws.send(JSON.stringify(reject.message));
            this.closeSocket(ws, reject.closeCode, reject.message.code);
          }
          return;
        }
        this.ready(ws, step);
        return;
      }
    }
  }

  private ready(
    ws: WebSocket,
    step: Extract<HandshakeStep, { kind: "ready" }>,
  ): void {
    const { sourceId, coreVersion } = step.hello;
    const tunnelId = randomBytes(16).toString("hex");
    const now = Date.now();
    const attachment: TunnelAttachment = {
      kind: "tunnel",
      phase: "ready",
      sourceId,
      lastHeardAtMs: now,
      readyAtMs: now,
      snapshot: null,
    };
    const record = this.tunnelRecord(
      ws,
      attachment,
      (hooks) =>
        new Tunnel<TunnelEndpoint>(
          tunnelId,
          sourceId,
          coreVersion,
          this.transport(ws),
          this.limits,
          hooks,
          endpoints((endpoint) => this.touched(endpoint)),
        ),
    );
    record.tunnel.capabilities = new Set(step.hello.capabilities);
    this.settleHandshake(ws, "ok");
    const previous = this.current;
    this.current = record;
    previous?.tunnel.goaway("replaced");
    const ready: Ready = {
      type: "ready",
      tunnelId,
      limits: {
        maxStreams: this.limits.maxStreams,
        streamWindow: this.limits.streamWindow,
        tunnelWindow: this.limits.tunnelWindow,
        maxFrameBytes: LIMITS.maxFrameBytes,
      },
      heartbeatMs: this.limits.heartbeatMs,
      protocol: { ...PROTOCOL_VERSION },
    };
    ws.send(JSON.stringify(ready));
    this.publish({
      type: "sourceOnline",
      sourceId,
      node: WORKERS_NODE,
      tunnelId,
      coreVersion,
      atMs: now,
    });
    this.log.info("隧道已就绪", { sourceId, tunnelId });
    void this.ensureAlarm();
    this.persist();
  }

  private tunnelRecord(
    ws: WebSocket,
    attachment: TunnelAttachment,
    build: (hooks: TunnelHooks) => Tunnel<TunnelEndpoint>,
  ): TunnelRecord {
    const holder: { record?: TunnelRecord } = {};
    const tunnel = build({
      onBytes: (direction, bytes) => {
        this.metrics.addBytes(direction, bytes);
        this.budget.bytes(bytes);
      },
      onClose: (info) => {
        if (holder.record) this.tunnelClosed(holder.record, info);
      },
    });
    const record: TunnelRecord = { ws, tunnel, attachment };
    holder.record = record;
    this.tunnels.set(ws, record);
    return record;
  }

  private transport(ws: WebSocket) {
    return {
      send: (bytes: Uint8Array) => {
        try {
          ws.send(bytes);
        } catch {
          // 已经断了：close 事件随后到。
        }
      },
      close: (code: number, reason: string) =>
        this.closeSocket(ws, code, reason),
      terminate: () => this.closeSocket(ws, CLOSE.goingAway, "terminated"),
    };
  }

  private tunnelClosed(record: TunnelRecord, info: TunnelCloseInfo): void {
    this.tunnels.delete(record.ws);
    this.log.info("隧道已关闭", {
      sourceId: record.tunnel.sourceId,
      tunnelId: record.tunnel.tunnelId,
      code: info.code,
      reason: info.reason,
    });
    if (this.current !== record) return;
    this.current = null;
    this.persist();
    if (info.replaced) return;
    this.publish({
      type: "sourceOffline",
      sourceId: record.tunnel.sourceId,
      node: WORKERS_NODE,
      tunnelId: record.tunnel.tunnelId,
      atMs: Date.now(),
    });
  }

  private tunnelMessage(record: TunnelRecord, message: string | ArrayBuffer) {
    record.attachment.lastHeardAtMs = Date.now();
    if (typeof message === "string") {
      record.tunnel.close(CLOSE.badFrame, "text_after_ready");
      return;
    }
    if (message.byteLength > LIMITS.maxFrameBytes + 16) {
      record.tunnel.close(CLOSE.frameTooLarge, "frame_too_large");
      return;
    }
    record.tunnel.receive(new Uint8Array(message));
  }

  private lookup(sourceId: string): Tunnel<TunnelEndpoint> | null {
    if (sourceId !== this.sourceId) return null;
    const tunnel = this.current?.tunnel;
    return tunnel?.accepting ? tunnel : null;
  }

  // ------------------------------------------------------------ 客户端边缘

  private async edge(request: Request, url: URL): Promise<Response> {
    const host = request.headers.get("host") ?? url.host;
    const target = this.addressing.resolve({
      url: `${url.pathname}${url.search}`,
      host,
    });
    if (!target || !this.claim(target.sourceId)) return relayError("not_found");
    this.budget.request();
    await this.refreshCors();
    const edgeRequest: EdgeRequest = {
      method: request.method,
      url: `${url.pathname}${url.search}`,
      headers: headerBag(request.headers),
      clientIp: request.headers.get("cf-connecting-ip") ?? "0.0.0.0",
    };
    if (this.budget.exceeded)
      return relayError("limit_reached", {
        cors: this.policy.errorCors(edgeRequest, target.sourceId),
      });
    if (isUpgrade(request)) return this.edgeUpgrade(request, edgeRequest);
    const decision = await this.policy.decideRequest(edgeRequest);
    switch (decision.kind) {
      case "error":
        return relayError(decision.code, {
          retryAfterMs: decision.retryAfterMs,
          cors: decision.cors,
        });
      case "preflight":
        return new Response(null, { status: 204, headers: decision.headers });
      case "issue": {
        const cors = this.policy.errorCors(
          edgeRequest,
          decision.admission.target.sourceId,
        );
        if (request.method !== "POST")
          return relayError("bad_request", { cors });
        const body = await readSmallJson(request, MEDIA_ISSUE_BODY_MAX);
        const issued = this.policy.issueMedia(decision.admission, body);
        if (!issued.ok) return relayError(issued.code, { cors });
        return new Response(issued.json, {
          status: 200,
          headers: {
            ...(cors ?? {}),
            "content-type": "application/json",
            "cache-control": "no-store",
          },
        });
      }
      case "forward":
        return forwardHttp({
          request,
          edgeRequest,
          target: decision.target,
          tunnel: decision.tunnel,
          remoteIp: decision.remoteIp,
          policy: this.policy,
          log: this.log,
          traffic: decision.traffic,
        });
    }
  }

  private refuseUpgrade(
    request: Request,
    edgeRequest: EdgeRequest,
    code: ErrorCode,
    sourceId?: string,
    reason?: string,
  ): Response {
    const closeCode = wsCloseCode(code);
    if (closeCode !== undefined && request.headers.has("sec-websocket-key"))
      return refuseWithClose(request, closeCode, reason ?? code);
    return relayError(code, {
      cors: this.policy.errorCors(edgeRequest, sourceId),
    });
  }

  private async edgeUpgrade(
    request: Request,
    edgeRequest: EdgeRequest,
  ): Promise<Response> {
    const decision = await this.policy.decideUpgrade(edgeRequest);
    if (decision.kind === "refuse")
      return this.refuseUpgrade(
        request,
        edgeRequest,
        decision.code,
        decision.sourceId,
        decision.reason,
      );
    const { target, tunnel, subject, remoteIp } = decision;
    const deviceId = decision.device ?? "";
    const release = () => this.policy.releaseUpgrade(subject);
    if (this.ctx.getWebSockets().length >= MAX_OBJECT_SOCKETS) {
      release();
      return this.refuseUpgrade(
        request,
        edgeRequest,
        "limit_reached",
        target.sourceId,
      );
    }
    if (decision.mode === "e2e") {
      const opened = this.policy.openE2e(tunnel, edgeRequest, remoteIp, {
        subject,
        deviceId,
      });
      if (!opened.ok) {
        release();
        return this.refuseUpgrade(
          request,
          edgeRequest,
          opened.code,
          target.sourceId,
        );
      }
      const pair = new WebSocketPair();
      const [client, server] = [pair[0], pair[1]];
      this.ctx.acceptWebSocket(server, ["client"]);
      const record = this.bridgeRecord(server, opened.stream, {
        kind: "client",
        mode: "e2e",
        sourceId: target.sourceId,
        tunnelId: tunnel.tunnelId,
        streamId: opened.stream.streamId,
        subject,
        ...(deviceId ? { deviceId } : {}),
        lastActivityMs: Date.now(),
        core: null,
      });
      record.bridge.start();
      this.persist();
      void this.ensureAlarm();
      return new Response(null, {
        status: 101,
        webSocket: client,
        headers: { "sec-websocket-protocol": E2E_SUBPROTOCOL },
      });
    }
    // 不协商扩展：中继自己解帧，`permessage-deflate` 的 RSV 位会被当成协议错误。
    const headers = forwardRequestHeaders(
      rawHeadersOf(
        request.headers,
        (name) => name === "sec-websocket-extensions",
      ),
      { remoteIp, upgrade: { subprotocols: decision.subprotocols } },
    );
    const opened = this.policy.open(
      tunnel,
      "ws",
      edgeRequest,
      target,
      headers,
      remoteIp,
      { subject, deviceId },
    );
    if (!opened.ok) {
      release();
      return this.refuseUpgrade(
        request,
        edgeRequest,
        opened.code,
        target.sourceId,
      );
    }
    const endpoint = opened.stream;
    const answer = readResponse(endpoint, "GET", { upgrade: true });
    void endpoint
      .write(serializeRequestHead(request.method, target.path, headers))
      .catch(() => {});
    const result = await answer;
    const cors = this.policy.errorCors(edgeRequest, target.sourceId);
    if (!result.ok) {
      release();
      return relayError(result.code, { cors });
    }
    const { head } = result.response;
    if (head.status !== 101) {
      // core 不肯升级：用它的状态码与头拒绝客户端（不转体）。
      release();
      endpoint.destroy();
      if (head.status < 200 || head.status > 599)
        return relayError("bad_gateway", { cors });
      return new Response(null, {
        status: head.status,
        headers: responseHeaders(head),
      });
    }
    const protocol = head.headers.find(
      ([name]) => name.toLowerCase() === "sec-websocket-protocol",
    )?.[1];
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server, ["client"]);
    const attachment: ClientAttachment = {
      kind: "client",
      sourceId: target.sourceId,
      tunnelId: tunnel.tunnelId,
      streamId: endpoint.streamId,
      subject,
      ...(deviceId ? { deviceId } : {}),
      lastActivityMs: Date.now(),
      core: null,
    };
    const record = this.bridgeRecord(server, endpoint, attachment);
    record.bridge.start(result.response.upgraded);
    this.persist();
    void this.ensureAlarm();
    return new Response(null, {
      status: 101,
      webSocket: client,
      headers: protocol ? { "sec-websocket-protocol": protocol } : {},
    });
  }

  private bridgeRecord(
    ws: WebSocket,
    endpoint: TunnelEndpoint,
    attachment: ClientAttachment,
  ): BridgeRecord {
    const hooks = {
      activity: () => {
        attachment.lastActivityMs = Date.now();
        this.dirty.add(record);
        this.persist();
      },
      closed: () => {
        this.bridges.delete(ws);
        this.bridgeOf.delete(endpoint);
        this.dirty.delete(record);
        this.policy.untrack(ws);
        this.policy.releaseUpgrade(attachment.subject);
        this.persist();
      },
    };
    const record: BridgeRecord = {
      attachment,
      bridge:
        attachment.mode === "e2e"
          ? new E2eBridge(ws, endpoint, hooks)
          : new WsBridge(ws, endpoint, hooks),
    };
    this.bridges.set(ws, record);
    this.bridgeOf.set(endpoint, record);
    this.dirty.add(record);
    this.policy.track(ws, attachment.sourceId, attachment.subject);
    return record;
  }

  // ------------------------------------------------------------ Hibernation 事件

  async webSocketMessage(
    ws: WebSocket,
    message: string | ArrayBuffer,
  ): Promise<void> {
    this.budget.message();
    const tunnel = this.tunnels.get(ws);
    if (tunnel) {
      this.tunnelMessage(tunnel, message);
      this.persist();
      return;
    }
    const pending = this.handshakes.get(ws);
    if (pending) {
      this.handshakeMessage(ws, pending, message);
      return;
    }
    const bridge = this.bridges.get(ws);
    if (bridge) {
      bridge.bridge.clientMessage(message);
      return;
    }
    this.closeSocket(ws, 1011, "unknown_socket");
  }

  async webSocketClose(
    ws: WebSocket,
    code: number,
    reason: string,
  ): Promise<void> {
    this.socketGone(ws, code, reason);
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    this.socketGone(ws, 1006, "error");
  }

  private socketGone(ws: WebSocket, code: number, reason: string): void {
    const tunnel = this.tunnels.get(ws);
    if (tunnel) {
      tunnel.tunnel.transportClosed(code);
      this.tunnels.delete(ws);
    }
    if (this.handshakes.has(ws)) this.settleHandshake(ws, "aborted");
    this.bridges.get(ws)?.bridge.clientClose(code, reason);
    // 回一个关闭帧完成握手（已经关了就什么也不做）。
    this.closeSocket(ws, code === 1005 || code === 1006 ? 1000 : code, "");
    this.persist();
  }

  async alarm(): Promise<void> {
    const config = this.config;
    if (!config) return;
    const now = Date.now();
    for (const record of [...this.tunnels.values()]) {
      if (now - record.attachment.lastHeardAtMs > 2 * config.heartbeatMs) {
        this.metrics.heartbeatMiss();
        record.tunnel.close(CLOSE.goingAway, "heartbeat_timeout");
      }
    }
    for (const record of [...this.bridges.values()])
      if (now - record.attachment.lastActivityMs > config.clientIdleMs)
        record.bridge.disconnect(4000, "idle");
    const tickets = this.tickets.sweep();
    await this.budget.flush();
    await this.flushTraffic();
    if (config.mode === "saas" && this.current)
      this.publish({
        type: "sourceOnline",
        sourceId: this.current.tunnel.sourceId,
        node: WORKERS_NODE,
        tunnelId: this.current.tunnel.tunnelId,
        coreVersion: this.current.tunnel.coreVersion,
        atMs: now,
      });
    this.persist();
    if (this.tunnels.size > 0 || this.bridges.size > 0 || tickets > 0)
      await this.ctx.storage.setAlarm(now + config.alarmMs);
  }

  private async ensureAlarm(): Promise<void> {
    if (!this.config) return;
    if ((await this.ctx.storage.getAlarm()) !== null) return;
    await this.ctx.storage.setAlarm(Date.now() + this.config.alarmMs);
  }

  // ------------------------------------------------------------ 快照与还原

  /** 一条流动过信用：它的桥（若有）要重写附件。 */
  private touched(endpoint: TunnelEndpoint): void {
    const record = this.bridgeOf.get(endpoint);
    if (record) this.dirty.add(record);
    this.persist();
  }

  /** 动过热状态：本轮事件结束前把附件写一遍（合并成一次）。 */
  private persist(): void {
    if (this.persistQueued) return;
    this.persistQueued = true;
    queueMicrotask(() => {
      this.persistQueued = false;
      this.persistNow();
    });
  }

  private persistNow(): void {
    let busy = false;
    for (const record of this.tunnels.values()) {
      record.attachment.snapshot = record.tunnel.snapshot();
      this.attach(record.ws, record.attachment);
    }
    for (const record of this.dirty) {
      if (!this.bridges.has(record.bridge.ws)) continue;
      if (record.bridge.idle)
        record.attachment.core = record.bridge.endpoint.core.snapshot();
      else {
        record.attachment.core = null;
        busy = true;
      }
      this.attach(record.bridge.ws, record.attachment);
    }
    this.dirty.clear();
    if (busy) this.keepAwake.hold(() => this.anyBusy());
  }

  /** 还有不能存的字节就继续留在内存里；都走完了把所有桥重写一遍。 */
  private anyBusy(): boolean {
    for (const record of this.bridges.values())
      if (!record.bridge.idle) return true;
    for (const record of this.bridges.values()) this.dirty.add(record);
    this.persistNow();
    return false;
  }

  private attach(
    ws: WebSocket,
    attachment: TunnelAttachment | ClientAttachment,
  ) {
    try {
      ws.serializeAttachment(attachment);
    } catch {
      // WS 已经关了。
    }
  }

  /** 唤醒：从 WS 附件还原隧道与客户端桥（构造函数里调）。 */
  private restore(): void {
    const sockets = this.ctx.getWebSockets();
    if (sockets.length === 0) return;
    const clients = new Map<
      string,
      { ws: WebSocket; attachment: ClientAttachment }
    >();
    const tunnels: { ws: WebSocket; attachment: TunnelAttachment }[] = [];
    for (const ws of sockets) {
      const attachment = attachmentOf(ws);
      if (attachment?.kind === "client")
        clients.set(`${attachment.tunnelId}:${attachment.streamId}`, {
          ws,
          attachment,
        });
      else if (attachment?.kind === "tunnel") tunnels.push({ ws, attachment });
      else this.closeSocket(ws, 1011, "unknown_socket");
    }
    tunnels.sort(
      (left, right) =>
        (right.attachment.readyAtMs ?? 0) - (left.attachment.readyAtMs ?? 0),
    );
    for (const { ws, attachment } of tunnels) {
      const snapshot = attachment.snapshot;
      if (attachment.phase !== "ready" || !snapshot || this.current) {
        // 握手中、排空中或多出来的旧隧道不能跨休眠：关掉，源会重连。
        this.closeSocket(ws, CLOSE.goingAway, "interrupted");
        continue;
      }
      this.sourceId ??= attachment.sourceId;
      const adopted: { streamId: number; client: ClientAttachment }[] = [];
      const record = this.tunnelRecord(ws, attachment, (hooks) =>
        Tunnel.restore<TunnelEndpoint>(
          snapshot,
          this.transport(ws),
          this.limits,
          hooks,
          endpoints((endpoint) => this.touched(endpoint)),
          (streamId, host, options) => {
            const key = `${snapshot.tunnelId}:${streamId}`;
            const found = clients.get(key);
            if (!found?.attachment.core) return null;
            clients.delete(key);
            const endpoint = new TunnelEndpoint(streamId, host, options, (e) =>
              this.touched(e),
            );
            endpoint.restore(found.attachment.core);
            const bridge = this.bridgeRecord(
              found.ws,
              endpoint,
              found.attachment,
            );
            // 并发名额在内存里，休眠丢了：按还原的连接补回去。
            void this.control.limits.hit(
              "edge.concurrent.acct",
              found.attachment.subject,
            );
            bridge.bridge.start();
            adopted.push({ streamId, client: found.attachment });
            return endpoint;
          },
        ),
      );
      // 统计句柄在内存里，休眠丢了：按还原的连接接回去（不再计一次连接）。
      for (const { streamId, client } of adopted)
        if (this.traffic)
          record.tunnel.meter(
            streamId,
            this.traffic.counter({
              sourceId: client.sourceId,
              subject: client.subject,
              deviceId: client.deviceId ?? "",
              kind: client.mode === "e2e" ? "e2e" : "ws",
            }),
          );
      this.current = record;
    }
    // 没对上隧道的客户端：源已经不在这条隧道上了。
    for (const { ws } of clients.values())
      this.closeSocket(ws, CLOSE.sourceOffline, "source_offline");
    this.persist();
  }

  // ------------------------------------------------------------ 控制面

  private controlStub() {
    const env = this.env;
    return env.CONTROL.get(env.CONTROL.idFromName(CONTROL_OBJECT_NAME), {
      locationHint: this.config?.controlLocation,
    });
  }

  private async jwks(): Promise<JwkSet> {
    const config = this.config as RelayConfig;
    if (config.mode === "saas") return cloudJwks(config.cloudIssuer as string);
    return this.controlStub().jwks();
  }

  /** 撤销判断：personal 问 `ControlDO`（按 `jti` 的形状查源 / 设备 / 链接），失败按已撤销处理。 */
  private async isRevoked(jti: string): Promise<boolean> {
    if (this.config?.mode !== "personal") return false;
    const now = Date.now();
    const cached = this.revocations.get(jti);
    if (cached && (cached.revoked || now - cached.atMs < REVOCATION_CACHE_MS))
      return cached.revoked;
    let revoked = true;
    try {
      revoked = await this.controlStub().isRevoked(jti);
    } catch {
      revoked = true;
    }
    if (this.revocations.size > 4096) this.revocations.clear();
    this.revocations.set(jti, { revoked, atMs: now });
    return revoked;
  }

  private async refreshCors(): Promise<void> {
    if (this.config?.mode !== "personal") {
      this.cors ??= { origins: [], atMs: Date.now() };
      return;
    }
    const now = Date.now();
    if (this.cors && now - this.cors.atMs < CORS_CACHE_MS) return;
    try {
      this.cors = {
        origins: await this.controlStub().corsOrigins(),
        atMs: now,
      };
    } catch {
      // 拿不到就沿用旧的（没有旧的就只认原生 App）。
      this.cors ??= { origins: [], atMs: now - CORS_CACHE_MS + 5_000 };
    }
  }

  private publish(event: PresenceEvent): void {
    const config = this.config;
    if (!config) return;
    const sent =
      config.mode === "saas"
        ? reportPresence(config, this.env.RELAY_NODE_TOKEN, event)
        : this.controlStub().presence(event);
    this.ctx.waitUntil(
      Promise.resolve(sent).catch((error: unknown) =>
        this.log.warn("在线事件没送到", { error: errorName(error) }),
      ),
    );
  }

  private async flushBudget(delta: {
    requests: number;
    bytes: number;
  }): Promise<boolean> {
    return (await this.controlStub().budgetAdd(delta)).exceeded;
  }

  // ------------------------------------------------------------ RPC（ControlDO 调）

  /** 在线状态：有可开流的隧道答它的版本。 */
  route(): { coreVersion: string; tunnelId: string } | null {
    const tunnel = this.current?.tunnel;
    return tunnel?.accepting
      ? { coreVersion: tunnel.coreVersion, tunnelId: tunnel.tunnelId }
      : null;
  }

  /** 撤销源：先以 4403 断开经它的客户端 WS，再关隧道（4492）。 */
  closeTunnel(): void {
    if (this.sourceId) this.disconnect({ sourceId: this.sourceId });
    this.current?.tunnel.close(CLOSE.tunnelGoaway, "closed_by_control");
  }

  /** 断开匹配的客户端 WS（撤销源 / 访客），答断开了几条。 */
  disconnect(filter: EdgeConnectionFilter): number {
    const sockets = this.policy.take(filter);
    for (const ws of sockets)
      this.bridges.get(ws)?.bridge.disconnect(CLOSE.forbidden, "revoked");
    return sockets.length;
  }

  /** 撤销或设置变了：作废撤销判断与可信来源的缓存。 */
  invalidate(): void {
    this.revocations.clear();
    this.cors = null;
  }

  /** 流量统计（`ControlDO` 合并查询用）：先把内存里攒的写掉、过了一天先清保留期，再查本对象的表。 */
  async trafficQuery(query: TrafficQuery): Promise<TrafficRow[]> {
    await this.flushTraffic();
    const store = this.trafficTable();
    if (!store) return [];
    await this.pruneTraffic(store);
    return store.query(DEFAULT_TENANT, query);
  }

  /** 本对象的流量表（第一次用时跑迁移）；开不了答 null 并记一次日志，转发不受影响。 */
  private trafficTable(): TrafficStore | null {
    if (this.trafficStore !== undefined) return this.trafficStore;
    try {
      const db = new DoSqlDatabase(this.ctx.storage, "source");
      applyMigrations(db, bundledMigrations());
      this.trafficStore = createSqliteTraffic(db);
    } catch (error) {
      this.log.error("流量统计的存储打不开，不再统计", {
        error: errorName(error),
      });
      this.trafficStore = null;
    }
    return this.trafficStore;
  }

  private async writeTraffic(rows: TrafficRow[]): Promise<void> {
    const store = this.trafficTable();
    if (!store) return;
    await store.upsert(DEFAULT_TENANT, rows);
    await this.pruneTraffic(store);
  }

  /** 一天最多一次：删掉桶起点早于保留期的行。 */
  private async pruneTraffic(store: TrafficStore): Promise<void> {
    const days = this.config?.trafficRetentionDays ?? 0;
    if (days <= 0) return;
    const now = Date.now();
    const last = this.ctx.storage.kv.get<number>(TRAFFIC_PRUNED) ?? 0;
    if (now - last < DAY_MS) return;
    await store.prune(DEFAULT_TENANT, now - days * DAY_MS);
    this.ctx.storage.kv.put(TRAFFIC_PRUNED, now);
  }

  /** 攒下了新行：定时写（定时器在，对象就不休眠，计数不会随休眠丢掉）。 */
  private scheduleTrafficFlush(): void {
    if (this.trafficTimer !== undefined) return;
    this.trafficTimer = setTimeout(
      () => void this.flushTraffic(),
      TRAFFIC_FLUSH_DELAY_MS,
    );
  }

  /** 现在就写（定时器、检查周期与查询都走这里）；写完撤掉定时器，对象可以休眠。 */
  private async flushTraffic(): Promise<void> {
    clearTimeout(this.trafficTimer);
    this.trafficTimer = undefined;
    await this.traffic?.flush();
  }

  stats(): {
    tunnels: number;
    streams: number;
    clients: number;
  } {
    return { ...this.counts(), clients: this.bridges.size };
  }

  private counts(): { tunnels: number; streams: number } {
    let streams = 0;
    for (const record of this.tunnels.values())
      streams += record.tunnel.streamCount;
    return { tunnels: this.tunnels.size, streams };
  }

  private closeSocket(ws: WebSocket, code: number, reason: string): void {
    try {
      ws.close(code, reason);
    } catch {
      // 已经关了。
    }
  }
}
