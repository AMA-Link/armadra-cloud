/**
 * 多节点控制面（`--control memory|redis|postgres`）的共同形状：内核的 {@link ControlPlane} 加一个
 * 广播给全部节点（含本节点）的命令通道与 `close()`。
 *
 * - `memory`（缺省）：内核的 `createMemoryControlPlane`，单节点时就是正确的；
 * - `redis`：`redis.ts`（路由 `SET … PX`、pub/sub、`INCR` 限流、`SET … PXAT` 撤销）；
 * - `postgres`：`postgres.ts`（路由 / 撤销 / 限流表 + `LISTEN/NOTIFY`）。
 *
 * 本文件只有类型与纯函数，不 import 任何存储客户端；两个实现由装配方按需动态 import。
 */
import type {
  ControlPlane,
  JwkSet,
  LimitKind,
  LimitRule,
  NodeCommand,
  PresenceEvent,
} from "@armadra/relay-kernel/control/types";

export const CONTROL_KINDS = ["memory", "redis", "postgres"] as const;
export type ControlKind = (typeof CONTROL_KINDS)[number];

/** 装配方用到的控制面：内核接口 + 命令通道（内核的内存实现也满足）。 */
export interface RelayControl extends ControlPlane {
  nodeCommands: { subscribe(fn: (command: NodeCommand) => void): () => void };
  /** 发给全部节点（本节点立即收到，其余经广播）。 */
  command(command: NodeCommand): void;
}

export interface ClusterControlPlane extends RelayControl {
  close(): Promise<void>;
}

export interface ClusterControlOptions {
  issuer: string;
  jwks: () => JwkSet | Promise<JwkSet>;
  /** 本节点的名字（路由的 `node`）。 */
  nodeId: string;
  mode?: "saas" | "personal";
  now?: () => number;
  limits?: Partial<Record<LimitKind, LimitRule>>;
  /** 撤销表之外的撤销判断（personal：按存储判源 / 设备是否已撤），任一为真即撤销。 */
  revoked?: (jti: string) => boolean | Promise<boolean>;
  /** 后台操作（续路由、广播、清扫）失败时回调；只给错误，不带令牌与正文。 */
  onError?: (error: unknown, operation: string) => void;
}

/** 广播消息的外壳：`origin` 是发送实例，自己发的不再投递一次。 */
export type Broadcast =
  | { origin: string; kind: "presence"; event: PresenceEvent }
  | { origin: string; kind: "command"; command: NodeCommand }
  | { origin: string; kind: "revoked"; jti: string; expMs: number };

/** 解析广播；不认识的形状答 null（别的版本的节点发的，忽略）。 */
export function parseBroadcast(text: string): Broadcast | null {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (!value || typeof value !== "object") return null;
  const message = value as Record<string, unknown>;
  if (typeof message.origin !== "string") return null;
  if (
    message.kind === "presence" &&
    message.event &&
    typeof (message.event as PresenceEvent).sourceId === "string"
  )
    return message as Broadcast;
  if (
    message.kind === "command" &&
    message.command &&
    typeof (message.command as NodeCommand).sourceId === "string"
  )
    return message as Broadcast;
  if (
    message.kind === "revoked" &&
    typeof message.jti === "string" &&
    typeof message.expMs === "number"
  )
    return message as Broadcast;
  return null;
}

/** 一组订阅者：一个出错不影响其余与发布方。 */
export function listeners<T>(): {
  add(fn: (value: T) => void): () => void;
  emit(value: T): void;
} {
  const set = new Set<(value: T) => void>();
  return {
    add(fn) {
      set.add(fn);
      return () => set.delete(fn);
    },
    emit(value) {
      for (const fn of [...set]) {
        try {
          fn(value);
        } catch {
          // 一个订阅者出错不影响其它订阅者与发布方。
        }
      }
    },
  };
}
