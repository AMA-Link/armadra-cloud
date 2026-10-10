/**
 * DO SQLite（`ctx.storage.sql`）包成控制核存储要的 {@link SqlDatabase}：端口、行映射、租户与签名钥
 * 全部复用 `@armadra/control-store-sqlite`（Node 中继 `--store sqlite` 的同一份代码与 SQL）。
 *
 * - 事务：DO 不接受 `BEGIN` / `COMMIT` 语句，写事务走 `ctx.storage.transactionSync`；嵌套并进外层。
 * - 绑定值：DO 只收 `string | number | null | ArrayBuffer`；`bigint` 转 number，`Uint8Array` 转
 *   `ArrayBuffer`；读出来的 `ArrayBuffer` 转回 `Uint8Array`。
 * - `run` 答改动行数：DO 游标的 `rowsWritten` 连索引一起算，这里另问一次 `changes()`。
 */
import type {
  Row,
  SqlDatabase,
  SqlValue,
} from "@armadra/control-store-sqlite/db";

type Bound = string | number | null | ArrayBuffer;

function bind(value: SqlValue): Bound {
  if (typeof value === "bigint") return Number(value);
  if (value instanceof Uint8Array)
    return value.buffer.slice(
      value.byteOffset,
      value.byteOffset + value.byteLength,
    ) as ArrayBuffer;
  return value;
}

function unbind(row: Record<string, SqlStorageValue>): Row {
  const out: Row = {};
  for (const [key, value] of Object.entries(row))
    out[key] = value instanceof ArrayBuffer ? new Uint8Array(value) : value;
  return out;
}

export class DoSqlDatabase implements SqlDatabase {
  readonly path: string;
  private readonly storage: DurableObjectStorage;
  private depth = 0;

  constructor(storage: DurableObjectStorage, name: string) {
    this.storage = storage;
    this.path = `durable-object:${name}`;
  }

  exec(sql: string): void {
    // 多条语句时 DO 依次执行；游标要走完，语句才全部生效。
    for (const _row of this.storage.sql.exec(sql)) {
      // 丢弃结果。
    }
  }

  all(sql: string, ...params: SqlValue[]): Row[] {
    return this.storage.sql
      .exec(sql, ...params.map(bind))
      .toArray()
      .map(unbind);
  }

  get(sql: string, ...params: SqlValue[]): Row | undefined {
    const [first] = this.all(sql, ...params);
    return first;
  }

  run(sql: string, ...params: SqlValue[]): number {
    this.storage.sql.exec(sql, ...params.map(bind)).toArray();
    const changed = this.storage.sql
      .exec<{ n: number }>("SELECT changes() AS n")
      .one();
    return Number(changed.n);
  }

  transaction<T>(fn: () => T): T {
    if (this.depth > 0) {
      this.depth += 1;
      try {
        return fn();
      } finally {
        this.depth -= 1;
      }
    }
    this.depth = 1;
    try {
      return this.storage.transactionSync(fn);
    } finally {
      this.depth = 0;
    }
  }
}
