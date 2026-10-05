/**
 * 身份算法参数（协议包设计 §8）。从 Armadra core 的 `identity/policy.ts`、
 * `passwords.ts`、`tokens.ts`、`throttle.ts`、`service.ts`、`transport.ts` 抄来；
 * core 的单测对这里断言相等，cloud / relay 的实现直接 import。改任何一个值都要
 * 两仓同步，并重新生成 vectors.json（`node scripts/gen-vectors.mjs`）。
 */
export const PASSWORD_POLICY = {
  /** 码点数下限的缺省值；可配 floor–ceiling。 */
  minLengthDefault: 12,
  minLengthFloor: 10,
  minLengthCeiling: 64,
  /** UTF-8 字节上限（`password_too_long`）。 */
  maxBytes: 1024,
  rejectCommon: true,
  rejectContainsName: true,
  /** 短于这个码点数的名字不参与 `password_contains_name`。 */
  minNameLength: 3,
} as const;

export const SCRYPT = {
  cost: 1 << 15,
  block: 8,
  parallel: 1,
  length: 32,
  saltBytes: 16,
} as const;

/** 令牌哈希：`sha256(TOKEN_HASH_PREFIX + kind + "\0" + value)`。 */
export const TOKEN_HASH_PREFIX = "armadra/identity/v1/";

export const TOKEN_HASH_KINDS = [
  "bootstrap",
  "access",
  "refresh",
  "csrf",
  "reset",
] as const;
export type TokenHashKind = (typeof TOKEN_HASH_KINDS)[number];

export const THROTTLE = {
  lockThreshold: 5,
  lockBaseMs: 60_000,
  lockMaxMs: 15 * 60_000,
  failureWindowMs: 60 * 60_000,
  ipBucketCapacity: 20,
  ipBucketWindowMs: 60_000,
} as const;

export const SESSION = {
  accessTtlMs: 15 * 60_000,
  refreshTtlMs: 30 * 24 * 60 * 60_000,
  refreshLeadMs: 2 * 60_000,
  wsTicketTtlMs: 30_000,
} as const;

export const ASSERTION = {
  ttlMs: 5 * 60_000,
  replayWindowMs: 10 * 60_000,
  skewMs: 5 * 60_000,
} as const;

export const RELAY_TOKEN = { ttlMs: 60 * 60_000 } as const;
export const TUNNEL_TOKEN = { ttlMs: 60 * 60_000 } as const;
export const SOURCE_JWS = { ttlMs: 60_000 } as const;
export const REGISTRATION_TOKEN = { ttlMs: 10 * 60_000 } as const;

export const DEVICE_CODE = {
  ttlMs: 10 * 60_000,
  intervalMs: 5_000,
  /** 没有元音与易混字符，`XXXX-XXXX`。 */
  userCodeAlphabet: "BCDFGHJKLMNPQRSTVWXZ",
  userCodeLength: 8,
} as const;

/** 锁定时长：第 `lockThreshold` 次失败起锁 `lockBaseMs`，之后每次翻倍，封顶 `lockMaxMs`。 */
export function lockDurationMs(failures: number): number {
  if (failures < THROTTLE.lockThreshold) return 0;
  const doublings = Math.min(failures - THROTTLE.lockThreshold, 16);
  return Math.min(THROTTLE.lockBaseMs * 2 ** doublings, THROTTLE.lockMaxMs);
}
