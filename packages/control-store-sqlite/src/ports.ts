/**
 * 控制核全部存储端口的 SQLite 实现（多租户，按 `tenant_id` 隔离；personal 只用 `DEFAULT_TENANT`）。
 *
 * 语义与内存参照（`@armadra/control-core/testing/memory-ports`）一致，同一套合同测试守住：
 * - `update<T>` 是一个 `BEGIN IMMEDIATE` 事务：读行 → 在副本上跑 `fn` → 变了才写（并给租户
 *   修订加一）；`fn` 抛错就回滚，什么也不写；
 * - 读方法答新对象；列表按创建顺序（rowid）。
 *
 * 方法虽然是 async，读写都在调用的那一刻同步完成。
 */
import type {
  ControlPorts,
  DeviceRecord,
  TenantSettings,
} from "@armadra/control-core/ports";
import { sameHex } from "@armadra/control-core/secrets";
import { isStaleDevice } from "@armadra/control-core/sessions";
import { guestPrefix } from "@armadra/control-core/subjects";

import type { Database, Row } from "./open.ts";
import {
  bumpRevision,
  insertDevice,
  insertLink,
  insertRegistration,
  insertSource,
  toAccount,
  toDevice,
  toLink,
  toRegistration,
  toSource,
  writeAccount,
  writeDevice,
  writeLink,
  writeSource,
} from "./rows.ts";

const same = (left: unknown, right: unknown) =>
  JSON.stringify(left) === JSON.stringify(right);

/**
 * 一行的原子读-改-写：`fn` 拿到副本（或 null），改了就按原主键写回。
 * 记录与副本序列化相同就不写。
 */
function updateRow<R, T>(
  db: Database,
  tenantId: string,
  read: () => Row | undefined,
  map: (row: Row) => R,
  write: (draft: R) => void,
  fn: (draft: R | null) => T,
): T {
  return db.transaction(() => {
    const row = read();
    if (!row) return fn(null);
    const before = map(row);
    const draft = structuredClone(before);
    const result = fn(draft);
    if (!same(before, draft)) {
      write(draft);
      bumpRevision(db, tenantId);
    }
    return result;
  });
}

/** 租户设置（`tenants.get`）；没有这个租户就抛错（与内存参照一致）。 */
export function readTenantSettings(
  db: Database,
  tenantId: string,
): TenantSettings {
  const row = db.get(
    "SELECT issuer, allow_guests, trusted_origins_json FROM tenants WHERE tenant_id = ?",
    tenantId,
  );
  if (!row) throw new Error(`没有这个租户：${tenantId}`);
  return {
    issuer: String(row.issuer),
    allowGuests: Number(row.allow_guests) === 1,
    trustedOrigins: JSON.parse(String(row.trusted_origins_json)) as string[],
  };
}

export function createSqlitePorts(db: Database): ControlPorts {
  const account = (tenantId: string, accountId: string) =>
    db.get(
      "SELECT * FROM accounts WHERE tenant_id = ? AND account_id = ?",
      tenantId,
      accountId,
    );
  const device = (tenantId: string, deviceId: string) =>
    db.get(
      "SELECT * FROM devices WHERE tenant_id = ? AND device_id = ?",
      tenantId,
      deviceId,
    );
  const source = (tenantId: string, sourceId: string) =>
    db.get(
      "SELECT * FROM sources WHERE tenant_id = ? AND source_id = ?",
      tenantId,
      sourceId,
    );
  const link = (tenantId: string, linkId: string) =>
    db.get(
      "SELECT * FROM links WHERE tenant_id = ? AND link_id = ?",
      tenantId,
      linkId,
    );

  return {
    accounts: {
      async findByName(tenantId, name) {
        const row = db.get(
          "SELECT * FROM accounts WHERE tenant_id = ? AND name = ?",
          tenantId,
          name,
        );
        return row ? toAccount(row) : null;
      },
      async get(tenantId, accountId) {
        const row = account(tenantId, accountId);
        return row ? toAccount(row) : null;
      },
      async update(tenantId, accountId, fn) {
        return updateRow(
          db,
          tenantId,
          () => account(tenantId, accountId),
          toAccount,
          (draft) => writeAccount(db, tenantId, accountId, draft),
          fn,
        );
      },
      async resetPassword(tenantId, accountId, password, nowMs) {
        return db.transaction(() => {
          const row = account(tenantId, accountId);
          if (!row) return false;
          writeAccount(db, tenantId, accountId, {
            ...toAccount(row),
            password: structuredClone(password),
            updatedAtMs: nowMs,
            failures: 0,
            lockedUntilMs: 0,
          });
          db.run(
            "UPDATE devices SET revoked_at_ms = ? WHERE tenant_id = ? AND subject = ? AND revoked_at_ms IS NULL",
            nowMs,
            tenantId,
            accountId,
          );
          bumpRevision(db, tenantId);
          return true;
        });
      },
    },

    devices: {
      async get(tenantId, deviceId) {
        const row = device(tenantId, deviceId);
        return row ? toDevice(row) : null;
      },
      async listBySubject(tenantId, subject) {
        return db
          .all(
            "SELECT * FROM devices WHERE tenant_id = ? AND subject = ? ORDER BY rowid",
            tenantId,
            subject,
          )
          .map(toDevice);
      },
      async insert(tenantId, record: DeviceRecord, nowMs) {
        db.transaction(() => {
          // 顺手清掉早已失效的设备行，表不无限长（判定与内存参照同一个函数）。
          for (const row of db.all(
            "SELECT * FROM devices WHERE tenant_id = ?",
            tenantId,
          )) {
            const existing = toDevice(row);
            if (isStaleDevice(existing, nowMs))
              db.run(
                "DELETE FROM devices WHERE tenant_id = ? AND device_id = ?",
                tenantId,
                existing.deviceId,
              );
          }
          insertDevice(db, tenantId, record);
          bumpRevision(db, tenantId);
        });
      },
      async update(tenantId, deviceId, fn) {
        return updateRow(
          db,
          tenantId,
          () => device(tenantId, deviceId),
          toDevice,
          (draft) => writeDevice(db, tenantId, deviceId, draft),
          fn,
        );
      },
      async touch(tenantId, seen) {
        if (seen.size === 0) return;
        db.transaction(() => {
          let changed = 0;
          for (const [deviceId, atMs] of seen)
            changed += db.run(
              `UPDATE devices SET last_seen_at_ms = ?
               WHERE tenant_id = ? AND device_id = ? AND COALESCE(last_seen_at_ms, 0) < ?`,
              atMs,
              tenantId,
              deviceId,
              atMs,
            );
          if (changed > 0) bumpRevision(db, tenantId);
        });
      },
    },

    sources: {
      async get(tenantId, sourceId) {
        const row = source(tenantId, sourceId);
        return row ? toSource(row) : null;
      },
      async list(tenantId) {
        return db
          .all(
            "SELECT * FROM sources WHERE tenant_id = ? ORDER BY rowid",
            tenantId,
          )
          .map(toSource);
      },
      async put(tenantId, sourceId, fn) {
        return db.transaction(() => {
          const row = source(tenantId, sourceId);
          const record = fn(row ? toSource(row) : null);
          // 替换 = 删掉旧行再追加（与内存参照一样排到最后）。
          db.run(
            "DELETE FROM sources WHERE tenant_id = ? AND source_id = ?",
            tenantId,
            sourceId,
          );
          insertSource(db, tenantId, record);
          bumpRevision(db, tenantId);
          return structuredClone(record);
        });
      },
      async update(tenantId, sourceId, fn) {
        return updateRow(
          db,
          tenantId,
          () => source(tenantId, sourceId),
          toSource,
          (draft) => writeSource(db, tenantId, sourceId, draft),
          fn,
        );
      },
    },

    registrations: {
      async add(tenantId, token, options) {
        return db.transaction(() => {
          const live = db.get(
            "SELECT count(*) AS n FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms > ?",
            tenantId,
            options.nowMs,
          );
          if (Number(live?.n ?? 0) >= options.max) return false;
          db.run(
            "DELETE FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms <= ?",
            tenantId,
            options.nowMs,
          );
          insertRegistration(db, tenantId, {
            tokenHash: token.tokenHash,
            expiresAtMs: token.expiresAtMs,
            accountId: token.accountId,
          });
          bumpRevision(db, tenantId);
          return true;
        });
      },
      async consume(tenantId, tokenHash, nowMs) {
        return db.transaction(() => {
          // 逐条定时安全地比较（与内存参照一样不按哈希直接查）。
          const hit = db
            .all(
              "SELECT * FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms > ? ORDER BY rowid",
              tenantId,
              nowMs,
            )
            .map(toRegistration)
            .find((row) => sameHex(row.tokenHash, tokenHash));
          if (!hit) return null;
          db.run(
            "DELETE FROM registration_tokens WHERE tenant_id = ? AND token_hash = ?",
            tenantId,
            hit.tokenHash,
          );
          bumpRevision(db, tenantId);
          return { accountId: hit.accountId };
        });
      },
    },

    links: {
      async get(tenantId, linkId) {
        const row = link(tenantId, linkId);
        return row ? toLink(row) : null;
      },
      async list(tenantId, filter = {}) {
        const rows =
          filter.sourceId === undefined
            ? db.all(
                "SELECT * FROM links WHERE tenant_id = ? ORDER BY rowid",
                tenantId,
              )
            : db.all(
                "SELECT * FROM links WHERE tenant_id = ? AND source_id = ? ORDER BY rowid",
                tenantId,
                filter.sourceId,
              );
        return rows.map(toLink);
      },
      async insert(tenantId, record) {
        db.transaction(() => {
          insertLink(db, tenantId, record);
          bumpRevision(db, tenantId);
        });
      },
      async update(tenantId, linkId, fn) {
        return updateRow(
          db,
          tenantId,
          () => link(tenantId, linkId),
          toLink,
          (draft) => writeLink(db, tenantId, linkId, draft),
          fn,
        );
      },
      async revoke(tenantId, linkId, nowMs) {
        return db.transaction(() => {
          const row = db.get(
            "SELECT source_id FROM links WHERE tenant_id = ? AND link_id = ? AND revoked_at_ms IS NULL",
            tenantId,
            linkId,
          );
          if (!row) return null;
          db.run(
            "UPDATE links SET revoked_at_ms = ? WHERE tenant_id = ? AND link_id = ?",
            nowMs,
            tenantId,
            linkId,
          );
          const prefix = guestPrefix(linkId);
          db.run(
            `UPDATE devices SET revoked_at_ms = ?
             WHERE tenant_id = ? AND revoked_at_ms IS NULL AND substr(subject, 1, ?) = ?`,
            nowMs,
            tenantId,
            prefix.length,
            prefix,
          );
          bumpRevision(db, tenantId);
          return String(row.source_id);
        });
      },
    },

    tenants: {
      async get(tenantId) {
        return readTenantSettings(db, tenantId);
      },
    },
  };
}
