/**
 * 账号与登录（中继设计 §6.2，平台设计 §17.3）。只有一个账号。
 *
 * 登录：按 IP 限流（`login.ip`，20 次 / 分钟）→ 账号名相等 → 锁定检查 → scrypt 比对（定时
 * 安全）→ 失败计数 / 锁定 → 成功清零 → 建设备 → 签会话。
 *
 * 锁定与 core `identity/throttle.ts` 同一算法（常量来自 identity-vectors 的 `THROTTLE`）：
 * 连续 5 次失败锁 1 分钟，此后每多一次翻倍，封顶 15 分钟；最后一次失败一小时后清零；
 * 锁着的时候不比对口令，直接答 `account_locked` 带 `retryAfterMs`。账号名不对也计数（只在
 * 内存），不泄露账号名是否存在。
 */
import type {
  DeviceInput,
  Session,
} from "@armadra/platform-protocol/cloud-api";
import {
  lockDurationMs,
  THROTTLE,
} from "@armadra/platform-protocol/identity-vectors";

import {
  ownerSubject,
  PersonalError,
  type PersonalContext,
} from "./context.ts";
import {
  burnPasswordCheck,
  hashPassword,
  passwordViolation,
  verifyPassword,
} from "./passwords.ts";
import { createDevice, sessionFor } from "./sessions.ts";
import type { State } from "./state.ts";

interface Lockout {
  failures: number;
  lockedUntilMs: number;
  lastFailureAtMs: number;
}

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
}

export function createAccounts(context: PersonalContext) {
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
      throw new PersonalError("account_locked", { retryAfterMs: wait });
    await burnPasswordCheck(password);
    if (unknown.size >= MAX_UNKNOWN_NAMES) unknown.clear();
    unknown.set(name, nextLockout(previous, context.now()));
    throw new PersonalError("credentials_invalid");
  }

  return {
    async login(input: LoginInput, ip: string): Promise<Session> {
      const byIp = await context.control.limits.hit("login.ip", ip);
      if (!byIp.allowed)
        throw new PersonalError("rate_limited", {
          retryAfterMs: byIp.retryAfterMs,
        });
      const account = context.store.current.account;
      if (input.account !== account.name)
        return loginUnknown(input.account.slice(0, 128), input.password);
      const wait = lockedFor(account, context.now());
      if (wait > 0)
        throw new PersonalError("account_locked", { retryAfterMs: wait });
      const ok = await verifyPassword(input.password, account.password);
      const nowMs = context.now();
      if (!ok) {
        context.store.update((draft) => {
          Object.assign(draft.account, nextLockout(draft.account, nowMs));
        });
        throw new PersonalError("credentials_invalid");
      }
      const created = context.store.update((draft) => {
        if (draft.account.failures !== 0 || draft.account.lockedUntilMs !== 0) {
          draft.account.failures = 0;
          draft.account.lockedUntilMs = 0;
        }
        return createDevice(draft, {
          subject: ownerSubject(draft.account.name),
          device: input.device,
          nowMs,
        });
      });
      return sessionFor(context, created.device, created.refreshToken);
    },
  };
}

/** 口令策略不过就抛（CLI 用；码与 core 同名）。 */
export function checkPassword(password: string, name: string): void {
  const rule = passwordViolation(password, [name]);
  if (rule) throw new PersonalError(rule);
}

/** 改口令：重新派生、清锁、撤销账号本人的全部设备（访客不受影响，由链接管）。 */
export async function changePassword(
  update: (fn: (draft: State) => void) => void,
  name: string,
  password: string,
  nowMs: number,
): Promise<void> {
  checkPassword(password, name);
  const record = await hashPassword(password);
  update((draft) => {
    draft.account.password = record;
    draft.account.updatedAtMs = nowMs;
    draft.account.failures = 0;
    draft.account.lockedUntilMs = 0;
    const subject = ownerSubject(draft.account.name);
    for (const device of draft.devices)
      if (device.subject === subject && device.revokedAtMs === null)
        device.revokedAtMs = nowMs;
  });
}
