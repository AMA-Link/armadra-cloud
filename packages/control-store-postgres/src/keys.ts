/**
 * 签名钥（{@link KeyStore}，同步读）：端口要同步，库是异步的——{@link PostgresKeyCache} 开库时读一遍
 * 租户的 issuer 与签名钥，之后由调用方在察觉修订号变了（别的进程 / 节点 `rotate-key`）时 `refresh()`。
 * 轮换在一个写事务里。私钥以 PKCS#8 base64 存（不包裹，与 Node 中继的 SQLite 一致），不进日志与响应。
 */
import type { KeyStore, SigningKeyRecord } from "@armadra/control-core/ports";
import { rotateSigningKey } from "@armadra/control-core/signer";

import {
  bumpRevision,
  lockTenant,
  transaction,
  type Queryable,
  type Sql,
} from "./db.ts";
import { PostgresStoreError } from "./errors.ts";
import { insertSigningKey, toSigningKey } from "./rows.ts";

export async function readSigningKeys(
  db: Queryable,
  tenantId: string,
): Promise<SigningKeyRecord[]> {
  const rows =
    await db`SELECT * FROM signing_keys WHERE tenant_id = ${tenantId} ORDER BY seq`;
  return rows.map(toSigningKey);
}

async function readIssuer(db: Queryable, tenantId: string): Promise<string> {
  const [row] =
    await db`SELECT issuer FROM tenants WHERE tenant_id = ${tenantId}`;
  if (!row)
    throw new PostgresStoreError("not_found", `没有这个租户：${tenantId}`);
  return String(row.issuer);
}

/** 同步的 {@link KeyStore}：读缓存；`refresh()` 从库重读。 */
export class PostgresKeyCache implements KeyStore {
  private cachedIssuer = "";
  private keys: SigningKeyRecord[] = [];

  private readonly sql: Sql;
  private readonly tenantId: string;

  private constructor(sql: Sql, tenantId: string) {
    this.sql = sql;
    this.tenantId = tenantId;
  }

  static async load(sql: Sql, tenantId: string): Promise<PostgresKeyCache> {
    const cache = new PostgresKeyCache(sql, tenantId);
    await cache.refresh();
    return cache;
  }

  async refresh(): Promise<void> {
    const [issuer, keys] = await Promise.all([
      readIssuer(this.sql, this.tenantId),
      readSigningKeys(this.sql, this.tenantId),
    ]);
    this.cachedIssuer = issuer;
    this.keys = keys;
  }

  issuer(): string {
    return this.cachedIssuer;
  }

  signingKeys(): readonly SigningKeyRecord[] {
    return this.keys;
  }
}

/** 加一把钥（建租户、导入时用）。 */
export async function addSigningKey(
  sql: Sql,
  tenantId: string,
  key: SigningKeyRecord,
): Promise<void> {
  await transaction(sql, async (tx) => {
    await lockTenant(tx, tenantId);
    await insertSigningKey(tx, tenantId, key);
    await bumpRevision(tx, tenantId);
  });
}

/**
 * 轮换（`rotate-key`）：与状态文件、SQLite 同一个 `rotateSigningKey`——追加新钥，其余未退役的定在
 * 一小时后退役，早已退役的删掉。答新 kid。
 */
export async function rotatePostgresKey(
  sql: Sql,
  tenantId: string,
  nowMs: number,
  onWrite?: (revision: number) => void,
): Promise<string> {
  let revision: number | null = null;
  const kid = await transaction(sql, async (tx) => {
    await lockTenant(tx, tenantId);
    const before = await readSigningKeys(tx, tenantId);
    const draft = { signingKeys: structuredClone(before) };
    const kid = rotateSigningKey(draft, nowMs);
    const kept = new Map(draft.signingKeys.map((key) => [key.kid, key]));
    for (const key of before) {
      const next = kept.get(key.kid);
      if (!next)
        await tx`DELETE FROM signing_keys WHERE tenant_id = ${tenantId} AND kid = ${key.kid}`;
      else if (next.retiredAtMs !== key.retiredAtMs)
        await tx`
          UPDATE signing_keys SET retired_at_ms = ${next.retiredAtMs}
          WHERE tenant_id = ${tenantId} AND kid = ${key.kid}`;
    }
    const known = new Set(before.map((key) => key.kid));
    for (const key of draft.signingKeys)
      if (!known.has(key.kid)) await insertSigningKey(tx, tenantId, key);
    revision = await bumpRevision(tx, tenantId);
    return kid;
  });
  if (revision !== null) onWrite?.(revision);
  return kid;
}
