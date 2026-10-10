/**
 * personal 的存储（`--store file|sqlite|postgres`，Node 中继存储设计 §5）：三种实现给 `serve` 与各
 * 子命令同一组接口——控制核端口、签名钥、对外地址与设置、轮换、源的最近在线、快照、外部改动通知。
 *
 * - `file`：`state.json`（`state.ts`），已部署的个人中转照旧；
 * - `sqlite`（**新装缺省**）：`relay.db`（`sqlite-store.ts`）；
 * - `postgres`（多节点，显式选）：`postgres-store.ts`，**按需动态 import**，缺省装配不加载驱动。
 *
 * 选哪个（{@link resolveStoreKind}）：数据目录里有 `relay.db` 用 sqlite；只有 `state.json` 用 file
 * （老部署零改动，`serve` 提示可以迁移）；有 `postgres.json` 标记用 postgres；都没有（新装）用 sqlite；
 * 有两种以上 → 拒绝启动。`--store` 与目录里已有的不符也拒绝——数据只经 `personal migrate --to` 换存储。
 */
import { existsSync } from "node:fs";
import { join } from "node:path";

import { rotateSigningKey } from "@armadra/control-core/signer";

import { UsageError } from "./args.ts";
import { createStatePorts, stateKeys } from "./ports.ts";
import { createSqliteStore, openSqliteStore } from "./sqlite-store.ts";
import { STATE_FILE, StateError, StateStore, type State } from "./state.ts";
import {
  applySeen,
  DATABASE_FILE,
  databasePath,
  POSTGRES_MARKER,
  STORE_LABELS,
  type DatabaseOptions,
  type PersonalStore,
  type StoreKind,
} from "./store-types.ts";

/** 数据目录里已有哪些存储。 */
function present(dataDir: string): StoreKind[] {
  const kinds: StoreKind[] = [];
  if (existsSync(databasePath(dataDir))) kinds.push("sqlite");
  if (existsSync(join(dataDir, STATE_FILE))) kinds.push("file");
  if (existsSync(join(dataDir, POSTGRES_MARKER))) kinds.push("postgres");
  return kinds;
}

const FILES: Record<StoreKind, string> = {
  file: STATE_FILE,
  sqlite: DATABASE_FILE,
  postgres: POSTGRES_MARKER,
};

/** 按数据目录里已有的文件与 `--store` 定存储；`legacy` = 老部署（只有状态文件、没写 `--store`）。 */
export function resolveStoreKind(
  dataDir: string,
  requested?: StoreKind,
): { kind: StoreKind; legacy: boolean } {
  const kinds = present(dataDir);
  if (kinds.length > 1)
    throw new StateError(
      "invalid",
      `数据目录里同时有 ${kinds.map((kind) => FILES[kind]).join(" 与 ")}，拒绝启动：只留下要用的那个`,
    );
  const [existing] = kinds;
  if (requested && existing && requested !== existing) {
    if (requested === "file" && existing === "sqlite")
      throw new StateError(
        "invalid",
        `数据目录里是 SQLite 库：去掉 --store file，或先运行 armadra-relay personal migrate --to file`,
      );
    if (requested === "sqlite" && existing === "file")
      throw new StateError(
        "invalid",
        `数据目录里是状态文件：先运行 armadra-relay personal migrate --to sqlite`,
      );
    throw new StateError(
      "invalid",
      `数据目录里用的是 ${STORE_LABELS[existing]}：去掉 --store ${requested}，或先运行 armadra-relay personal migrate --to ${requested}`,
    );
  }
  if (requested) return { kind: requested, legacy: false };
  if (existing) return { kind: existing, legacy: existing === "file" };
  return { kind: "sqlite", legacy: false };
}

/** 数据目录里已经有存储了吗（任一种；postgres 只看标记，库里有没有由调用方再问）。 */
export function storeExists(dataDir: string): boolean {
  return present(dataDir).length > 0;
}

function requireDatabase(database?: DatabaseOptions): DatabaseOptions {
  if (!database)
    throw new UsageError(
      "数据目录用的是 PostgreSQL：给 --database-url（或 RELAY_DATABASE_URL / RELAY_DATABASE_URL_FILE）",
    );
  return database;
}

/** 按需加载 PostgreSQL 存储（缺省装配不碰驱动）。 */
export const loadPostgresStore = () => import("./postgres-store.ts");

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
    async setIssuer(issuer) {
      store.update((draft) => {
        draft.issuer = issuer;
      });
    },
    rotateKey: async (nowMs) =>
      store.update((draft) => rotateSigningKey(draft, nowMs)),
    async recordSourcesSeen(batch) {
      store.update((draft) => {
        for (const source of draft.sources) applySeen(source, batch);
      });
    },
    snapshot: async () => structuredClone(store.current),
    revoked: async () => revoked(store.current),
    watch: (onChange, onError, intervalMs) =>
      store.watch(
        () => void Promise.resolve(onChange()).catch(onError),
        onError,
        intervalMs,
      ),
    async close() {},
  };
}

// ------------------------------------------------------------ 打开与新建

/** 打开已有的存储；没有 → `not_initialized`，坏的 / 不认识的 → `invalid`。 */
export async function openPersonalStore(
  dataDir: string,
  requested?: StoreKind,
  database?: DatabaseOptions,
): Promise<PersonalStore> {
  const { kind } = resolveStoreKind(dataDir, requested);
  if (kind === "file") return fileStore(StateStore.open(dataDir));
  if (kind === "sqlite") return openSqliteStore(dataDir);
  const { openPostgresStore } = await loadPostgresStore();
  return openPostgresStore(dataDir, requireDatabase(database));
}

/** 首次写入（`init` 与 `serve` 的自动初始化）；已有任一种存储 → `exists`，不覆盖。 */
export async function createPersonalStore(
  dataDir: string,
  kind: StoreKind,
  state: State,
  database?: DatabaseOptions,
): Promise<PersonalStore> {
  if (storeExists(dataDir))
    throw new StateError("exists", "已经初始化过：数据目录里已有存储");
  if (kind === "file") return fileStore(StateStore.create(dataDir, state));
  if (kind === "sqlite") return createSqliteStore(dataDir, state);
  const { createPostgresStore } = await loadPostgresStore();
  return createPostgresStore(dataDir, state, requireDatabase(database));
}
