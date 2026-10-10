import { readFileSync } from "node:fs";

import {
  changePassword,
  createAccounts,
  nextLockout,
} from "@armadra/control-core/accounts";
import { ControlError } from "@armadra/control-core/errors";
import {
  createAuthenticator,
  refresh,
  revokeDevice,
} from "@armadra/control-core/sessions";
import { sessionSchema } from "@armadra/platform-protocol/cloud-api";
import {
  lockDurationMs,
  SCRYPT,
  THROTTLE,
  vectors,
} from "@armadra/platform-protocol/identity-vectors";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  DEVICE,
  PASSWORD,
  personalHarness,
  TEST_STORE_KINDS,
  type Harness,
} from "../../test/personal.ts";
import {
  commonPasswordsFile,
  hashPassword,
  passwordViolation,
  verifyPassword,
} from "./passwords.ts";

describe.each(TEST_STORE_KINDS)("%s 存储", (store) => {
  let h: Harness;
  beforeAll(async () => {
    h = await personalHarness({ store });
  });
  afterAll(() => h.cleanup());

  const codeOf = async (promise: Promise<unknown> | (() => unknown)) => {
    try {
      await (typeof promise === "function" ? promise() : promise);
    } catch (error) {
      return error instanceof ControlError ? error.code : String(error);
    }
    return "ok";
  };

  describe("口令", () => {
    it.each(vectors.scrypt.map((row) => [row.password, row] as const))(
      "scrypt 对 identity-vectors：%s",
      async (_password, row) => {
        const record = await hashPassword(
          row.password,
          Buffer.from(row.salt, "hex"),
        );
        expect(Buffer.from(record.hash, "base64").toString("hex")).toBe(
          row.hex,
        );
        expect(record).toMatchObject({
          kdf: "scrypt",
          cost: SCRYPT.cost,
          block: SCRYPT.block,
          parallel: SCRYPT.parallel,
          length: SCRYPT.length,
        });
        expect(await verifyPassword(row.password, record)).toBe(true);
        expect(await verifyPassword(`${row.password}x`, record)).toBe(false);
      },
    );

    it("策略按顺序报第一条不过的规则，码与 core 同名", () => {
      expect(passwordViolation("short-pw")).toBe("password_too_short");
      expect(passwordViolation("x".repeat(1025))).toBe("password_too_long");
      expect(passwordViolation("my-name-is-alice-ok", ["Alice"])).toBe(
        "password_contains_name",
      );
      expect(passwordViolation("my-name-is-al-ok", ["al"])).toBeUndefined();
      expect(passwordViolation("password1234")).toBe("password_too_common");
      expect(passwordViolation(PASSWORD, ["me"])).toBeUndefined();
    });

    it("常见口令表与 core 那份同格式（# 注释、小写、≥10 字）", () => {
      const text = readFileSync(commonPasswordsFile(), "utf8");
      const entries = text
        .split(/\r?\n/)
        .filter((line) => line.trim() !== "" && !line.startsWith("#"));
      expect(entries.length).toBe(10_000);
      expect(entries.every((line) => [...line].length >= 10)).toBe(true);
    });
  });

  describe("锁定序列（THROTTLE）", () => {
    it("5 次起锁 1 分钟、每次翻倍、15 分钟封顶、一小时无失败清零", () => {
      let lockout = { failures: 0, lockedUntilMs: 0, lastFailureAtMs: 0 };
      let at = 1_000_000;
      const durations: number[] = [];
      for (let index = 0; index < 10; index += 1) {
        lockout = nextLockout(lockout, at);
        durations.push(
          lockout.lockedUntilMs === 0 ? 0 : lockout.lockedUntilMs - at,
        );
        at = Math.max(at + 1, lockout.lockedUntilMs);
      }
      expect(durations).toEqual([
        0, 0, 0, 0, 60_000, 120_000, 240_000, 480_000, 900_000, 900_000,
      ]);
      expect(durations.map((_, index) => lockDurationMs(index + 1))).toEqual(
        durations,
      );
      const later = nextLockout(
        lockout,
        lockout.lockedUntilMs + THROTTLE.failureWindowMs + 1,
      );
      expect(later.failures).toBe(1);
    });

    it("登录：错 5 次后 account_locked（带 retryAfterMs，不再比对口令），到期后对的口令能进，计数清零", async () => {
      const accounts = createAccounts(h.context);
      const login = (password: string, ip = "198.51.100.1") =>
        accounts.login({ account: "me", password, device: DEVICE }, ip);
      for (let index = 0; index < 5; index += 1)
        expect(await codeOf(login("wrong-password-123"))).toBe(
          "credentials_invalid",
        );
      let locked: ControlError | undefined;
      try {
        await login(PASSWORD);
      } catch (error) {
        locked = error as ControlError;
      }
      expect(locked?.code).toBe("account_locked");
      expect(locked?.retryAfterMs).toBe(60_000);
      h.clock.now += 60_001;
      const session = await login(PASSWORD);
      expect(sessionSchema.parse(session)).toBeTruthy();
      expect((await h.context.store.snapshot()).account).toMatchObject({
        failures: 0,
        lockedUntilMs: 0,
      });
    });

    it("账号名不对：同样答 credentials_invalid，也会被锁（只在内存），不泄露账号名", async () => {
      const accounts = createAccounts(h.context);
      for (let index = 0; index < 5; index += 1)
        expect(
          await codeOf(
            accounts.login(
              { account: "nobody", password: PASSWORD, device: DEVICE },
              "198.51.100.2",
            ),
          ),
        ).toBe("credentials_invalid");
      expect(
        await codeOf(
          accounts.login(
            { account: "nobody", password: PASSWORD, device: DEVICE },
            "198.51.100.2",
          ),
        ),
      ).toBe("account_locked");
    });

    it("按 IP 限流：同一 IP 每分钟 20 次", async () => {
      const accounts = createAccounts(h.context);
      const codes: string[] = [];
      for (let index = 0; index < 21; index += 1)
        codes.push(
          await codeOf(
            accounts.login(
              { account: `x${index}`, password: "p", device: DEVICE },
              "203.0.113.77",
            ),
          ),
        );
      expect(
        codes.slice(0, 20).every((code) => code === "credentials_invalid"),
      ).toBe(true);
      expect(codes[20]).toBe("rate_limited");
    });
  });

  describe("会话", () => {
    it("刷新旋转；旧令牌再来 = 重放，整台设备作废", async () => {
      const accounts = createAccounts(h.context);
      const auth = createAuthenticator(h.context);
      const first = await accounts.login(
        { account: "me", password: PASSWORD, device: DEVICE },
        "198.51.100.9",
      );
      const caller = await auth.authenticate(`Bearer ${first.accessToken}`);
      expect(caller).toMatchObject({
        subject: "acct:me",
        role: "owner",
        linkId: null,
      });
      const second = await refresh(h.context, first.refreshToken as string);
      expect(second.refreshToken).not.toBe(first.refreshToken);
      expect(second.device.deviceId).toBe(first.device.deviceId);
      expect(
        await codeOf(() => refresh(h.context, first.refreshToken as string)),
      ).toBe("session_revoked");
      // 重放之后连新令牌与访问令牌都作废。
      expect(
        await codeOf(() => refresh(h.context, second.refreshToken as string)),
      ).toBe("session_revoked");
      expect(
        await codeOf(auth.authenticate(`Bearer ${second.accessToken}`)),
      ).toBe("unauthenticated");
    });

    it("登出 / 改口令即时让访问令牌失效；状态文件里只有刷新令牌的哈希", async () => {
      const accounts = createAccounts(h.context);
      const auth = createAuthenticator(h.context);
      const session = await accounts.login(
        { account: "me", password: PASSWORD, device: DEVICE },
        "198.51.100.10",
      );
      const text = h.persisted();
      expect(text).not.toContain(session.refreshToken as string);
      expect(text).not.toContain(session.accessToken);
      expect(await revokeDevice(h.context, session.device.deviceId)).toBe(true);
      expect(
        await codeOf(auth.authenticate(`Bearer ${session.accessToken}`)),
      ).toBe("unauthenticated");
      const other = await accounts.login(
        { account: "me", password: PASSWORD, device: DEVICE },
        "198.51.100.10",
      );
      await changePassword(
        h.context,
        "me",
        "saffron-glacier-meadow-77",
        h.clock.now,
      );
      expect(
        await codeOf(auth.authenticate(`Bearer ${other.accessToken}`)),
      ).toBe("unauthenticated");
      expect(
        await codeOf(
          accounts.login(
            { account: "me", password: PASSWORD, device: DEVICE },
            "198.51.100.11",
          ),
        ),
      ).toBe("credentials_invalid");
      await changePassword(h.context, "me", PASSWORD, h.clock.now);
    });

    it("访问令牌过期（15 分钟 + 5 分钟偏差）后 unauthenticated", async () => {
      const accounts = createAccounts(h.context);
      const auth = createAuthenticator(h.context);
      const session = await accounts.login(
        { account: "me", password: PASSWORD, device: DEVICE },
        "198.51.100.12",
      );
      h.clock.now += 21 * 60_000;
      expect(
        await codeOf(auth.authenticate(`Bearer ${session.accessToken}`)),
      ).toBe("unauthenticated");
      expect(await codeOf(auth.authenticate(undefined))).toBe(
        "unauthenticated",
      );
    });
  });
});
