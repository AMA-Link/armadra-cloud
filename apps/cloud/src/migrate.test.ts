import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, expect, it } from "vitest";

import { PostgresStoreError } from "@armadra/control-store-postgres/errors";
import { defaultMigrationsDir } from "@armadra/control-store-postgres/migrate";

import { listMigrations, migrate } from "./migrate.ts";

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

it("随包的迁移从 0001 起连续", () => {
  expect(listMigrations(defaultMigrationsDir())).toEqual([
    "0001_control_init.sql",
    "0002_relay_control.sql",
    "0003_traffic.sql",
  ]);
});

it("连不上：拒绝，错误里不带连接串与口令", async () => {
  const error = await migrate({
    databaseUrl: "postgres://someone:hunter2@127.0.0.1:1/x",
    migrationsDir: defaultMigrationsDir(),
    logLevel: "info",
  }).catch((caught: unknown) => caught);
  expect(error).toBeInstanceOf(PostgresStoreError);
  expect((error as Error).message).not.toMatch(/hunter2|someone/);
});

it("迁移目录编号不连续：打包错误，不连库就拒绝", async () => {
  const dir = tempDir();
  writeFileSync(join(dir, "0002_b.sql"), "select 1;");
  await expect(
    migrate({
      databaseUrl: "postgres://x@127.0.0.1:1/x",
      migrationsDir: dir,
      logLevel: "info",
    }),
  ).rejects.toThrow();
});
