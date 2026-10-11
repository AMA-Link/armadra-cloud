/**
 * 合同测试的装配：每次 `ports()` 一个新的临时 schema（迁移到最新），包成合同测试要的
 * {@link MemoryPorts}。建库与合同的 `addTenant` / `addAccount` 都是同步调用：这里把它们排进一条
 * 队列，端口的每个方法先等队列走完再执行，用例看到的先后与内存参照一致。`reset()` 删掉用过的 schema。
 */
import type { ControlPorts } from "@armadra/control-core/ports";
import type { MemoryPorts } from "@armadra/control-core/testing/memory-ports";

import type { Sql } from "../../src/db.ts";
import { createPostgresPorts } from "../../src/ports.ts";
import { addAccount, createTenant } from "../../src/tenants.ts";
import { tempSchema, type TempSchema } from "../pg.ts";

const PORTS = [
  "accounts",
  "devices",
  "sources",
  "registrations",
  "links",
  "tenants",
  "traffic",
] as const;

export interface ContractDatabase {
  ports(): MemoryPorts;
  reset(): Promise<void>;
}

export function contractDatabase(): ContractDatabase {
  const schemas: TempSchema[] = [];
  return {
    ports() {
      const schema = tempSchema("s2_contract");
      schemas.push(schema);
      let sql: Sql | undefined;
      let real: ControlPorts | undefined;
      let queue: Promise<void> = schema.open({ max: 4 }).then((db) => {
        sql = db.sql;
        real = createPostgresPorts(db.sql);
      });
      queue.catch(() => {});
      const enqueue = (step: (sql: Sql) => Promise<void>) => {
        queue = queue.then(() => step(sql as Sql));
        // 出错留给下一次等队列时抛给用例；这里不让它成为未处理的拒绝。
        queue.catch(() => {});
      };
      const port = (name: (typeof PORTS)[number]) =>
        new Proxy(
          {},
          {
            get:
              (_target, method: string) =>
              async (...args: unknown[]) => {
                await queue;
                const target = (real as ControlPorts)[
                  name
                ] as unknown as Record<string, (...a: unknown[]) => unknown>;
                return target[method]?.(...args);
              },
          },
        );
      return {
        ...(Object.fromEntries(
          PORTS.map((name) => [name, port(name)]),
        ) as unknown as ControlPorts),
        addTenant: (tenantId, settings) =>
          enqueue((db) => createTenant(db, tenantId, settings)),
        addAccount: (tenantId, account) =>
          enqueue((db) => addAccount(db, tenantId, account)),
      };
    },
    async reset() {
      for (const schema of schemas.splice(0)) await schema.drop();
    },
  };
}
