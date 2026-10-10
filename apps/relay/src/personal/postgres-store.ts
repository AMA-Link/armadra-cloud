/**
 * personal 的 PostgreSQL 存储（`--store postgres`，多节点；Node 中继存储设计 §5.1）：
 * `@armadra/control-store-postgres` 的端口，一个租户（`DEFAULT_TENANT`）。**只由 `store.ts` 按需
 * 动态 import**——缺省装配（file / sqlite）的静态 import 图里没有 PostgreSQL 驱动（`boundaries.test.ts` 守）。
 *
 * - 数据与状态文件同一份（`snapshot.ts` 两个方向逐字段对应，迁移往返等价）；
 * - 多个节点 / 进程同时打开同一个库：写都在事务里（先锁租户行）；对外地址、设置与签名钥是同步读的，
 *   开库时读一遍缓存，`watch` 每秒看租户修订号，别人写过就重读并回调（自己写的不算）；
 * - 数据目录里留 `postgres.json` 标记（不含连接串），之后不写 `--store` 也认得；
 * - 连不上、库不认识、账本对不上 → `StateError("invalid")` 拒绝启动，不自动清库或重建。
 */
import { chmodSync, existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { PostgresStoreError } from "@armadra/control-store-postgres/errors";
import {
  PostgresKeyCache,
  rotatePostgresKey,
} from "@armadra/control-store-postgres/keys";
import {
  openPostgres,
  type PostgresDatabase,
} from "@armadra/control-store-postgres/open";
import { createPostgresPorts } from "@armadra/control-store-postgres/ports";
import {
  exportTenant,
  importTenant,
  listTenants,
  readRevision,
  readTenant,
  revokedIds,
  updateSources,
  updateTenant,
  type TenantRow,
} from "@armadra/control-store-postgres/tenants";

import {
  snapshotToState,
  stateToSnapshot,
  tenantSettings,
} from "./snapshot.ts";
import { StateError, stateSchema, type State } from "./state.ts";
import {
  applySeen,
  POSTGRES_MARKER,
  type DatabaseOptions,
  type PersonalStore,
} from "./store-types.ts";

const POOL_MAX = 5;

function asStateError(error: unknown): unknown {
  if (!(error instanceof PostgresStoreError)) return error;
  if (error.code === "not_found")
    return new StateError(
      "not_initialized",
      "还没有初始化：先运行 armadra-relay personal init --store postgres",
    );
  if (error.code === "exists")
    return new StateError("exists", "已经初始化过：PostgreSQL 里已有个人中转");
  return new StateError("invalid", error.message);
}

async function connect(database: DatabaseOptions): Promise<PostgresDatabase> {
  try {
    return await openPostgres({
      url: database.url,
      ...(database.schema ? { schema: database.schema } : {}),
      max: POOL_MAX,
      applicationName: "armadra-relay-personal",
    });
  } catch (error) {
    throw asStateError(error);
  }
}

/** 数据目录里的标记：之后不写 `--store` 也按 PostgreSQL 开。 */
export function writePostgresMarker(dataDir: string): void {
  const path = join(dataDir, POSTGRES_MARKER);
  if (existsSync(path)) return;
  mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  writeFileSync(path, `${JSON.stringify({ store: "postgres" })}\n`, {
    mode: 0o600,
    flag: "wx",
  });
  chmodSync(path, 0o600);
}

/** 库里只能有个人中转的那一个租户（与 SQLite 一样）；没有它答 false。 */
async function checkTenants(db: PostgresDatabase): Promise<boolean> {
  const tenants = await listTenants(db.sql);
  if (tenants.length === 0) return false;
  if (tenants.length !== 1 || tenants[0] !== DEFAULT_TENANT)
    throw new StateError(
      "invalid",
      "数据库不是个人中转的库（租户不对），拒绝启动",
    );
  return true;
}

async function wrap(
  dataDir: string,
  db: PostgresDatabase,
): Promise<PersonalStore> {
  const tenantId = DEFAULT_TENANT;
  const { sql } = db;
  let known = (await readRevision(sql, tenantId)) ?? 0;
  /** 自己的写：修订号正好接上就记下（中间夹了别人的写就留给 watch 察觉）。 */
  const ownWrite = (revision: number) => {
    if (revision === known + 1) known = revision;
  };
  const keys = await PostgresKeyCache.load(sql, tenantId);
  let tenant: TenantRow;
  const reload = async () => {
    const row = await readTenant(sql, tenantId);
    if (!row) throw new StateError("invalid", "数据库里没有个人中转的租户");
    await keys.refresh();
    tenant = row;
  };
  await reload();
  const ports = createPostgresPorts(sql, {
    onWrite: (_tenant, revision) => ownWrite(revision),
  });
  return {
    kind: "postgres",
    dataDir,
    path: `postgres:${db.schema ?? "public"}`,
    ports,
    keys,
    issuer: () => keys.issuer(),
    settings: () => tenantSettings(tenant),
    async setIssuer(issuer) {
      await updateTenant(
        sql,
        tenantId,
        (draft) => {
          draft.issuer = issuer;
        },
        ownWrite,
      );
      await reload();
    },
    async rotateKey(nowMs) {
      const kid = await rotatePostgresKey(sql, tenantId, nowMs, ownWrite);
      await reload();
      return kid;
    },
    async recordSourcesSeen(batch) {
      if (batch.size === 0) return;
      await updateSources(
        sql,
        tenantId,
        (source) => applySeen(source, batch),
        ownWrite,
      );
    },
    snapshot: async () => snapshotToState(await exportTenant(sql, tenantId)),
    async revoked() {
      const ids = await revokedIds(sql, tenantId);
      return { sources: new Set(ids.sources), links: new Set(ids.links) };
    },
    watch(onChange, onError, intervalMs = 1000) {
      let busy = false;
      const timer = setInterval(() => {
        if (busy) return;
        busy = true;
        void (async () => {
          const revision = await readRevision(sql, tenantId);
          if (revision === null || revision === known) return;
          known = revision;
          await reload();
          await onChange();
        })()
          .catch(onError)
          .finally(() => {
            busy = false;
          });
      }, intervalMs);
      timer.unref();
      return () => clearInterval(timer);
    },
    close: () => db.close(),
  };
}

/** 库里有没有个人中转（新节点自动初始化前看一眼）；连不上照样拒绝。 */
export async function postgresStoreExists(
  database: DatabaseOptions,
): Promise<boolean> {
  const db = await connect(database);
  try {
    return await checkTenants(db);
  } finally {
    await db.close();
  }
}

/** 打开库并核对它是一个个人中转（一个租户、一个账号、字段合 schema）；顺手留下标记。 */
export async function openPostgresStore(
  dataDir: string,
  database: DatabaseOptions,
): Promise<PersonalStore> {
  const db = await connect(database);
  try {
    if (!(await checkTenants(db)))
      throw new StateError(
        "not_initialized",
        "还没有初始化：先运行 armadra-relay personal init --store postgres",
      );
    // 字段合 schema 才算个人中转的库（不合就拒绝启动）。
    snapshotToState(await exportTenant(db.sql, DEFAULT_TENANT));
    const store = await wrap(dataDir, db);
    writePostgresMarker(dataDir);
    return store;
  } catch (error) {
    await db.close();
    throw asStateError(error);
  }
}

/** 在一个事务里写入整份状态（`init`、自动初始化与 `migrate --to postgres`）；已有个人中转就拒绝。 */
export async function createPostgresStore(
  dataDir: string,
  state: State,
  database: DatabaseOptions,
): Promise<PersonalStore> {
  const checked = stateSchema.parse(state);
  const db = await connect(database);
  try {
    await checkTenants(db);
    await importTenant(db.sql, DEFAULT_TENANT, stateToSnapshot(checked));
    const store = await wrap(dataDir, db);
    writePostgresMarker(dataDir);
    return store;
  } catch (error) {
    await db.close();
    throw asStateError(error);
  }
}
