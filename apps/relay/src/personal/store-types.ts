/**
 * personal 存储的接口与三种实现共用的小件（`store.ts` 选实现；file 实现在 `store.ts`，
 * `sqlite-store.ts` 与按需动态 import 的 `postgres-store.ts` 各自实现 {@link PersonalStore}）。
 */
import { join } from "node:path";

import type {
  ControlPorts,
  KeyStore,
  SourceRecord,
} from "@armadra/control-core/ports";

import type { State } from "./state.ts";

export const STORE_KINDS = ["file", "sqlite", "postgres"] as const;
export type StoreKind = (typeof STORE_KINDS)[number];

export const DATABASE_FILE = "relay.db";
/**
 * `--store postgres` 时数据目录里的标记文件（只记「用 PostgreSQL」，不记连接串）：之后不写
 * `--store` 也按 PostgreSQL 开，忘了给连接串就拒绝启动，而不是悄悄新建一个本地库。
 */
export const POSTGRES_MARKER = "postgres.json";

/** PostgreSQL 的连接（`--database-url` / `RELAY_DATABASE_URL`，口令不进日志与输出）。 */
export interface DatabaseOptions {
  url: string;
  /** `--database-schema` / `RELAY_DATABASE_SCHEMA`；缺省连接的缺省 schema。 */
  schema?: string;
}

/** 存储的显示名（`status` / `doctor` / 日志）。 */
export const STORE_LABELS: Record<StoreKind, string> = {
  file: "状态文件",
  sqlite: "SQLite",
  postgres: "PostgreSQL",
};

export type StoreSettings = State["settings"];

/** 源上下线的累计（只在内存，按批落盘）。 */
export type SourcesSeen = ReadonlyMap<
  string,
  { atMs: number; coreVersion?: string }
>;

export interface PersonalStore {
  readonly kind: StoreKind;
  readonly dataDir: string;
  /** 存储的位置（`state.json`、`relay.db`，PostgreSQL 是 `postgres:<schema>`，不含连接串）。 */
  readonly path: string;
  readonly ports: ControlPorts;
  readonly keys: KeyStore;
  /**
   * 对外地址与设置（每个请求都可能读，同步）：file / sqlite 读一行，postgres 读缓存——开库时读一遍，
   * `watch` 察觉修订号变了与自己写过之后重读。
   */
  issuer(): string;
  settings(): StoreSettings;
  setIssuer(issuer: string): Promise<void>;
  /** 启用新签名钥，旧钥一小时后退役；答新 kid。 */
  rotateKey(nowMs: number): Promise<string>;
  /** 源的最近在线与版本（只往后推）。 */
  recordSourcesSeen(batch: SourcesSeen): Promise<void>;
  /** 整份状态（schema v1 形状）：`status`、`doctor` 与迁移用。 */
  snapshot(): Promise<State>;
  /** 已撤销的源与链接：察觉到外部改动后比对，断开新撤销的。 */
  revoked(): Promise<{ sources: Set<string>; links: Set<string> }>;
  /**
   * 别的进程（或节点）改了存储就回调（file 看文件修订，sqlite 看 `data_version`，postgres 看租户
   * 修订号）。
   */
  watch(
    onChange: () => void | Promise<void>,
    onError: (error: unknown) => void,
    intervalMs?: number,
  ): () => void;
  close(): Promise<void>;
}

export function databasePath(dataDir: string): string {
  return join(dataDir, DATABASE_FILE);
}

/** 一条源记录按累计的最近在线改（两种实现共用）。 */
export function applySeen(source: SourceRecord, batch: SourcesSeen): void {
  const seen = batch.get(source.sourceId);
  if (!seen) return;
  source.lastSeenAtMs = Math.max(source.lastSeenAtMs ?? 0, seen.atMs);
  if (seen.coreVersion) source.coreVersion = seen.coreVersion;
}
