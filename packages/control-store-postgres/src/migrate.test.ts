/** 迁移目录（不连库）：编号从 1 起连续、文件名合规、读得出 SHA-384。连库的拒绝条件在集成测试。 */
import { checkAvailable } from "@armadra/control-store-sql/ledger";
import { expect, it } from "vitest";

import {
  defaultMigrationsDir,
  loadMigrations,
  migrationFile,
} from "./migrate.ts";

it("随包的迁移编号从 1 起连续，SQL 不带事务语句", () => {
  const migrations = loadMigrations(defaultMigrationsDir());
  expect(migrations.map((row) => row.name)).toEqual([
    "0001_control_init",
    "0002_relay_control",
    "0003_traffic",
    "0004_saas_accounts",
    "0005_saas_teams",
    "0006_saas_hosts",
    "0007_saas_grants",
    "0008_saas_audit",
    "0009_saas_ops",
  ]);
  expect(() => checkAvailable(migrations)).not.toThrow();
  for (const migration of migrations) {
    expect(migration.sha384).toMatch(/^[0-9a-f]{96}$/);
    // 事务语句带分号；plpgsql 函数体里的 BEGIN 没有分号，不算。
    expect(migration.sql).not.toMatch(/^\s*(BEGIN|COMMIT|ROLLBACK)\s*;/im);
  }
});

it("不是迁移文件名答 null；不是 UTF-8 拒绝", () => {
  expect(migrationFile("notes.md", new Uint8Array())).toBeNull();
  expect(() =>
    migrationFile("0001_x.sql", new Uint8Array([0xff, 0xfe, 0xfd])),
  ).toThrow();
});
