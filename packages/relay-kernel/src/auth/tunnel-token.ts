/**
 * 隧道令牌（协议包 §4.4、中继设计 §3.1）：校验 `typ`、签名、`aud === "relay-tunnel"`、
 * `iss`、时间、`jti` 未撤销、`sub === hello.sourceId`，然后取出 `cnf.jwk`——握手第 3 步
 * 用它验源的签名。失败映射到握手的 reject 码。
 */
import { createPublicKey, type KeyObject } from "node:crypto";

import { verifyJwt, type JwtFailure } from "@armadra/cloud-shared/jwt";
import {
  AUDIENCES,
  TOKEN_TYPES,
  tunnelTokenClaimsSchema,
  type TunnelTokenClaims,
} from "@armadra/platform-protocol/assertion";
import type { TunnelRejectCode } from "@armadra/platform-protocol/tunnel";

import { jwtOptions, type AuthContext } from "./context.ts";

export type TunnelTokenResult =
  | { ok: true; claims: TunnelTokenClaims; sourceKey: KeyObject }
  | {
      ok: false;
      reject: TunnelRejectCode;
      failure: JwtFailure | "subject_mismatch" | "bad_cnf";
    };

export async function verifyTunnelToken(
  context: AuthContext,
  token: string,
  sourceId: string,
): Promise<TunnelTokenResult> {
  const result = await verifyJwt(token, {
    ...jwtOptions(context),
    typ: TOKEN_TYPES.tunnel,
    schema: tunnelTokenClaimsSchema,
    expectedAud: AUDIENCES.tunnel,
  });
  if (!result.ok) {
    return {
      ok: false,
      reject:
        result.failure === "expired"
          ? "tunnel_token_expired"
          : "tunnel_token_invalid",
      failure: result.failure,
    };
  }
  if (result.claims.sub !== sourceId)
    return {
      ok: false,
      reject: "tunnel_token_invalid",
      failure: "subject_mismatch",
    };
  let sourceKey: KeyObject;
  try {
    const { kty, crv, x } = result.claims.cnf.jwk;
    sourceKey = createPublicKey({ key: { kty, crv, x }, format: "jwk" });
  } catch {
    return { ok: false, reject: "tunnel_token_invalid", failure: "bad_cnf" };
  }
  return { ok: true, claims: result.claims, sourceKey };
}
