/** 库的问题（连不上、不认识、比程序新、没有 / 已有租户）：调用方打印 message 后退出；不带库内容与口令。 */
export type PostgresStoreErrorCode =
  | "unreachable"
  | "not_found"
  | "exists"
  | "unknown"
  | "newer";

export class PostgresStoreError extends Error {
  readonly code: PostgresStoreErrorCode;
  constructor(code: PostgresStoreErrorCode, message: string) {
    super(message);
    this.name = "PostgresStoreError";
    this.code = code;
  }
}
