/**
 * 验 EdDSA compact JWS 并校验声明（协议包 §4）：拆包 → `typ` → 按 `kid` 取钥验签 →
 * zod 校验声明 → `iss` / `aud` → 时间 → `jti` 未撤销。顺序保证：签名没过之前不看声明内容。
 *
 * 失败原因是本模块内部的枚举（不是线上错误码）；调用方映射到注册表里的码。
 */
import { verify, type KeyObject } from "node:crypto";

import {
  checkAudience,
  checkTimes,
  splitCompactJws,
} from "@armadra/platform-protocol/assertion";
import type { z } from "zod";

export type JwtFailure =
  | "malformed"
  | "wrong_type"
  | "unknown_key"
  | "bad_signature"
  | "bad_claims"
  | "audience_mismatch"
  | "issuer_mismatch"
  | "expired"
  | "not_yet_valid"
  | "revoked";

export type JwtResult<T> =
  | { ok: true; claims: T }
  | { ok: false; failure: JwtFailure };

export interface VerifyJwtOptions<S extends z.ZodType> {
  typ: string;
  schema: S;
  expectedAud: string;
  expectedIss: string;
  nowMs: number;
  key: (kid: string) => Promise<KeyObject | null>;
  isRevoked: (jti: string) => Promise<boolean>;
}

export async function verifyJwt<S extends z.ZodType>(
  token: string,
  options: VerifyJwtOptions<S>,
): Promise<JwtResult<z.infer<S>>> {
  let parts: ReturnType<typeof splitCompactJws>;
  try {
    parts = splitCompactJws(token);
  } catch {
    return { ok: false, failure: "malformed" };
  }
  if (parts.header.typ !== options.typ)
    return { ok: false, failure: "wrong_type" };
  const key = await options.key(parts.header.kid);
  if (!key) return { ok: false, failure: "unknown_key" };
  let valid = false;
  try {
    valid = verify(null, parts.signingInput, key, parts.signature);
  } catch {
    valid = false;
  }
  if (!valid) return { ok: false, failure: "bad_signature" };
  const parsed = options.schema.safeParse(parts.payload);
  if (!parsed.success) return { ok: false, failure: "bad_claims" };
  const claims = parsed.data as {
    aud: string;
    iss: string;
    exp: number;
    iat: number;
    nbf?: number;
    jti: string;
  };
  const check = {
    nowMs: options.nowMs,
    expectedAud: options.expectedAud,
    expectedIss: options.expectedIss,
  };
  const audience = checkAudience(claims, check);
  if (audience) return { ok: false, failure: audience.code };
  const times = checkTimes(claims, check);
  if (times) return { ok: false, failure: times.code };
  if (await options.isRevoked(claims.jti))
    return { ok: false, failure: "revoked" };
  return { ok: true, claims: parsed.data };
}
