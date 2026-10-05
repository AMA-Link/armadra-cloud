import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, expect, it } from "vitest";

import {
  listMigrations,
  migrate,
  MigrationsNotSupportedError,
} from "./migrate.ts";

const dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs.splice(0))
    rmSync(dir, { recursive: true, force: true });
});

function tempDir() {
  const dir = mkdtempSync(join(tmpdir(), "cloud-migrate-"));
  dirs.push(dir);
  return dir;
}

it("只认 NNNN_name.sql，按编号排序", () => {
  const dir = tempDir();
  for (const name of ["0002_b.sql", "0001_a.sql", ".gitkeep", "notes.md"])
    writeFileSync(join(dir, name), "");
  expect(listMigrations(dir)).toEqual(["0001_a.sql", "0002_b.sql"]);
});

it("本仓迁移目录现在是空的", () => {
  expect(
    listMigrations(fileURLToPath(new URL("./db/migrations/", import.meta.url))),
  ).toEqual([]);
});

it("目录里有迁移而运行器未落地时拒绝，不连数据库也不跳过", async () => {
  const dir = tempDir();
  writeFileSync(join(dir, "0001_accounts.sql"), "select 1;");
  await expect(
    migrate({
      databaseUrl: "postgres://x@127.0.0.1:1/x",
      migrationsDir: dir,
      logLevel: "info",
    }),
  ).rejects.toBeInstanceOf(MigrationsNotSupportedError);
});
