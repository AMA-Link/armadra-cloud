/**
 * 账号与登录（中继设计 §6.2，平台设计 §17.3）。
 *
 * 登录：按 IP 限流（`login.ip`，20 次 / 分钟）→ 人机挑战（配置了才有，cloud-api §16；失败不计入
 * 失败次数）→ 按账号名找账号 → 锁定检查 → scrypt 比对（定时
 * 安全）→ 失败计数 / 锁定 → 成功清零 → 建设备 → 签会话。
 *
 * 锁定与 core `identity/throttle.ts` 同一算法（常量来自 identity-vectors 的 `THROTTLE`）：
 * 连续 5 次失败锁 1 分钟，此后每多一次翻倍，封顶 15 分钟；最后一次失败一小时后清零；
 * 锁着的时候不比对口令，直接答 `account_locked` 带 `retryAfterMs`。账号名不对也计数（只在
 * 内存），不泄露账号名是否存在。
 *
 * 改口令（cloud-api §18）：账号本人的会话 + 旧口令。按 IP 限流（与登录同一个桶）→ 挑战（scope
 * 含 `auth.changePassword` 才要）→ 锁定检查 → 新口令策略 → 比对旧口令（失败计入同一个锁定计数）→
 * 换口令记录、清锁、撤销本人除当前设备以外的全部设备 → 当前设备的刷新令牌换新。`personal passwd`
 * 走同一个 {@link setPassword}，只是不留设备。
 */
import type {
  ChallengeToken,
  DeviceInput,
  Session,
} from "@armadra/platform-protocol/cloud-api";
import {
  lockDurationMs,
  THROTTLE,
} from "@armadra/platform-protocol/identity-vectors";

import { ControlError } from "./errors.ts";
import {
  burnPasswordCheck,
  hashPassword,
  verifyPassword,
} from "./passwords.ts";
import type { ControlContext, Lockout, ResetPasswordOptions } from "./ports.ts";
import { refreshHash, randomToken } from "./secrets.ts";
import { createDevice, sessionFor, type Caller } from "./sessions.ts";

/** 一次失败之后的计数与锁（纯函数）。 */
export function nextLockout(previous: Lockout, nowMs: number): Lockout {
  const stale =
    previous.lockedUntilMs <= nowMs &&
    nowMs - previous.lastFailureAtMs > THROTTLE.failureWindowMs;
  const failures = (stale ? 0 : previous.failures) + 1;
  const duration = lockDurationMs(failures);
  return {
    failures,
    lockedUntilMs: duration > 0 ? nowMs + duration : 0,
    lastFailureAtMs: nowMs,
  };
}

const MAX_UNKNOWN_NAMES = 1_000;

export interface LoginInput {
  account: string;
  password: string;
  device: DeviceInput;
  challenge?: ChallengeToken;
}

export interface ChangePasswordInput {
  /** 旧口令。 */
  password: string;
  newPassword: string;
  challenge?: ChallengeToken;
}

export function createAccounts(context: ControlContext) {
  /** 不存在的账号名的失败计数，只在内存。 */
  const unknown = new Map<string, Lockout>();

  const lockedFor = (lockout: Lockout, nowMs: number) =>
    lockout.lockedUntilMs > nowMs ? lockout.lockedUntilMs - nowMs : 0;

  async function loginUnknown(name: string, password: string): Promise<never> {
    const nowMs = context.now();
    const previous = unknown.get(name) ?? {
      failures: 0,
      lockedUntilMs: 0,
      lastFailureAtMs: 0,
    };
    const wait = lockedFor(previous, nowMs);
    if (wait > 0)
      throw new ControlError("account_locked", { retryAfterMs: wait });
    await burnPasswordCheck(password);
    if (unknown.size >= MAX_UNKNOWN_NAMES) unknown.clear();
    unknown.set(name, nextLockout(previous, context.now()));
    throw new ControlError("credentials_invalid");
  }

  /** 失败一次：记进账号的锁定计数。 */
  const recordFailure = (accountId: string, nowMs: number) =>
    context.ports.accounts.update(context.tenantId, accountId, (draft) => {
      if (draft) Object.assign(draft, nextLockout(draft, nowMs));
    });

  return {
    /**
     * 改口令（cloud-api §18）：成功答当前设备换了刷新令牌的新会话；其余本人设备全部撤销。
     */
    async changePassword(
      caller: Caller,
      input: ChangePasswordInput,
      ip: string,
    ): Promise<Session> {
      if (caller.role !== "owner") throw new ControlError("forbidden");
      const byIp = await context.limits.hit("login.ip", ip);
      if (!byIp.allowed)
        throw new ControlError("rate_limited", {
          retryAfterMs: byIp.retryAfterMs,
        });
      await context.challenge?.check(
        "auth.changePassword",
        input.challenge,
        ip,
      );
      const account = await context.ports.accounts.get(
        context.tenantId,
        caller.subject,
      );
      if (!account) throw new ControlError("unauthenticated");
      const wait = lockedFor(account, context.now());
      if (wait > 0)
        throw new ControlError("account_locked", { retryAfterMs: wait });
      checkPassword(context, input.newPassword, account.name);
      if (!(await verifyPassword(input.password, account.password))) {
        await recordFailure(account.accountId, context.now());
        throw new ControlError("credentials_invalid");
      }
      const nowMs = context.now();
      await setPassword(
        context,
        account.accountId,
        account.name,
        input.newPassword,
        nowMs,
        {
          keepDeviceId: caller.deviceId,
        },
      );
      // 当前设备的刷新令牌也换新：旧的哪怕泄露过也就此作废（不当作重放，不撤销这台）。
      const rotated = await context.ports.devices.update(
        context.tenantId,
        caller.deviceId,
        (device) => {
          if (!device || device.revokedAtMs !== null) return null;
          const next = `${device.deviceId}.${randomToken(32)}`;
          device.refreshHash = refreshHash(next);
          device.previousRefreshHash = null;
          device.rotation += 1;
          device.lastSeenAtMs = nowMs;
          return { device: { ...device }, refreshToken: next };
        },
      );
      if (!rotated) throw new ControlError("unauthenticated");
      return sessionFor(context, rotated.device, rotated.refreshToken);
    },

    async login(input: LoginInput, ip: string): Promise<Session> {
      const byIp = await context.limits.hit("login.ip", ip);
      if (!byIp.allowed)
        throw new ControlError("rate_limited", {
          retryAfterMs: byIp.retryAfterMs,
        });
      await context.challenge?.check("auth.login", input.challenge, ip);
      const account = await context.ports.accounts.findByName(
        context.tenantId,
        input.account,
      );
      if (!account)
        return loginUnknown(input.account.slice(0, 128), input.password);
      const { accountId } = account;
      const wait = lockedFor(account, context.now());
      if (wait > 0)
        throw new ControlError("account_locked", { retryAfterMs: wait });
      const ok = await verifyPassword(input.password, account.password);
      const nowMs = context.now();
      if (!ok) {
        await recordFailure(accountId, nowMs);
        throw new ControlError("credentials_invalid");
      }
      await context.ports.accounts.update(
        context.tenantId,
        accountId,
        (draft) => {
          if (draft && (draft.failures !== 0 || draft.lockedUntilMs !== 0)) {
            draft.failures = 0;
            draft.lockedUntilMs = 0;
          }
        },
      );
      const created = await createDevice(context, {
        subject: accountId,
        device: input.device,
        nowMs,
      });
      return sessionFor(context, created.device, created.refreshToken);
    },
  };
}

/** 口令策略不过就抛（码与 core 同名）。 */
export function checkPassword(
  context: Pick<ControlContext, "passwordPolicy">,
  password: string,
  name: string,
): void {
  const rule = context.passwordPolicy(password, [name]);
  if (rule) throw new ControlError(rule);
}

/**
 * 换口令的唯一实现（API 与 `personal passwd` 共用）：过策略、重新派生、清锁，撤销账号本人的
 * 设备（`keepDeviceId` 那台除外；访客不受影响，由链接管）。账号不存在答 `not_found`。
 */
export async function setPassword(
  context: Pick<ControlContext, "tenantId" | "ports" | "passwordPolicy">,
  accountId: string,
  name: string,
  password: string,
  nowMs: number,
  options: ResetPasswordOptions = {},
): Promise<void> {
  checkPassword(context, password, name);
  const record = await hashPassword(password);
  const found = await context.ports.accounts.resetPassword(
    context.tenantId,
    accountId,
    record,
    nowMs,
    options,
  );
  if (!found) throw new ControlError("not_found");
}

/** 运维改口令（`personal passwd`）：按账号名找到账号，撤销本人的全部设备。 */
export async function changePassword(
  context: Pick<ControlContext, "tenantId" | "ports" | "passwordPolicy">,
  name: string,
  password: string,
  nowMs: number,
): Promise<void> {
  checkPassword(context, password, name);
  const account = await context.ports.accounts.findByName(
    context.tenantId,
    name,
  );
  if (!account) throw new ControlError("not_found");
  await setPassword(context, account.accountId, account.name, password, nowMs);
}
