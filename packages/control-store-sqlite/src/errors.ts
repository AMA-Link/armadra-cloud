/** 库的问题（打不开、不认识、已损坏、比程序新）：调用方打印 message 后退出；不带库内容。 */
export type SqliteStoreErrorCode =
  | "not_found"
  | "exists"
  | "unknown"
  | "corrupt"
  | "newer";

export class SqliteStoreError extends Error {
  readonly code: SqliteStoreErrorCode;
  constructor(code: SqliteStoreErrorCode, message: string) {
    super(message);
    this.name = "SqliteStoreError";
    this.code = code;
  }
}
