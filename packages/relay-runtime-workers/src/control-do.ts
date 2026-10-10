/**
 * `ControlDO`：一租户一个 Durable Object（personal 恒 `tenant:default`，中继设计 §2.1、§2.6）。
 *
 * - **存储**：DO SQLite 跑与 Node 中继同一份迁移（`control-store-sql/migrations/`，账本
 *   `relay_migrations`），端口全部复用 `@armadra/control-store-sqlite`；签名私钥用 `RELAY_MASTER_KEY`
 *   包一层。库认不出、损坏、账本对不上或比程序新 → 一律答 503 `storage_unavailable`，**绝不**自动
 *   清库或重建。
 * - **`/v1/*` 与 `/.well-known/*`**：控制核的路由表（personal 子集），HTTP 外壳在 `api.ts`。
 * - **`/v1/me/stream`**：Hibernation 持有客户端 WS；附件里记调用方，唤醒后重新订阅；每
 *   `ME_STREAM_PING_MS` 一次检查周期发 `ping` 并复核设备，失效以 4401 关闭。
 * - **给 `SourceDO` 的 RPC**：JWKS、撤销判断、可信来源、在线事件、日配额。
 * - **对 `SourceDO` 的 RPC**：在线状态、关隧道、断开客户端、作废缓存（撤销即时生效）。
 *
 * saas 模式只用它的日配额（对象名 `budget`）。
 */
import { DurableObject } from "cloudflare:workers";

import {
  createChallengeGate,
  type ChallengeGate,
} from "@armadra/control-core/challenge";
import { LAST_SEEN_FLUSH_MS } from "@armadra/control-core/sessions";
import type { Caller } from "@armadra/control-core/sessions";
import { isRevokedJti } from "@armadra/control-core/revocation";
import { SqliteStoreError } from "@armadra/control-store-sqlite/errors";
import { applyMigrations } from "@armadra/control-store-sqlite/apply";
import type { KeyWrap } from "@armadra/control-store-sqlite/db";
import { TICKET_SUBPROTOCOL_PREFIX } from "@armadra/platform-protocol";
import type { JwkSet } from "@armadra/platform-protocol/assertion";
import {
  ME_STREAM_PATH,
  ME_STREAM_PING_MS,
  meStreamClientMessageSchema,
  type MeStreamEvent,
} from "@armadra/platform-protocol/cloud-api";
import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import type { PresenceEvent } from "@armadra/relay-kernel/control/types";
import type { EdgeConnectionFilter } from "@armadra/relay-kernel/edge/policy";
import { errorName } from "@armadra/relay-kernel/log";

import { API_CORS_HEADERS, isApiPath } from "./api.ts";
import { BudgetLedger, type BudgetDelta } from "./budget.ts";
import { createWorkersTurnstileVerifier } from "./challenge.ts";
import {
  ConfigError,
  readConfig,
  sourceObjectName,
  type RelayConfig,
  type RelayEnv,
} from "./env.ts";
import { workersLogger } from "./log.ts";
import {
  initializePersonal,
  NotInitializedError,
  openPersonal,
  type PersonalControl,
  type PersonalInput,
  type SourceObjects,
} from "./personal-control.ts";
import { isUpgrade, relayError } from "./responses.ts";
import { DoSqlDatabase } from "./store-do-sqlite/database.ts";
import { masterKeyWrap, MasterKeyError } from "./store-do-sqlite/keywrap.ts";
import { bundledMigrations } from "./store-do-sqlite/migrations.ts";

interface MeAttachment {
  kind: "me";
  caller: Caller;
}

interface MeSocket {
  caller: Caller;
  detach: () => void;
}

/** 这次为什么不能服务：存储（503 `storage_unavailable`）或还没初始化（503 `not_initialized`）。 */
type Unavailable = {
  code: Extract<
    ErrorCode,
    "storage_unavailable" | "not_initialized" | "unavailable"
  >;
};

export interface ReadyState {
  ok: boolean;
  code?: string;
}

export class ControlDO extends DurableObject<RelayEnv> {
  private readonly config: RelayConfig | null;
  private readonly db: DoSqlDatabase;
  private readonly storageError: SqliteStoreError | null;
  private readonly wrap: KeyWrap | null;
  private readonly budget: BudgetLedger;
  private readonly log;
  private personalControl: PersonalControl | null = null;
  private initializing: Promise<PersonalControl> | null = null;
  private readonly meSockets = new Map<WebSocket, MeSocket>();
  private lastFlushMs = Date.now();
  /** 托管页面的产物在不在（第一次装配前探一次）。 */
  private assetsReady = false;
  private assetsProbed = false;
  private warnedSecrets = false;
  /** 人机挑战（防重放表跟着对象活）；没配是 undefined。 */
  private readonly challenge: ChallengeGate | undefined;

  constructor(ctx: DurableObjectState, env: RelayEnv) {
    super(ctx, env);
    let config: RelayConfig | null = null;
    try {
      config = readConfig(env);
    } catch (error) {
      if (!(error instanceof ConfigError)) throw error;
    }
    this.config = config;
    this.log = workersLogger("relay-control", config?.logLevel ?? "info");
    this.db = new DoSqlDatabase(ctx.storage, "control");
    let storageError: SqliteStoreError | null = null;
    try {
      applyMigrations(this.db, bundledMigrations());
    } catch (error) {
      storageError =
        error instanceof SqliteStoreError
          ? error
          : new SqliteStoreError("corrupt", "存储打不开，拒绝服务");
      this.log.error("存储不可用，拒绝服务", { code: storageError.code });
    }
    this.storageError = storageError;
    let wrap: KeyWrap | null = null;
    try {
      wrap = masterKeyWrap(env.RELAY_MASTER_KEY);
    } catch (error) {
      if (!(error instanceof MasterKeyError)) throw error;
    }
    this.wrap = wrap;
    this.budget = new BudgetLedger(ctx.storage.kv, {
      requests: config?.dailyRequestBudget ?? 0,
      bytes: config?.dailyTunnelBytesBudget ?? 0,
    });
    this.challenge =
      config?.challenge && env.RELAY_TURNSTILE_SECRET
        ? createChallengeGate({
            siteKey: config.challenge.siteKey,
            scope: config.challenge.scope,
            verifier: createWorkersTurnstileVerifier({
              secret: env.RELAY_TURNSTILE_SECRET.trim(),
            }),
            now: Date.now,
            onRejected: (procedure, reasons) =>
              this.log.info("人机挑战没通过", {
                procedure,
                reasons: reasons.join(","),
              }),
          })
        : undefined;
    this.restoreMeSockets();
  }

  // ------------------------------------------------------------ 装配

  private input(): PersonalInput {
    return {
      db: this.db,
      config: this.config as RelayConfig,
      wrap: this.wrap as KeyWrap,
      sources: this.sources(),
      hasAssets: () => this.assetsReady,
      now: Date.now,
      onError: (error) =>
        this.log.error("请求处理失败", { error: errorName(error) }),
      ...(this.challenge ? { challenge: this.challenge } : {}),
    };
  }

  /** 为什么不能装配（配置、存储、主密钥）；能就答 null。 */
  private blocked(): Unavailable | null {
    if (!this.config || this.config.mode !== "personal")
      return { code: "unavailable" };
    if (this.storageError) return { code: "storage_unavailable" };
    if (!this.wrap) return { code: "not_initialized" };
    return null;
  }

  /** 同步装配（库里已有租户）；装配不了答 null。 */
  private personalSync(): PersonalControl | null {
    if (this.personalControl) return this.personalControl;
    if (this.blocked()) return null;
    try {
      this.personalControl = openPersonal(this.input());
    } catch (error) {
      this.log.error("存储不可用，拒绝服务", { error: errorName(error) });
      return null;
    }
    return this.personalControl;
  }

  /** 装配；第一次且库里没有租户时用 secret 初始化。 */
  /** 产物里有 `index.html` 才报 `webApp`（与 Node 中继相同）。 */
  private async probeAssets(): Promise<void> {
    if (this.assetsProbed || !this.env.ASSETS || !this.config) return;
    this.assetsProbed = true;
    try {
      const response = await this.env.ASSETS.fetch(
        new Request(`${this.config.issuer}/index.html`),
      );
      this.assetsReady = response.ok;
      await response.body?.cancel();
    } catch {
      this.assetsReady = false;
    }
  }

  private async personal(): Promise<PersonalControl | Unavailable> {
    const blocked = this.blocked();
    if (blocked) return blocked;
    await this.probeAssets();
    if (this.personalControl) return this.personalControl;
    try {
      const opened = openPersonal(this.input());
      if (opened) {
        this.personalControl = opened;
        this.warnSecrets();
        return opened;
      }
    } catch (error) {
      this.log.error("存储不可用，拒绝服务", { error: errorName(error) });
      return { code: "storage_unavailable" };
    }
    this.initializing ??= (async () => {
      await initializePersonal(this.input(), {
        account: this.env.RELAY_PERSONAL_ACCOUNT,
        password: this.env.RELAY_PERSONAL_PASSWORD,
      });
      const opened = openPersonal(this.input());
      if (!opened) throw new NotInitializedError("missing_tenant");
      this.log.info("已初始化个人中转", {
        account: this.env.RELAY_PERSONAL_ACCOUNT?.trim(),
      });
      this.personalControl = opened;
      return opened;
    })();
    try {
      return await this.initializing;
    } catch (error) {
      this.initializing = null;
      if (error instanceof NotInitializedError) {
        this.log.warn("还没初始化：缺账号 / 口令 secret 或口令不合策略", {
          reason: error.message,
        });
        return { code: "not_initialized" };
      }
      this.log.error("初始化失败", { error: errorName(error) });
      return { code: "storage_unavailable" };
    }
  }

  private warnSecrets(): void {
    if (this.warnedSecrets || !this.env.RELAY_PERSONAL_PASSWORD) return;
    this.warnedSecrets = true;
    this.log.warn(
      "已初始化：可以删掉 RELAY_PERSONAL_ACCOUNT / RELAY_PERSONAL_PASSWORD 两个 secret",
    );
  }

  private sourceStub(sourceId: string) {
    return this.env.SOURCE.get(
      this.env.SOURCE.idFromName(sourceObjectName(sourceId)),
    );
  }

  private sources(): SourceObjects {
    const background = (work: Promise<unknown>) =>
      this.ctx.waitUntil(
        work.catch((error: unknown) =>
          this.log.warn("源对象没答", { error: errorName(error) }),
        ),
      );
    return {
      route: async (sourceId) => {
        try {
          return await this.sourceStub(sourceId).route();
        } catch {
          return null;
        }
      },
      closeTunnel: (sourceId) =>
        background(this.sourceStub(sourceId).closeTunnel()),
      disconnect: (sourceId, filter) =>
        background(
          this.sourceStub(sourceId).disconnect(filter as EdgeConnectionFilter),
        ),
      invalidate: (sourceIds) => {
        for (const sourceId of sourceIds)
          background(this.sourceStub(sourceId).invalidate());
      },
    };
  }

  // ------------------------------------------------------------ HTTP 入口

  async fetch(request: Request): Promise<Response> {
    if (!this.config) return relayError("unavailable");
    const url = new URL(request.url);
    this.budget.note();
    if (url.pathname === ME_STREAM_PATH && isUpgrade(request))
      return this.meStreamUpgrade(request);
    if (!isApiPath(url.pathname)) return relayError("not_found");
    const personal = await this.personal();
    if ("code" in personal)
      return relayError(personal.code, { headers: API_CORS_HEADERS });
    const response = await personal.api.handle(
      request,
      request.headers.get("cf-connecting-ip") ?? "0.0.0.0",
    );
    this.maybeFlush(personal);
    return response;
  }

  private maybeFlush(personal: PersonalControl): void {
    const now = Date.now();
    if (now - this.lastFlushMs < LAST_SEEN_FLUSH_MS) return;
    this.lastFlushMs = now;
    this.ctx.waitUntil(
      personal.api.authenticator
        .flush()
        .catch((error: unknown) =>
          this.log.warn("最近在线时间写入失败", { error: errorName(error) }),
        ),
    );
  }

  // ------------------------------------------------------------ me-stream

  private async meStreamUpgrade(request: Request): Promise<Response> {
    const personal = await this.personal();
    if ("code" in personal) return relayError(personal.code);
    const offered = (request.headers.get("sec-websocket-protocol") ?? "")
      .split(",")
      .map((value) => value.trim())
      .find((value) => value.startsWith(TICKET_SUBPROTOCOL_PREFIX));
    let caller: Caller | null = null;
    try {
      caller = await personal.stream.redeem(
        offered?.slice(TICKET_SUBPROTOCOL_PREFIX.length),
      );
    } catch {
      return relayError("unavailable");
    }
    if (!caller || !offered) return relayError("unauthenticated");
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server, ["me"]);
    const attachment: MeAttachment = { kind: "me", caller };
    server.serializeAttachment(attachment);
    this.attachMe(personal, server, caller);
    await this.ensureAlarm();
    return new Response(null, {
      status: 101,
      webSocket: client,
      headers: { "sec-websocket-protocol": offered },
    });
  }

  private attachMe(personal: PersonalControl, ws: WebSocket, caller: Caller) {
    const send = (event: MeStreamEvent) => {
      try {
        ws.send(JSON.stringify(event));
      } catch {
        // 已经关了。
      }
    };
    this.meSockets.set(ws, {
      caller,
      detach: personal.stream.attach(caller, send),
    });
  }

  private restoreMeSockets(): void {
    const sockets = this.ctx.getWebSockets("me");
    if (sockets.length === 0) return;
    const personal = this.personalSync();
    for (const ws of sockets) {
      const attachment = ws.deserializeAttachment() as MeAttachment | null;
      if (!personal || attachment?.kind !== "me") {
        try {
          ws.close(1011, "unavailable");
        } catch {
          // 已经关了。
        }
        continue;
      }
      this.attachMe(personal, ws, attachment.caller);
    }
  }

  async webSocketMessage(
    ws: WebSocket,
    message: string | ArrayBuffer,
  ): Promise<void> {
    if (!this.meSockets.has(ws) || typeof message !== "string") return;
    if (message.length > 4096) return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(message);
    } catch {
      return;
    }
    if (meStreamClientMessageSchema.safeParse(parsed).success)
      ws.send(JSON.stringify({ type: "pong", ts: Date.now() }));
  }

  async webSocketClose(ws: WebSocket, code: number): Promise<void> {
    this.dropMe(ws);
    try {
      ws.close(code === 1005 || code === 1006 ? 1000 : code, "");
    } catch {
      // 已经关了。
    }
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    this.dropMe(ws);
  }

  private dropMe(ws: WebSocket): void {
    this.meSockets.get(ws)?.detach();
    this.meSockets.delete(ws);
  }

  async alarm(): Promise<void> {
    const personal = this.personalSync();
    if (!personal) {
      for (const ws of [...this.meSockets.keys()]) {
        this.dropMe(ws);
        try {
          ws.close(1011, "unavailable");
        } catch {
          // 已经关了。
        }
      }
      return;
    }
    await Promise.all(
      [...this.meSockets].map(async ([ws, { caller }]) => {
        const valid = await personal.stream
          .stillValid(caller)
          .catch(() => true);
        if (!valid) {
          this.dropMe(ws);
          try {
            ws.close(CLOSE.unauthenticated, "session_revoked");
          } catch {
            // 已经关了。
          }
          return;
        }
        try {
          ws.send(JSON.stringify({ type: "ping", ts: Date.now() }));
        } catch {
          this.dropMe(ws);
        }
      }),
    );
    await personal.api.authenticator.flush().catch(() => {});
    if (this.meSockets.size > 0)
      await this.ctx.storage.setAlarm(Date.now() + ME_STREAM_PING_MS);
  }

  private async ensureAlarm(): Promise<void> {
    if ((await this.ctx.storage.getAlarm()) !== null) return;
    await this.ctx.storage.setAlarm(Date.now() + ME_STREAM_PING_MS);
  }

  // ------------------------------------------------------------ RPC（SourceDO 调）

  private async required(): Promise<PersonalControl> {
    const personal = await this.personal();
    if ("code" in personal) throw new Error(personal.code);
    return personal;
  }

  async jwks(): Promise<JwkSet> {
    return (await this.required()).context.signer.jwks();
  }

  /** 中继令牌 / 隧道令牌的撤销判断（按 `jti` 的形状查源、设备、链接）。 */
  async isRevoked(jti: string): Promise<boolean> {
    const personal = await this.required();
    return isRevokedJti(
      { tenantId: personal.context.tenantId, ports: personal.ports },
      jti,
      Date.now(),
    );
  }

  async corsOrigins(): Promise<string[]> {
    return (await this.required()).corsOrigins();
  }

  async presence(event: PresenceEvent): Promise<void> {
    await (await this.required()).presence(event);
  }

  budgetAdd(delta: BudgetDelta): { exceeded: boolean } {
    return { exceeded: this.budget.add(delta).exceeded };
  }

  async ready(): Promise<ReadyState> {
    if (!this.config) return { ok: false, code: "unavailable" };
    if (this.config.mode !== "personal") return { ok: true };
    const personal = await this.personal();
    return "code" in personal
      ? { ok: false, code: personal.code }
      : { ok: true };
  }

  /** `/metrics` 的文本（Prometheus 0.0.4）：各源对象的隧道 / 流 / 客户端数与当日配额。 */
  async metrics(): Promise<string> {
    let tunnels = 0;
    let streams = 0;
    let clients = 0;
    const personal = this.personalSync();
    if (personal) {
      const rows = await personal.ports.sources.list(personal.context.tenantId);
      const stats = await Promise.all(
        rows
          .filter((row) => row.revokedAtMs === null)
          .map((row) =>
            this.sourceStub(row.sourceId)
              .stats()
              .catch(() => ({ tunnels: 0, streams: 0, clients: 0 })),
          ),
      );
      for (const item of stats) {
        tunnels += item.tunnels;
        streams += item.streams;
        clients += item.clients;
      }
    }
    const usage = this.budget.usage();
    const lines = [
      "# HELP relay_tunnels Open tunnels.",
      "# TYPE relay_tunnels gauge",
      `relay_tunnels ${tunnels}`,
      "# HELP relay_streams Open streams.",
      "# TYPE relay_streams gauge",
      `relay_streams ${streams}`,
      "# HELP relay_client_websockets Relayed client WebSockets.",
      "# TYPE relay_client_websockets gauge",
      `relay_client_websockets ${clients}`,
      "# HELP relay_budget_used Daily budget used (UTC day).",
      "# TYPE relay_budget_used gauge",
      `relay_budget_used{kind="requests"} ${usage.requests}`,
      `relay_budget_used{kind="tunnel_bytes"} ${usage.bytes}`,
      "# HELP relay_budget_limit Daily budget limit (0 = unlimited).",
      "# TYPE relay_budget_limit gauge",
      `relay_budget_limit{kind="requests"} ${this.budget.limitRequests}`,
      `relay_budget_limit{kind="tunnel_bytes"} ${this.budget.limitBytes}`,
    ];
    return `${lines.join("\n")}\n`;
  }
}
