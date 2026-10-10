/**
 * 测试装配：临时目录里的新库，包成合同测试要的 {@link MemoryPorts}（`addTenant` / `addAccount`
 * 直接写库）。用完 `cleanup()` 关库删目录。
 */
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { MemoryPorts } from "@armadra/control-core/testing/memory-ports";

import { openDatabase, type Database } from "../src/open.ts";
import { createSqlitePorts } from "../src/ports.ts";
import { addAccount, createTenant } from "../src/tenants.ts";

export interface TempDatabases {
  root: string;
  /** 新建一个库（文件名自增）。 */
  create(): Database;
  ports(): MemoryPorts;
  cleanup(): void;
}

export function tempDatabases(prefix = "control-store-sqlite-"): TempDatabases {
  const root = mkdtempSync(join(tmpdir(), prefix));
  const opened: Database[] = [];
  let counter = 0;
  const create = () => {
    counter += 1;
    const db = openDatabase(join(root, `db-${counter}.db`), { create: true });
    opened.push(db);
    return db;
  };
  return {
    root,
    create,
    ports() {
      const db = create();
      return {
        ...createSqlitePorts(db),
        addTenant: (tenantId, settings) =>
          db.transaction(() => createTenant(db, tenantId, settings)),
        addAccount: (tenantId, account) => addAccount(db, tenantId, account),
      };
    },
    cleanup() {
      for (const db of opened) db.close();
      rmSync(root, { recursive: true, force: true });
    },
  };
}
