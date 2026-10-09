/**
 * 控制面的随机值与哈希。存储里只存哈希，明文只在签发那一刻返回一次。
 *
 * - 刷新令牌：`sha256("armadra/identity/v1/refresh\0" + 值)`，与 core 的令牌哈希同一拼法
 *   （协议包 identity-vectors 的 `TOKEN_HASH_PREFIX`）。
 * - 注册令牌、分享链接秘密、流票据：同一拼法，前缀换成本仓自己的 `armadra/relay/v1/`。
 */
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";

import { TOKEN_HASH_PREFIX } from "@armadra/platform-protocol/identity-vectors";

export type RelaySecretKind = "registration" | "link" | "ticket";

const RELAY_HASH_PREFIX = "armadra/relay/v1/";

/** `bytes` 字节随机数的 base64url（无填充）。 */
export function randomToken(bytes = 32): string {
  return randomBytes(bytes).toString("base64url");
}

/** `bytes` 字节随机数的小写 hex。 */
export function randomHex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

export function refreshHash(value: string): string {
  return createHash("sha256")
    .update(`${TOKEN_HASH_PREFIX}refresh\u0000${value}`, "utf8")
    .digest("hex");
}

export function secretHash(kind: RelaySecretKind, value: string): string {
  return createHash("sha256")
    .update(`${RELAY_HASH_PREFIX}${kind}\u0000${value}`, "utf8")
    .digest("hex");
}

/** 两个 hex 摘要定时安全地比较；长度不同直接假。 */
export function sameHex(left: string, right: string): boolean {
  const a = Buffer.from(left, "hex");
  const b = Buffer.from(right, "hex");
  return a.length === b.length && a.length > 0 && timingSafeEqual(a, b);
}
