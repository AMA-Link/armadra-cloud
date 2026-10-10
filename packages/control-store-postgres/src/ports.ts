/**
 * 控制核全部存储端口的 PostgreSQL 实现（多租户，按 `tenant_id` 隔离；个人中转只用 `DEFAULT_TENANT`）。
 *
 * 语义与内存参照（`@armadra/control-core/testing/memory-ports`）、SQLite 实现一致，同一套合同测试守住：
 * - 写都是一个事务：先锁租户行（同一租户的写串行）→ 读行 → 在副本上跑 `fn` → 变了才写（并给租户
 *   修订加一）；`fn` 抛错就回滚，什么也不写；
 * - 读方法答新对象；列表按创建顺序（`seq`）。
 *
 * `onWrite(tenantId, revision)`：本实例的写事务提交、修订号变了之后回调（个人中转用它区分
 * 「自己写的」与「别的进程写的」）。
 */
import type {
  ControlPorts,
  DeviceRecord,
  TenantSettings,
} from "@armadra/control-core/ports";
import { sameHex } from "@armadra/control-core/secrets";
import { isStaleDevice } from "@armadra/control-core/sessions";
import { guestPrefix } from "@armadra/control-core/subjects";

import {
  bumpRevision,
  lockTenant,
  transaction,
  type Queryable,
  type Row,
  type Sql,
  type Tx,
} from "./db.ts";
import {
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

export interface PostgresPortsOptions {
  onWrite?: (tenantId: string, revision: number) => void;
}

/** 租户设置（`tenants.get`）；没有这个租户就抛错（与内存参照一致）。 */
export async function readTenantSettings(
  db: Queryable,
  tenantId: string,
): Promise<TenantSettings> {
  const [row] = await db`
    SELECT issuer, allow_guests, trusted_origins_json FROM tenants WHERE tenant_id = ${tenantId}`;
  if (!row) throw new Error(`没有这个租户：${tenantId}`);
  return {
    issuer: String(row.issuer),
    allowGuests: row.allow_guests === true,
    trustedOrigins: JSON.parse(String(row.trusted_origins_json)) as string[],
  };
}

export function createPostgresPorts(
  sql: Sql,
  options: PostgresPortsOptions = {},
): ControlPorts {
  /**
   * 一个租户的写事务：锁租户行后跑 `fn`；`fn` 答 `changed` 为真时修订加一，提交后回调 `onWrite`。
   */
  const write = async <T>(
    tenantId: string,
    fn: (tx: Tx) => Promise<{ result: T; changed: boolean }>,
  ): Promise<T> => {
    let revision: number | null = null;
    const result = await transaction(sql, async (tx) => {
      await lockTenant(tx, tenantId);
      const outcome = await fn(tx);
      if (outcome.changed) revision = await bumpRevision(tx, tenantId);
      return outcome.result;
    });
    if (revision !== null) options.onWrite?.(tenantId, revision);
    return result;
  };

  /** 一行的原子读-改-写：`fn` 拿到副本（或 null），改了就按原主键写回。 */
  const updateRow = <R, T>(
    tenantId: string,
    read: (tx: Tx) => Promise<Row | undefined>,
    map: (row: Row) => R,
    store: (tx: Tx, draft: R) => Promise<void>,
    fn: (draft: R | null) => T,
  ): Promise<T> =>
    write(tenantId, async (tx) => {
      const row = await read(tx);
      if (!row) return { result: fn(null), changed: false };
      const before = map(row);
      const draft = structuredClone(before);
      const result = fn(draft);
      if (same(before, draft)) return { result, changed: false };
      await store(tx, draft);
      return { result, changed: true };
    });

  const first = async (rows: Promise<readonly Row[]>) => (await rows)[0];
  const account = (db: Queryable, tenantId: string, accountId: string) =>
    first(
      db`SELECT * FROM accounts WHERE tenant_id = ${tenantId} AND account_id = ${accountId}`,
    );
  const device = (db: Queryable, tenantId: string, deviceId: string) =>
    first(
      db`SELECT * FROM devices WHERE tenant_id = ${tenantId} AND device_id = ${deviceId}`,
    );
  const source = (db: Queryable, tenantId: string, sourceId: string) =>
    first(
      db`SELECT * FROM sources WHERE tenant_id = ${tenantId} AND source_id = ${sourceId}`,
    );
  const link = (db: Queryable, tenantId: string, linkId: string) =>
    first(
      db`SELECT * FROM links WHERE tenant_id = ${tenantId} AND link_id = ${linkId}`,
    );

  return {
    accounts: {
      async findByName(tenantId, name) {
        const row = await first(
          sql`SELECT * FROM accounts WHERE tenant_id = ${tenantId} AND name = ${name}`,
        );
        return row ? toAccount(row) : null;
      },
      async get(tenantId, accountId) {
        const row = await account(sql, tenantId, accountId);
        return row ? toAccount(row) : null;
      },
      update(tenantId, accountId, fn) {
        return updateRow(
          tenantId,
          (tx) => account(tx, tenantId, accountId),
          toAccount,
          (tx, draft) => writeAccount(tx, tenantId, accountId, draft),
          fn,
        );
      },
      resetPassword(tenantId, accountId, password, nowMs) {
        return write(tenantId, async (tx) => {
          const row = await account(tx, tenantId, accountId);
          if (!row) return { result: false, changed: false };
          await writeAccount(tx, tenantId, accountId, {
            ...toAccount(row),
            password: structuredClone(password),
            updatedAtMs: nowMs,
            failures: 0,
            lockedUntilMs: 0,
          });
          await tx`
            UPDATE devices SET revoked_at_ms = ${nowMs}
            WHERE tenant_id = ${tenantId} AND subject = ${accountId} AND revoked_at_ms IS NULL`;
          return { result: true, changed: true };
        });
      },
    },

    devices: {
      async get(tenantId, deviceId) {
        const row = await device(sql, tenantId, deviceId);
        return row ? toDevice(row) : null;
      },
      async listBySubject(tenantId, subject) {
        const rows = await sql`
          SELECT * FROM devices WHERE tenant_id = ${tenantId} AND subject = ${subject} ORDER BY seq`;
        return rows.map(toDevice);
      },
      insert(tenantId, record: DeviceRecord, nowMs) {
        return write(tenantId, async (tx) => {
          // 顺手清掉早已失效的设备行，表不无限长（判定与内存参照同一个函数；只看已撤销或已过期的）。
          const candidates = await tx`
            SELECT * FROM devices
            WHERE tenant_id = ${tenantId} AND (revoked_at_ms IS NOT NULL OR expires_at_ms <= ${nowMs})`;
          for (const row of candidates) {
            const existing = toDevice(row);
            if (isStaleDevice(existing, nowMs))
              await tx`
                DELETE FROM devices WHERE tenant_id = ${tenantId} AND device_id = ${existing.deviceId}`;
          }
          await insertDevice(tx, tenantId, record);
          return { result: undefined, changed: true };
        });
      },
      update(tenantId, deviceId, fn) {
        return updateRow(
          tenantId,
          (tx) => device(tx, tenantId, deviceId),
          toDevice,
          (tx, draft) => writeDevice(tx, tenantId, deviceId, draft),
          fn,
        );
      },
      async touch(tenantId, seen) {
        if (seen.size === 0) return;
        await write(tenantId, async (tx) => {
          let changed = 0;
          for (const [deviceId, atMs] of seen) {
            const result = await tx`
              UPDATE devices SET last_seen_at_ms = ${atMs}
              WHERE tenant_id = ${tenantId} AND device_id = ${deviceId}
                AND COALESCE(last_seen_at_ms, 0) < ${atMs}`;
            changed += result.count;
          }
          return { result: undefined, changed: changed > 0 };
        });
      },
    },

    sources: {
      async get(tenantId, sourceId) {
        const row = await source(sql, tenantId, sourceId);
        return row ? toSource(row) : null;
      },
      async list(tenantId) {
        const rows =
          await sql`SELECT * FROM sources WHERE tenant_id = ${tenantId} ORDER BY seq`;
        return rows.map(toSource);
      },
      put(tenantId, sourceId, fn) {
        return write(tenantId, async (tx) => {
          const row = await source(tx, tenantId, sourceId);
          const record = fn(row ? toSource(row) : null);
          // 替换 = 删掉旧行再追加（与内存参照一样排到最后）。
          await tx`DELETE FROM sources WHERE tenant_id = ${tenantId} AND source_id = ${sourceId}`;
          await insertSource(tx, tenantId, record);
          return { result: structuredClone(record), changed: true };
        });
      },
      update(tenantId, sourceId, fn) {
        return updateRow(
          tenantId,
          (tx) => source(tx, tenantId, sourceId),
          toSource,
          (tx, draft) => writeSource(tx, tenantId, sourceId, draft),
          fn,
        );
      },
    },

    registrations: {
      add(tenantId, token, options) {
        return write(tenantId, async (tx) => {
          const [live] = await tx`
            SELECT count(*)::int AS n FROM registration_tokens
            WHERE tenant_id = ${tenantId} AND expires_at_ms > ${options.nowMs}`;
          if (Number(live?.n ?? 0) >= options.max)
            return { result: false, changed: false };
          await tx`
            DELETE FROM registration_tokens
            WHERE tenant_id = ${tenantId} AND expires_at_ms <= ${options.nowMs}`;
          await insertRegistration(tx, tenantId, {
            tokenHash: token.tokenHash,
            expiresAtMs: token.expiresAtMs,
            accountId: token.accountId,
          });
          return { result: true, changed: true };
        });
      },
      consume(tenantId, tokenHash, nowMs) {
        return write(tenantId, async (tx) => {
          // 逐条定时安全地比较（与内存参照一样不按哈希直接查）。
          const rows = await tx`
            SELECT * FROM registration_tokens
            WHERE tenant_id = ${tenantId} AND expires_at_ms > ${nowMs} ORDER BY seq`;
          const hit = rows
            .map(toRegistration)
            .find((row) => sameHex(row.tokenHash, tokenHash));
          if (!hit) return { result: null, changed: false };
          await tx`
            DELETE FROM registration_tokens
            WHERE tenant_id = ${tenantId} AND token_hash = ${hit.tokenHash}`;
          return { result: { accountId: hit.accountId }, changed: true };
        });
      },
    },

    links: {
      async get(tenantId, linkId) {
        const row = await link(sql, tenantId, linkId);
        return row ? toLink(row) : null;
      },
      async list(tenantId, filter = {}) {
        const rows =
          filter.sourceId === undefined
            ? await sql`SELECT * FROM links WHERE tenant_id = ${tenantId} ORDER BY seq`
            : await sql`
                SELECT * FROM links WHERE tenant_id = ${tenantId} AND source_id = ${filter.sourceId}
                ORDER BY seq`;
        return rows.map(toLink);
      },
      async insert(tenantId, record) {
        await write(tenantId, async (tx) => {
          await insertLink(tx, tenantId, record);
          return { result: undefined, changed: true };
        });
      },
      update(tenantId, linkId, fn) {
        return updateRow(
          tenantId,
          (tx) => link(tx, tenantId, linkId),
          toLink,
          (tx, draft) => writeLink(tx, tenantId, linkId, draft),
          fn,
        );
      },
      revoke(tenantId, linkId, nowMs) {
        return write(tenantId, async (tx) => {
          const [row] = await tx`
            SELECT source_id FROM links
            WHERE tenant_id = ${tenantId} AND link_id = ${linkId} AND revoked_at_ms IS NULL`;
          if (!row) return { result: null, changed: false };
          await tx`
            UPDATE links SET revoked_at_ms = ${nowMs}
            WHERE tenant_id = ${tenantId} AND link_id = ${linkId}`;
          await tx`
            UPDATE devices SET revoked_at_ms = ${nowMs}
            WHERE tenant_id = ${tenantId} AND revoked_at_ms IS NULL
              AND starts_with(subject, ${guestPrefix(linkId)})`;
          return { result: String(row.source_id), changed: true };
        });
      },
    },

    tenants: {
      get: (tenantId) => readTenantSettings(sql, tenantId),
    },
  };
}
