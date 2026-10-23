/**
 * 状态文件（schema v1）→ 一个租户的快照（账号 id 与注册令牌的签发人都是 `acct:<账号名>`），
 * 只给 make-fixtures.mjs 生成夹具用；Go 的对应实现是 go/internal/store/statefile。
 */
import { ownerSubject } from "../../../packages/control-core/src/subjects.ts";
import type { TenantSnapshot } from "../../../packages/control-store-sqlite/src/tenants.ts";

import type { State } from "./state-schema.ts";

/** 状态文件 → 一个租户的快照（账号 id 与注册令牌的签发人都是 `acct:<账号名>`）。 */
export function stateToSnapshot(state: State): TenantSnapshot {
  const accountId = ownerSubject(state.account.name);
  const { settings } = state;
  return {
    tenant: {
      issuer: state.issuer,
      allowGuests: settings.allowGuests,
      trustedOrigins: [...settings.trustedOrigins],
      ...(settings.rateLimits
        ? { rateLimits: { ...settings.rateLimits } }
        : {}),
      ...(settings.trustedProxy
        ? { trustedProxy: structuredClone(settings.trustedProxy) }
        : {}),
      ...(settings.displayName !== undefined
        ? { displayName: settings.displayName }
        : {}),
      revision: state.revision,
    },
    accounts: [{ ...structuredClone(state.account), accountId }],
    signingKeys: structuredClone(state.signingKeys),
    sources: structuredClone(state.sources),
    registrations: state.registrationTokens.map((row) => ({
      ...row,
      accountId,
    })),
    devices: structuredClone(state.devices),
    links: structuredClone(state.links),
  };
}
