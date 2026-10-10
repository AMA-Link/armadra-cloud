/**
 * 与运行时无关的那一半（Workers 的 DO SQLite 共用）：迁移的对账与执行、私钥包裹。
 */
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { generateSigningKey } from "@armadra/control-core/signer";
import { afterAll, describe, expect, it } from "vitest";

import { applyMigrations, migrationFile } from "../src/apply.ts";
import type { KeyWrap } from "../src/db.ts";
import {
  addSigningKey,
  createSqliteKeys,
  readSigningKeys,
  rotateSqliteKey,
} from "../src/keys.ts";
import { defaultMigrationsDir, loadMigrations } from "../src/migrate.ts";
import { openDatabase } from "../src/open.ts";
import { createTenant, exportTenant, importTenant } from "../src/tenants.ts";
import { tempDatabases } from "./harness.ts";

const databases = tempDatabases("control-store-portable-");
afterAll(() => databases.cleanup());

/** 测试用的包裹：可逆、看得出不是明文。 */
const reverse: KeyWrap = {
  name: "test-reverse",
  seal: (plain) => `sealed:${[...plain].reverse().join("")}`,
  open: (sealed) => {
    if (!sealed.startsWith("sealed:")) throw new Error("bad");
    return [...sealed.slice("sealed:".length)].reverse().join("");
  },
};

const SETTINGS = {
  issuer: "https://relay.armadra.test",
  allowGuests: true,
  trustedOrigins: [],
};

describe("迁移（apply.ts）", () => {
  it("migrationFile 与 loadMigrations 的 SHA-384 一致，不是迁移文件名答 null", () => {
    const dir = defaultMigrationsDir();
    const [first] = loadMigrations(dir);
    const bytes = readFileSync(join(dir, "0001_init.sql"));
    expect(migrationFile("0001_init.sql", bytes)).toEqual(first);
    expect(migrationFile("README.md", bytes)).toBeNull();
  });

  it("运行时内部表（sqlite_* 与 DO 的 _cf_*）不算业务表；别的表在而没有账本 → 拒绝", () => {
    const migrations = loadMigrations();
    const fresh = databases.create();
    // 新建的库已经跑过迁移：再跑一遍什么也不做。
    expect(applyMigrations(fresh, migrations)).toBe(0);

    const path = join(databases.root, "foreign.db");
    const foreign = openDatabase(path, { create: true });
    foreign.exec("DROP TABLE relay_migrations");
    expect(() => applyMigrations(foreign, migrations)).toThrow(/没有迁移账本/);
    foreign.close();
  });
});

describe("私钥包裹（KeyWrap）", () => {
  it("写进库的是密文，读回原样；导出导入同一包裹往返", () => {
    const db = databases.create();
    db.transaction(() => createTenant(db, DEFAULT_TENANT, SETTINGS));
    const key = generateSigningKey("k1", 1);
    addSigningKey(db, DEFAULT_TENANT, key, reverse);
    const row = db.get(
      "SELECT private_key_pkcs8, private_key_wrap FROM signing_keys",
    );
    expect(row?.private_key_wrap).toBe("test-reverse");
    expect(String(row?.private_key_pkcs8)).not.toContain(key.privateKeyPkcs8);
    expect(createSqliteKeys(db, DEFAULT_TENANT, reverse).signingKeys()).toEqual(
      [key],
    );
    expect(rotateSqliteKey(db, DEFAULT_TENANT, 10, reverse)).toBe("k2");
    expect(
      db
        .all("SELECT private_key_wrap FROM signing_keys")
        .map((r) => r.private_key_wrap),
    ).toEqual(["test-reverse", "test-reverse"]);

    const snapshot = exportTenant(db, DEFAULT_TENANT, reverse);
    const copy = databases.create();
    importTenant(copy, DEFAULT_TENANT, snapshot, reverse);
    expect(readSigningKeys(copy, DEFAULT_TENANT, reverse)).toEqual(
      snapshot.signingKeys,
    );
  });

  it("包过的私钥没给包裹、给错包裹或密文坏了 → 拒绝，不当明文用", () => {
    const db = databases.create();
    db.transaction(() => createTenant(db, DEFAULT_TENANT, SETTINGS));
    addSigningKey(db, DEFAULT_TENANT, generateSigningKey("k1", 1), reverse);
    const code = (fn: () => unknown) => {
      try {
        fn();
      } catch (error) {
        return (error as { code?: string }).code;
      }
      return "ok";
    };
    expect(code(() => readSigningKeys(db, DEFAULT_TENANT))).toBe("unknown");
    expect(
      code(() =>
        readSigningKeys(db, DEFAULT_TENANT, { ...reverse, name: "other" }),
      ),
    ).toBe("unknown");
    db.run("UPDATE signing_keys SET private_key_pkcs8 = 'garbage'");
    expect(code(() => readSigningKeys(db, DEFAULT_TENANT, reverse))).toBe(
      "corrupt",
    );
  });

  it("没包的私钥（Node 中继）照常读，给不给包裹都一样", () => {
    const db = databases.create();
    db.transaction(() => createTenant(db, DEFAULT_TENANT, SETTINGS));
    const key = generateSigningKey("k1", 1);
    addSigningKey(db, DEFAULT_TENANT, key);
    expect(readSigningKeys(db, DEFAULT_TENANT)).toEqual([key]);
    expect(readSigningKeys(db, DEFAULT_TENANT, reverse)).toEqual([key]);
  });
});
