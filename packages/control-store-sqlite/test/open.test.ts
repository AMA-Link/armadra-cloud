/**
 * 打开与迁移：新建 0600 + WAL + 账本；不认识 / 损坏 / 账本对不上 / 比程序新的库一律拒绝，
 * 而且一个字节也不动；补跑新迁移；多进程并发写不丢更新；VACUUM INTO 备份可恢复。
 */
import { execFile } from "node:child_process";
import {
  copyFileSync,
  mkdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { generateSigningKey } from "@armadra/control-core/signer";
import { afterAll, describe, expect, it } from "vitest";

import {
  createSqliteKeys,
  rotateSqliteKey,
  addSigningKey,
} from "../src/keys.ts";
import { defaultMigrationsDir, loadMigrations } from "../src/migrate.ts";
import { APPLICATION_ID, openDatabase } from "../src/open.ts";
import { createSqlitePorts } from "../src/ports.ts";
import {
  createTenant,
  exportTenant,
  importTenant,
  updateTenant,
} from "../src/tenants.ts";
import { tempDatabases } from "./harness.ts";

const run = promisify(execFile);
const databases = tempDatabases();
afterAll(() => databases.cleanup());
const file = (name: string) => join(databases.root, name);

const SETTINGS = {
  issuer: "https://relay.armadra.test",
  allowGuests: true,
  trustedOrigins: [],
};

function seeded(name: string) {
  const db = openDatabase(file(name), { create: true });
  db.transaction(() => createTenant(db, DEFAULT_TENANT, SETTINGS));
  addSigningKey(db, DEFAULT_TENANT, generateSigningKey("k1", 1));
  return db;
}

const codeOf = (fn: () => unknown) => {
  try {
    fn();
  } catch (error) {
    return (error as { code?: string }).code ?? String(error);
  }
  return "ok";
};

/** 打开失败之后文件字节不变。 */
function refusedUnchanged(path: string, expected: string) {
  const before = readFileSync(path);
  expect(codeOf(() => openDatabase(path))).toBe(expected);
  expect(readFileSync(path).equals(before)).toBe(true);
}

describe("新建", () => {
  it("0600、WAL、application_id，账本记下随包迁移的 SHA-384", () => {
    const db = openDatabase(file("fresh.db"), { create: true });
    if (process.platform !== "win32")
      expect(statSync(file("fresh.db")).mode & 0o777).toBe(0o600);
    expect(db.get("PRAGMA journal_mode")?.journal_mode).toBe("wal");
    expect(Number(db.get("PRAGMA application_id")?.application_id)).toBe(
      APPLICATION_ID,
    );
    const ledger = db.all("SELECT id, name, sha384 FROM relay_migrations");
    expect(ledger.map((row) => ({ ...row }))).toEqual(
      loadMigrations().map(({ id, name, sha384 }) => ({ id, name, sha384 })),
    );
    expect(ledger[0]?.name).toBe("0001_init");
    expect(db.integrityCheck()).toEqual(["ok"]);
    db.close();
    // 再打开：账本对得上，什么也不跑。
    openDatabase(file("fresh.db")).close();
    expect(codeOf(() => openDatabase(file("fresh.db"), { create: true }))).toBe(
      "exists",
    );
    expect(codeOf(() => openDatabase(file("missing.db")))).toBe("not_found");
  });
});

describe("拒绝启动，不动文件", () => {
  it("不是 SQLite 的文件、空文件", () => {
    writeFileSync(file("garbage.db"), "not a database at all".repeat(100));
    refusedUnchanged(file("garbage.db"), "corrupt");
    writeFileSync(file("empty.db"), "");
    refusedUnchanged(file("empty.db"), "unknown");
  });

  it("别的程序的 SQLite 库（application_id 不对）", () => {
    const db = openDatabase(file("other.db"), { create: true });
    db.exec("PRAGMA application_id = 7");
    db.close();
    refusedUnchanged(file("other.db"), "unknown");
  });

  it("账本被改、账本缺失、库比程序新", () => {
    const tampered = seeded("tampered.db");
    tampered.exec("UPDATE relay_migrations SET sha384 = 'x'");
    tampered.close();
    refusedUnchanged(file("tampered.db"), "unknown");

    const ledgerless = seeded("ledgerless.db");
    ledgerless.exec("DROP TABLE relay_migrations");
    ledgerless.close();
    refusedUnchanged(file("ledgerless.db"), "unknown");

    const newer = seeded("newer.db");
    newer.exec(
      "INSERT INTO relay_migrations (id, name, sha384, applied_at_ms) VALUES (2, '0002_future', 'y', 0)",
    );
    newer.close();
    refusedUnchanged(file("newer.db"), "newer");
  });

  it("页面损坏：quick_check 不过", () => {
    const db = seeded("broken.db");
    const ports = createSqlitePorts(db);
    for (let index = 0; index < 200; index += 1)
      void ports.links.insert(DEFAULT_TENANT, {
        linkId: index.toString(16).padStart(16, "0"),
        sourceId: "0".repeat(32),
        invitationId: "x".repeat(200),
        label: "y".repeat(200),
        role: "viewer",
        secretHash: "a".repeat(64),
        maxUses: null,
        uses: 0,
        createdAtMs: 0,
        expiresAtMs: 0,
        revokedAtMs: null,
      });
    db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
    db.close();
    const bytes = readFileSync(file("broken.db"));
    // 第一页（库头）之后的页面整片写坏。
    bytes.fill(0xa5, 4096 * 2, 4096 * 6);
    writeFileSync(file("broken.db"), bytes);
    refusedUnchanged(file("broken.db"), "corrupt");
  });
});

describe("迁移", () => {
  it("账本是前缀时补跑新迁移；目录编号不连续是打包错误", () => {
    const dir = file("migrations");
    mkdirSync(dir);
    const bundled = defaultMigrationsDir();
    copyFileSync(join(bundled, "0001_init.sql"), join(dir, "0001_init.sql"));
    openDatabase(file("upgrade.db"), {
      create: true,
      migrationsDir: dir,
    }).close();
    writeFileSync(
      join(dir, "0002_extra.sql"),
      "CREATE TABLE extra (id INTEGER) STRICT;\n",
    );
    const db = openDatabase(file("upgrade.db"), { migrationsDir: dir });
    expect(
      db
        .all("SELECT name FROM relay_migrations ORDER BY id")
        .map((row) => row.name),
    ).toEqual(["0001_init", "0002_extra"]);
    db.close();
    // 同一个库给只认识 0001 的程序：比程序新，拒绝。
    expect(codeOf(() => openDatabase(file("upgrade.db")))).toBe("newer");
    writeFileSync(join(dir, "0004_gap.sql"), "SELECT 1;\n");
    expect(() =>
      openDatabase(file("gap.db"), { create: true, migrationsDir: dir }),
    ).toThrow(/不连续/);
  });
});

describe("多进程与备份", () => {
  it("四个进程同时读-改-写同一行：不丢更新、不报忙", async () => {
    const db = seeded("shared.db");
    const linkId = "0123456789abcdef";
    await createSqlitePorts(db).links.insert(DEFAULT_TENANT, {
      linkId,
      sourceId: "0".repeat(32),
      invitationId: "inv",
      label: "",
      role: "viewer",
      secretHash: "a".repeat(64),
      maxUses: null,
      uses: 0,
      createdAtMs: 0,
      expiresAtMs: 0,
      revokedAtMs: null,
    });
    const child = fileURLToPath(
      new URL("./fixtures/increment.ts", import.meta.url),
    );
    await Promise.all(
      [1, 2, 3, 4].map(() =>
        run(process.execPath, [child, file("shared.db"), linkId, "50"]),
      ),
    );
    const link = await createSqlitePorts(db).links.get(DEFAULT_TENANT, linkId);
    expect(link?.uses).toBe(200);
    // 别的进程提交过：data_version 变了（serve 靠它察觉 CLI 的改动）。
    db.close();
  }, 60_000);

  it("别的连接轮换签名钥、改设置：本连接立即读到，data_version 变化", () => {
    const db = seeded("keys.db");
    const other = openDatabase(file("keys.db"));
    const keys = createSqliteKeys(db, DEFAULT_TENANT);
    expect(keys.signingKeys().map((key) => key.kid)).toEqual(["k1"]);
    const version = db.dataVersion();
    expect(rotateSqliteKey(other, DEFAULT_TENANT, 10)).toBe("k2");
    updateTenant(other, DEFAULT_TENANT, (draft) => {
      draft.issuer = "https://moved.armadra.test";
    });
    expect(db.dataVersion()).not.toBe(version);
    expect(keys.issuer()).toBe("https://moved.armadra.test");
    expect(keys.signingKeys().map((key) => [key.kid, key.retiredAtMs])).toEqual(
      [
        ["k1", 10 + 3_600_000],
        ["k2", null],
      ],
    );
    // 早已退役的钥在下一次轮换时删掉。
    rotateSqliteKey(other, DEFAULT_TENANT, 10 + 3_600_001);
    expect(keys.signingKeys().map((key) => key.kid)).toEqual(["k2", "k3"]);
    other.close();
    db.close();
  });

  it("VACUUM INTO 备份：0600、能打开、内容一致；目标已存在就拒绝；导出再导入行序不变", async () => {
    const db = seeded("live.db");
    const ports = createSqlitePorts(db);
    await ports.sources.put(DEFAULT_TENANT, "5".repeat(32), () => ({
      sourceId: "5".repeat(32),
      name: "mac",
      kind: "desktop",
      publicJwk: { kty: "OKP", crv: "Ed25519", x: "x".repeat(43) },
      coreVersion: "0.3.0",
      capabilities: [],
      registeredAtMs: 1,
      lastSeenAtMs: null,
      revokedAtMs: null,
      trustedOrigins: [],
    }));
    db.backup(file("backup.db"));
    if (process.platform !== "win32")
      expect(statSync(file("backup.db")).mode & 0o777).toBe(0o600);
    expect(codeOf(() => db.backup(file("backup.db")))).toBe("exists");
    const restored = openDatabase(file("backup.db"));
    const snapshot = exportTenant(db, DEFAULT_TENANT);
    expect(exportTenant(restored, DEFAULT_TENANT)).toEqual(snapshot);
    const copy = openDatabase(file("copy.db"), { create: true });
    importTenant(copy, DEFAULT_TENANT, snapshot);
    expect(exportTenant(copy, DEFAULT_TENANT)).toEqual(snapshot);
    for (const handle of [restored, copy, db]) handle.close();
  });
});
