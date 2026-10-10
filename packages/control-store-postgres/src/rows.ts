/**
 * 行 ↔ 记录：列与 `ports.ts` 的记录一一对应（camelCase ↔ snake_case），`*_json` 列存 JSON 文本。
 * bigint 列驱动答字符串，这里一律转回 number（毫秒时间戳在 2^53 以内）。读出来的都是新对象；
 * 写都按整行写。
 */
import type {
  AccountRecord,
  DeviceRecord,
  LinkRecord,
  RegistrationTokenRecord,
  SigningKeyRecord,
  SourceRecord,
} from "@armadra/control-core/ports";

import type { Queryable, Row } from "./db.ts";
import { PostgresStoreError } from "./errors.ts";

const text = (value: unknown): string => String(value ?? "");
const int = (value: unknown): number => Number(value ?? 0);
const nullableInt = (value: unknown): number | null =>
  value === null || value === undefined ? null : Number(value);
const nullableText = (value: unknown): string | null =>
  value === null || value === undefined ? null : String(value);
const json = <T>(value: unknown): T => JSON.parse(text(value)) as T;

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

export async function insertAccount(
  db: Queryable,
  tenantId: string,
  account: AccountRecord,
): Promise<void> {
  await db`
    INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
      last_failure_at_ms, created_at_ms, updated_at_ms)
    VALUES (${tenantId}, ${account.accountId}, ${account.name}, ${JSON.stringify(account.password)},
      ${account.failures}, ${account.lockedUntilMs}, ${account.lastFailureAtMs},
      ${account.createdAtMs}, ${account.updatedAtMs})`;
}

export async function writeAccount(
  db: Queryable,
  tenantId: string,
  accountId: string,
  account: AccountRecord,
): Promise<void> {
  await db`
    UPDATE accounts SET name = ${account.name}, password_json = ${JSON.stringify(account.password)},
      failures = ${account.failures}, locked_until_ms = ${account.lockedUntilMs},
      last_failure_at_ms = ${account.lastFailureAtMs}, created_at_ms = ${account.createdAtMs},
      updated_at_ms = ${account.updatedAtMs}
    WHERE tenant_id = ${tenantId} AND account_id = ${accountId}`;
}

// ------------------------------------------------------------ 签名钥

/** 没包裹的私钥列值。 */
export const KEY_WRAP_NONE = "none";

/** 读签名钥：只认没包裹的（`none`）；别的包裹方式本实现不认识 → 拒绝，不把密文当私钥用。 */
export function toSigningKey(row: Row): SigningKeyRecord {
  const kind = text(row.private_key_wrap) || KEY_WRAP_NONE;
  if (kind !== KEY_WRAP_NONE)
    throw new PostgresStoreError(
      "unknown",
      `签名钥的包裹方式不认识，拒绝启动：${kind}`,
    );
  return {
    kid: text(row.kid),
    privateKeyPkcs8: text(row.private_key_pkcs8),
    publicJwk: json(row.public_jwk_json),
    activeFromMs: int(row.active_from_ms),
    retiredAtMs: nullableInt(row.retired_at_ms),
  };
}

export async function insertSigningKey(
  db: Queryable,
  tenantId: string,
  key: SigningKeyRecord,
): Promise<void> {
  await db`
    INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json,
      active_from_ms, retired_at_ms)
    VALUES (${tenantId}, ${key.kid}, ${key.privateKeyPkcs8}, ${KEY_WRAP_NONE},
      ${JSON.stringify(key.publicJwk)}, ${key.activeFromMs}, ${key.retiredAtMs})`;
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

export async function insertSource(
  db: Queryable,
  tenantId: string,
  source: SourceRecord,
): Promise<void> {
  await db`
    INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version,
      capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json)
    VALUES (${tenantId}, ${source.sourceId}, ${source.name}, ${source.kind},
      ${JSON.stringify(source.publicJwk)}, ${source.coreVersion},
      ${JSON.stringify(source.capabilities)}, ${source.registeredAtMs}, ${source.lastSeenAtMs},
      ${source.revokedAtMs}, ${JSON.stringify(source.trustedOrigins)})`;
}

export async function writeSource(
  db: Queryable,
  tenantId: string,
  sourceId: string,
  source: SourceRecord,
): Promise<void> {
  await db`
    UPDATE sources SET name = ${source.name}, kind = ${source.kind},
      public_jwk_json = ${JSON.stringify(source.publicJwk)}, core_version = ${source.coreVersion},
      capabilities_json = ${JSON.stringify(source.capabilities)},
      registered_at_ms = ${source.registeredAtMs}, last_seen_at_ms = ${source.lastSeenAtMs},
      revoked_at_ms = ${source.revokedAtMs},
      trusted_origins_json = ${JSON.stringify(source.trustedOrigins)}
    WHERE tenant_id = ${tenantId} AND source_id = ${sourceId}`;
}

// ------------------------------------------------------------ 注册令牌

export function toRegistration(row: Row): RegistrationRow {
  return {
    tokenHash: text(row.token_hash),
    expiresAtMs: int(row.expires_at_ms),
    accountId: text(row.account_id),
  };
}

export async function insertRegistration(
  db: Queryable,
  tenantId: string,
  token: RegistrationRow,
): Promise<void> {
  await db`
    INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms)
    VALUES (${tenantId}, ${token.tokenHash}, ${token.accountId}, ${token.expiresAtMs})`;
}

// ------------------------------------------------------------ 设备

export function toDevice(row: Row): DeviceRecord {
  return {
    deviceId: text(row.device_id),
    subject: text(row.subject),
    platform: text(row.platform) as DeviceRecord["platform"],
    name: text(row.name),
    refreshHash: text(row.refresh_hash),
    previousRefreshHash: nullableText(row.previous_refresh_hash),
    rotation: int(row.rotation),
    createdAtMs: int(row.created_at_ms),
    expiresAtMs: int(row.expires_at_ms),
    revokedAtMs: nullableInt(row.revoked_at_ms),
    lastSeenAtMs: nullableInt(row.last_seen_at_ms),
  };
}

export async function insertDevice(
  db: Queryable,
  tenantId: string,
  device: DeviceRecord,
): Promise<void> {
  await db`
    INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash,
      previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms)
    VALUES (${tenantId}, ${device.deviceId}, ${device.subject}, ${device.platform}, ${device.name},
      ${device.refreshHash}, ${device.previousRefreshHash}, ${device.rotation},
      ${device.createdAtMs}, ${device.expiresAtMs}, ${device.revokedAtMs}, ${device.lastSeenAtMs})`;
}

export async function writeDevice(
  db: Queryable,
  tenantId: string,
  deviceId: string,
  device: DeviceRecord,
): Promise<void> {
  await db`
    UPDATE devices SET subject = ${device.subject}, platform = ${device.platform},
      name = ${device.name}, refresh_hash = ${device.refreshHash},
      previous_refresh_hash = ${device.previousRefreshHash}, rotation = ${device.rotation},
      created_at_ms = ${device.createdAtMs}, expires_at_ms = ${device.expiresAtMs},
      revoked_at_ms = ${device.revokedAtMs}, last_seen_at_ms = ${device.lastSeenAtMs}
    WHERE tenant_id = ${tenantId} AND device_id = ${deviceId}`;
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

export async function insertLink(
  db: Queryable,
  tenantId: string,
  link: LinkRecord,
): Promise<void> {
  await db`
    INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash,
      max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json)
    VALUES (${tenantId}, ${link.linkId}, ${link.sourceId}, ${link.invitationId}, ${link.label},
      ${link.role}, ${link.secretHash}, ${link.maxUses}, ${link.uses}, ${link.createdAtMs},
      ${link.expiresAtMs}, ${link.revokedAtMs}, ${scopeJson(link)})`;
}

export async function writeLink(
  db: Queryable,
  tenantId: string,
  linkId: string,
  link: LinkRecord,
): Promise<void> {
  await db`
    UPDATE links SET source_id = ${link.sourceId}, invitation_id = ${link.invitationId},
      label = ${link.label}, role = ${link.role}, secret_hash = ${link.secretHash},
      max_uses = ${link.maxUses}, uses = ${link.uses}, created_at_ms = ${link.createdAtMs},
      expires_at_ms = ${link.expiresAtMs}, revoked_at_ms = ${link.revokedAtMs},
      scope_json = ${scopeJson(link)}
    WHERE tenant_id = ${tenantId} AND link_id = ${linkId}`;
}
