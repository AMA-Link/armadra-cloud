/**
 * 迁移账本的判定（纯逻辑，Node SQLite 与 DO SQLite 共用）：库里记的 `relay_migrations` 与程序带的
 * `migrations/NNNN_*.sql` 对账，答还要跑哪些；对不上一律拒绝，不自动清库、不重建。
 *
 * - 程序带的迁移编号从 1 起连续；
 * - 库里记的必须是程序带的前缀，每条的名字与 SHA-384 都一致；
 * - 库里比程序多（库是新版本写的）→ 拒绝，旧程序不碰新库。
 */

/** 账本的建表语句（迁移器在空库上先建它）。 */
export const LEDGER_DDL =
  "CREATE TABLE relay_migrations (id INTEGER PRIMARY KEY, name TEXT NOT NULL, sha384 TEXT NOT NULL, applied_at_ms INTEGER NOT NULL) STRICT";

export interface MigrationFile {
  id: number;
  name: string;
  sha384: string;
  sql: string;
}

export interface AppliedMigration {
  id: number;
  name: string;
  sha384: string;
}

export type LedgerProblem = "newer" | "mismatch" | "gap";

export class LedgerError extends Error {
  readonly problem: LedgerProblem;
  constructor(problem: LedgerProblem, message: string) {
    super(message);
    this.name = "LedgerError";
    this.problem = problem;
  }
}

const FILE_NAME = /^(\d{4})_([a-z0-9_]+)\.sql$/;

/** `0001_init.sql` → `{ id: 1, name: "0001_init" }`；不是迁移文件答 null。 */
export function parseMigrationName(
  file: string,
): { id: number; name: string } | null {
  const match = FILE_NAME.exec(file);
  if (!match) return null;
  return { id: Number(match[1]), name: file.slice(0, -".sql".length) };
}

/** 程序带的迁移：编号从 1 起连续，否则是打包错误。 */
export function checkAvailable(files: readonly MigrationFile[]): void {
  files.forEach((file, index) => {
    if (file.id !== index + 1)
      throw new LedgerError(
        "gap",
        `迁移编号不连续：第 ${index + 1} 个是 ${file.name}`,
      );
  });
}

/** 对账：答还没跑的迁移（按编号）。 */
export function pendingMigrations(
  applied: readonly AppliedMigration[],
  available: readonly MigrationFile[],
): MigrationFile[] {
  checkAvailable(available);
  if (applied.length > available.length)
    throw new LedgerError(
      "newer",
      `库是更新的版本写的：已记 ${applied.length} 个迁移，程序只认识 ${available.length} 个`,
    );
  applied.forEach((row, index) => {
    const file = available[index];
    if (row.id !== index + 1)
      throw new LedgerError(
        "gap",
        `迁移账本不连续：第 ${index + 1} 行是 ${row.id}`,
      );
    if (!file || row.name !== file.name || row.sha384 !== file.sha384)
      throw new LedgerError("mismatch", `迁移账本对不上：${row.name}`);
  });
  return available.slice(applied.length);
}
