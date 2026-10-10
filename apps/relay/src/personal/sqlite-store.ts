/**
 * personal 的 SQLite 存储：`<data-dir>/relay.db`（`@armadra/control-store-sqlite`，WAL、0600）。
 *
 * - 只有一个租户（`DEFAULT_TENANT`）、一个账号（`acct:<账号名>`）；与状态文件同一份数据，
 *   `snapshot.ts` 的两个函数逐字段对应（迁移往返等价）；
 * - 同一个库可以被 `serve` 与 CLI 同时打开：写都在 `BEGIN IMMEDIATE` 事务里，读每次走库，
 *   没有状态文件那样的目录锁与「每秒看一眼」；`serve` 每秒看一次 `data_version`，别的进程提交过
 *   就比对撤销、断开；
 * - 打不开、不认识、校验不过、迁移账本对不上 → `StateError("invalid")` 拒绝启动，不自动重建。
 */
import { mkdirSync, rmSync } from "node:fs";

import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { SqliteStoreError } from "@armadra/control-store-sqlite/errors";
import {
  createSqliteKeys,
  rotateSqliteKey,
} from "@armadra/control-store-sqlite/keys";
import {
  openDatabase,
  type Database,
} from "@armadra/control-store-sqlite/open";
import { createSqlitePorts } from "@armadra/control-store-sqlite/ports";
import {
  bumpRevision,
  toSource,
  writeSource,
} from "@armadra/control-store-sqlite/rows";
import {
  exportTenant,
  importTenant,
  listTenants,
  readTenant,
  revokedIds,
  updateTenant,
} from "@armadra/control-store-sqlite/tenants";

import {
  snapshotToState,
  stateToSnapshot,
  tenantSettings,
} from "./snapshot.ts";
import { StateError, stateSchema, type State } from "./state.ts";
import {
  applySeen,
  databasePath,
  type PersonalStore,
  type StoreSettings,
} from "./store-types.ts";

function asStateError(error: unknown): unknown {
  if (!(error instanceof SqliteStoreError)) return error;
  if (error.code === "not_found")
    return new StateError(
      "not_initialized",
      "还没有初始化：先运行 armadra-relay personal init",
    );
  if (error.code === "exists")
    return new StateError("exists", "已经初始化过：数据库已存在");
  return new StateError("invalid", error.message);
}

function wrap(dataDir: string, db: Database): PersonalStore {
  const tenantId = DEFAULT_TENANT;
  const ports = createSqlitePorts(db);
  const snapshot = () => snapshotToState(exportTenant(db, tenantId));
  const settings = (): StoreSettings => {
    const tenant = readTenant(db, tenantId);
    if (!tenant) throw new StateError("invalid", "数据库里没有个人中转的租户");
    return tenantSettings(tenant);
  };
  return {
    kind: "sqlite",
    dataDir,
    path: db.path,
    ports,
    keys: createSqliteKeys(db, tenantId),
    issuer: () => createSqliteKeys(db, tenantId).issuer(),
    settings,
    async setIssuer(issuer) {
      updateTenant(db, tenantId, (draft) => {
        draft.issuer = issuer;
      });
    },
    rotateKey: async (nowMs) => rotateSqliteKey(db, tenantId, nowMs),
    async recordSourcesSeen(batch) {
      if (batch.size === 0) return;
      db.transaction(() => {
        let changed = false;
        for (const row of db.all(
          "SELECT * FROM sources WHERE tenant_id = ?",
          tenantId,
        )) {
          const before = toSource(row);
          const draft = structuredClone(before);
          applySeen(draft, batch);
          if (JSON.stringify(draft) === JSON.stringify(before)) continue;
          writeSource(db, tenantId, before.sourceId, draft);
          changed = true;
        }
        if (changed) bumpRevision(db, tenantId);
      });
    },
    snapshot: async () => snapshot(),
    async revoked() {
      const ids = revokedIds(db, tenantId);
      return { sources: new Set(ids.sources), links: new Set(ids.links) };
    },
    watch(onChange, onError, intervalMs = 1000) {
      let version = db.dataVersion();
      const timer = setInterval(() => {
        try {
          const next = db.dataVersion();
          if (next === version) return;
          version = next;
          void Promise.resolve(onChange()).catch(onError);
        } catch (error) {
          onError(error);
        }
      }, intervalMs);
      timer.unref();
      return () => clearInterval(timer);
    },
    close: async () => db.close(),
  };
}

/** 打开 `relay.db` 并核对它是一个个人中转的库（一个租户、一个账号、字段合 schema）。 */
export function openSqliteStore(dataDir: string): PersonalStore {
  let db: Database;
  try {
    db = openDatabase(databasePath(dataDir));
  } catch (error) {
    throw asStateError(error);
  }
  try {
    const tenants = listTenants(db);
    if (tenants.length !== 1 || tenants[0] !== DEFAULT_TENANT)
      throw new StateError(
        "invalid",
        "数据库不是个人中转的库（租户不对），拒绝启动",
      );
    // 字段合 schema 才算个人中转的库（不合就拒绝启动）。
    snapshotToState(exportTenant(db, DEFAULT_TENANT));
    return wrap(dataDir, db);
  } catch (error) {
    db.close();
    throw error;
  }
}

/** 新建 `relay.db` 并在一个事务里写入整份状态（`init`、自动初始化与 `migrate --to sqlite`）。 */
export function createSqliteStore(
  dataDir: string,
  state: State,
  path = databasePath(dataDir),
): PersonalStore {
  mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  const checked = stateSchema.parse(state);
  let db: Database;
  try {
    db = openDatabase(path, { create: true });
  } catch (error) {
    throw asStateError(error);
  }
  try {
    importTenant(db, DEFAULT_TENANT, stateToSnapshot(checked));
  } catch (error) {
    db.close();
    // 自己刚建的库没写成：删掉，不留半个库。
    for (const suffix of ["", "-wal", "-shm"])
      rmSync(`${path}${suffix}`, { force: true });
    throw error;
  }
  return wrap(dataDir, db);
}
