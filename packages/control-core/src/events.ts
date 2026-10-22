/**
 * `/v1/me/stream` 的事件总线（进程内）。saas 换成跨节点的实现时保持同一个形状。
 */
import type { MeStreamEvent } from "@armadra/platform-protocol/cloud-api";

/**
 * 给 `/v1/me/stream` 的事件（不含 ping / pong），带上它关心的源。协议 1.4 的团队事件
 * （`memberChanged` 等，cloud-api §32）只有 SaaS 控制面发，personal 的总线不收。
 */
export type ControlEvent = Exclude<
  MeStreamEvent,
  | { type: "ping" }
  | { type: "pong" }
  | { type: "memberChanged" }
  | { type: "invitationChanged" }
  | { type: "grantChanged" }
  | { type: "accessRequested" }
  | { type: "deviceTrustRequested" }
  | { type: "deviceTrustResolved" }
  | { type: "planChanged" }
>;

export interface EventAudience {
  linkId: string;
}

export interface EventBus {
  /** 给了 `audience` 就只发给那条链接的访客。 */
  emit(event: ControlEvent, audience?: EventAudience): void;
  subscribe(
    fn: (event: ControlEvent, audience?: EventAudience) => void,
  ): () => void;
}

export function createEventBus(): EventBus {
  const listeners = new Set<
    (event: ControlEvent, audience?: EventAudience) => void
  >();
  return {
    emit(event, audience) {
      for (const listener of [...listeners]) {
        try {
          listener(event, audience);
        } catch {
          // 一个订阅者出错不影响其它订阅者。
        }
      }
    },
    subscribe(fn) {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}
