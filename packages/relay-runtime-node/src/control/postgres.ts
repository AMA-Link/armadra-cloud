/**
 * `ControlPlane` 的 PostgreSQL 实现（`--control postgres`，多节点；Node 中继存储设计 §5.2）。
 *
 * | 功能 | 实现 |
 * | --- | --- |
 * | 路由 | `relay_routes`（UNLOGGED）：`get` 每次读表，`set` upsert，`touch` 续期，`delete` 按 `tunnel_id` CAS |
 * | 在线事件 | 本节点立即投递 + `NOTIFY relay_presence` |
 * | 撤销 | `relay_revoked_tokens` + `NOTIFY relay_revoked`；各节点内存里一份未过期的 `jti`，`isRevoked` 只查内存 |
 * | 速率限流 | `relay_limits`（UNLOGGED）固定窗口 upsert 计数，各节点共用一个桶；库出错时退回本节点内存 |
 * | 并发限流 | 本节点内存（节点崩溃不留占用） |
 * | 节点命令 | 本节点立即投递 + `NOTIFY relay_ctl` |
 *
 * `LISTEN` 用驱动的常驻连接，断线后驱动自己重连并重新 `LISTEN`；每次（重新）监听上撤销频道都
 * 全量重读撤销表，断线期间漏掉的撤销不会漏判。表由 `@armadra/control-store-postgres` 的迁移建，
 * 这里只核对表在不在（不在就拒绝启动，先迁移）。不写令牌明文（撤销只有 `jti`）。
 */
import { createHash, randomBytes } from "node:crypto";

import type { Sql } from "@armadra/cloud-shared/db";
import { createMemoryControlPlane } from "@armadra/relay-kernel/control/memory";
import {
  DEFAULT_LIMIT_RULES,
  type LimitResult,
  type NodeCommand,
  type PresenceEvent,
  type Route,
} from "@armadra/relay-kernel/control/types";

import {
  listeners,
  parseBroadcast,
  type Broadcast,
  type ClusterControlOptions,
  type ClusterControlPlane,
} from "./types.ts";

export const CHANNELS = {
  presence: "relay_presence",
  revoked: "relay_revoked",
  command: "relay_ctl",
} as const;

/** 每隔多久清一次过期的路由、限流桶与撤销记录（任一节点做都行）。 */
const SWEEP_MS = 60_000;

export interface PostgresControlOptions extends ClusterControlOptions {
  /** 查询用的连接池（`LISTEN` 由它另开一条常驻连接）。 */
  sql: Sql;
}

export interface PostgresControlPlane extends ClusterControlPlane {
  /** 测试用：内存里未过期的撤销条数。 */
  revokedCount(): number;
  /** 等所有已发出的后台写完成（测试与关闭前用）。 */
  settle(): Promise<void>;
}

/** 限流桶的键：种类 + 键的 SHA-256（IP、账号不以明文落库，长度也有界）。 */
export function limitId(kind: string, key: string): string {
  return `${kind}:${createHash("sha256").update(key).digest("hex")}`;
}

class MissingSchemaError extends Error {
  constructor() {
    super(
      "PostgreSQL 里没有中继控制面的表：先运行迁移（armadra-cloud migrate）",
    );
    this.name = "MissingSchemaError";
  }
}

export async function createPostgresControlPlane(
  options: PostgresControlOptions,
): Promise<PostgresControlPlane> {
  const { sql } = options;
  const now = options.now ?? Date.now;
  const rules = { ...DEFAULT_LIMIT_RULES, ...options.limits };
  const origin = randomBytes(8).toString("hex");
  const onError = (error: unknown, operation: string) =>
    options.onError?.(error, operation);
  // 并发限流与库出错时的速率限流退回本节点内存。
  const local = createMemoryControlPlane({
    issuer: options.issuer,
    jwks: options.jwks,
    limits: options.limits,
    now,
  });
  const presence = listeners<PresenceEvent>();
  const commands = listeners<NodeCommand>();
  const revoked = new Map<string, number>();
  const inflight = new Set<Promise<unknown>>();
  const track = <T>(task: Promise<T>): Promise<T> => {
    inflight.add(task);
    void task.then(
      () => inflight.delete(task),
      () => inflight.delete(task),
    );
    return task;
  };
  /** 后台写：不让失败变成未处理的拒绝（内核对这些调用是 `void`）。 */
  const background = (operation: string, task: () => Promise<unknown>) =>
    track(
      task().catch((error: unknown) => {
        onError(error, operation);
      }),
    ).then(() => undefined);

  try {
    await sql`SELECT 1 FROM relay_routes, relay_revoked_tokens, relay_limits LIMIT 0`;
  } catch {
    throw new MissingSchemaError();
  }

  const notify = (channel: string, message: Broadcast) =>
    background(
      `notify ${channel}`,
      () => sql`SELECT pg_notify(${channel}, ${JSON.stringify(message)})`,
    );

  const reloadRevoked = async () => {
    const rows = await sql`
      SELECT jti, exp_ms FROM relay_revoked_tokens WHERE exp_ms > ${now()}`;
    revoked.clear();
    for (const row of rows) revoked.set(String(row.jti), Number(row.exp_ms));
  };

  const receive = (payload: string) => {
    const message = parseBroadcast(payload);
    if (!message || message.origin === origin) return;
    if (message.kind === "presence") presence.emit(message.event);
    else if (message.kind === "command") commands.emit(message.command);
    else if (message.expMs > now()) revoked.set(message.jti, message.expMs);
  };

  const subscriptions = await Promise.all([
    sql.listen(CHANNELS.presence, receive),
    sql.listen(CHANNELS.command, receive),
    // 每次（重新）监听上都全量重读：断线期间的撤销不漏。
    sql.listen(CHANNELS.revoked, receive, () => {
      void background("reload revoked", reloadRevoked);
    }),
  ]);
  await reloadRevoked();

  const sweep = setInterval(() => {
    void background("sweep", async () => {
      const at = now();
      await sql`DELETE FROM relay_routes WHERE expires_at_ms <= ${at}`;
      await sql`DELETE FROM relay_limits WHERE reset_at_ms <= ${at}`;
      await sql`DELETE FROM relay_revoked_tokens WHERE exp_ms <= ${at}`;
      for (const [jti, until] of revoked) if (until <= at) revoked.delete(jti);
    });
  }, SWEEP_MS);
  sweep.unref();

  const toRoute = (row: Record<string, unknown>): Route => ({
    node: String(row.node),
    tunnelId: String(row.tunnel_id),
    sinceMs: Number(row.since_ms),
    coreVersion: String(row.core_version),
  });

  const settle = async () => {
    while (inflight.size > 0) await Promise.allSettled([...inflight]);
  };

  let closed = false;
  return {
    mode: options.mode ?? "personal",
    issuer: options.issuer,
    jwks: async () => options.jwks(),
    routes: {
      async get(sourceId) {
        const [row] = await sql`
          SELECT node, tunnel_id, since_ms, core_version FROM relay_routes
          WHERE source_id = ${sourceId} AND expires_at_ms > ${now()}`;
        return row ? toRoute(row) : null;
      },
      async set(sourceId, route, ttlMs) {
        await track(sql`
          INSERT INTO relay_routes (source_id, node, tunnel_id, since_ms, core_version, expires_at_ms)
          VALUES (${sourceId}, ${route.node}, ${route.tunnelId}, ${route.sinceMs},
            ${route.coreVersion}, ${now() + ttlMs})
          ON CONFLICT (source_id) DO UPDATE SET node = EXCLUDED.node,
            tunnel_id = EXCLUDED.tunnel_id, since_ms = EXCLUDED.since_ms,
            core_version = EXCLUDED.core_version, expires_at_ms = EXCLUDED.expires_at_ms`);
      },
      touch(sourceId, ttlMs) {
        const at = now();
        return background(
          "route touch",
          () =>
            sql`
            UPDATE relay_routes SET expires_at_ms = ${at + ttlMs}
            WHERE source_id = ${sourceId} AND expires_at_ms > ${at}`,
        );
      },
      delete(sourceId, tunnelId) {
        return background(
          "route delete",
          () =>
            sql`DELETE FROM relay_routes WHERE source_id = ${sourceId} AND tunnel_id = ${tunnelId}`,
        );
      },
    },
    presence: {
      async publish(event) {
        presence.emit(event);
        await notify(CHANNELS.presence, { origin, kind: "presence", event });
      },
      subscribe: (fn) => presence.add(fn),
    },
    limits: {
      async hit(kind, key): Promise<LimitResult> {
        const rule = rules[kind];
        if (rule.type === "concurrent") return local.limits.hit(kind, key);
        const at = now();
        const id = limitId(kind, key);
        try {
          const [row] = await sql`
            INSERT INTO relay_limits (id, count, reset_at_ms)
            VALUES (${id}, 1, ${at + rule.windowMs}::bigint)
            ON CONFLICT (id) DO UPDATE SET
              count = CASE WHEN relay_limits.reset_at_ms <= ${at}::bigint
                THEN 1 ELSE relay_limits.count + 1 END,
              reset_at_ms = CASE WHEN relay_limits.reset_at_ms <= ${at}::bigint
                THEN ${at + rule.windowMs}::bigint ELSE relay_limits.reset_at_ms END
            RETURNING count, reset_at_ms`;
          const count = Number(row?.count ?? 0);
          if (count <= rule.max) return { allowed: true };
          return {
            allowed: false,
            retryAfterMs: Math.max(0, Number(row?.reset_at_ms ?? at) - at),
          };
        } catch (error) {
          onError(error, "limit");
          return local.limits.hit(kind, key);
        }
      },
      release: (kind, key) =>
        local.limits.release?.(kind, key) ?? Promise.resolve(),
    },
    tokens: {
      async isRevoked(jti) {
        const until = revoked.get(jti);
        if (until !== undefined && until <= now()) revoked.delete(jti);
        else if (until !== undefined) return true;
        return options.revoked ? await options.revoked(jti) : false;
      },
      async revoke(jti, expMs) {
        if (expMs <= now()) return;
        revoked.set(jti, expMs);
        await track(sql`
          INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES (${jti}, ${expMs})
          ON CONFLICT (jti) DO UPDATE SET exp_ms = GREATEST(relay_revoked_tokens.exp_ms, EXCLUDED.exp_ms)`);
        await notify(CHANNELS.revoked, { origin, kind: "revoked", jti, expMs });
      },
    },
    nodeCommands: { subscribe: (fn) => commands.add(fn) },
    command(command) {
      commands.emit(command);
      void notify(CHANNELS.command, { origin, kind: "command", command });
    },
    revokedCount: () => {
      const at = now();
      let count = 0;
      for (const until of revoked.values()) if (until > at) count += 1;
      return count;
    },
    settle,
    async close() {
      if (closed) return;
      closed = true;
      clearInterval(sweep);
      await settle();
      await Promise.allSettled(
        subscriptions.map((subscription) => subscription.unlisten()),
      );
    },
  };
}
