/**
 * 租户行与整租户的导入 / 导出（个人中转 `--store postgres` 的 `init`、`migrate --to postgres|sqlite`、
 * `status` 用）。形状与 `control-store-sqlite` 的 `tenants.ts` 一致，两边的快照可以直接互导。
 *
 * - 租户行 = 控制核的 {@link TenantSettings} + 中继自己的设置（边缘限流额度、受信代理、名称）+ 修订号；
 * - 快照里各表按创建顺序（`seq`），导入按数组顺序写，导出再导入行序不变。
 */
import type {
  AccountRecord,
  DeviceRecord,
  LinkRecord,
  SigningKeyRecord,
  SourceRecord,
  TenantSettings,
} from "@armadra/control-core/ports";

import {
  bumpRevision,
  lockTenant,
  transaction,
  type Queryable,
  type Row,
  type Sql,
} from "./db.ts";
import { PostgresStoreError } from "./errors.ts";
import { readSigningKeys } from "./keys.ts";
import {
  insertAccount,
  insertDevice,
  insertLink,
  insertRegistration,
  insertSigningKey,
  insertSource,
  toAccount,
  toDevice,
  toLink,
  toRegistration,
  toSource,
  writeSource,
  type RegistrationRow,
} from "./rows.ts";

export interface TrustedProxyRecord {
  proxies: string[];
  prefer?: "forwarded-for" | "real-ip";
}

/** 中继自己的租户设置（都可省略）。 */
export interface TenantExtras {
  /** 边缘限流额度（次 / 分钟）。 */
  rateLimits?: Record<string, number>;
  trustedProxy?: TrustedProxyRecord;
  /** 服务端名称（`platform.info.name`）。 */
  displayName?: string;
}

export type TenantRecord = TenantSettings & TenantExtras;

export interface TenantRow extends TenantRecord {
  revision: number;
}

export interface TenantSnapshot {
  tenant: TenantRow;
  accounts: AccountRecord[];
  signingKeys: SigningKeyRecord[];
  sources: SourceRecord[];
  registrations: RegistrationRow[];
  devices: DeviceRecord[];
  links: LinkRecord[];
}

function toTenant(row: Row): TenantRow {
  const tenant: TenantRow = {
    issuer: String(row.issuer),
    allowGuests: row.allow_guests === true,
    trustedOrigins: JSON.parse(String(row.trusted_origins_json)) as string[],
    revision: Number(row.revision),
  };
  if (row.rate_limits_json !== null)
    tenant.rateLimits = JSON.parse(String(row.rate_limits_json)) as Record<
      string,
      number
    >;
  if (row.trusted_proxy_json !== null)
    tenant.trustedProxy = JSON.parse(
      String(row.trusted_proxy_json),
    ) as TrustedProxyRecord;
  if (row.display_name !== null) tenant.displayName = String(row.display_name);
  return tenant;
}

const optionalJson = (value: unknown) =>
  value === undefined ? null : JSON.stringify(value);

export async function listTenants(db: Queryable): Promise<string[]> {
  const rows = await db`SELECT tenant_id FROM tenants ORDER BY seq`;
  return rows.map((row) => String(row.tenant_id));
}

export async function readTenant(
  db: Queryable,
  tenantId: string,
): Promise<TenantRow | null> {
  const [row] = await db`SELECT * FROM tenants WHERE tenant_id = ${tenantId}`;
  return row ? toTenant(row) : null;
}

/** 租户的修订号；没有这个租户答 null（个人中转每秒看一次，察觉别的进程 / 节点的写）。 */
export async function readRevision(
  db: Queryable,
  tenantId: string,
): Promise<number | null> {
  const [row] =
    await db`SELECT revision FROM tenants WHERE tenant_id = ${tenantId}`;
  return row ? Number(row.revision) : null;
}

/** 建租户（已有同 id 的会因主键冲突抛错）。 */
export async function createTenant(
  db: Queryable,
  tenantId: string,
  tenant: TenantRecord,
  revision = 0,
): Promise<void> {
  await db`
    INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, rate_limits_json,
      trusted_proxy_json, display_name, revision)
    VALUES (${tenantId}, ${tenant.issuer}, ${tenant.allowGuests},
      ${JSON.stringify(tenant.trustedOrigins)}, ${optionalJson(tenant.rateLimits)},
      ${optionalJson(tenant.trustedProxy)}, ${tenant.displayName ?? null}, ${revision})`;
}

/** 租户设置的原子读-改-写；没变不写。答 `fn` 的结果。 */
export async function updateTenant<T>(
  sql: Sql,
  tenantId: string,
  fn: (draft: TenantRecord) => T,
  onWrite?: (revision: number) => void,
): Promise<T> {
  let revision: number | null = null;
  const result = await transaction(sql, async (tx) => {
    await lockTenant(tx, tenantId);
    const current = await readTenant(tx, tenantId);
    if (!current)
      throw new PostgresStoreError("not_found", `没有这个租户：${tenantId}`);
    const { revision: _revision, ...before } = current;
    const draft = structuredClone(before);
    const result = fn(draft);
    if (JSON.stringify(draft) !== JSON.stringify(before)) {
      await tx`
        UPDATE tenants SET issuer = ${draft.issuer}, allow_guests = ${draft.allowGuests},
          trusted_origins_json = ${JSON.stringify(draft.trustedOrigins)},
          rate_limits_json = ${optionalJson(draft.rateLimits)},
          trusted_proxy_json = ${optionalJson(draft.trustedProxy)},
          display_name = ${draft.displayName ?? null}
        WHERE tenant_id = ${tenantId}`;
      revision = await bumpRevision(tx, tenantId);
    }
    return result;
  });
  if (revision !== null) onWrite?.(revision);
  return result;
}

/** 直接放一个账号（建租户与测试装配用）。 */
export async function addAccount(
  sql: Sql,
  tenantId: string,
  account: AccountRecord,
): Promise<void> {
  await transaction(sql, async (tx) => {
    await lockTenant(tx, tenantId);
    await insertAccount(tx, tenantId, account);
    await bumpRevision(tx, tenantId);
  });
}

/** 一个事务写入整个租户（租户必须还不存在，否则 `exists`）；修订号照快照。 */
export async function importTenant(
  sql: Sql,
  tenantId: string,
  snapshot: TenantSnapshot,
): Promise<void> {
  await transaction(sql, async (tx) => {
    const existing = await readTenant(tx, tenantId);
    if (existing)
      throw new PostgresStoreError("exists", `租户已存在：${tenantId}`);
    const { revision, ...tenant } = snapshot.tenant;
    await createTenant(tx, tenantId, tenant, revision);
    for (const row of snapshot.accounts) await insertAccount(tx, tenantId, row);
    for (const row of snapshot.signingKeys)
      await insertSigningKey(tx, tenantId, row);
    for (const row of snapshot.sources) await insertSource(tx, tenantId, row);
    for (const row of snapshot.registrations)
      await insertRegistration(tx, tenantId, row);
    for (const row of snapshot.devices) await insertDevice(tx, tenantId, row);
    for (const row of snapshot.links) await insertLink(tx, tenantId, row);
  });
}

/** 整个租户的一致快照（读在一个 `REPEATABLE READ` 只读事务里，别的写不进中间）。 */
export async function exportTenant(
  sql: Sql,
  tenantId: string,
): Promise<TenantSnapshot> {
  return (await sql.begin(
    "isolation level repeatable read read only",
    async (tx) => {
      const tenant = await readTenant(tx, tenantId);
      if (!tenant)
        throw new PostgresStoreError("not_found", `没有这个租户：${tenantId}`);
      const accounts =
        await tx`SELECT * FROM accounts WHERE tenant_id = ${tenantId} ORDER BY seq`;
      const sources =
        await tx`SELECT * FROM sources WHERE tenant_id = ${tenantId} ORDER BY seq`;
      const registrations =
        await tx`SELECT * FROM registration_tokens WHERE tenant_id = ${tenantId} ORDER BY seq`;
      const devices =
        await tx`SELECT * FROM devices WHERE tenant_id = ${tenantId} ORDER BY seq`;
      const links =
        await tx`SELECT * FROM links WHERE tenant_id = ${tenantId} ORDER BY seq`;
      return {
        tenant,
        accounts: accounts.map(toAccount),
        signingKeys: await readSigningKeys(tx, tenantId),
        sources: sources.map(toSource),
        registrations: registrations.map(toRegistration),
        devices: devices.map(toDevice),
        links: links.map(toLink),
      };
    },
  )) as TenantSnapshot;
}

/** 已撤销的源与链接（个人中转察觉到外部改动时比对用）。 */
export async function revokedIds(
  db: Queryable,
  tenantId: string,
): Promise<{ sources: string[]; links: string[] }> {
  const [sources, links] = await Promise.all([
    db`SELECT source_id FROM sources WHERE tenant_id = ${tenantId} AND revoked_at_ms IS NOT NULL`,
    db`SELECT link_id FROM links WHERE tenant_id = ${tenantId} AND revoked_at_ms IS NOT NULL`,
  ]);
  return {
    sources: sources.map((row) => String(row.source_id)),
    links: links.map((row) => String(row.link_id)),
  };
}

/** 源的最近在线与版本（个人中转按批落盘；只往后推）。改了答 true。 */
export async function updateSources(
  sql: Sql,
  tenantId: string,
  fn: (source: SourceRecord) => void,
  onWrite?: (revision: number) => void,
): Promise<boolean> {
  let revision: number | null = null;
  const changed = await transaction(sql, async (tx) => {
    await lockTenant(tx, tenantId);
    const rows =
      await tx`SELECT * FROM sources WHERE tenant_id = ${tenantId} ORDER BY seq`;
    let changed = false;
    for (const row of rows) {
      const before = toSource(row);
      const draft = structuredClone(before);
      fn(draft);
      if (JSON.stringify(draft) === JSON.stringify(before)) continue;
      await writeSource(tx, tenantId, before.sourceId, draft);
      changed = true;
    }
    if (changed) revision = await bumpRevision(tx, tenantId);
    return changed;
  });
  if (revision !== null) onWrite?.(revision);
  return changed;
}
