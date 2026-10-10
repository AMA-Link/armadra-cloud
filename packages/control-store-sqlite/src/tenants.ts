/**
 * 租户行与整租户的导入 / 导出（personal 的 `migrate --to sqlite|file`、`status`、`doctor` 用）。
 *
 * - 租户行 = 控制核的 {@link TenantSettings} + 中继自己的设置（边缘限流额度、受信代理）+ 修订号；
 * - 快照里各表按创建顺序（rowid），导入按数组顺序写，导出再导入行序不变。
 */
import type {
  AccountRecord,
  DeviceRecord,
  LinkRecord,
  SigningKeyRecord,
  SourceRecord,
  TenantSettings,
} from "@armadra/control-core/ports";

import type { SqlDatabase as Database, KeyWrap, Row } from "./db.ts";
import { readSigningKeys } from "./keys.ts";
import {
  bumpRevision,
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
    allowGuests: Number(row.allow_guests) === 1,
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

export function listTenants(db: Database): string[] {
  return db
    .all("SELECT tenant_id FROM tenants ORDER BY rowid")
    .map((row) => String(row.tenant_id));
}

export function readTenant(db: Database, tenantId: string): TenantRow | null {
  const row = db.get("SELECT * FROM tenants WHERE tenant_id = ?", tenantId);
  return row ? toTenant(row) : null;
}

/** 建租户（已有同 id 的会因主键冲突抛错）。 */
export function createTenant(
  db: Database,
  tenantId: string,
  tenant: TenantRecord,
  revision = 0,
): void {
  db.run(
    `INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, rate_limits_json,
       trusted_proxy_json, display_name, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
    tenantId,
    tenant.issuer,
    tenant.allowGuests ? 1 : 0,
    JSON.stringify(tenant.trustedOrigins),
    optionalJson(tenant.rateLimits),
    optionalJson(tenant.trustedProxy),
    tenant.displayName ?? null,
    revision,
  );
}

/** 租户设置的原子读-改-写；没变不写。 */
export function updateTenant<T>(
  db: Database,
  tenantId: string,
  fn: (draft: TenantRecord) => T,
): T {
  return db.transaction(() => {
    const current = readTenant(db, tenantId);
    if (!current) throw new Error(`没有这个租户：${tenantId}`);
    const { revision: _revision, ...before } = current;
    const draft = structuredClone(before);
    const result = fn(draft);
    if (JSON.stringify(draft) !== JSON.stringify(before)) {
      db.run(
        `UPDATE tenants SET issuer = ?, allow_guests = ?, trusted_origins_json = ?,
           rate_limits_json = ?, trusted_proxy_json = ?, display_name = ?
         WHERE tenant_id = ?`,
        draft.issuer,
        draft.allowGuests ? 1 : 0,
        JSON.stringify(draft.trustedOrigins),
        optionalJson(draft.rateLimits),
        optionalJson(draft.trustedProxy),
        draft.displayName ?? null,
        tenantId,
      );
      bumpRevision(db, tenantId);
    }
    return result;
  });
}

/** 直接放一个账号（建租户与测试装配用）。 */
export function addAccount(
  db: Database,
  tenantId: string,
  account: AccountRecord,
): void {
  db.transaction(() => {
    insertAccount(db, tenantId, account);
    bumpRevision(db, tenantId);
  });
}

/** 一个事务写入整个租户（租户必须还不存在）；修订号照快照。 */
export function importTenant(
  db: Database,
  tenantId: string,
  snapshot: TenantSnapshot,
  wrap?: KeyWrap,
): void {
  db.transaction(() => {
    const { revision, ...tenant } = snapshot.tenant;
    createTenant(db, tenantId, tenant, revision);
    for (const row of snapshot.accounts) insertAccount(db, tenantId, row);
    for (const row of snapshot.signingKeys)
      insertSigningKey(db, tenantId, row, wrap);
    for (const row of snapshot.sources) insertSource(db, tenantId, row);
    for (const row of snapshot.registrations)
      insertRegistration(db, tenantId, row);
    for (const row of snapshot.devices) insertDevice(db, tenantId, row);
    for (const row of snapshot.links) insertLink(db, tenantId, row);
  });
}

/** 整个租户的一致快照（读在一个事务里，别的进程写不进中间）。 */
export function exportTenant(
  db: Database,
  tenantId: string,
  wrap?: KeyWrap,
): TenantSnapshot {
  return db.transaction(() => {
    const tenant = readTenant(db, tenantId);
    if (!tenant) throw new Error(`没有这个租户：${tenantId}`);
    const rows = (table: string) =>
      db.all(
        `SELECT * FROM ${table} WHERE tenant_id = ? ORDER BY rowid`,
        tenantId,
      );
    return {
      tenant,
      accounts: rows("accounts").map(toAccount),
      signingKeys: readSigningKeys(db, tenantId, wrap),
      sources: rows("sources").map(toSource),
      registrations: rows("registration_tokens").map(toRegistration),
      devices: rows("devices").map(toDevice),
      links: rows("links").map(toLink),
    };
  });
}

/** 已撤销的源与链接（`serve` 察觉到别的进程改了库时比对用）。 */
export function revokedIds(
  db: Database,
  tenantId: string,
): { sources: string[]; links: string[] } {
  return {
    sources: db
      .all(
        "SELECT source_id FROM sources WHERE tenant_id = ? AND revoked_at_ms IS NOT NULL",
        tenantId,
      )
      .map((row) => String(row.source_id)),
    links: db
      .all(
        "SELECT link_id FROM links WHERE tenant_id = ? AND revoked_at_ms IS NOT NULL",
        tenantId,
      )
      .map((row) => String(row.link_id)),
  };
}
