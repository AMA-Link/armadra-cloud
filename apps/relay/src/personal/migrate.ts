/**
 * `personal migrate --to sqlite|file` 与 `personal backup`（Node 中继存储设计 §5.3）。
 *
 * 迁移只换存储、不改数据：两个方向都在一个事务里整份写入，写完重新读出来与原状态逐字段比对
 * （修订号也一样），对不上就删掉新写的、原文件不动。成功后原文件改名为
 * `<文件>.migrated-<时间>` 留作回退，不删。拒绝条件：目标已存在、源不存在或认不出、`serve` 正在跑。
 *
 * 备份：SQLite 用 `VACUUM INTO`（在线、一致）；状态文件直接拷一份。备份文件 0600。
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
  STATE_FILE,
  StateError,
  StateStore,
  statePath,
  withLock,
} from "./state.ts";
import { databasePath, type StoreKind } from "./store-types.ts";

const stamp = (nowMs: number) =>
  new Date(nowMs)
    .toISOString()
    .replace(/\.\d{3}Z$/, "Z")
    .replace(/:/g, "-");

const removeDatabase = (path: string) => {
  for (const suffix of ["", "-wal", "-shm"])
    rmSync(`${path}${suffix}`, { force: true });
};

function refuseWhileServing(dataDir: string): void {
  const pid = runningServe(dataDir);
  if (pid !== null)
    throw new UsageError(
      `serve 正在运行（进程 ${pid}）：先停掉它再迁移；确认已停止就删掉 ${join(dataDir, "serve.pid")}`,
    );
}

/** 状态文件 → `relay.db`。答新库与留下的原文件。 */
export function migrateToSqlite(
  dataDir: string,
  nowMs = Date.now(),
): { database: string; kept: string } {
  refuseWhileServing(dataDir);
  const database = databasePath(dataDir);
  const refuseExisting = () => {
    if (existsSync(database))
      throw new StateError("exists", `目标已存在：${database}`);
  };
  refuseExisting();
  if (!existsSync(statePath(dataDir)))
    throw new StateError(
      "not_initialized",
      `没有状态文件可迁：${statePath(dataDir)}`,
    );
  return withLock(dataDir, () => {
    refuseExisting();
    // 锁内读：passwd / rotate-key 不会在中间写进来。
    const state = StateStore.open(dataDir).current;
    const temporary = `${database}.migrating-${process.pid}`;
    removeDatabase(temporary);
    try {
      const store = createSqliteStore(dataDir, state, temporary);
      try {
        if (!isDeepStrictEqual(store.snapshot(), state))
          throw new StateError("invalid", "迁移后读出的内容与状态文件不一致");
      } finally {
        store.close();
      }
      // 关库时 WAL 已并入主文件：只剩主文件才能整体改名。
      if (
        existsSync(`${temporary}-wal`) &&
        statSync(`${temporary}-wal`).size > 0
      )
        throw new StateError("invalid", "迁移后的数据库没有落盘完整");
      rmSync(`${temporary}-wal`, { force: true });
      rmSync(`${temporary}-shm`, { force: true });
      renameSync(temporary, database);
      const reopened = openSqliteStore(dataDir);
      try {
        if (!isDeepStrictEqual(reopened.snapshot(), state))
          throw new StateError("invalid", "迁移后读出的内容与状态文件不一致");
      } finally {
        reopened.close();
      }
    } catch (error) {
      if (existsSync(database) && !existsSync(temporary))
        removeDatabase(database);
      removeDatabase(temporary);
      throw error;
    }
    const kept = `${statePath(dataDir)}.migrated-${stamp(nowMs)}`;
    renameSync(statePath(dataDir), kept);
    return { database, kept };
  });
}

/** `relay.db` → 状态文件（schema v1）。答新文件与留下的原库。 */
export function migrateToFile(
  dataDir: string,
  nowMs = Date.now(),
): { file: string; kept: string } {
  refuseWhileServing(dataDir);
  const file = statePath(dataDir);
  if (existsSync(file)) throw new StateError("exists", `目标已存在：${file}`);
  const database = databasePath(dataDir);
  const store = openSqliteStore(dataDir);
  let state;
  try {
    state = store.snapshot();
  } finally {
    store.close();
  }
  StateStore.create(dataDir, state);
  if (!isDeepStrictEqual(StateStore.open(dataDir).current, state)) {
    rmSync(file, { force: true });
    throw new StateError("invalid", "迁移后读出的内容与数据库不一致");
  }
  const kept = `${database}.migrated-${stamp(nowMs)}`;
  renameSync(database, kept);
  for (const suffix of ["-wal", "-shm"])
    if (existsSync(`${database}${suffix}`))
      renameSync(`${database}${suffix}`, `${kept}${suffix}`);
  return { file, kept };
}

export function runMigrate(
  dataDir: string,
  to: StoreKind,
  print: Print,
  nowMs = Date.now(),
): void {
  if (to === "sqlite") {
    const { database, kept } = migrateToSqlite(dataDir, nowMs);
    print(`已迁移到 SQLite：${database}`);
    print(`原状态文件留作回退：${kept}`);
    return;
  }
  const { file, kept } = migrateToFile(dataDir, nowMs);
  print(`已迁移到状态文件：${file}`);
  print(`原数据库留作回退：${kept}`);
}

/** 备份到 `out`（不覆盖已有文件）。 */
export async function runBackup(
  dataDir: string,
  out: string,
  print: Print,
): Promise<void> {
  if (existsSync(out)) throw new UsageError(`备份目标已存在：${out}`);
  await withStore(dataDir, (store) => {
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
  });
  print(`已备份：${out}`);
}
