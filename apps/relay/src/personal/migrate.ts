/**
 * `personal migrate --to sqlite|file|postgres` 与 `personal backup`（Node 中继存储设计 §5.3）。
 *
 * 迁移只换存储、不改数据：整份写入目标（一个事务），写完重新读出来与原状态逐字段比对（修订号也一样），
 * 对不上就删掉新写的本地文件、原存储不动。成功后原文件改名为 `<文件>.migrated-<时间>` 留作回退，
 * 不删；从 PostgreSQL 迁走时库里的数据原样保留（绝不自动清库），只把数据目录里的 `postgres.json`
 * 标记改名。拒绝条件：目标已存在（PostgreSQL 里已有个人中转也算）、源不存在或认不出、`serve` 正在跑
 * （多节点要先停掉所有节点）。
 *
 * 备份：SQLite 用 `VACUUM INTO`（在线、一致）；状态文件直接拷一份；PostgreSQL 用库自己的备份。
 * 备份文件 0600。
 */
import { isDeepStrictEqual } from "node:util";
import {
  chmodSync,
  constants,
  copyFileSync,
  existsSync,
  renameSync,
  rmSync,
  statSync,
} from "node:fs";
import { join } from "node:path";

import { openDatabase } from "@armadra/control-store-sqlite/open";

import { UsageError } from "./args.ts";
import { withStore, type Print } from "./commands.ts";
import { runningServe } from "./runlock.ts";
import { createSqliteStore, openSqliteStore } from "./sqlite-store.ts";
import {
  acquireLock,
  STATE_FILE,
  StateError,
  StateStore,
  statePath,
  type State,
} from "./state.ts";
import { loadPostgresStore, resolveStoreKind } from "./store.ts";
import {
  databasePath,
  POSTGRES_MARKER,
  STORE_LABELS,
  type DatabaseOptions,
  type PersonalStore,
  type StoreKind,
} from "./store-types.ts";

const stamp = (nowMs: number) =>
  new Date(nowMs)
    .toISOString()
    .replace(/\.\d{3}Z$/, "Z")
    .replace(/:/g, "-");

const removeDatabase = (path: string) => {
  for (const suffix of ["", "-wal", "-shm"])
    rmSync(`${path}${suffix}`, { force: true });
};

const markerPath = (dataDir: string) => join(dataDir, POSTGRES_MARKER);

/** 「迁移到 SQLite」「迁移到状态文件」：拉丁字母前后留空格。 */
const labelOf = (kind: StoreKind) =>
  kind === "file" ? STORE_LABELS[kind] : ` ${STORE_LABELS[kind]} `.trimEnd();

function refuseWhileServing(dataDir: string): void {
  const pid = runningServe(dataDir);
  if (pid !== null)
    throw new UsageError(
      `serve 正在运行（进程 ${pid}）：先停掉它再迁移；确认已停止就删掉 ${join(dataDir, "serve.pid")}`,
    );
}

function requireDatabase(database?: DatabaseOptions): DatabaseOptions {
  if (!database)
    throw new UsageError(
      "要给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）",
    );
  return database;
}

/** 写完读回来比对；对不上就是迁移失败。 */
async function verify(store: PersonalStore, state: State, what: string) {
  try {
    if (!isDeepStrictEqual(await store.snapshot(), state))
      throw new StateError("invalid", `迁移后读出的内容与${what}不一致`);
  } finally {
    await store.close();
  }
}

/** 迁移的源：读出整份状态，成功后把它退下来（改名留作回退）。 */
interface Source {
  kind: StoreKind;
  state: State;
  /** 答留下的回退文件。 */
  retire(nowMs: number): string;
  /** 状态文件的目录锁（迁移期间 passwd / rotate-key 写不进来）。 */
  release(): void;
}

async function openSource(
  dataDir: string,
  database: DatabaseOptions | undefined,
): Promise<Source> {
  const kinds = (["file", "sqlite", "postgres"] as const).filter((kind) =>
    existsSync(
      kind === "file"
        ? statePath(dataDir)
        : kind === "sqlite"
          ? databasePath(dataDir)
          : markerPath(dataDir),
    ),
  );
  if (kinds.length === 0)
    throw new StateError(
      "not_initialized",
      `没有可迁的存储：${statePath(dataDir)}`,
    );
  const { kind } = resolveStoreKind(dataDir);
  if (kind === "file") {
    const release = acquireLock(dataDir);
    try {
      // 锁内读：passwd / rotate-key 不会在中间写进来。
      const state = StateStore.open(dataDir).current;
      return {
        kind,
        state,
        release,
        retire(nowMs) {
          const kept = `${statePath(dataDir)}.migrated-${stamp(nowMs)}`;
          renameSync(statePath(dataDir), kept);
          return kept;
        },
      };
    } catch (error) {
      release();
      throw error;
    }
  }
  if (kind === "sqlite") {
    const store = openSqliteStore(dataDir);
    let state: State;
    try {
      state = await store.snapshot();
    } finally {
      await store.close();
    }
    const database = databasePath(dataDir);
    return {
      kind,
      state,
      release() {},
      retire(nowMs) {
        const kept = `${database}.migrated-${stamp(nowMs)}`;
        renameSync(database, kept);
        for (const suffix of ["-wal", "-shm"])
          if (existsSync(`${database}${suffix}`))
            renameSync(`${database}${suffix}`, `${kept}${suffix}`);
        return kept;
      },
    };
  }
  const { openPostgresStore } = await loadPostgresStore();
  const store = await openPostgresStore(dataDir, requireDatabase(database));
  let state: State;
  try {
    state = await store.snapshot();
  } finally {
    await store.close();
  }
  return {
    kind,
    state,
    release() {},
    retire(nowMs) {
      // 库里的数据原样保留：只把标记改名，数据目录之后按新存储开。
      const kept = `${markerPath(dataDir)}.migrated-${stamp(nowMs)}`;
      renameSync(markerPath(dataDir), kept);
      return kept;
    },
  };
}

/** 写成 `relay.db`（先写临时库，比对后整体改名）。 */
async function writeSqlite(dataDir: string, state: State): Promise<string> {
  const database = databasePath(dataDir);
  const temporary = `${database}.migrating-${process.pid}`;
  removeDatabase(temporary);
  try {
    await verify(createSqliteStore(dataDir, state, temporary), state, "原存储");
    // 关库时 WAL 已并入主文件：只剩主文件才能整体改名。
    if (existsSync(`${temporary}-wal`) && statSync(`${temporary}-wal`).size > 0)
      throw new StateError("invalid", "迁移后的数据库没有落盘完整");
    rmSync(`${temporary}-wal`, { force: true });
    rmSync(`${temporary}-shm`, { force: true });
    renameSync(temporary, database);
    // 再按正式路径打开一次核对（此时源还在，只按 sqlite 开）。
    await verify(openSqliteStore(dataDir), state, "原存储");
  } catch (error) {
    if (existsSync(database) && !existsSync(temporary))
      removeDatabase(database);
    removeDatabase(temporary);
    throw error;
  }
  return database;
}

/** 写成 `state.json`。 */
function writeFile(dataDir: string, state: State): string {
  const file = statePath(dataDir);
  StateStore.create(dataDir, state);
  if (!isDeepStrictEqual(StateStore.open(dataDir).current, state)) {
    rmSync(file, { force: true });
    throw new StateError("invalid", "迁移后读出的内容与原存储不一致");
  }
  return file;
}

/** 写进 PostgreSQL（一个事务）；库里已有个人中转就拒绝。 */
async function writePostgres(
  dataDir: string,
  state: State,
  database: DatabaseOptions,
): Promise<string> {
  const { createPostgresStore } = await loadPostgresStore();
  const store = await createPostgresStore(dataDir, state, database);
  const path = store.path;
  try {
    await verify(store, state, "原存储");
  } catch (error) {
    // 刚写进库的租户不自动删（不碰库里的数据），只撤掉标记，数据目录照旧用原存储。
    rmSync(markerPath(dataDir), { force: true });
    throw error;
  }
  return path;
}

/** 换存储：答新存储的位置与留下的回退文件。 */
export async function migrateStore(
  dataDir: string,
  to: StoreKind,
  options: { nowMs?: number; database?: DatabaseOptions } = {},
): Promise<{ from: StoreKind; target: string; kept: string }> {
  const nowMs = options.nowMs ?? Date.now();
  refuseWhileServing(dataDir);
  if (to === "sqlite" && existsSync(databasePath(dataDir)))
    throw new StateError("exists", `目标已存在：${databasePath(dataDir)}`);
  if (to === "file" && existsSync(statePath(dataDir)))
    throw new StateError("exists", `目标已存在：${statePath(dataDir)}`);
  if (to === "postgres" && existsSync(markerPath(dataDir)))
    throw new StateError("exists", `数据目录已经在用 PostgreSQL`);
  if (to === "postgres") requireDatabase(options.database);
  const source = await openSource(dataDir, options.database);
  try {
    if (source.kind === to)
      throw new StateError("exists", `已经在用${labelOf(to)}`);
    const target =
      to === "sqlite"
        ? await writeSqlite(dataDir, source.state)
        : to === "file"
          ? writeFile(dataDir, source.state)
          : await writePostgres(
              dataDir,
              source.state,
              options.database as DatabaseOptions,
            );
    return { from: source.kind, target, kept: source.retire(nowMs) };
  } finally {
    source.release();
  }
}

/** 状态文件 → `relay.db`。答新库与留下的原文件。 */
export async function migrateToSqlite(
  dataDir: string,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<{ database: string; kept: string }> {
  const { target, kept } = await migrateStore(dataDir, "sqlite", {
    nowMs,
    ...(database ? { database } : {}),
  });
  return { database: target, kept };
}

/** `relay.db`（或 PostgreSQL）→ 状态文件（schema v1）。答新文件与留下的原存储。 */
export async function migrateToFile(
  dataDir: string,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<{ file: string; kept: string }> {
  const { target, kept } = await migrateStore(dataDir, "file", {
    nowMs,
    ...(database ? { database } : {}),
  });
  return { file: target, kept };
}

export async function runMigrate(
  dataDir: string,
  to: StoreKind,
  print: Print,
  nowMs = Date.now(),
  database?: DatabaseOptions,
): Promise<void> {
  const { from, target, kept } = await migrateStore(dataDir, to, {
    nowMs,
    ...(database ? { database } : {}),
  });
  print(`已迁移到${labelOf(to)}：${target}`);
  print(
    from === "postgres"
      ? `PostgreSQL 里的数据原样保留；原标记留作回退：${kept}`
      : `原${from === "file" ? "状态文件" : "数据库"}留作回退：${kept}`,
  );
}

/** 备份到 `out`（不覆盖已有文件）。 */
export async function runBackup(
  dataDir: string,
  out: string,
  print: Print,
  database?: DatabaseOptions,
): Promise<void> {
  if (existsSync(out)) throw new UsageError(`备份目标已存在：${out}`);
  await withStore(
    dataDir,
    (store) => {
      if (store.kind === "postgres")
        throw new UsageError(
          "PostgreSQL 存储用库自己的备份（pg_dump 或托管服务的快照）",
        );
      if (store.kind === "sqlite") {
        const db = openDatabase(store.path);
        try {
          db.backup(out);
        } finally {
          db.close();
        }
        // 备份要能原样打开。
        openDatabase(out).close();
      } else {
        copyFileSync(join(dataDir, STATE_FILE), out, constants.COPYFILE_EXCL);
        chmodSync(out, 0o600);
      }
    },
    database,
  );
  print(`已备份：${out}`);
}
