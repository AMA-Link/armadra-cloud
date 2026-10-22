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
    // 先剥掉 $$…$$ 函数体（plpgsql 的 BEGIN / END 不是事务语句），再按语句开头匹配事务控制。
    const outside = migration.sql.replace(/\$\$[\s\S]*?\$\$/g, "");
    expect(outside).not.toMatch(
      /^\s*(BEGIN|START\s+TRANSACTION|COMMIT|END|ROLLBACK|ABORT)\b/im,
    );
  }
});

it("不是迁移文件名答 null；不是 UTF-8 拒绝", () => {
  expect(migrationFile("notes.md", new Uint8Array())).toBeNull();
  expect(() =>
    migrationFile("0001_x.sql", new Uint8Array([0xff, 0xfe, 0xfd])),
  ).toThrow();
});

it("事务语句检查对函数体外的 BEGIN / START TRANSACTION / END 等都能发现", () => {
  const strip = (sql: string) => sql.replace(/\$\$[\s\S]*?\$\$/g, "");
  const pattern =
    /^\s*(BEGIN|START\s+TRANSACTION|COMMIT|END|ROLLBACK|ABORT)\b/im;
  expect(strip("CREATE FUNCTION f() AS $$\nBEGIN\nEND\n$$;")).not.toMatch(
    pattern,
  );
  for (const word of [
    "BEGIN",
    "START TRANSACTION",
    "COMMIT",
    "END",
    "ROLLBACK",
    "ABORT",
  ])
    expect(strip(`SELECT 1;\n${word}\n`)).toMatch(pattern);
});
