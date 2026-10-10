/**
 * personal 控制面在 `ControlDO` 里的装配（Node 中继 `personal/serve.ts` + `context.ts` 的同一条路）：
 * DO SQLite 的端口（`@armadra/control-store-sqlite`，包一层撤销通知）→ 签名钥（主密钥包裹）→
 * 限流（登录与接受链接按 IP，内存桶；Worker 入口另有 Rate Limiting 绑定）→ 在线总线（`SourceDO`
 * 推过来）→ 中继端口（RPC 到 `SourceDO`）→ `createPersonalProcedures` + `createCloudRouter`。
 *
 * 首次初始化：库里还没有租户时，用 secret `RELAY_PERSONAL_ACCOUNT` / `RELAY_PERSONAL_PASSWORD` 建
 * 账号（口令过 identity-vectors 策略、scrypt 哈希）与第一把签名钥；之后这两个 secret 就该删掉。
 */
import { checkPassword } from "@armadra/control-core/accounts";
import { ControlError } from "@armadra/control-core/errors";
import { createEventBus } from "@armadra/control-core/events";
import {
  createMeStreamCore,
  type MeStreamCore,
} from "@armadra/control-core/me-stream";
import { hashPassword } from "@armadra/control-core/passwords";
import {
  DEFAULT_TENANT,
  type ConnectionFilter,
  type ControlContext,
  type ControlPorts,
  type PresenceEvent,
} from "@armadra/control-core/ports";
import { generateSigningKey, Signer } from "@armadra/control-core/signer";
import { trustedOriginsOf } from "@armadra/control-core/sources";
import { ownerSubject } from "@armadra/control-core/subjects";
import type { KeyWrap } from "@armadra/control-store-sqlite/db";
import {
  addSigningKey,
  createSqliteKeys,
} from "@armadra/control-store-sqlite/keys";
import { createSqlitePorts } from "@armadra/control-store-sqlite/ports";
import {
  addAccount,
  createTenant,
  readTenant,
  updateTenant,
} from "@armadra/control-store-sqlite/tenants";
import { createMemoryControlPlane } from "@armadra/relay-kernel/control/memory";

import { createWorkersApi, type WorkersApi } from "./api.ts";
import { tunnelNodeUrl, type RelayConfig } from "./env.ts";
import { passwordViolation } from "./passwords.ts";
import { watchRevocations } from "./revocation.ts";
import type { DoSqlDatabase } from "./store-do-sqlite/database.ts";

/** 还没初始化（缺账号 / 口令 secret，或口令不合策略）：`/v1` 答 503 `not_initialized`。 */
export class NotInitializedError extends Error {
  constructor(reason: string) {
    super(reason);
    this.name = "NotInitializedError";
  }
}

/** 控制面对各源对象的 RPC（`SourceDO` 的公开方法）。 */
export interface SourceObjects {
  route(sourceId: string): Promise<{ coreVersion: string } | null>;
  closeTunnel(sourceId: string): void;
  disconnect(sourceId: string, filter: ConnectionFilter): void;
  invalidate(sourceIds: readonly string[]): void;
}

export interface PersonalControl {
  context: ControlContext;
  ports: ControlPorts;
  stream: MeStreamCore;
  api: WorkersApi;
  /** `SourceDO` 推来的上下线：记最近在线与版本，扇出给 me-stream。 */
  presence(event: PresenceEvent): Promise<void>;
  corsOrigins(): string[];
}

export interface PersonalInput {
  db: DoSqlDatabase;
  config: RelayConfig;
  wrap: KeyWrap;
  sources: SourceObjects;
  hasAssets: boolean;
  now: () => number;
  onError: (error: unknown) => void;
}

/** 库里有租户时同步装配（构造函数里还原 me-stream 也走这里）；没有答 null。 */
export function openPersonal(input: PersonalInput): PersonalControl | null {
  const { db, config, wrap, sources, now } = input;
  const tenantId = DEFAULT_TENANT;
  const tenant = readTenant(db, tenantId);
  if (!tenant) return null;
  if (tenant.issuer !== config.issuer)
    updateTenant(db, tenantId, (draft) => {
      draft.issuer = config.issuer;
    });
  const keys = createSqliteKeys(db, tenantId, wrap);
  // 读一遍签名钥：主密钥不对或数据坏了在这里就拒绝（SqliteStoreError）。
  keys.signingKeys();
  const signer = new Signer(keys, now);

  let pendingInvalidate = false;
  const raw = createSqlitePorts(db);
  const ports = watchRevocations(raw, () => {
    if (pendingInvalidate) return;
    pendingInvalidate = true;
    queueMicrotask(() => {
      pendingInvalidate = false;
      void raw.sources.list(tenantId).then(
        (rows) => sources.invalidate(rows.map((row) => row.sourceId)),
        () => {},
      );
    });
  });
  const limiter = createMemoryControlPlane({
    issuer: config.issuer,
    jwks: () => signer.jwks(),
    now,
  });
  const listeners = new Set<(event: PresenceEvent) => void>();
  const context: ControlContext = {
    tenantId,
    ports,
    signer,
    limits: limiter.limits,
    presence: {
      subscribe(fn) {
        listeners.add(fn);
        return () => listeners.delete(fn);
      },
    },
    relay: {
      route: (sourceId) => sources.route(sourceId),
      closeTunnel: (sourceId) => sources.closeTunnel(sourceId),
      disconnect: (filter) => {
        if (filter.sourceId !== undefined) {
          sources.disconnect(filter.sourceId, filter);
          return;
        }
        const linkId =
          /^guest:([0-9a-f]{16}):$/.exec(filter.subjectPrefix ?? "")?.[1] ??
          null;
        void (async () => {
          const link = linkId ? await raw.links.get(tenantId, linkId) : null;
          const targets = link
            ? [link.sourceId]
            : (await raw.sources.list(tenantId)).map((row) => row.sourceId);
          for (const sourceId of targets) sources.disconnect(sourceId, filter);
        })().catch(() => {});
      },
    },
    passwordPolicy: passwordViolation,
    now,
    tunnelUrl: (sourceId) => tunnelNodeUrl(config.issuer, sourceId),
    ...createEventBus(),
  };
  const stream = createMeStreamCore(context);
  const api = createWorkersApi(context, stream, {
    webApp: () => (input.hasAssets ? `${config.issuer}/app/` : null),
    name: () =>
      config.name ??
      readTenant(db, tenantId)?.displayName ??
      new URL(config.issuer).hostname,
    onError: input.onError,
  });

  return {
    context,
    ports,
    stream,
    api,
    async presence(event) {
      await raw.sources.update(tenantId, event.sourceId, (draft) => {
        if (!draft) return;
        draft.lastSeenAtMs = Math.max(draft.lastSeenAtMs ?? 0, event.atMs);
        if (event.type === "sourceOnline")
          draft.coreVersion = event.coreVersion;
      });
      for (const listener of [...listeners]) {
        try {
          listener(event);
        } catch {
          // 一个订阅者出错不影响其它。
        }
      }
    },
    corsOrigins() {
      const current = readTenant(db, tenantId);
      return current
        ? trustedOriginsOf({
            issuer: current.issuer,
            trustedOrigins: current.trustedOrigins,
          })
        : [];
    },
  };
}

/**
 * 首次初始化：建租户、账号与第一把签名钥（一个事务）。账号 / 口令 secret 缺失或口令不合策略 →
 * {@link NotInitializedError}（不建任何东西）。
 */
export async function initializePersonal(
  input: PersonalInput,
  secrets: { account?: string; password?: string },
): Promise<void> {
  const { db, config, wrap, now } = input;
  const account = secrets.account?.trim();
  const password = secrets.password;
  if (!account || !password)
    throw new NotInitializedError("missing_credentials");
  try {
    checkPassword({ passwordPolicy: passwordViolation }, password, account);
  } catch (error) {
    if (error instanceof ControlError)
      throw new NotInitializedError(error.code);
    throw error;
  }
  const hashed = await hashPassword(password);
  const nowMs = now();
  db.transaction(() => {
    if (readTenant(db, DEFAULT_TENANT)) return;
    createTenant(db, DEFAULT_TENANT, {
      issuer: config.issuer,
      allowGuests: true,
      trustedOrigins: config.trustedOrigins,
    });
    addAccount(db, DEFAULT_TENANT, {
      accountId: ownerSubject(account),
      name: account,
      password: hashed,
      createdAtMs: nowMs,
      updatedAtMs: nowMs,
      failures: 0,
      lockedUntilMs: 0,
      lastFailureAtMs: 0,
    });
    addSigningKey(db, DEFAULT_TENANT, generateSigningKey("k1", nowMs), wrap);
  });
}
