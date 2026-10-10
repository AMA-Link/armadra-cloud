/**
 * 打开控制面的 SQLite 库（`node:sqlite` 的 `DatabaseSync`，同步 API）。
 *
 * - **新建**（`create`）：先以 0600 独占建文件，再设 WAL、`application_id`、跑迁移；文件已在就拒绝。
 * - **打开已有的**：不是 SQLite、`application_id` 不是本程序的、`quick_check` 不过、迁移账本对不上
 *   ——一律拒绝启动（{@link SqliteStoreError}），绝不自动清库或重建。账本是前缀时补跑新迁移。
 * - 同一文件可以被多个进程同时打开（`serve` 与 `passwd` / `rotate-key` / `links revoke`）：写都在
 *   `BEGIN IMMEDIATE` 事务里，`busy_timeout` 5 秒内等对方提交。
 */
import { closeSync, existsSync, openSync, rmSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import type { DatabaseSync, StatementSync } from "node:sqlite";

import type { Row, SqlDatabase, SqlValue } from "./db.ts";
import { SqliteStoreError } from "./errors.ts";
import { loadMigrations, migrate } from "./migrate.ts";

export type { Row, SqlValue };

/** `PRAGMA application_id`：「ARMD」。别的程序的 SQLite 库一眼认出来，拒绝打开。 */
export const APPLICATION_ID = 0x41524d44;
const BUSY_TIMEOUT_MS = 5_000;

let sqlite: typeof import("node:sqlite") | undefined;

/**
 * 按需加载 `node:sqlite`。Node 22 每次加载都打一行「实验特性」警告；这一行对运维没有用，
 * 加载期间只吞掉它，其余警告照常。
 */
function loadSqlite(): typeof import("node:sqlite") {
  if (sqlite) return sqlite;
  const emit = process.emitWarning;
  process.emitWarning = ((warning: string | Error, ...rest: unknown[]) => {
    const text = typeof warning === "string" ? warning : warning.message;
    if (/SQLite is an experimental feature/.test(text)) return;
    return (emit as (...args: unknown[]) => void).call(
      process,
      warning,
      ...rest,
    );
  }) as typeof process.emitWarning;
  try {
    sqlite = createRequire(import.meta.url)(
      "node:sqlite",
    ) as typeof import("node:sqlite");
  } finally {
    process.emitWarning = emit;
  }
  return sqlite;
}

/** 一个打开的库：语句缓存 + 可嵌套的写事务。 */
export class Database implements SqlDatabase {
  readonly path: string;
  private readonly db: DatabaseSync;
  private readonly statements = new Map<string, StatementSync>();
  private depth = 0;
  private closed = false;

  constructor(path: string, db: DatabaseSync) {
    this.path = path;
    this.db = db;
  }

  exec(sql: string): void {
    this.db.exec(sql);
  }

  private statement(sql: string): StatementSync {
    let found = this.statements.get(sql);
    if (!found) {
      found = this.db.prepare(sql);
      this.statements.set(sql, found);
    }
    return found;
  }

  all(sql: string, ...params: SqlValue[]): Row[] {
    return this.statement(sql).all(...params) as Row[];
  }

  get(sql: string, ...params: SqlValue[]): Row | undefined {
    return this.statement(sql).get(...params) as Row | undefined;
  }

  run(sql: string, ...params: SqlValue[]): number {
    return Number(this.statement(sql).run(...params).changes);
  }

  /**
   * 写事务（`BEGIN IMMEDIATE`：一开始就拿写锁，读-改-写之间别人插不进来）。`fn` 抛错就回滚；
   * 嵌套调用并进外层事务。
   */
  transaction<T>(fn: () => T): T {
    if (this.depth > 0) {
      this.depth += 1;
      try {
        return fn();
      } finally {
        this.depth -= 1;
      }
    }
    this.db.exec("BEGIN IMMEDIATE");
    this.depth = 1;
    try {
      const result = fn();
      this.db.exec("COMMIT");
      return result;
    } catch (error) {
      this.db.exec("ROLLBACK");
      throw error;
    } finally {
      this.depth = 0;
    }
  }

  /** 别的连接提交过就变（自己的提交不变）：`serve` 用它察觉 CLI 的改动。 */
  dataVersion(): number {
    return Number(this.get("PRAGMA data_version")?.data_version ?? 0);
  }

  /** `PRAGMA integrity_check` 的结果行；只有一行 `ok` 是好的。 */
  integrityCheck(): string[] {
    return this.all("PRAGMA integrity_check").map((row) =>
      String(row.integrity_check),
    );
  }

  /** 在线、一致的备份（`VACUUM INTO`）；目标已存在就拒绝。备份文件 0600。 */
  backup(target: string): void {
    if (existsSync(target))
      throw new SqliteStoreError("exists", `备份目标已存在：${target}`);
    closeSync(openSync(target, "wx", 0o600));
    // VACUUM INTO 要目标不存在或为空：先建一个 0600 的空文件，权限随它。
    this.db.prepare("VACUUM INTO ?").run(target);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.statements.clear();
    this.db.close();
  }
}

function wrapOpenError(path: string, error: unknown): SqliteStoreError {
  if (error instanceof SqliteStoreError) return error;
  return new SqliteStoreError(
    "corrupt",
    `数据库打不开或已损坏，拒绝启动：${path}`,
  );
}

export interface OpenOptions {
  /** 新建（文件必须不存在）；缺省打开已有的。 */
  create?: boolean;
  /** 迁移目录；缺省随包的 `@armadra/control-store-sql/migrations`。 */
  migrationsDir?: string;
  now?: () => number;
}

/** 打开（或新建）库并迁移到最新。 */
export function openDatabase(
  path: string,
  options: OpenOptions = {},
): Database {
  const migrations = loadMigrations(options.migrationsDir);
  const now = options.now ?? Date.now;
  const exists = existsSync(path);
  if (options.create && exists)
    throw new SqliteStoreError("exists", `数据库已存在：${path}`);
  if (!options.create && !exists)
    throw new SqliteStoreError("not_found", `数据库不存在：${path}`);
  if (options.create) closeSync(openSync(path, "wx", 0o600));
  else if (statSync(path).size === 0)
    throw new SqliteStoreError("unknown", `数据库是空文件，拒绝启动：${path}`);

  const { DatabaseSync } = loadSqlite();
  let raw: DatabaseSync;
  try {
    raw = new DatabaseSync(path);
  } catch (error) {
    if (options.create) rmSync(path, { force: true });
    throw wrapOpenError(path, error);
  }
  const db = new Database(path, raw);
  try {
    db.exec(`PRAGMA busy_timeout = ${BUSY_TIMEOUT_MS}`);
    const applicationId = Number(
      db.get("PRAGMA application_id")?.application_id ?? 0,
    );
    if (options.create) {
      db.exec("PRAGMA journal_mode = WAL");
      db.exec(`PRAGMA application_id = ${APPLICATION_ID}`);
    } else {
      if (applicationId !== APPLICATION_ID)
        throw new SqliteStoreError(
          "unknown",
          `不是本程序的数据库，拒绝启动：${path}`,
        );
      const check = db.get("PRAGMA quick_check");
      if (String(check?.quick_check) !== "ok")
        throw new SqliteStoreError(
          "corrupt",
          `数据库校验不通过，拒绝启动：${path}`,
        );
      db.exec("PRAGMA journal_mode = WAL");
    }
    db.exec("PRAGMA foreign_keys = ON");
    migrate(db, migrations, now);
  } catch (error) {
    db.close();
    // 自己刚建的文件没写成：删掉，下次还能重建；已有的库一个字节也不动。
    if (options.create)
      for (const suffix of ["", "-wal", "-shm"])
        rmSync(`${path}${suffix}`, { force: true });
    throw wrapOpenError(path, error);
  }
  return db;
}
