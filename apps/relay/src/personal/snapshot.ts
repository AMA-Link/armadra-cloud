/**
 * 状态文件（schema v1）↔ 一个租户的快照：SQLite 与 PostgreSQL 两种库存储共用（两边的
 * `TenantSnapshot` 同形）。账号 id 与注册令牌的签发人都是 `acct:<账号名>`；两个方向逐字段对应，
 * 迁移往返等价。
 */
import { ownerSubject } from "@armadra/control-core/subjects";
import type { TenantSnapshot } from "@armadra/control-store-sqlite/tenants";

import { STATE_VERSION, StateError, stateSchema, type State } from "./state.ts";
import type { StoreSettings } from "./store-types.ts";

export type { TenantSnapshot };

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

/** 快照 → 状态文件（schema v1）；不是「一个账号」或字段不符 → `invalid`。 */
export function snapshotToState(snapshot: TenantSnapshot): State {
  const [account, ...extra] = snapshot.accounts;
  if (!account || extra.length > 0)
    throw new StateError(
      "invalid",
      `个人中转只有一个账号，库里有 ${snapshot.accounts.length} 个，拒绝启动`,
    );
  const { accountId: _accountId, ...accountFields } = account;
  const { tenant } = snapshot;
  const parsed = stateSchema.safeParse({
    version: STATE_VERSION,
    revision: tenant.revision,
    issuer: tenant.issuer,
    account: accountFields,
    signingKeys: snapshot.signingKeys,
    sources: snapshot.sources,
    registrationTokens: snapshot.registrations.map(
      ({ tokenHash, expiresAtMs }) => ({ tokenHash, expiresAtMs }),
    ),
    devices: snapshot.devices,
    links: snapshot.links,
    settings: {
      allowGuests: tenant.allowGuests,
      trustedOrigins: tenant.trustedOrigins,
      ...(tenant.rateLimits ? { rateLimits: tenant.rateLimits } : {}),
      ...(tenant.displayName !== undefined
        ? { displayName: tenant.displayName }
        : {}),
      ...(tenant.trustedProxy ? { trustedProxy: tenant.trustedProxy } : {}),
    },
  });
  if (!parsed.success)
    throw new StateError("invalid", "数据库内容与状态 schema 不符，拒绝启动");
  return parsed.data;
}

/** 租户行 → `settings`（`serve` 每个请求都可能读）。 */
export function tenantSettings(
  tenant: TenantSnapshot["tenant"],
): StoreSettings {
  return {
    allowGuests: tenant.allowGuests,
    trustedOrigins: tenant.trustedOrigins,
    ...(tenant.rateLimits
      ? { rateLimits: tenant.rateLimits as StoreSettings["rateLimits"] }
      : {}),
    ...(tenant.displayName !== undefined
      ? { displayName: tenant.displayName }
      : {}),
    ...(tenant.trustedProxy ? { trustedProxy: tenant.trustedProxy } : {}),
  };
}
