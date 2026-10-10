/**
 * personal 的控制面装配：存储（状态文件、SQLite 或 PostgreSQL）的端口 + 签名钥 + 控制面（限流、
 * 在线、关隧道；单节点内存，多节点 Redis / PostgreSQL）+ 事件总线。
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
import type { RelayControl } from "@armadra/relay-runtime-node/control/types";

import { passwordViolation } from "./passwords.ts";
import type { PersonalStore } from "./store-types.ts";

export interface PersonalContext extends ControlContext {
  store: PersonalStore;
  control: RelayControl;
}

export function createPersonalContext(input: {
  store: PersonalStore;
  control: RelayControl;
  signer: Signer;
  now: () => number;
  /** 中继内核起来之后才有：断开撤销掉的客户端 WS。 */
  disconnect: (filter: ConnectionFilter) => void;
}): PersonalContext {
  const { store, control } = input;
  return {
    tenantId: DEFAULT_TENANT,
    ports: store.ports,
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

/** 签名钥读存储。 */
export function stateSigner(store: PersonalStore, now: () => number): Signer {
  return new Signer(store.keys, now);
}

/** 中继令牌 / 隧道令牌的撤销判断（按 `jti` 的形状查存储）。 */
export function stateRevocation(
  store: PersonalStore,
  now: () => number,
): (jti: string) => Promise<boolean> {
  const context = { tenantId: DEFAULT_TENANT, ports: store.ports };
  return (jti) => isRevokedJti(context, jti, now());
}

/** CLI（不经 serve）用的最小装配：只有存储端口与口令策略。 */
export function stateOnly(store: PersonalStore) {
  return {
    tenantId: DEFAULT_TENANT,
    ports: store.ports,
    passwordPolicy: passwordViolation,
  };
}
