/**
 * 账号与登录（中继设计 §6.2，平台设计 §17.3）。
 *
 * 登录：按 IP 限流（`login.ip`，20 次 / 分钟）→ 按账号名找账号 → 锁定检查 → scrypt 比对（定时
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

import { ControlError } from "./errors.ts";
import {
  burnPasswordCheck,
  hashPassword,
  verifyPassword,
} from "./passwords.ts";
import type { ControlContext, Lockout } from "./ports.ts";
import { createDevice, sessionFor } from "./sessions.ts";

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

  return {
    async login(input: LoginInput, ip: string): Promise<Session> {
      const byIp = await context.limits.hit("login.ip", ip);
      if (!byIp.allowed)
        throw new ControlError("rate_limited", {
          retryAfterMs: byIp.retryAfterMs,
        });
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
        await context.ports.accounts.update(
          context.tenantId,
          accountId,
          (draft) => {
            if (draft) Object.assign(draft, nextLockout(draft, nowMs));
          },
        );
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

/** 改口令：重新派生、清锁、撤销账号本人的全部设备（访客不受影响，由链接管）。 */
export async function changePassword(
  context: Pick<ControlContext, "tenantId" | "ports" | "passwordPolicy">,
  name: string,
  password: string,
  nowMs: number,
): Promise<void> {
  checkPassword(context, password, name);
  const record = await hashPassword(password);
  const account = await context.ports.accounts.findByName(
    context.tenantId,
    name,
  );
  const found =
    account !== null &&
    (await context.ports.accounts.resetPassword(
      context.tenantId,
      account.accountId,
      record,
      nowMs,
    ));
  if (!found) throw new ControlError("not_found");
}
