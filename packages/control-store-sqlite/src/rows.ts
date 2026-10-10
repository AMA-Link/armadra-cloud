/**
 * 行 ↔ 记录：列与 `ports.ts` 的记录一一对应（camelCase ↔ snake_case），`*_json` 列存 JSON。
 * 读出来的都是新对象（调用方改它不会动到库）；写都按整行写。
 */
import type {
  AccountRecord,
  DeviceRecord,
  LinkRecord,
  RegistrationTokenRecord,
  SigningKeyRecord,
  SourceRecord,
} from "@armadra/control-core/ports";

import type { Database, Row, SqlValue } from "./open.ts";

const text = (value: SqlValue | undefined): string => String(value ?? "");
const int = (value: SqlValue | undefined): number => Number(value ?? 0);
const nullableInt = (value: SqlValue | undefined): number | null =>
  value === null || value === undefined ? null : Number(value);
const json = <T>(value: SqlValue | undefined): T =>
  JSON.parse(text(value)) as T;

export type RegistrationRow = RegistrationTokenRecord & { accountId: string };

// ------------------------------------------------------------ 账号

export function toAccount(row: Row): AccountRecord {
  return {
    accountId: text(row.account_id),
    name: text(row.name),
    password: json(row.password_json),
    createdAtMs: int(row.created_at_ms),
    updatedAtMs: int(row.updated_at_ms),
    failures: int(row.failures),
    lockedUntilMs: int(row.locked_until_ms),
    lastFailureAtMs: int(row.last_failure_at_ms),
  };
}

export function insertAccount(
  db: Database,
  tenantId: string,
  account: AccountRecord,
): void {
  db.run(
    `INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
       last_failure_at_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    tenantId,
    account.accountId,
    account.name,
    JSON.stringify(account.password),
    account.failures,
    account.lockedUntilMs,
    account.lastFailureAtMs,
    account.createdAtMs,
    account.updatedAtMs,
  );
}

export function writeAccount(
  db: Database,
  tenantId: string,
  accountId: string,
  account: AccountRecord,
): void {
  db.run(
    `UPDATE accounts SET name = ?, password_json = ?, failures = ?, locked_until_ms = ?,
       last_failure_at_ms = ?, created_at_ms = ?, updated_at_ms = ?
     WHERE tenant_id = ? AND account_id = ?`,
    account.name,
    JSON.stringify(account.password),
    account.failures,
    account.lockedUntilMs,
    account.lastFailureAtMs,
    account.createdAtMs,
    account.updatedAtMs,
    tenantId,
    accountId,
  );
}

// ------------------------------------------------------------ 签名钥

export function toSigningKey(row: Row): SigningKeyRecord {
  return {
    kid: text(row.kid),
    privateKeyPkcs8: text(row.private_key_pkcs8),
    publicJwk: json(row.public_jwk_json),
    activeFromMs: int(row.active_from_ms),
    retiredAtMs: nullableInt(row.retired_at_ms),
  };
}

export function insertSigningKey(
  db: Database,
  tenantId: string,
  key: SigningKeyRecord,
): void {
  db.run(
    `INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json,
       active_from_ms, retired_at_ms) VALUES (?, ?, ?, 'none', ?, ?, ?)`,
    tenantId,
    key.kid,
    key.privateKeyPkcs8,
    JSON.stringify(key.publicJwk),
    key.activeFromMs,
    key.retiredAtMs,
  );
}

// ------------------------------------------------------------ 源

export function toSource(row: Row): SourceRecord {
  return {
    sourceId: text(row.source_id),
    name: text(row.name),
    kind: text(row.kind) as SourceRecord["kind"],
    publicJwk: json(row.public_jwk_json),
    coreVersion: text(row.core_version),
    capabilities: json(row.capabilities_json),
    registeredAtMs: int(row.registered_at_ms),
    lastSeenAtMs: nullableInt(row.last_seen_at_ms),
    revokedAtMs: nullableInt(row.revoked_at_ms),
    trustedOrigins: json(row.trusted_origins_json),
  };
}

export function insertSource(
  db: Database,
  tenantId: string,
  source: SourceRecord,
): void {
  db.run(
    `INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version,
       capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    tenantId,
    source.sourceId,
    source.name,
    source.kind,
    JSON.stringify(source.publicJwk),
    source.coreVersion,
    JSON.stringify(source.capabilities),
    source.registeredAtMs,
    source.lastSeenAtMs,
    source.revokedAtMs,
    JSON.stringify(source.trustedOrigins),
  );
}

export function writeSource(
  db: Database,
  tenantId: string,
  sourceId: string,
  source: SourceRecord,
): void {
  db.run(
    `UPDATE sources SET name = ?, kind = ?, public_jwk_json = ?, core_version = ?,
       capabilities_json = ?, registered_at_ms = ?, last_seen_at_ms = ?, revoked_at_ms = ?,
       trusted_origins_json = ?
     WHERE tenant_id = ? AND source_id = ?`,
    source.name,
    source.kind,
    JSON.stringify(source.publicJwk),
    source.coreVersion,
    JSON.stringify(source.capabilities),
    source.registeredAtMs,
    source.lastSeenAtMs,
    source.revokedAtMs,
    JSON.stringify(source.trustedOrigins),
    tenantId,
    sourceId,
  );
}

// ------------------------------------------------------------ 注册令牌

export function toRegistration(row: Row): RegistrationRow {
  return {
    tokenHash: text(row.token_hash),
    expiresAtMs: int(row.expires_at_ms),
    accountId: text(row.account_id),
  };
}

export function insertRegistration(
  db: Database,
  tenantId: string,
  token: RegistrationRow,
): void {
  db.run(
    "INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms) VALUES (?, ?, ?, ?)",
    tenantId,
    token.tokenHash,
    token.accountId,
    token.expiresAtMs,
  );
}

// ------------------------------------------------------------ 设备

export function toDevice(row: Row): DeviceRecord {
  return {
    deviceId: text(row.device_id),
    subject: text(row.subject),
    platform: text(row.platform) as DeviceRecord["platform"],
    name: text(row.name),
    refreshHash: text(row.refresh_hash),
    previousRefreshHash:
      row.previous_refresh_hash === null ||
      row.previous_refresh_hash === undefined
        ? null
        : text(row.previous_refresh_hash),
    rotation: int(row.rotation),
    createdAtMs: int(row.created_at_ms),
    expiresAtMs: int(row.expires_at_ms),
    revokedAtMs: nullableInt(row.revoked_at_ms),
    lastSeenAtMs: nullableInt(row.last_seen_at_ms),
  };
}

export function insertDevice(
  db: Database,
  tenantId: string,
  device: DeviceRecord,
): void {
  db.run(
    `INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash,
       previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    tenantId,
    device.deviceId,
    device.subject,
    device.platform,
    device.name,
    device.refreshHash,
    device.previousRefreshHash,
    device.rotation,
    device.createdAtMs,
    device.expiresAtMs,
    device.revokedAtMs,
    device.lastSeenAtMs,
  );
}

export function writeDevice(
  db: Database,
  tenantId: string,
  deviceId: string,
  device: DeviceRecord,
): void {
  db.run(
    `UPDATE devices SET subject = ?, platform = ?, name = ?, refresh_hash = ?,
       previous_refresh_hash = ?, rotation = ?, created_at_ms = ?, expires_at_ms = ?,
       revoked_at_ms = ?, last_seen_at_ms = ?
     WHERE tenant_id = ? AND device_id = ?`,
    device.subject,
    device.platform,
    device.name,
    device.refreshHash,
    device.previousRefreshHash,
    device.rotation,
    device.createdAtMs,
    device.expiresAtMs,
    device.revokedAtMs,
    device.lastSeenAtMs,
    tenantId,
    deviceId,
  );
}

// ------------------------------------------------------------ 链接

export function toLink(row: Row): LinkRecord {
  const link: LinkRecord = {
    linkId: text(row.link_id),
    sourceId: text(row.source_id),
    invitationId: text(row.invitation_id),
    label: text(row.label),
    role: text(row.role),
    secretHash: text(row.secret_hash),
    maxUses: nullableInt(row.max_uses),
    uses: int(row.uses),
    createdAtMs: int(row.created_at_ms),
    expiresAtMs: int(row.expires_at_ms),
    revokedAtMs: nullableInt(row.revoked_at_ms),
  };
  if (row.scope_json !== null && row.scope_json !== undefined)
    link.scope = json(row.scope_json);
  return link;
}

const scopeJson = (link: LinkRecord): string | null =>
  link.scope === undefined ? null : JSON.stringify(link.scope);

export function insertLink(
  db: Database,
  tenantId: string,
  link: LinkRecord,
): void {
  db.run(
    `INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash,
       max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    tenantId,
    link.linkId,
    link.sourceId,
    link.invitationId,
    link.label,
    link.role,
    link.secretHash,
    link.maxUses,
    link.uses,
    link.createdAtMs,
    link.expiresAtMs,
    link.revokedAtMs,
    scopeJson(link),
  );
}

export function writeLink(
  db: Database,
  tenantId: string,
  linkId: string,
  link: LinkRecord,
): void {
  db.run(
    `UPDATE links SET source_id = ?, invitation_id = ?, label = ?, role = ?, secret_hash = ?,
       max_uses = ?, uses = ?, created_at_ms = ?, expires_at_ms = ?, revoked_at_ms = ?, scope_json = ?
     WHERE tenant_id = ? AND link_id = ?`,
    link.sourceId,
    link.invitationId,
    link.label,
    link.role,
    link.secretHash,
    link.maxUses,
    link.uses,
    link.createdAtMs,
    link.expiresAtMs,
    link.revokedAtMs,
    scopeJson(link),
    tenantId,
    linkId,
  );
}

// ------------------------------------------------------------ 修订

/** 租户的修订加一（每个改了东西的写事务一次）。 */
export function bumpRevision(db: Database, tenantId: string): void {
  db.run(
    "UPDATE tenants SET revision = revision + 1 WHERE tenant_id = ?",
    tenantId,
  );
}
