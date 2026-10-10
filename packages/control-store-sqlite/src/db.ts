/**
 * 端口实现对库的全部要求：几个同步方法与可嵌套的写事务。两个实现：Node 的 `node:sqlite`
 * （`open.ts` 的 `Database`）与 Workers 的 DO SQLite
 * （`@armadra/relay-runtime-workers` 的 `store-do-sqlite/`）。本文件与 `ports.ts` / `rows.ts` /
 * `tenants.ts` / `keys.ts` / `apply.ts` 不 import 任何 Node 专有模块（只许 `node:crypto`），
 * Workers 直接复用。
 */
export type SqlValue = string | number | bigint | null | Uint8Array;
export type Row = Record<string, SqlValue>;

export interface SqlDatabase {
  /** 库的位置（日志与错误提示用；DO 是对象名）。 */
  readonly path: string;
  exec(sql: string): void;
  all(sql: string, ...params: SqlValue[]): Row[];
  get(sql: string, ...params: SqlValue[]): Row | undefined;
  /** 答改动的行数。 */
  run(sql: string, ...params: SqlValue[]): number;
  /** 写事务：`fn` 抛错就回滚；嵌套调用并进外层事务。 */
  transaction<T>(fn: () => T): T;
}

/**
 * 签名私钥的包裹（`signing_keys.private_key_wrap`）：Node 中继不包（`none`，靠库文件 0600）；
 * Workers 中继用 `RELAY_MASTER_KEY` 做 AES-256-GCM（`aes-256-gcm`）。`open` 认不出密文就抛错。
 */
export interface KeyWrap {
  readonly name: string;
  seal(plain: string): string;
  open(sealed: string): string;
}
