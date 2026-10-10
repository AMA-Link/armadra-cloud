/**
 * 存储端口的合同测试：端口约定（`ports.ts` 文件头）逐条落成用例——原子读-改-写、抛错不写、
 * 读答副本、按创建顺序、租户隔离、各「原子地」方法的连带效果。内存参照与每个存储实现各跑一遍。
 *
 * 用法：`portsContract("SQLite 端口", () => 新建端口())`。
 */
import { describe, expect, it } from "vitest";

import type {
  AccountRecord,
  DeviceRecord,
  LinkRecord,
  PasswordRecord,
  SourceRecord,
  TenantSettings,
} from "../ports.ts";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";

import type { MemoryPorts } from "./memory-ports.ts";

const NOW = Date.UTC(2026, 9, 10, 12);
const HASH_A = "a".repeat(64);
const HASH_B = "b".repeat(64);
const SETTINGS: TenantSettings = {
  issuer: "https://relay.armadra.test",
  allowGuests: true,
  trustedOrigins: ["https://app.armadra.test"],
};
const PASSWORD: PasswordRecord = {
  kdf: "scrypt",
  cost: 16384,
  block: 8,
  parallel: 1,
  length: 32,
  salt: "c2FsdA==",
  hash: "aGFzaA==",
};

function account(name: string, accountId = `acct:${name}`): AccountRecord {
  return {
    accountId,
    name,
    password: structuredClone(PASSWORD),
    createdAtMs: 1,
    updatedAtMs: 1,
    failures: 0,
    lockedUntilMs: 0,
    lastFailureAtMs: 0,
  };
}

function device(
  deviceId: string,
  subject: string,
  overrides: Partial<DeviceRecord> = {},
): DeviceRecord {
  return {
    deviceId,
    subject,
    platform: "desktop",
    name: `d-${deviceId}`,
    refreshHash: HASH_A,
    previousRefreshHash: null,
    rotation: 1,
    createdAtMs: NOW,
    expiresAtMs: NOW + 86_400_000,
    revokedAtMs: null,
    lastSeenAtMs: null,
    ...overrides,
  };
}

function source(sourceId: string, name = "mac"): SourceRecord {
  return {
    sourceId,
    name,
    kind: "desktop",
    publicJwk: { kty: "OKP", crv: "Ed25519", x: "x".repeat(43) },
    coreVersion: "0.3.0",
    capabilities: ["terminal"],
    registeredAtMs: NOW,
    lastSeenAtMs: null,
    revokedAtMs: null,
    trustedOrigins: [],
  };
}

function link(
  linkId: string,
  sourceId: string,
  overrides: Partial<LinkRecord> = {},
): LinkRecord {
  return {
    linkId,
    sourceId,
    invitationId: `inv-${linkId}`,
    label: "",
    role: "viewer",
    secretHash: HASH_A,
    maxUses: null,
    uses: 0,
    createdAtMs: NOW,
    expiresAtMs: NOW + 60_000,
    revokedAtMs: null,
    ...overrides,
  };
}

const D1 = "d1".repeat(16);
const D2 = "d2".repeat(16);
const D3 = "d3".repeat(16);
const S1 = "51".repeat(16);
const S2 = "52".repeat(16);
const L1 = "1".repeat(16);
const L2 = "2".repeat(16);

/** 对一个端口实现跑端口合同；每个用例拿一份新端口，建好 `t1`、`t2` 两个租户。 */
export function portsContract(
  label: string,
  createPorts: () => MemoryPorts,
): void {
  const fresh = () => {
    const ports = createPorts();
    ports.addTenant("t1", SETTINGS);
    ports.addTenant("t2", { ...SETTINGS, allowGuests: false });
    return ports;
  };

  describe(`${label}：端口合同`, () => {
    it("租户设置按租户答；没有的租户抛错", async () => {
      const ports = fresh();
      expect(await ports.tenants.get("t1")).toEqual(SETTINGS);
      expect((await ports.tenants.get("t2")).allowGuests).toBe(false);
      await expect(ports.tenants.get("nope")).rejects.toThrow();
    });

    it("账号：按名 / id 读、读答副本、租户隔离", async () => {
      const ports = fresh();
      ports.addAccount("t1", account("alice", "u-1"));
      ports.addAccount("t2", account("alice", "u-2"));
      expect(await ports.accounts.findByName("t1", "alice")).toEqual(
        account("alice", "u-1"),
      );
      expect((await ports.accounts.findByName("t2", "alice"))?.accountId).toBe(
        "u-2",
      );
      expect(await ports.accounts.get("t1", "u-2")).toBeNull();
      expect(await ports.accounts.findByName("t1", "bob")).toBeNull();
      const copy = await ports.accounts.get("t1", "u-1");
      if (copy) copy.failures = 99;
      expect((await ports.accounts.get("t1", "u-1"))?.failures).toBe(0);
    });

    it("update：改副本才写；fn 抛错什么也不写；不存在时 fn 拿到 null", async () => {
      const ports = fresh();
      ports.addAccount("t1", account("alice", "u-1"));
      const result = await ports.accounts.update("t1", "u-1", (draft) => {
        if (draft) draft.failures = 2;
        return "done";
      });
      expect(result).toBe("done");
      expect((await ports.accounts.get("t1", "u-1"))?.failures).toBe(2);
      await expect(
        ports.accounts.update("t1", "u-1", (draft) => {
          if (draft) draft.failures = 7;
          throw new Error("nope");
        }),
      ).rejects.toThrow("nope");
      expect((await ports.accounts.get("t1", "u-1"))?.failures).toBe(2);
      expect(
        await ports.accounts.update("t1", "u-9", (draft) => draft === null),
      ).toBe(true);
      expect(await ports.accounts.get("t1", "u-9")).toBeNull();
    });

    it("resetPassword：换口令、清锁，只撤销本人未撤销的设备", async () => {
      const ports = fresh();
      ports.addAccount("t1", {
        ...account("alice", "u-1"),
        failures: 5,
        lockedUntilMs: NOW + 1,
      });
      await ports.devices.insert("t1", device(D1, "u-1"), NOW);
      await ports.devices.insert(
        "t1",
        device(D2, "u-1", { revokedAtMs: NOW - 5 }),
        NOW,
      );
      await ports.devices.insert("t1", device(D3, `guest:${L1}:ab`), NOW);
      await ports.devices.insert("t2", device(D1, "u-1"), NOW);
      const next = { ...PASSWORD, salt: "bmV3", hash: "bmV3aGFzaA==" };
      expect(await ports.accounts.resetPassword("t1", "u-1", next, NOW)).toBe(
        true,
      );
      expect(await ports.accounts.get("t1", "u-1")).toMatchObject({
        password: next,
        updatedAtMs: NOW,
        failures: 0,
        lockedUntilMs: 0,
      });
      expect((await ports.devices.get("t1", D1))?.revokedAtMs).toBe(NOW);
      expect((await ports.devices.get("t1", D2))?.revokedAtMs).toBe(NOW - 5);
      expect((await ports.devices.get("t1", D3))?.revokedAtMs).toBeNull();
      expect((await ports.devices.get("t2", D1))?.revokedAtMs).toBeNull();
      expect(
        await ports.accounts.resetPassword("t1", "nobody", next, NOW),
      ).toBe(false);
    });

    it("设备：按创建顺序列出；新增时清掉早已失效的；update 与 touch 只往后推", async () => {
      const ports = fresh();
      const stale = device(D3, "u-1", {
        expiresAtMs: NOW - SESSION.refreshTtlMs - 1,
      });
      await ports.devices.insert("t1", stale, NOW - SESSION.refreshTtlMs * 3);
      await ports.devices.insert("t1", device(D2, "u-1"), NOW);
      await ports.devices.insert("t1", device(D1, "u-1"), NOW);
      expect(
        (await ports.devices.listBySubject("t1", "u-1")).map((d) => d.deviceId),
      ).toEqual([D2, D1]);
      expect(await ports.devices.get("t1", D3)).toBeNull();
      expect(await ports.devices.listBySubject("t2", "u-1")).toEqual([]);

      await ports.devices.update("t1", D1, (draft) => {
        if (draft) {
          draft.previousRefreshHash = draft.refreshHash;
          draft.refreshHash = HASH_B;
          draft.rotation += 1;
        }
      });
      expect(await ports.devices.get("t1", D1)).toMatchObject({
        refreshHash: HASH_B,
        previousRefreshHash: HASH_A,
        rotation: 2,
      });
      await ports.devices.touch(
        "t1",
        new Map([
          [D1, NOW + 10],
          [D2, NOW + 20],
          ["ff".repeat(16), NOW],
        ]),
      );
      await ports.devices.touch("t1", new Map([[D1, NOW + 5]]));
      expect((await ports.devices.get("t1", D1))?.lastSeenAtMs).toBe(NOW + 10);
      expect((await ports.devices.get("t1", D2))?.lastSeenAtMs).toBe(NOW + 20);
    });

    it("源：put 新增与替换（替换后排到最后）、update、按顺序列出、租户隔离", async () => {
      const ports = fresh();
      const first = await ports.sources.put("t1", S1, (existing) => {
        expect(existing).toBeNull();
        return source(S1, "one");
      });
      expect(first).toEqual(source(S1, "one"));
      await ports.sources.put("t1", S2, () => source(S2, "two"));
      await ports.sources.put("t1", S1, (existing) => ({
        ...(existing ?? source(S1)),
        coreVersion: "0.4.0",
      }));
      expect((await ports.sources.list("t1")).map((s) => s.sourceId)).toEqual([
        S2,
        S1,
      ]);
      expect((await ports.sources.get("t1", S1))?.coreVersion).toBe("0.4.0");
      await ports.sources.update("t1", S2, (draft) => {
        if (draft) draft.revokedAtMs = NOW;
      });
      expect((await ports.sources.get("t1", S2))?.revokedAtMs).toBe(NOW);
      expect(await ports.sources.list("t2")).toEqual([]);
      expect(await ports.sources.get("t2", S1)).toBeNull();
    });

    it("注册令牌：有效的满了答 false；过期的不算并被清掉；一次性消费，答签发账号", async () => {
      const ports = fresh();
      const add = (hash: string, expiresAtMs: number, nowMs = NOW) =>
        ports.registrations.add(
          "t1",
          { tokenHash: hash, expiresAtMs, accountId: "u-1" },
          { nowMs, max: 2 },
        );
      expect(await add("01".repeat(32), NOW + 10)).toBe(true);
      expect(await add("02".repeat(32), NOW + 1000)).toBe(true);
      expect(await add("03".repeat(32), NOW + 1000)).toBe(false);
      // 第一条过期之后又能加。
      expect(await add("03".repeat(32), NOW + 1000, NOW + 20)).toBe(true);
      expect(
        await ports.registrations.consume("t1", "01".repeat(32), NOW + 20),
      ).toBeNull();
      expect(
        await ports.registrations.consume("t2", "02".repeat(32), NOW),
      ).toBeNull();
      expect(
        await ports.registrations.consume("t1", "02".repeat(32), NOW),
      ).toEqual({ accountId: "u-1" });
      expect(
        await ports.registrations.consume("t1", "02".repeat(32), NOW),
      ).toBeNull();
      expect(
        await ports.registrations.consume("t1", "03".repeat(32), NOW + 2000),
      ).toBeNull();
    });

    it("链接：范围原样往返（整台时没有 scope 键）、按源过滤、按创建顺序、update", async () => {
      const ports = fresh();
      await ports.links.insert("t1", link(L1, S1));
      await ports.links.insert(
        "t1",
        link(L2, S2, {
          maxUses: 3,
          scope: { workspaceId: "ws_01", readOnly: true },
        }),
      );
      const plain = await ports.links.get("t1", L1);
      expect(plain).toEqual(link(L1, S1));
      expect(plain && "scope" in plain).toBe(false);
      expect((await ports.links.get("t1", L2))?.scope).toEqual({
        workspaceId: "ws_01",
        readOnly: true,
      });
      expect((await ports.links.list("t1")).map((l) => l.linkId)).toEqual([
        L1,
        L2,
      ]);
      expect(
        (await ports.links.list("t1", { sourceId: S2 })).map((l) => l.linkId),
      ).toEqual([L2]);
      expect(await ports.links.list("t2")).toEqual([]);
      await ports.links.update("t1", L2, (draft) => {
        if (draft) draft.uses += 1;
      });
      expect((await ports.links.get("t1", L2))?.uses).toBe(1);
    });

    it("撤销链接：连同它名下的访客设备；答源；再撤一次答 null", async () => {
      const ports = fresh();
      await ports.links.insert("t1", link(L1, S1));
      await ports.links.insert("t1", link(L2, S1));
      await ports.devices.insert("t1", device(D1, `guest:${L1}:aa`), NOW);
      await ports.devices.insert("t1", device(D2, `guest:${L2}:bb`), NOW);
      await ports.devices.insert("t1", device(D3, "u-1"), NOW);
      expect(await ports.links.revoke("t1", L1, NOW)).toBe(S1);
      expect((await ports.links.get("t1", L1))?.revokedAtMs).toBe(NOW);
      expect((await ports.devices.get("t1", D1))?.revokedAtMs).toBe(NOW);
      expect((await ports.devices.get("t1", D2))?.revokedAtMs).toBeNull();
      expect((await ports.devices.get("t1", D3))?.revokedAtMs).toBeNull();
      expect(await ports.links.revoke("t1", L1, NOW + 1)).toBeNull();
      expect(await ports.links.revoke("t2", L2, NOW)).toBeNull();
    });
  });
}
