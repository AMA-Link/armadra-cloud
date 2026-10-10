/**
 * personal 存储的接口与两种实现共用的小件（`store.ts` 选实现，`sqlite-store.ts` 与 file 实现
 * 各自实现 {@link PersonalStore}）。
 */
import { join } from "node:path";

import type {
  ControlPorts,
  KeyStore,
  SourceRecord,
} from "@armadra/control-core/ports";

import type { State } from "./state.ts";

export const STORE_KINDS = ["file", "sqlite"] as const;
export type StoreKind = (typeof STORE_KINDS)[number];

export const DATABASE_FILE = "relay.db";

export type StoreSettings = State["settings"];

/** 源上下线的累计（只在内存，按批落盘）。 */
export type SourcesSeen = ReadonlyMap<
  string,
  { atMs: number; coreVersion?: string }
>;

export interface PersonalStore {
  readonly kind: StoreKind;
  readonly dataDir: string;
  /** 存储文件（`state.json` 或 `relay.db`）。 */
  readonly path: string;
  readonly ports: ControlPorts;
  readonly keys: KeyStore;
  /** 对外地址（每个请求都可能读：两种实现都只读一行）。 */
  issuer(): string;
  settings(): StoreSettings;
  setIssuer(issuer: string): void;
  /** 启用新签名钥，旧钥一小时后退役；答新 kid。 */
  rotateKey(nowMs: number): string;
  /** 源的最近在线与版本（只往后推）。 */
  recordSourcesSeen(batch: SourcesSeen): void;
  /** 整份状态（schema v1 形状）：`status`、`doctor` 与迁移用。 */
  snapshot(): State;
  /** 已撤销的源与链接：察觉到外部改动后比对，断开新撤销的。 */
  revoked(): { sources: Set<string>; links: Set<string> };
  /** 别的进程改了存储就回调（file 看文件修订，sqlite 看 `data_version`）。 */
  watch(
    onChange: () => void,
    onError: (error: unknown) => void,
    intervalMs?: number,
  ): () => void;
  close(): void;
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
