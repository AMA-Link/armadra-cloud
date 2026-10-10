/**
 * DO SQLite：适配层的语义、迁移账本、私钥包裹、初始化前置条件，以及「账本对不上就拒绝服务、
 * 绝不清库」（最后一个用例会弄坏账本，放在最后）。
 */
import { createHash } from "node:crypto";

import { env, evictDurableObject, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";

import type { ControlDO } from "../src/control-do.ts";
import { CONTROL_OBJECT_NAME } from "../src/env.ts";
import {
  initializePersonal,
  NotInitializedError,
} from "../src/personal-control.ts";
import { DoSqlDatabase } from "../src/store-do-sqlite/database.ts";
import {
  KEY_WRAP_AES_GCM,
  masterKeyWrap,
  MasterKeyError,
} from "../src/store-do-sqlite/keywrap.ts";
import {
  BUNDLED_MIGRATION_FILES,
  bundledMigrations,
} from "../src/store-do-sqlite/migrations.ts";
import { call, json, login } from "./api.ts";

const control = () =>
  env.CONTROL.get(env.CONTROL.idFromName(CONTROL_OBJECT_NAME));

describe("DoSqlDatabase", () => {
  it("读写、changes()、BLOB 往返、事务回滚与嵌套", async () => {
    await runInDurableObject(control(), (_instance: ControlDO, state) => {
      const db = new DoSqlDatabase(state.storage, "test");
      db.exec(
        "CREATE TABLE probe (id INTEGER PRIMARY KEY, name TEXT, data BLOB); CREATE INDEX probe_name ON probe (name)",
      );
      expect(
        db.run(
          "INSERT INTO probe (name, data) VALUES (?, ?)",
          "a",
          Uint8Array.of(1, 2),
        ),
      ).toBe(1);
      expect(db.run("INSERT INTO probe (name) VALUES (?), (?)", "b", "c")).toBe(
        2,
      );
      expect(
        db.run("UPDATE probe SET name = name || ? WHERE id > ?", "!", 0n),
      ).toBe(3);
      const row = db.get("SELECT * FROM probe WHERE id = ?", 1);
      expect(row?.name).toBe("a!");
      expect([...(row?.data as Uint8Array)]).toEqual([1, 2]);
      expect(() =>
        db.transaction(() => {
          db.run("DELETE FROM probe");
          db.transaction(() =>
            db.run("INSERT INTO probe (name) VALUES ('nested')"),
          );
          throw new Error("rollback");
        }),
      ).toThrow("rollback");
      expect(
        db.all("SELECT name FROM probe ORDER BY id").map((r) => r.name),
      ).toEqual(["a!", "b!", "c!"]);
      expect(db.get("SELECT * FROM probe WHERE id = 99")).toBeUndefined();
      db.exec("DROP TABLE probe");
    });
  });

  it("账本：与 Node 中继同一份迁移、SHA-384 一致；控制面的库里记着它们", async () => {
    const migrations = bundledMigrations();
    expect(BUNDLED_MIGRATION_FILES).toEqual(["0001_init.sql"]);
    expect(migrations.map((m) => m.id)).toEqual([1]);
    // 打进 Worker 的字节与 migrations.lock 记的一致（lock 记 SHA-256，由 repo:check 守）。
    const lock = JSON.parse(
      String((await import("../../../migrations.lock?raw")).default),
    ) as Record<string, Record<string, string>>;
    const recorded = lock["packages/control-store-sql/migrations"] ?? {};
    expect(Object.keys(recorded)).toEqual([...BUNDLED_MIGRATION_FILES]);
    expect(
      createHash("sha256")
        .update(new TextEncoder().encode(migrations[0]?.sql ?? ""))
        .digest("hex"),
    ).toBe(recorded["0001_init.sql"]);
    await login();
    const ledger = await runInDurableObject(control(), (_instance, state) =>
      state.storage.sql
        .exec<{
          id: number;
          name: string;
          sha384: string;
        }>("SELECT id, name, sha384 FROM relay_migrations")
        .toArray(),
    );
    expect(ledger).toEqual([
      { id: 1, name: "0001_init", sha384: migrations[0]?.sha384 },
    ]);
  });
});

describe("签名私钥的包裹", () => {
  it("库里是 AES-256-GCM 密文；换主密钥解不开；主密钥格式不对直接拒绝", async () => {
    await login();
    const row = await runInDurableObject(control(), (_instance, state) =>
      state.storage.sql
        .exec<{
          private_key_pkcs8: string;
          private_key_wrap: string;
        }>("SELECT private_key_pkcs8, private_key_wrap FROM signing_keys")
        .one(),
    );
    expect(row.private_key_wrap).toBe(KEY_WRAP_AES_GCM);
    // PKCS#8 Ed25519 的 DER 以 MC4CAQAwBQYDK2Vw 开头（base64）；密文里不该有。
    expect(row.private_key_pkcs8.startsWith("MC4CAQAwBQYDK2Vw")).toBe(false);
    const right = masterKeyWrap(env.RELAY_MASTER_KEY);
    expect(
      right.open(row.private_key_pkcs8).startsWith("MC4CAQAwBQYDK2Vw"),
    ).toBe(true);
    const wrong = masterKeyWrap("ff".repeat(32));
    expect(() => wrong.open(row.private_key_pkcs8)).toThrow();
    expect(() => masterKeyWrap("short")).toThrow(MasterKeyError);
    expect(() => masterKeyWrap(undefined)).toThrow(MasterKeyError);
  });
});

describe("初始化的前置条件", () => {
  it("缺账号 / 口令或口令不合策略：not_initialized，什么也不建", async () => {
    const input = {
      db: null as unknown as DoSqlDatabase,
      config: null as never,
      wrap: masterKeyWrap(env.RELAY_MASTER_KEY),
      sources: null as never,
      hasAssets: () => false,
      now: Date.now,
      onError: () => {},
    };
    await expect(initializePersonal(input, {})).rejects.toBeInstanceOf(
      NotInitializedError,
    );
    await expect(
      initializePersonal(input, { account: "owner", password: "short" }),
    ).rejects.toMatchObject({ message: "password_too_short" });
    await expect(
      initializePersonal(input, { account: "owner", password: "password1234" }),
    ).rejects.toMatchObject({ message: "password_too_common" });
  });
});

describe("存储认不出就拒绝服务", () => {
  it("账本的 SHA-384 被改：/v1 与 /ready 答 503 storage_unavailable，数据一行不动", async () => {
    await login();
    await runInDurableObject(control(), (_instance, state) => {
      state.storage.sql.exec("UPDATE relay_migrations SET sha384 = 'tampered'");
    });
    await evictDurableObject(control());
    const info = await call("/.well-known/armadra-platform");
    expect(info.status).toBe(503);
    expect((await json(info)).code).toBe("storage_unavailable");
    const ready = await call("/ready");
    expect(ready.status).toBe(503);
    expect(await json(ready)).toMatchObject({
      ok: false,
      code: "storage_unavailable",
    });
    const rows = await runInDurableObject(control(), (_instance, state) => ({
      tenants: state.storage.sql.exec("SELECT count(*) AS n FROM tenants").one()
        .n,
      accounts: state.storage.sql
        .exec("SELECT count(*) AS n FROM accounts")
        .one().n,
      ledger: state.storage.sql
        .exec("SELECT sha384 FROM relay_migrations")
        .one().sha384,
    }));
    expect(rows).toEqual({ tenants: 1, accounts: 1, ledger: "tampered" });
  });
});
