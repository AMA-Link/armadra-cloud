/**
 * `ControlPlane`：隧道与边缘对控制面的全部依赖（中继设计 §2，平台设计 §17.2）。
 *
 * - personal：`control/memory.ts`（进程内 Map、事件总线、限流桶）。
 * - saas：`control/redis.ts`（R3，Redis 路由 / 限流 / 撤销 / `relay:ctl`）。
 *
 * `tunnel/`、`edge/`、`auth/` 只依赖本接口，对模式零分支。
 */
import type { JwkSet } from "@armadra/platform-protocol/assertion";

export type { JwkSet };

/** 路由表的一行：哪个节点、哪条隧道持有这个源。 */
export interface Route {
  node: string;
  tunnelId: string;
  sinceMs: number;
  coreVersion: string;
}

export type PresenceEvent =
  | {
      type: "sourceOnline";
      sourceId: string;
      node: string;
      tunnelId: string;
      coreVersion: string;
      atMs: number;
    }
  | {
      type: "sourceOffline";
      sourceId: string;
      node: string;
      tunnelId: string;
      atMs: number;
    };

/** saas：`relay:node:<node>` 上发给本节点的命令。 */
export type NodeCommand =
  | { type: "closeTunnel"; sourceId: string }
  | { type: "kick"; sourceId: string; reason: string };

export type LimitKind =
  | "tunnel.connect.src"
  | "edge.connect.ip"
  | "edge.preflight.ip"
  | "edge.connect.token"
  | "edge.connect.acct"
  | "edge.concurrent.acct"
  | "login.ip"
  | "login.acct"
  | "link.accept.ip";

export interface LimitResult {
  allowed: boolean;
  retryAfterMs?: number;
}

export interface ControlPlane {
  readonly mode: "saas" | "personal";
  /** 断言 / 令牌的 `iss`；校验中继令牌与隧道令牌时逐字比较。 */
  readonly issuer: string;
  /** 验令牌用的公钥集；personal 是自己的，saas 是 cloud 的（带缓存）。 */
  jwks(): Promise<JwkSet>;
  routes: {
    get(sourceId: string): Promise<Route | null>;
    set(sourceId: string, route: Route, ttlMs: number): Promise<void>;
    /** 心跳刷新 TTL；没有这条路由时什么也不做。 */
    touch(sourceId: string, ttlMs: number): Promise<void>;
    /** 只删 `tunnelId` 相同的那条（CAS），避免旧隧道删掉替换它的新路由。 */
    delete(sourceId: string, tunnelId: string): Promise<void>;
  };
  presence: {
    publish(event: PresenceEvent): Promise<void>;
    subscribe(fn: (event: PresenceEvent) => void): () => void;
  };
  limits: {
    /** 速率类：固定窗口计数；并发类（`edge.concurrent.acct`）：占一个名额，配 `release`。 */
    hit(kind: LimitKind, key: string): Promise<LimitResult>;
    release?(kind: LimitKind, key: string): Promise<void>;
  };
  tokens: {
    isRevoked(jti: string): Promise<boolean>;
    /** 记到 `expMs` 为止（令牌过期后撤销记录就没必要了）。 */
    revoke(jti: string, expMs: number): Promise<void>;
  };
  /** 只有 saas 有：本节点的命令通道。 */
  nodeCommands?: { subscribe(fn: (command: NodeCommand) => void): () => void };
}

/** 一种限流的规则：速率类是「窗口内最多 `max` 次」；并发类是「同时最多 `max` 个」。 */
export type LimitRule =
  | { type: "rate"; max: number; windowMs: number }
  | { type: "concurrent"; max: number };

/** 缺省限流（中继设计 §3.1、§4、§6；平台设计 §17.3）。 */
export const DEFAULT_LIMIT_RULES: Readonly<Record<LimitKind, LimitRule>> = {
  "tunnel.connect.src": { type: "rate", max: 10, windowMs: 60_000 },
  // 无令牌或令牌无效的请求（含 401 路径）：按来源 IP 严格限流，防撞库与扫描。
  "edge.connect.ip": { type: "rate", max: 60, windowMs: 60_000 },
  // CORS 预检：单独按 IP 计，突发不挤占已鉴权的请求。
  "edge.preflight.ip": { type: "rate", max: 300, windowMs: 60_000 },
  // 带有效中继令牌的请求：按令牌（jti）、再按账号（sub）计数，不看 IP。
  "edge.connect.token": { type: "rate", max: 600, windowMs: 60_000 },
  "edge.connect.acct": { type: "rate", max: 1800, windowMs: 60_000 },
  "edge.concurrent.acct": { type: "concurrent", max: 64 },
  "login.ip": { type: "rate", max: 20, windowMs: 60_000 },
  "login.acct": { type: "rate", max: 20, windowMs: 60_000 },
  "link.accept.ip": { type: "rate", max: 20, windowMs: 60_000 },
};

/** 路由 TTL：心跳（20 秒）两次多一点；心跳 `PONG` 时 `touch`。 */
export const ROUTE_TTL_MS = 45_000;
