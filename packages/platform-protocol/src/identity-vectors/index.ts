import data from "./vectors.json" with { type: "json" };

import type { TokenHashKind } from "./constants.ts";

export {
  ASSERTION,
  DEVICE_CODE,
  lockDurationMs,
  PASSWORD_POLICY,
  REGISTRATION_TOKEN,
  RELAY_TOKEN,
  SCRYPT,
  SESSION,
  SOURCE_JWS,
  THROTTLE,
  TOKEN_HASH_KINDS,
  TOKEN_HASH_PREFIX,
  TUNNEL_TOKEN,
  type TokenHashKind,
} from "./constants.ts";

export interface IdentityVectors {
  /** `scrypt(password, hex(salt), SCRYPT)` 的输出 hex。 */
  scrypt: { password: string; salt: string; hex: string }[];
  /** `sha256(TOKEN_HASH_PREFIX + kind + "\0" + value)` 的 hex。 */
  tokenHash: { kind: TokenHashKind; value: string; hex: string }[];
}

/** 由 `scripts/gen-vectors.mjs` 用 `node:crypto` 生成并提交。 */
export const vectors: IdentityVectors = data as IdentityVectors;
