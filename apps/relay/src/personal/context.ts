/**
 * personal 控制面各模块共用的上下文与错误。
 *
 * `PersonalError` 只带注册表里的码；HTTP 层按码查状态码，`message` 是固定英文短句，
 * 界面按码取 i18n 文案。
 */
import type { MeStreamEvent } from "@armadra/platform-protocol/cloud-api";
import type { ErrorCode } from "@armadra/platform-protocol/errors";

import type { MemoryControlPlane } from "../control/memory.ts";
import type { EdgeConnectionFilter } from "../edge/server.ts";
import type { Signer } from "./signer.ts";
import type { StateStore } from "./state.ts";

export class PersonalError extends Error {
  readonly code: ErrorCode;
  readonly retryAfterMs?: number;
  constructor(code: ErrorCode, options: { retryAfterMs?: number } = {}) {
    super(code);
    this.name = "PersonalError";
    this.code = code;
    if (options.retryAfterMs !== undefined)
      this.retryAfterMs = Math.max(0, Math.ceil(options.retryAfterMs));
  }
}

/** 给 `/v1/me/stream` 的事件（不含 ping / pong），带上它关心的源。 */
export type PersonalEvent = Exclude<
  MeStreamEvent,
  { type: "ping" } | { type: "pong" }
>;

export interface PersonalContext {
  store: StateStore;
  signer: Signer;
  control: MemoryControlPlane;
  now: () => number;
  /** 中继内核起来之后才有：断开撤销掉的客户端 WS。 */
  disconnect: (filter: EdgeConnectionFilter) => void;
  /** 进程内事件总线（`me/stream`）。给了 `audience` 就只发给那条链接的访客。 */
  emit: (event: PersonalEvent, audience?: EventAudience) => void;
  subscribe: (
    fn: (event: PersonalEvent, audience?: EventAudience) => void,
  ) => () => void;
}

export interface EventAudience {
  linkId: string;
}

/** 进程内事件总线。 */
export function createEventBus(): Pick<PersonalContext, "emit" | "subscribe"> {
  const listeners = new Set<
    (event: PersonalEvent, audience?: EventAudience) => void
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

/** `acct:<账号名>`：personal 唯一账号的 accountId。 */
export function ownerSubject(name: string): string {
  return `acct:${name}`;
}

export function guestPrefix(linkId: string): string {
  return `guest:${linkId}:`;
}

/** `guest:<linkId>:<随机>` → linkId；不是访客答 null。 */
export function guestLinkId(subject: string): string | null {
  const match = /^guest:([0-9a-f]{16}):[0-9a-f]+$/.exec(subject);
  return match?.[1] ?? null;
}
