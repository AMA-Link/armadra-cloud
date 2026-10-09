/**
 * personal 的控制面装配：状态文件端口 + 签名钥 + 内存控制面（限流、在线、关隧道）+ 事件总线。
 * 服务本身在 `@armadra/control-core`。
 */
import { createEventBus } from "@armadra/control-core/events";
import {
  DEFAULT_TENANT,
  type ConnectionFilter,
  type ControlContext,
} from "@armadra/control-core/ports";
import { isRevokedJti } from "@armadra/control-core/revocation";
import { Signer } from "@armadra/control-core/signer";
import type { MemoryControlPlane } from "@armadra/relay-kernel/control/memory";

import { passwordViolation } from "./passwords.ts";
import { createStatePorts, stateKeys } from "./ports.ts";
import type { StateStore } from "./state.ts";

export interface PersonalContext extends ControlContext {
  store: StateStore;
  control: MemoryControlPlane;
}

export function createPersonalContext(input: {
  store: StateStore;
  control: MemoryControlPlane;
  signer: Signer;
  now: () => number;
  /** 中继内核起来之后才有：断开撤销掉的客户端 WS。 */
  disconnect: (filter: ConnectionFilter) => void;
}): PersonalContext {
  const { store, control } = input;
  return {
    tenantId: DEFAULT_TENANT,
    ports: createStatePorts(store),
    signer: input.signer,
    limits: control.limits,
    presence: control.presence,
    relay: {
      route: (sourceId) => control.routes.get(sourceId),
      closeTunnel: (sourceId) =>
        control.command({ type: "closeTunnel", sourceId }),
      disconnect: input.disconnect,
    },
    passwordPolicy: passwordViolation,
    now: input.now,
    ...createEventBus(),
    store,
    control,
  };
}

/** 签名钥读状态文件。 */
export function stateSigner(store: StateStore, now: () => number): Signer {
  return new Signer(stateKeys(store), now);
}

/** 中继令牌 / 隧道令牌的撤销判断（按 `jti` 的形状查状态文件）。 */
export function stateRevocation(
  store: StateStore,
  now: () => number,
): (jti: string) => Promise<boolean> {
  const context = { tenantId: DEFAULT_TENANT, ports: createStatePorts(store) };
  return (jti) => isRevokedJti(context, jti, now());
}

/** CLI（不经 serve）用的最小装配：只有状态文件端口与口令策略。 */
export function stateOnly(store: StateStore) {
  return {
    tenantId: DEFAULT_TENANT,
    ports: createStatePorts(store),
    passwordPolicy: passwordViolation,
  };
}
