/**
 * `ControlPlane` 的 Redis 实现（`--control redis`，多节点；Node 中继存储设计 §5.2）。
 *
 * | 功能 | 实现 |
 * | --- | --- |
 * | 路由 | `<前缀>route:src:<id>` = JSON，`SET … PX`；`touch` 用 `PEXPIRE`；`delete` 用脚本按 `tunnelId` CAS |
 * | 在线事件 / 节点命令 | 本节点立即投递 + `PUBLISH <前缀>presence` / `<前缀>ctl` |
 * | 撤销 | `<前缀>revoked:<jti>`，`SET … PXAT`，`PUBLISH <前缀>revoked`；各节点内存一份，订阅连接每次就绪都 `SCAN` 全量重读 |
 * | 速率限流 | `<前缀>limit:<种类>:<键的 SHA-256>`，脚本 `INCR` + 首次 `PEXPIRE`，各节点共用一个桶；出错退回本节点内存 |
 * | 并发限流 | 本节点内存 |
 *
 * 订阅用 `duplicate()` 出来的第二条连接；两条连接断线都由客户端自己重连。不写令牌明文（撤销只有 `jti`）。
 */
import { createHash, randomBytes } from "node:crypto";

import type { Redis } from "@armadra/cloud-shared/redis";
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

export interface RedisControlOptions extends ClusterControlOptions {
  /** 已连上的客户端（订阅另开一条 `duplicate()`）。 */
  redis: Redis;
  /** 键与频道的前缀；缺省 `relay:`。 */
  prefix?: string;
}

export interface RedisControlPlane extends ClusterControlPlane {
  revokedCount(): number;
  settle(): Promise<void>;
}

const DELETE_ROUTE = `
local value = redis.call('GET', KEYS[1])
if value and cjson.decode(value).tunnelId == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`;

const HIT = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return { count, ttl }`;

export async function createRedisControlPlane(
  options: RedisControlOptions,
): Promise<RedisControlPlane> {
  const { redis } = options;
  const prefix = options.prefix ?? "relay:";
  const now = options.now ?? Date.now;
  const rules = { ...DEFAULT_LIMIT_RULES, ...options.limits };
  const origin = randomBytes(8).toString("hex");
  const onError = (error: unknown, operation: string) =>
    options.onError?.(error, operation);
  const local = createMemoryControlPlane({
    issuer: options.issuer,
    jwks: options.jwks,
    limits: options.limits,
    now,
  });
  const keys = {
    route: (sourceId: string) => `${prefix}route:src:${sourceId}`,
    limit: (kind: string, key: string) =>
      `${prefix}limit:${kind}:${createHash("sha256").update(key).digest("hex")}`,
    revoked: (jti: string) => `${prefix}revoked:${jti}`,
  };
  const channels = {
    presence: `${prefix}presence`,
    command: `${prefix}ctl`,
    revoked: `${prefix}revoked`,
  };
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
  const background = (operation: string, task: () => Promise<unknown>) =>
    track(
      task().catch((error: unknown) => {
        onError(error, operation);
      }),
    ).then(() => undefined);

  const reloadRevoked = async () => {
    const found = new Map<string, number>();
    const at = now();
    for await (const batch of redis.scanIterator({
      MATCH: keys.revoked("*"),
      COUNT: 500,
    })) {
      for (const key of batch) {
        const expiry = await redis.pExpireTime(key);
        if (expiry > at) found.set(key.slice(keys.revoked("").length), expiry);
      }
    }
    revoked.clear();
    for (const [jti, until] of found) revoked.set(jti, until);
  };

  const receive = (payload: string) => {
    const message = parseBroadcast(payload);
    if (!message || message.origin === origin) return;
    if (message.kind === "presence") presence.emit(message.event);
    else if (message.kind === "command") commands.emit(message.command);
    else if (message.expMs > now()) revoked.set(message.jti, message.expMs);
  };

  const subscriber = redis.duplicate();
  subscriber.on("error", (error: unknown) => onError(error, "subscriber"));
  // 每次（重新）就绪都全量重读撤销：断线期间的撤销不漏。
  subscriber.on("ready", () => {
    void background("reload revoked", reloadRevoked);
  });
  await subscriber.connect();
  await subscriber.subscribe(
    [channels.presence, channels.command, channels.revoked],
    receive,
  );
  await reloadRevoked();

  const publish = (channel: string, message: Broadcast) =>
    background(`publish ${channel}`, () =>
      redis.publish(channel, JSON.stringify(message)),
    );

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
        const value = await redis.get(keys.route(sourceId));
        return value ? (JSON.parse(value) as Route) : null;
      },
      async set(sourceId, route, ttlMs) {
        await track(
          redis.set(keys.route(sourceId), JSON.stringify(route), {
            expiration: { type: "PX", value: ttlMs },
          }),
        );
      },
      touch(sourceId, ttlMs) {
        return background("route touch", () =>
          redis.pExpire(keys.route(sourceId), ttlMs),
        );
      },
      delete(sourceId, tunnelId) {
        return background("route delete", () =>
          redis.eval(DELETE_ROUTE, {
            keys: [keys.route(sourceId)],
            arguments: [tunnelId],
          }),
        );
      },
    },
    presence: {
      async publish(event) {
        presence.emit(event);
        await publish(channels.presence, { origin, kind: "presence", event });
      },
      subscribe: (fn) => presence.add(fn),
    },
    limits: {
      async hit(kind, key): Promise<LimitResult> {
        const rule = rules[kind];
        if (rule.type === "concurrent") return local.limits.hit(kind, key);
        try {
          const reply = (await redis.eval(HIT, {
            keys: [keys.limit(kind, key)],
            arguments: [String(rule.windowMs)],
          })) as [number, number];
          const [count, ttl] = reply.map(Number) as [number, number];
          if (count <= rule.max) return { allowed: true };
          return { allowed: false, retryAfterMs: Math.max(0, ttl) };
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
        await track(
          redis.set(keys.revoked(jti), "1", {
            expiration: { type: "PXAT", value: expMs },
          }),
        );
        await publish(channels.revoked, {
          origin,
          kind: "revoked",
          jti,
          expMs,
        });
      },
    },
    nodeCommands: { subscribe: (fn) => commands.add(fn) },
    command(command) {
      commands.emit(command);
      void publish(channels.command, { origin, kind: "command", command });
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
      await settle();
      subscriber.destroy();
    },
  };
}
