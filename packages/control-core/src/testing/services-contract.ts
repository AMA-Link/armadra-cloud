/**
 * 控制核服务的合同测试：只经端口跑一遍登录、会话、源、链接、撤销与 me-stream 扇出。内存参照
 * （`memory-ports.ts`）与每个存储实现（SQLite……）各跑一遍，结果必须一样。
 *
 * 用法：在实现包的测试里 `servicesContract("SQLite 端口", () => 新建端口())`；端口要支持测试装配的
 * `addTenant` / `addAccount`（{@link MemoryPorts}）。
 */
import { identityFingerprint } from "@armadra/platform-protocol/e2e";
import { TEST_KEYS } from "@armadra/platform-protocol/fixtures";
import { beforeAll, describe, expect, it, vi } from "vitest";

import { changePassword, createAccounts } from "../accounts.ts";
import { ControlError } from "../errors.ts";
import { createLinks } from "../links.ts";
import { createMeStreamCore } from "../me-stream.ts";
import { hashPassword } from "../passwords.ts";
import type { AccountRecord, PasswordRecord } from "../ports.ts";
import { isRevokedJti } from "../revocation.ts";
import { createAuthenticator, refresh, type Caller } from "../sessions.ts";
import { createSources } from "../sources.ts";
import {
  memoryHarness,
  type MemoryHarness,
  type MemoryPorts,
} from "./memory-ports.ts";

const PASSWORD = "plover-quartz-lantern-41";
const DEVICE = { platform: "desktop" as const, name: "test" };
const SOURCE_ID = "0123456789abcdef0123456789abcdef";
const SOURCE_X = TEST_KEYS.source.publicJwk.x;

let password: PasswordRecord | undefined;

function account(name: string, accountId: string): AccountRecord {
  if (!password) throw new Error("口令哈希还没算好");
  return {
    accountId,
    name,
    password,
    createdAtMs: 0,
    updatedAtMs: 0,
    failures: 0,
    lockedUntilMs: 0,
    lastFailureAtMs: 0,
  };
}

/** 令牌的载荷（只拆不验，验签由 relay-kernel / core 负责）。 */
const payloadOf = (token: string | undefined): Record<string, unknown> =>
  JSON.parse(
    Buffer.from((token ?? "").split(".")[1] ?? "", "base64url").toString(),
  ) as Record<string, unknown>;

const codeOf = async (run: () => unknown) => {
  try {
    await run();
  } catch (error) {
    return error instanceof ControlError ? error.code : String(error);
  }
  return "ok";
};

async function ownerOf(h: MemoryHarness): Promise<Caller> {
  h.ports.addAccount(h.context.tenantId, account("alice", "u-1"));
  const session = await createAccounts(h.context).login(
    { account: "alice", password: PASSWORD, device: DEVICE },
    "198.51.100.1",
  );
  return createAuthenticator(h.context).authenticate(
    `Bearer ${session.accessToken}`,
  );
}

async function registered(h: MemoryHarness, owner: Caller) {
  const sources = createSources(h.context);
  const { registrationToken } = await sources.registrationToken(owner);
  const result = await sources.register(
    {
      registrationToken,
      sourceId: SOURCE_ID,
      publicKey: { kty: "OKP", crv: "Ed25519", x: SOURCE_X },
      name: "mac",
      kind: "desktop",
      coreVersion: "0.2.0-test",
      capabilities: [],
      protocol: { major: 1, minor: 0 },
    },
    "ip",
  );
  return { sources, result };
}

type HarnessOptions = Omit<
  NonNullable<Parameters<typeof memoryHarness>[0]>,
  "ports"
>;

/** 对一个端口实现跑服务合同；`label` 进 describe 名。 */
export function servicesContract(
  label: string,
  createPorts: () => MemoryPorts,
): void {
  const harness = (options: HarnessOptions = {}) =>
    memoryHarness({ ...options, ports: createPorts() });

  describe(label, () => {
    beforeAll(async () => {
      password ??= await hashPassword(PASSWORD);
    });

    describe("账号与会话", () => {
      it("账号 id 是存储给的不透明 sub；刷新旋转、重放作废整台设备", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        expect(owner).toMatchObject({ subject: "u-1", role: "owner" });
        const first = await createAccounts(h.context).login(
          { account: "alice", password: PASSWORD, device: DEVICE },
          "198.51.100.1",
        );
        expect(first.account).toMatchObject({
          accountId: "u-1",
          displayName: "alice",
        });
        const second = await refresh(h.context, first.refreshToken as string);
        expect(second.device.deviceId).toBe(first.device.deviceId);
        expect(
          await codeOf(() => refresh(h.context, first.refreshToken as string)),
        ).toBe("session_revoked");
        expect(
          await codeOf(() => refresh(h.context, second.refreshToken as string)),
        ).toBe("session_revoked");
      });

      it("租户隔离：别的租户里同名账号登录不上", async () => {
        const h = harness({ tenantId: "t1" });
        h.ports.addTenant("t2", {
          issuer: "https://cloud.armadra.test",
          allowGuests: true,
          trustedOrigins: [],
        });
        h.ports.addAccount("t2", account("alice", "u-2"));
        expect(
          await codeOf(() =>
            createAccounts(h.context).login(
              { account: "alice", password: PASSWORD, device: DEVICE },
              "198.51.100.2",
            ),
          ),
        ).toBe("credentials_invalid");
      });

      it("改口令原子地撤销账号本人的全部设备；口令策略由装配方给", async () => {
        const h = harness({
          passwordPolicy: (value) =>
            value.length < 12 ? "password_too_short" : undefined,
        });
        const owner = await ownerOf(h);
        expect(
          await codeOf(() => changePassword(h.context, "alice", "short", 1)),
        ).toBe("password_too_short");
        await changePassword(
          h.context,
          "alice",
          "saffron-glacier-meadow-77",
          1,
        );
        const device = await h.ports.devices.get("t1", owner.deviceId);
        expect(device?.revokedAtMs).toBe(1);
        expect(
          await codeOf(() => changePassword(h.context, "nobody", PASSWORD, 1)),
        ).toBe("not_found");
      });

      describe("改口令（cloud-api §18）", () => {
        const NEXT = "saffron-glacier-meadow-77";
        const policy = {
          passwordPolicy: (value: string) =>
            value.length < 12 ? ("password_too_short" as const) : undefined,
        };
        const signIn = (h: MemoryHarness, secret = PASSWORD) =>
          createAccounts(h.context).login(
            { account: "alice", password: secret, device: DEVICE },
            "198.51.100.1",
          );
        const callerOf = (h: MemoryHarness, accessToken: string) =>
          createAuthenticator(h.context).authenticate(`Bearer ${accessToken}`);

        it("旧口令不对：credentials_invalid，计入锁定计数，连错 5 次锁定", async () => {
          const h = harness(policy);
          const owner = await ownerOf(h);
          const accounts = createAccounts(h.context);
          expect(
            await codeOf(() =>
              accounts.changePassword(
                owner,
                { password: "wrong-old-password", newPassword: NEXT },
                "ip",
              ),
            ),
          ).toBe("credentials_invalid");
          expect((await h.ports.accounts.get("t1", "u-1"))?.failures).toBe(1);
          for (let attempt = 0; attempt < 4; attempt += 1)
            await codeOf(() =>
              accounts.changePassword(
                owner,
                { password: "wrong-old-password", newPassword: NEXT },
                "ip",
              ),
            );
          expect(
            await codeOf(() =>
              accounts.changePassword(
                owner,
                { password: PASSWORD, newPassword: NEXT },
                "ip",
              ),
            ),
          ).toBe("account_locked");
          // 口令没换：锁着时新口令也登不上。
          expect(await codeOf(() => signIn(h, NEXT))).toBe("account_locked");
          expect((await h.ports.accounts.get("t1", "u-1"))?.password.hash).toBe(
            password?.hash,
          );
        });

        it("新口令不合策略：答策略码，什么也不改", async () => {
          const h = harness(policy);
          const owner = await ownerOf(h);
          expect(
            await codeOf(() =>
              createAccounts(h.context).changePassword(
                owner,
                { password: PASSWORD, newPassword: "short" },
                "ip",
              ),
            ),
          ).toBe("password_too_short");
          expect(await codeOf(() => signIn(h))).toBe("ok");
          expect(
            (await h.ports.devices.get("t1", owner.deviceId))?.revokedAtMs,
          ).toBeNull();
        });

        it("成功：其它设备与刷新令牌失效，当前设备保留并换新刷新令牌，旧口令登不上", async () => {
          const h = harness(policy);
          h.ports.addAccount("t1", account("alice", "u-1"));
          const current = await signIn(h);
          const other = await signIn(h);
          const caller = await callerOf(h, current.accessToken);
          const session = await createAccounts(h.context).changePassword(
            caller,
            { password: PASSWORD, newPassword: NEXT },
            "ip",
          );
          expect(session.device.deviceId).toBe(current.device.deviceId);
          expect(session.refreshToken).not.toBe(current.refreshToken);
          // 当前设备：访问令牌照样认，新刷新令牌能用；旧刷新令牌作废但不撤销这台。
          expect(await codeOf(() => callerOf(h, current.accessToken))).toBe(
            "ok",
          );
          expect(
            await codeOf(() =>
              refresh(h.context, current.refreshToken as string),
            ),
          ).toBe("session_revoked");
          const next = await refresh(h.context, session.refreshToken as string);
          expect(next.device.deviceId).toBe(current.device.deviceId);
          // 其它设备：访问令牌与刷新令牌都失效。
          expect(await codeOf(() => callerOf(h, other.accessToken))).toBe(
            "unauthenticated",
          );
          expect(
            await codeOf(() =>
              refresh(h.context, other.refreshToken as string),
            ),
          ).toBe("session_revoked");
          // 旧口令登不上，新口令能登。
          expect(await codeOf(() => signIn(h))).toBe("credentials_invalid");
          expect(await codeOf(() => signIn(h, NEXT))).toBe("ok");
        });

        it("访客不能改口令", async () => {
          const h = harness(policy);
          await ownerOf(h);
          expect(
            await codeOf(() =>
              createAccounts(h.context).changePassword(
                {
                  subject: `guest:${"a".repeat(16)}:x`,
                  deviceId: "d",
                  role: "guest",
                  linkId: "a".repeat(16),
                },
                { password: PASSWORD, newPassword: NEXT },
                "ip",
              ),
            ),
          ).toBe("forbidden");
        });
      });

      it("限流来自 RateLimiter 端口", async () => {
        const h = harness({
          limit: () => ({ allowed: false, retryAfterMs: 1234 }),
        });
        h.ports.addAccount("t1", account("alice", "u-1"));
        let error: ControlError | undefined;
        try {
          await createAccounts(h.context).login(
            { account: "alice", password: PASSWORD, device: DEVICE },
            "ip",
          );
        } catch (caught) {
          error = caught as ControlError;
        }
        expect(error?.code).toBe("rate_limited");
        expect(error?.retryAfterMs).toBe(1234);
      });
    });

    describe("源、链接与撤销", () => {
      it("登记的源归签发注册令牌的账号；在线状态来自中继端口", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources, result } = await registered(h, owner);
        expect(result.ownerAccountId).toBe("u-1");
        expect((await sources.list(owner))[0]?.online).toBe(false);
        h.online.set(SOURCE_ID, "0.3.0");
        expect((await sources.list(owner))[0]).toMatchObject({
          online: true,
          coreVersion: "0.3.0",
        });
      });

      it("隧道节点地址：缺省 issuer 的 wss /t/v1；装配方给 tunnelUrl 时原样答（可带 query，§17）", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const source = await h.context.ports.sources.get(
          h.context.tenantId,
          SOURCE_ID,
        );
        if (!source) throw new Error("源没登记上");
        expect(sources.relayFor(source).nodes).toEqual([
          { url: "wss://cloud.armadra.test/t/v1", weight: 1 },
        ]);
        h.context.tunnelUrl = (sourceId) =>
          `wss://relay.armadra.test/t/v1?src=${sourceId}`;
        expect(createSources(h.context).relayFor(source).nodes).toEqual([
          {
            url: `wss://relay.armadra.test/t/v1?src=${SOURCE_ID}`,
            weight: 1,
          },
        ]);
      });

      it("访客 accept → 断言；次数用完 link_exhausted；撤销链接连同访客设备与中继令牌一起失效", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const links = createLinks(h.context, sources);
        const created = await links.create(owner, {
          kind: "source_invite",
          sourceId: SOURCE_ID,
          invitationId: "inv",
          label: "",
          role: "viewer",
          expiresAtMs: h.clock.now + 60_000,
          maxUses: 1,
        });
        const guest = await links.accept(
          { linkId: created.linkId, secret: created.secret },
          "ip",
        );
        expect(guest.guestSession.account.displayName).toBe("guest");
        // cloud-api §20：源登记的公钥的指纹（访客按链接片段钉扎，这里只是提示）。
        expect(guest.identityFp).toBe(
          await identityFingerprint(
            new Uint8Array(Buffer.from(SOURCE_X, "base64url")),
          ),
        );
        expect(
          await codeOf(() =>
            links.accept(
              { linkId: created.linkId, secret: created.secret },
              "ip",
            ),
          ),
        ).toBe("link_exhausted");
        const relayJti = JSON.parse(
          Buffer.from(
            (guest.relayToken as string).split(".")[1] ?? "",
            "base64url",
          ).toString(),
        ).jti as string;
        expect(await isRevokedJti(h.context, relayJti, h.clock.now)).toBe(
          false,
        );
        await links.revoke(owner, created.linkId);
        expect(await isRevokedJti(h.context, relayJti, h.clock.now)).toBe(true);
        expect(h.disconnects.at(-1)).toEqual({
          subjectPrefix: `guest:${created.linkId}:`,
        });
        expect(
          await codeOf(() =>
            createAuthenticator(h.context).authenticate(
              `Bearer ${guest.guestSession.accessToken}`,
            ),
          ),
        ).toBe("unauthenticated");
      });

      it("分享范围：访客断言带 scp、只读时中继令牌带 ro；links.get / list 报范围；sources.assertion 沿用", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const links = createLinks(h.context, sources);
        const base = {
          kind: "source_invite" as const,
          sourceId: SOURCE_ID,
          invitationId: "inv",
          label: "",
          role: "viewer",
          expiresAtMs: h.clock.now + 60_000,
        };
        const scoped = await links.create(owner, {
          ...base,
          scope: { workspaceId: "ws_01", readOnly: true },
        });
        expect((await links.get(scoped.linkId)).scope).toEqual({
          workspaceId: "ws_01",
          readOnly: true,
        });
        const accepted = await links.accept(
          { linkId: scoped.linkId, secret: scoped.secret },
          "ip",
        );
        expect(payloadOf(accepted.assertion).scp).toEqual(["ws:ws_01", "ro"]);
        expect(payloadOf(accepted.relayToken).ro).toBe(true);
        const guest = await createAuthenticator(h.context).authenticate(
          `Bearer ${accepted.guestSession.accessToken}`,
        );
        const again = await sources.assertion(guest, SOURCE_ID, undefined);
        expect(payloadOf(again.assertion).scp).toEqual(["ws:ws_01", "ro"]);
        expect(payloadOf(again.relayToken).ro).toBe(true);

        const session = await links.create(owner, {
          ...base,
          scope: { workspaceId: "ws_01", sessionId: "sess_01" },
        });
        const viaSession = await links.accept(
          { linkId: session.linkId, secret: session.secret },
          "ip",
        );
        expect(payloadOf(viaSession.assertion).scp).toEqual([
          "ws:ws_01",
          "sess:sess_01",
          "ro",
        ]);
        const listed = await links.list(owner);
        expect(listed.map((row) => row.scope)).toEqual([
          { workspaceId: "ws_01", readOnly: true },
          { workspaceId: "ws_01", sessionId: "sess_01", readOnly: true },
        ]);

        // owner 的断言与令牌没有范围。
        const mine = await sources.assertion(owner, SOURCE_ID, undefined);
        expect(payloadOf(mine.assertion).scp).toBeUndefined();
        expect(payloadOf(mine.relayToken).ro).toBeUndefined();
        // 协议 1.3：中继令牌的 dev 就是断言里的设备 id（owner 与访客都带）。
        const device = (
          payloadOf(mine.assertion).device as { deviceId: string }
        ).deviceId;
        expect(payloadOf(mine.relayToken).dev).toBe(device);
        expect(payloadOf(again.relayToken).dev).toBe(
          (payloadOf(again.assertion).device as { deviceId: string }).deviceId,
        );
      });

      it("缺省 scope = 旧行为：断言没有 scp、中继令牌没有 ro，摘要没有 scope；可写的工作空间范围不带 ro", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const links = createLinks(h.context, sources);
        const base = {
          kind: "source_invite" as const,
          sourceId: SOURCE_ID,
          invitationId: "inv",
          label: "",
          role: "editor",
          expiresAtMs: h.clock.now + 60_000,
        };
        for (const scope of [undefined, {}, { readOnly: false }]) {
          const created = await links.create(owner, { ...base, scope });
          expect((await links.get(created.linkId)).scope).toBeUndefined();
          const accepted = await links.accept(
            { linkId: created.linkId, secret: created.secret },
            "ip",
          );
          const assertion = payloadOf(accepted.assertion);
          expect("scp" in assertion).toBe(false);
          expect("role" in assertion).toBe(false);
          expect("ro" in payloadOf(accepted.relayToken)).toBe(false);
        }
        const writable = await links.create(owner, {
          ...base,
          scope: { workspaceId: "ws_02", readOnly: false },
        });
        const accepted = await links.accept(
          { linkId: writable.linkId, secret: writable.secret },
          "ip",
        );
        expect(payloadOf(accepted.assertion).scp).toEqual(["ws:ws_02"]);
        expect("ro" in payloadOf(accepted.relayToken)).toBe(false);
        expect((await links.list(owner)).at(-1)?.scope).toEqual({
          workspaceId: "ws_02",
        });
      });

      it("会话范围要工作空间、不能可写：bad_request", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const links = createLinks(h.context, sources);
        const base = {
          kind: "source_invite" as const,
          sourceId: SOURCE_ID,
          invitationId: "inv",
          label: "",
          role: "viewer",
          expiresAtMs: h.clock.now + 60_000,
        };
        expect(
          await codeOf(() =>
            links.create(owner, { ...base, scope: { sessionId: "s" } }),
          ),
        ).toBe("bad_request");
        expect(
          await codeOf(() =>
            links.create(owner, {
              ...base,
              scope: { workspaceId: "w", sessionId: "s", readOnly: false },
            }),
          ),
        ).toBe("bad_request");
      });

      it("撤销源：关隧道、断开、发事件", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        await sources.revoke(owner, SOURCE_ID);
        expect(h.closedTunnels).toEqual([SOURCE_ID]);
        expect(h.disconnects).toEqual([{ sourceId: SOURCE_ID }]);
        expect(h.events.at(-1)).toEqual({
          type: "sourceRevoked",
          sourceId: SOURCE_ID,
        });
        expect(await isRevokedJti(h.context, `t.${SOURCE_ID}.x`, 0)).toBe(true);
      });
    });

    describe("me-stream 扇出", () => {
      it("票一次性；owner 收全部、访客只收链接的源；在线事件只转仍登记的源", async () => {
        const h = harness();
        const owner = await ownerOf(h);
        const { sources } = await registered(h, owner);
        const stream = createMeStreamCore(h.context);
        const { ticket } = stream.ticket(owner);
        expect(await stream.redeem(ticket)).toEqual(owner);
        expect(await stream.redeem(ticket)).toBeNull();
        expect(await stream.redeem(undefined)).toBeNull();

        const links = createLinks(h.context, sources);
        const created = await links.create(owner, {
          kind: "source_invite",
          sourceId: SOURCE_ID,
          invitationId: "inv",
          label: "",
          role: "viewer",
          expiresAtMs: h.clock.now + 60_000,
        });
        const accepted = await links.accept(
          { linkId: created.linkId, secret: created.secret },
          "ip",
        );
        const guest = await createAuthenticator(h.context).authenticate(
          `Bearer ${accepted.guestSession.accessToken}`,
        );
        const toOwner: string[] = [];
        const toGuest: string[] = [];
        stream.attach(owner, (event) => toOwner.push(event.type));
        stream.attach(guest, (event) => toGuest.push(event.type));
        h.context.emit({ type: "sourceRevoked", sourceId: "f".repeat(32) });
        h.presence.publish({
          type: "sourceOnline",
          sourceId: SOURCE_ID,
          coreVersion: "1",
          atMs: 1,
        });
        h.presence.publish({
          type: "sourceOnline",
          sourceId: "e".repeat(32),
          coreVersion: "1",
          atMs: 1,
        });
        // 扇出要查一次库：等到预期的事件到齐（库在网络另一端时不止 10 毫秒），再多等一会儿确认
        // 未登记的源始终没有转出来。
        const expected = () => {
          expect(toOwner).toEqual(["sourceRevoked", "sourceOnline"]);
          expect(toGuest).toEqual(["sourceOnline"]);
        };
        await vi.waitFor(expected, { timeout: 2_000, interval: 5 });
        await new Promise((resolve) => setTimeout(resolve, 20));
        expected();
        expect(await stream.stillValid(guest)).toBe(true);
      });
    });
  });
}
