import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import {
  checkAvailable,
  LedgerError,
  parseMigrationName,
  pendingMigrations,
  type MigrationFile,
} from "./ledger.ts";

const file = (id: number, name: string): MigrationFile => ({
  id,
  name,
  sha384: `sha-${name}`,
  sql: "",
});
const AVAILABLE = [file(1, "0001_init"), file(2, "0002_more")];
const problemOf = (fn: () => unknown) => {
  try {
    fn();
  } catch (error) {
    return error instanceof LedgerError ? error.problem : String(error);
  }
  return "ok";
};

describe("迁移账本", () => {
  it("随包迁移文件名合规、编号从 1 起连续", () => {
    const dir = fileURLToPath(new URL("../migrations/", import.meta.url));
    const names = readdirSync(dir).filter((name) => name.endsWith(".sql"));
    const parsed = names.map((name) => parseMigrationName(name));
    expect(parsed.every((entry) => entry !== null)).toBe(true);
    expect(parsed.map((entry) => entry?.id)).toEqual(
      names.map((_, index) => index + 1),
    );
    expect(parseMigrationName("1_bad.sql")).toBeNull();
    expect(parseMigrationName("0001_init.sql")).toEqual({
      id: 1,
      name: "0001_init",
    });
  });

  it("空账本跑全部；前缀跑剩下的；对不上 / 不连续 / 比程序新一律拒绝", () => {
    expect(pendingMigrations([], AVAILABLE)).toEqual(AVAILABLE);
    expect(
      pendingMigrations([AVAILABLE[0] as MigrationFile], AVAILABLE),
    ).toEqual([AVAILABLE[1]]);
    expect(pendingMigrations(AVAILABLE, AVAILABLE)).toEqual([]);
    expect(
      problemOf(() =>
        pendingMigrations(
          [{ ...file(1, "0001_init"), sha384: "x" }],
          AVAILABLE,
        ),
      ),
    ).toBe("mismatch");
    expect(
      problemOf(() => pendingMigrations([file(2, "0002_more")], AVAILABLE)),
    ).toBe("gap");
    expect(
      problemOf(() =>
        pendingMigrations([...AVAILABLE, file(3, "0003_x")], AVAILABLE),
      ),
    ).toBe("newer");
    expect(problemOf(() => checkAvailable([file(1, "a"), file(3, "c")]))).toBe(
      "gap",
    );
  });
});
