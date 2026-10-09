/**
 * `ControlPlane` 的进程内实现（personal 模式；也给测试用）。路由、限流、撤销都在 Map 里，
 * 过期是惰性的（读时判）加写时顺手清扫；不写文件、不连任何外部服务。
 *
 * 本文件**不得** import Redis / PostgreSQL（`boundaries.test.ts` 守）。
 */
import type {
  ControlPlane,
  JwkSet,
  LimitKind,
  LimitResult,
  LimitRule,
  NodeCommand,
  PresenceEvent,
  Route,
} from "./types.ts";
import { DEFAULT_LIMIT_RULES } from "./types.ts";

export interface MemoryControlPlaneOptions {
  issuer: string;
  /** personal：签名钥的公钥集（控制核的 `Signer`）；测试：协议包的测试钥。 */
  jwks: () => JwkSet | Promise<JwkSet>;
  mode?: "saas" | "personal";
  now?: () => number;
  limits?: Partial<Record<LimitKind, LimitRule>>;
  /**
   * 撤销表之外的撤销判断（personal：`jti` 里编码了源 / 设备，按状态文件判它们是否已撤）。
   * 两者任一为真即视为撤销。
   */
  revoked?: (jti: string) => boolean | Promise<boolean>;
}

export interface MemoryControlPlane extends ControlPlane {
  /** 测试与 `/health` 用：当前未过期的路由数。 */
  routeCount(): number;
  /** 进程内的节点命令通道（personal：撤销源时关它的隧道）。 */
  nodeCommands: { subscribe(fn: (command: NodeCommand) => void): () => void };
  command(command: NodeCommand): void;
}

/** 超过这个条目数时，写入顺手清一遍过期项。 */
const SWEEP_AT = 1024;

export function createMemoryControlPlane(
  options: MemoryControlPlaneOptions,
): MemoryControlPlane {
  const now = options.now ?? Date.now;
  const rules = { ...DEFAULT_LIMIT_RULES, ...options.limits };
  const routes = new Map<string, { route: Route; expiresAtMs: number }>();
  const windows = new Map<string, { count: number; resetAtMs: number }>();
  const concurrent = new Map<string, number>();
  const revoked = new Map<string, number>();
  const listeners = new Set<(event: PresenceEvent) => void>();
  const commandListeners = new Set<(command: NodeCommand) => void>();

  const sweep = <V>(map: Map<string, V>, expiry: (value: V) => number) => {
    if (map.size < SWEEP_AT) return;
    const at = now();
    for (const [key, value] of map) if (expiry(value) <= at) map.delete(key);
  };

  const liveRoute = (sourceId: string) => {
    const entry = routes.get(sourceId);
    if (!entry) return undefined;
    if (entry.expiresAtMs <= now()) {
      routes.delete(sourceId);
      return undefined;
    }
    return entry;
  };

  return {
    mode: options.mode ?? "personal",
    issuer: options.issuer,
    jwks: async () => options.jwks(),
    routes: {
      async get(sourceId) {
        const entry = liveRoute(sourceId);
        return entry ? { ...entry.route } : null;
      },
      async set(sourceId, route, ttlMs) {
        sweep(routes, (entry) => entry.expiresAtMs);
        routes.set(sourceId, {
          route: { ...route },
          expiresAtMs: now() + ttlMs,
        });
      },
      async touch(sourceId, ttlMs) {
        const entry = liveRoute(sourceId);
        if (entry) entry.expiresAtMs = now() + ttlMs;
      },
      async delete(sourceId, tunnelId) {
        const entry = routes.get(sourceId);
        if (entry && entry.route.tunnelId === tunnelId) routes.delete(sourceId);
      },
    },
    presence: {
      async publish(event) {
        for (const listener of [...listeners]) {
          try {
            listener(event);
          } catch {
            // 一个订阅者出错不影响其它订阅者与发布方。
          }
        }
      },
      subscribe(fn) {
        listeners.add(fn);
        return () => listeners.delete(fn);
      },
    },
    limits: {
      async hit(kind, key): Promise<LimitResult> {
        const rule = rules[kind];
        const id = `${kind}\u0000${key}`;
        if (rule.type === "concurrent") {
          const used = concurrent.get(id) ?? 0;
          if (used >= rule.max) return { allowed: false };
          concurrent.set(id, used + 1);
          return { allowed: true };
        }
        const at = now();
        sweep(windows, (entry) => entry.resetAtMs);
        let entry = windows.get(id);
        if (!entry || entry.resetAtMs <= at) {
          entry = { count: 0, resetAtMs: at + rule.windowMs };
          windows.set(id, entry);
        }
        if (entry.count >= rule.max)
          return { allowed: false, retryAfterMs: entry.resetAtMs - at };
        entry.count += 1;
        return { allowed: true };
      },
      async release(kind, key) {
        if (rules[kind].type !== "concurrent") return;
        const id = `${kind}\u0000${key}`;
        const used = concurrent.get(id) ?? 0;
        if (used <= 1) concurrent.delete(id);
        else concurrent.set(id, used - 1);
      },
    },
    tokens: {
      async isRevoked(jti) {
        const until = revoked.get(jti);
        if (until !== undefined && until <= now()) revoked.delete(jti);
        else if (until !== undefined) return true;
        return options.revoked ? await options.revoked(jti) : false;
      },
      async revoke(jti, expMs) {
        sweep(revoked, (until) => until);
        if (expMs > now()) revoked.set(jti, expMs);
      },
    },
    nodeCommands: {
      subscribe(fn) {
        commandListeners.add(fn);
        return () => commandListeners.delete(fn);
      },
    },
    command(command) {
      for (const listener of [...commandListeners]) {
        try {
          listener(command);
        } catch {
          // 同 presence：一个订阅者出错不影响其它。
        }
      }
    },
    routeCount() {
      const at = now();
      let count = 0;
      for (const entry of routes.values())
        if (entry.expiresAtMs > at) count += 1;
      return count;
    },
  };
}
