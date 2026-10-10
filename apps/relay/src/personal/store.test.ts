/**
 * 存储的选择、迁移往返、备份、外部改动通知与 doctor（Node 中继存储设计 §5.3、§5.4）。
 */
import { spawn, type ChildProcess } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { DEFAULT_TENANT } from "@armadra/control-core/ports";
import { openDatabase } from "@armadra/control-store-sqlite/open";
import { createSqlitePorts } from "@armadra/control-store-sqlite/ports";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { PASSWORD } from "../../test/personal.ts";
import { parsePersonalDoctor } from "./args.ts";
import { diagnose } from "./doctor.ts";
import { createState } from "./init.ts";
import { migrateToFile, migrateToSqlite, runBackup } from "./migrate.ts";
import { RUN_LOCK_FILE } from "./runlock.ts";
import { StateError, StateStore, type State } from "./state.ts";
import { openPersonalStore, resolveStoreKind } from "./store.ts";

const root = mkdtempSync(join(tmpdir(), "relay-store-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

const NOW = Date.UTC(2026, 9, 10, 12);
const SOURCE = "0123456789abcdef0123456789abcdef";
const LINK = "0123456789abcdef";
const HASH = (char: string) => char.repeat(64);

let template: State;
beforeAll(async () => {
  const store = await createState({
    dataDir: join(root, "template"),
    account: "me",
    password: PASSWORD,
    issuer: "https://relay.test:8443",
    store: "file",
    nowMs: NOW,
  });
  await store.rotateKey(NOW + 1);
  await store.close();
  // 每张表、每个可选字段都放一点：往返要逐字段等价。
  StateStore.open(join(root, "template")).update((draft) => {
    draft.sources.push({
      sourceId: SOURCE,
      name: "我的 MacBook",
      kind: "desktop",
      publicJwk: { kty: "OKP", crv: "Ed25519", x: "x".repeat(43) },
      coreVersion: "0.3.0",
      capabilities: ["terminal", "links.scope"],
      registeredAtMs: NOW,
      lastSeenAtMs: NOW + 5,
      revokedAtMs: null,
      trustedOrigins: ["https://relay.test:8443"],
    });
    draft.registrationTokens.push({ tokenHash: HASH("1"), expiresAtMs: NOW });
    draft.devices.push(
      {
        deviceId: "d".repeat(32),
        subject: "acct:me",
        platform: "desktop",
        name: "桌面",
        refreshHash: HASH("2"),
        previousRefreshHash: HASH("3"),
        rotation: 2,
        createdAtMs: NOW,
        expiresAtMs: NOW + 1000,
        revokedAtMs: null,
        lastSeenAtMs: NOW + 1,
      },
      {
        deviceId: "e".repeat(32),
        subject: `guest:${LINK}:ab`,
        platform: "browser",
        name: "访客",
        refreshHash: HASH("4"),
        previousRefreshHash: null,
        rotation: 1,
        createdAtMs: NOW,
        expiresAtMs: NOW + 1000,
        revokedAtMs: NOW + 2,
        lastSeenAtMs: null,
      },
    );
    draft.links.push(
      {
        linkId: LINK,
        sourceId: SOURCE,
        invitationId: "inv",
        label: "给同事",
        role: "viewer",
        secretHash: HASH("5"),
        maxUses: 2,
        uses: 1,
        createdAtMs: NOW,
        expiresAtMs: NOW + 60_000,
        revokedAtMs: null,
        scope: { workspaceId: "ws_01", readOnly: true },
      },
      {
        linkId: "fedcba9876543210",
        sourceId: SOURCE,
        invitationId: "inv2",
        label: "",
        role: "editor",
        secretHash: HASH("6"),
        maxUses: null,
        uses: 0,
        createdAtMs: NOW,
        expiresAtMs: NOW + 60_000,
        revokedAtMs: null,
      },
    );
    draft.settings.trustedOrigins = ["https://app.armadra.test"];
    draft.settings.rateLimits = { anonymous: 30, account: 100 };
    draft.settings.displayName = "家里的中转";
    draft.settings.trustedProxy = {
      proxies: ["127.0.0.1/32"],
      prefer: "real-ip",
    };
  });
  template = StateStore.open(join(root, "template")).current;
});

let counter = 0;
/** 一个只有状态文件的数据目录（老部署）。 */
function legacyDir(): string {
  counter += 1;
  const dir = join(root, `legacy-${counter}`);
  StateStore.create(dir, structuredClone(template));
  return dir;
}

const codeOf = async (fn: () => unknown) => {
  try {
    await fn();
  } catch (error) {
    return error instanceof StateError ? error.code : String(error);
  }
  return "ok";
};

describe("选存储", () => {
  it("有库用 sqlite；只有状态文件用 file（老部署）；都没有用 sqlite；两个都在拒绝", async () => {
    const dir = legacyDir();
    expect(resolveStoreKind(dir)).toEqual({ kind: "file", legacy: true });
    expect(resolveStoreKind(dir, "file")).toEqual({
      kind: "file",
      legacy: false,
    });
    expect(await codeOf(() => resolveStoreKind(dir, "sqlite"))).toBe("invalid");
    expect(await codeOf(() => resolveStoreKind(dir, "postgres"))).toBe(
      "invalid",
    );
    expect(resolveStoreKind(join(root, "empty"))).toEqual({
      kind: "sqlite",
      legacy: false,
    });
    writeFileSync(join(dir, "relay.db"), "");
    expect(await codeOf(() => resolveStoreKind(dir))).toBe("invalid");
  });
});

describe("迁移往返", () => {
  it("file → sqlite → file：逐字段等价、字节等价（修订号除外）；原文件改名留作回退", async () => {
    const dir = legacyDir();
    const original = readFileSync(join(dir, "state.json"), "utf8");
    const toSqlite = await migrateToSqlite(dir, NOW);
    expect(existsSync(join(dir, "state.json"))).toBe(false);
    expect(readFileSync(toSqlite.kept, "utf8")).toBe(original);
    expect(
      readdirSync(dir).filter((name) => name.includes("migrating")),
    ).toEqual([]);

    const store = await openPersonalStore(dir);
    expect(store.kind).toBe("sqlite");
    expect(await store.snapshot()).toEqual(template);
    // 控制核端口读到的与状态文件里的一样。
    expect(store.settings()).toEqual(template.settings);
    expect(store.keys.signingKeys()).toEqual(template.signingKeys);
    await store.close();

    const toFile = await migrateToFile(dir, NOW + 1);
    expect(existsSync(toFile.kept)).toBe(true);
    expect(existsSync(join(dir, "relay.db"))).toBe(false);
    const back = readFileSync(join(dir, "state.json"), "utf8");
    const strip = (text: string) => text.replace(/"revision": \d+/, "");
    expect(strip(back)).toBe(strip(original));
    expect(back).toBe(original);
  });

  it("在 SQLite 上改过之后再迁回：改动都在、修订号前进", async () => {
    const dir = legacyDir();
    await migrateToSqlite(dir, NOW);
    const store = await openPersonalStore(dir);
    await store.ports.links.revoke(DEFAULT_TENANT, LINK, NOW + 9);
    await store.rotateKey(NOW + 3_600_000 * 2);
    await store.close();
    await migrateToFile(dir, NOW);
    const state = StateStore.open(dir).current;
    expect(state.revision).toBeGreaterThan(template.revision);
    expect(state.links.find((row) => row.linkId === LINK)?.revokedAtMs).toBe(
      NOW + 9,
    );
    expect(state.signingKeys.map((key) => key.kid)).toEqual(["k2", "k3"]);
  });

  it("拒绝：目标已存在、源不存在、状态文件认不出（不建库、不动原文件）、serve 正在跑", async () => {
    const dir = legacyDir();
    writeFileSync(join(dir, "relay.db"), "x");
    expect(await codeOf(() => migrateToSqlite(dir))).toBe("exists");
    rmSync(join(dir, "relay.db"));
    expect(await codeOf(() => migrateToFile(dir))).toBe("exists");
    expect(await codeOf(() => migrateToSqlite(join(root, "nothing")))).toBe(
      "not_initialized",
    );

    const broken = legacyDir();
    writeFileSync(
      join(broken, "state.json"),
      JSON.stringify({ ...template, version: 9 }),
    );
    const before = readFileSync(join(broken, "state.json"));
    expect(await codeOf(() => migrateToSqlite(broken))).toBe("invalid");
    expect(readFileSync(join(broken, "state.json")).equals(before)).toBe(true);
    expect(readdirSync(broken).sort()).toEqual(["state.json"]);
  });

  it("serve 还在跑（serve.pid 指向活着的进程）：拒绝迁移；进程没了就照常", async () => {
    const dir = legacyDir();
    const child: ChildProcess = spawn(
      process.execPath,
      ["-e", "setTimeout(() => {}, 30000)"],
      { stdio: "ignore" },
    );
    try {
      writeFileSync(join(dir, RUN_LOCK_FILE), `${child.pid}\n`);
      await expect(migrateToSqlite(dir)).rejects.toThrow(/serve 正在运行/);
    } finally {
      child.kill();
      await new Promise((resolve) => child.once("exit", resolve));
    }
    expect((await migrateToSqlite(dir)).database).toBe(join(dir, "relay.db"));
  });
});

describe("SQLite 存储", () => {
  it("别的进程撤销链接：watch 一秒内回调，revoked() 里有它", async () => {
    const dir = legacyDir();
    await migrateToSqlite(dir);
    const store = await openPersonalStore(dir);
    expect((await store.revoked()).links.has(LINK)).toBe(false);
    let changes = 0;
    const stop = store.watch(
      () => {
        changes += 1;
      },
      (error) => {
        throw error;
      },
      20,
    );
    const other = openDatabase(join(dir, "relay.db"));
    await createSqlitePorts(other).links.revoke(DEFAULT_TENANT, LINK, NOW);
    other.close();
    const deadline = Date.now() + 2_000;
    while (changes === 0 && Date.now() < deadline)
      await new Promise((resolve) => setTimeout(resolve, 20));
    stop();
    expect(changes).toBeGreaterThan(0);
    expect((await store.revoked()).links.has(LINK)).toBe(true);
    await store.close();
  });

  it("损坏的库：serve / status 拒绝启动，库一个字节不动", async () => {
    const dir = join(root, "corrupt");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "relay.db"), "definitely not sqlite".repeat(64));
    const before = readFileSync(join(dir, "relay.db"));
    expect(await codeOf(() => openPersonalStore(dir))).toBe("invalid");
    expect(readFileSync(join(dir, "relay.db")).equals(before)).toBe(true);
  });

  it("备份：SQLite 用 VACUUM INTO、状态文件拷一份；都是 0600、不覆盖", async () => {
    const sqliteDir = legacyDir();
    await migrateToSqlite(sqliteDir);
    const out = join(root, "backup.db");
    await runBackup(sqliteDir, out, () => {});
    if (process.platform !== "win32")
      expect((await import("node:fs")).statSync(out).mode & 0o777).toBe(0o600);
    const restoreDir = join(root, "restored");
    mkdirSync(restoreDir);
    writeFileSync(join(restoreDir, "relay.db"), readFileSync(out));
    const restored = await openPersonalStore(restoreDir);
    expect(await restored.snapshot()).toEqual(template);
    await restored.close();
    await expect(runBackup(sqliteDir, out, () => {})).rejects.toThrow(/已存在/);

    const fileDir = legacyDir();
    const copy = join(root, "backup.json");
    await runBackup(fileDir, copy, () => {});
    expect(readFileSync(copy, "utf8")).toBe(
      readFileSync(join(fileDir, "state.json"), "utf8"),
    );
  });
});

describe("doctor", () => {
  const levels = (findings: { level: string; item: string }[]) =>
    Object.fromEntries(findings.map((f) => [f.item, f.level]));

  it("新装 SQLite 的缺省配置全部通过", async () => {
    const dir = legacyDir();
    await migrateToSqlite(dir);
    chmodSync(dir, 0o700);
    const findings = await diagnose(
      parsePersonalDoctor(["--data-dir", dir, "--host", "relay.test"], {}),
      NOW,
    );
    expect(findings.filter((f) => f.level !== "pass")).toEqual([]);
    expect(levels(findings)).toMatchObject({
      存储: "pass",
      数据库完整性: "pass",
      监听地址: "pass",
      受信代理: "pass",
      口令哈希: "pass",
    });
  });

  it("报问题：目录权限松、明文监听公网、受信代理太宽、明文却没配代理、残留口令文件", async () => {
    const dir = legacyDir();
    if (process.platform !== "win32") chmodSync(dir, 0o755);
    const passwordFile = join(root, "pw");
    writeFileSync(passwordFile, PASSWORD, { mode: 0o644 });
    chmodSync(passwordFile, 0o644);
    const loose = await diagnose(
      parsePersonalDoctor(
        [
          "--data-dir",
          dir,
          "--listen",
          "0.0.0.0:8443",
          "--tls",
          "plain",
          "--trusted-proxy",
          "0.0.0.0/0",
          "--password-file",
          passwordFile,
        ],
        {},
      ),
      NOW,
    );
    const found = levels(loose);
    if (process.platform !== "win32") {
      expect(found.数据目录权限).toBe("warn");
      expect(found.口令文件权限).toBe("warn");
    }
    expect(found.监听地址).toBe("fail");
    expect(found.受信代理).toBe("warn");
    expect(found.口令文件).toBe("pass");
    expect(found.存储).toBe("pass");

    const noProxy = await diagnose(
      parsePersonalDoctor(
        [
          "--data-dir",
          dir,
          "--listen",
          "127.0.0.1:8443",
          "--tls",
          "plain",
          "--trusted-proxy",
          "",
        ],
        {},
      ),
      NOW,
    );
    expect(levels(noProxy).受信代理).toBe("warn");
    expect(levels(noProxy).监听地址).toBe("pass");
  });

  it("存储打不开：报失败而不是抛错", async () => {
    const findings = await diagnose(
      parsePersonalDoctor(["--data-dir", join(root, "corrupt")], {}),
      NOW,
    );
    expect(levels(findings).存储).toBe("fail");
  });
});
