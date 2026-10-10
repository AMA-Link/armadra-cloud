/**
 * 签名钥（{@link KeyStore}）：同步读 `signing_keys`（`DatabaseSync` 本就是同步的，表只有几行，
 * 每次都读库，别的进程 `rotate-key` 之后立即可见）；轮换在一个写事务里。
 *
 * 私钥以 PKCS#8 base64 存：Node 中继不包（`private_key_wrap = 'none'`），靠库文件 0600 保护；
 * Workers 中继给 {@link KeyWrap}（主密钥 AES-256-GCM）。不进日志与响应。
 */
import type { KeyStore, SigningKeyRecord } from "@armadra/control-core/ports";
import { rotateSigningKey } from "@armadra/control-core/signer";

import type { SqlDatabase as Database, KeyWrap } from "./db.ts";
import { bumpRevision, insertSigningKey, toSigningKey } from "./rows.ts";

export function readSigningKeys(
  db: Database,
  tenantId: string,
  wrap?: KeyWrap,
): SigningKeyRecord[] {
  return db
    .all(
      "SELECT * FROM signing_keys WHERE tenant_id = ? ORDER BY rowid",
      tenantId,
    )
    .map((row) => toSigningKey(row, wrap));
}

export function createSqliteKeys(
  db: Database,
  tenantId: string,
  wrap?: KeyWrap,
): KeyStore {
  return {
    issuer() {
      const row = db.get(
        "SELECT issuer FROM tenants WHERE tenant_id = ?",
        tenantId,
      );
      if (!row) throw new Error(`没有这个租户：${tenantId}`);
      return String(row.issuer);
    },
    signingKeys: () => readSigningKeys(db, tenantId, wrap),
  };
}

/** 加一把钥（建租户、导入时用）。 */
export function addSigningKey(
  db: Database,
  tenantId: string,
  key: SigningKeyRecord,
  wrap?: KeyWrap,
): void {
  db.transaction(() => {
    insertSigningKey(db, tenantId, key, wrap);
    bumpRevision(db, tenantId);
  });
}

/**
 * 轮换（`rotate-key`）：与状态文件同一个 `rotateSigningKey`——追加新钥，其余未退役的定在
 * 一小时后退役，早已退役的删掉。答新 kid。
 */
export function rotateSqliteKey(
  db: Database,
  tenantId: string,
  nowMs: number,
  wrap?: KeyWrap,
): string {
  return db.transaction(() => {
    const before = readSigningKeys(db, tenantId, wrap);
    const draft = { signingKeys: structuredClone(before) };
    const kid = rotateSigningKey(draft, nowMs);
    const kept = new Map(draft.signingKeys.map((key) => [key.kid, key]));
    for (const key of before) {
      const next = kept.get(key.kid);
      if (!next)
        db.run(
          "DELETE FROM signing_keys WHERE tenant_id = ? AND kid = ?",
          tenantId,
          key.kid,
        );
      else if (next.retiredAtMs !== key.retiredAtMs)
        db.run(
          "UPDATE signing_keys SET retired_at_ms = ? WHERE tenant_id = ? AND kid = ?",
          next.retiredAtMs,
          tenantId,
          key.kid,
        );
    }
    const known = new Set(before.map((key) => key.kid));
    for (const key of draft.signingKeys)
      if (!known.has(key.kid)) insertSigningKey(db, tenantId, key, wrap);
    bumpRevision(db, tenantId);
    return kid;
  });
}
