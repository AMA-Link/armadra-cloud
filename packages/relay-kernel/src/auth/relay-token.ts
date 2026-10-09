/**
 * 中继令牌（平台设计 §8.1、中继设计 §4.2）：HTTP 头 `Armadra-Relay-Token`，WS 子协议
 * `armadra-relay.<jwt>`。校验 `typ`、签名、`aud === "relay"`、`iss`、时间、`jti` 未撤销，
 * 最后 `src === sourceId`。缺 / 坏 → `relay_token_invalid`（401）；`src` 不符 →
 * `relay_source_mismatch`（403）。
 */
import type { IncomingHttpHeaders } from "node:http";

import { verifyJwt, type JwtFailure } from "@armadra/cloud-shared/jwt";
import {
  RELAY_SUBPROTOCOL_PREFIX,
  RELAY_TOKEN_HEADER,
} from "@armadra/platform-protocol";
import {
  AUDIENCES,
  relayTokenClaimsSchema,
  TOKEN_TYPES,
  type RelayTokenClaims,
} from "@armadra/platform-protocol/assertion";

import { jwtOptions, type AuthContext } from "./context.ts";

export type RelayTokenResult =
  | { ok: true; claims: RelayTokenClaims }
  | {
      ok: false;
      error: "relay_token_invalid" | "relay_source_mismatch";
      failure: JwtFailure | "missing" | "source_mismatch";
    };

export async function verifyRelayToken(
  context: AuthContext,
  token: string | undefined,
  sourceId: string,
): Promise<RelayTokenResult> {
  if (!token)
    return { ok: false, error: "relay_token_invalid", failure: "missing" };
  const result = await verifyJwt(token, {
    ...jwtOptions(context),
    typ: TOKEN_TYPES.relay,
    schema: relayTokenClaimsSchema,
    expectedAud: AUDIENCES.relay,
  });
  if (!result.ok)
    return { ok: false, error: "relay_token_invalid", failure: result.failure };
  if (result.claims.src !== sourceId)
    return {
      ok: false,
      error: "relay_source_mismatch",
      failure: "source_mismatch",
    };
  return { ok: true, claims: result.claims };
}

/** HTTP：取 `Armadra-Relay-Token` 头（只认一个值）。 */
export function relayTokenFromHeaders(
  headers: IncomingHttpHeaders,
): string | undefined {
  const value = headers[RELAY_TOKEN_HEADER];
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

/**
 * WS：从 `Sec-WebSocket-Protocol` 里拿出 `armadra-relay.<jwt>`，其余子协议原样留下
 * （`armadra-ticket.<t>` 等要转给源）。
 */
export function splitRelaySubprotocol(header: string | undefined): {
  token: string | undefined;
  rest: string[];
} {
  const offered = (header ?? "")
    .split(",")
    .map((value) => value.trim())
    .filter(Boolean);
  let token: string | undefined;
  const rest: string[] = [];
  for (const protocol of offered) {
    if (protocol.startsWith(RELAY_SUBPROTOCOL_PREFIX)) {
      token ??= protocol.slice(RELAY_SUBPROTOCOL_PREFIX.length);
    } else rest.push(protocol);
  }
  return { token, rest };
}
