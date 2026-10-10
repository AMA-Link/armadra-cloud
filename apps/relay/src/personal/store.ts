/**
 * personal 的存储（`--store file|sqlite`，Node 中继存储设计 §5）：两种实现给 `serve` 与各子命令
 * 同一组接口——控制核端口、签名钥、对外地址与设置、轮换、源的最近在线、快照、外部改动通知。
 *
 * - `file`：`state.json`（`state.ts`），已部署的个人中转照旧；
 * - `sqlite`（**新装缺省**）：`relay.db`（`sqlite-store.ts`）。
 *
 * 选哪个（{@link resolveStoreKind}）：数据目录里有 `relay.db` 用 sqlite；只有 `state.json` 用 file
 * （老部署零改动，`serve` 提示可以迁移）；都没有（新装）用 sqlite；两个都在 → 拒绝启动。
 * `--store` 与目录里已有的不符也拒绝——数据只经 `personal migrate --to` 换存储。
 */
import { existsSync } from "node:fs";
import { join } from "node:path";

import { rotateSigningKey } from "@armadra/control-core/signer";

import { createStatePorts, stateKeys } from "./ports.ts";
import { createSqliteStore, openSqliteStore } from "./sqlite-store.ts";
import { STATE_FILE, StateError, StateStore, type State } from "./state.ts";
import {
  applySeen,
  DATABASE_FILE,
  databasePath,
  type PersonalStore,
  type StoreKind,
} from "./store-types.ts";

/** 按数据目录里已有的文件与 `--store` 定存储；`legacy` = 老部署（只有状态文件、没写 `--store`）。 */
export function resolveStoreKind(
  dataDir: string,
  requested?: StoreKind,
): { kind: StoreKind; legacy: boolean } {
  const hasDatabase = existsSync(databasePath(dataDir));
  const hasState = existsSync(join(dataDir, STATE_FILE));
  if (hasDatabase && hasState)
    throw new StateError(
      "invalid",
      `数据目录里同时有 ${STATE_FILE} 与 ${DATABASE_FILE}，拒绝启动：只留下要用的那个`,
    );
  if (requested === "file" && hasDatabase)
    throw new StateError(
      "invalid",
      `数据目录里是 SQLite 库：去掉 --store file，或先运行 armadra-relay personal migrate --to file`,
    );
  if (requested === "sqlite" && hasState)
    throw new StateError(
      "invalid",
      `数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite`,
    );
  if (requested) return { kind: requested, legacy: false };
  if (hasDatabase) return { kind: "sqlite", legacy: false };
  if (hasState) return { kind: "file", legacy: true };
  return { kind: "sqlite", legacy: false };
}

/** 数据目录里已经有存储了吗（任一种）。 */
export function storeExists(dataDir: string): boolean {
  return (
    existsSync(databasePath(dataDir)) || existsSync(join(dataDir, STATE_FILE))
  );
}

// ------------------------------------------------------------ file

function fileStore(store: StateStore): PersonalStore {
  const revoked = (state: State) => ({
    sources: new Set(
      state.sources
        .filter((row) => row.revokedAtMs !== null)
        .map((row) => row.sourceId),
    ),
    links: new Set(
      state.links
        .filter((row) => row.revokedAtMs !== null)
        .map((row) => row.linkId),
    ),
  });
  return {
    kind: "file",
    dataDir: store.dataDir,
    path: join(store.dataDir, STATE_FILE),
    ports: createStatePorts(store),
    keys: stateKeys(store),
    issuer: () => store.current.issuer,
    settings: () => store.current.settings,
    setIssuer(issuer) {
      store.update((draft) => {
        draft.issuer = issuer;
      });
    },
    rotateKey: (nowMs) =>
      store.update((draft) => rotateSigningKey(draft, nowMs)),
    recordSourcesSeen(batch) {
      store.update((draft) => {
        for (const source of draft.sources) applySeen(source, batch);
      });
    },
    snapshot: () => structuredClone(store.current),
    revoked: () => revoked(store.current),
    watch: (onChange, onError, intervalMs) =>
      store.watch(onChange, onError, intervalMs),
    close() {},
  };
}

// ------------------------------------------------------------ 打开与新建

/** 打开已有的存储；没有 → `not_initialized`，坏的 / 不认识的 → `invalid`。 */
export function openPersonalStore(
  dataDir: string,
  requested?: StoreKind,
): PersonalStore {
  const { kind } = resolveStoreKind(dataDir, requested);
  if (kind === "file") return fileStore(StateStore.open(dataDir));
  return openSqliteStore(dataDir);
}

/** 首次写入（`init` 与 `serve` 的自动初始化）；已有任一种存储 → `exists`，不覆盖。 */
export function createPersonalStore(
  dataDir: string,
  kind: StoreKind,
  state: State,
): PersonalStore {
  if (storeExists(dataDir))
    throw new StateError("exists", "已经初始化过：数据目录里已有存储");
  if (kind === "file") return fileStore(StateStore.create(dataDir, state));
  return createSqliteStore(dataDir, state);
}
