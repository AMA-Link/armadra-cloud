/**
 * 测试用签发：用协议包夹具里的测试钥（`TEST_KEYS`，私钥公开在仓库里）签隧道令牌。
 * 只给端到端的假 core 用。
 */
import { createPrivateKey, randomBytes, sign } from "node:crypto";

import {
  buildSigningInput,
  joinCompactJws,
  TOKEN_TYPES,
} from "@armadra/platform-protocol/assertion";
import { TEST_KEYS } from "@armadra/platform-protocol/fixtures";

const ISSUER = TEST_KEYS.issuer.issuer;
export const SOURCE_ID = TEST_KEYS.source.sourceId;
const issuerKey = createPrivateKey({
  key: TEST_KEYS.issuer.privateJwk,
  format: "jwk",
});
export const sourceKey = createPrivateKey({
  key: TEST_KEYS.source.privateJwk,
  format: "jwk",
});

function signJwt(
  typ: string,
  payload: Record<string, unknown>,
  options: { kid?: string; key?: typeof issuerKey } = {},
): string {
  const header = {
    alg: "EdDSA",
    kid: options.kid ?? TEST_KEYS.issuer.kid,
    typ,
  };
  const { signingInput, encodedHeader, encodedPayload } = buildSigningInput(
    header,
    payload,
  );
  return joinCompactJws(
    encodedHeader,
    encodedPayload,
    new Uint8Array(sign(null, signingInput, options.key ?? issuerKey)),
  );
}

const jti = () => `jti-${randomBytes(8).toString("hex")}`;

export function tunnelToken(
  overrides: Record<string, unknown> = {},
  nowMs = Date.now(),
): string {
  const iat = Math.floor(nowMs / 1000);
  return signJwt(TOKEN_TYPES.tunnel, {
    iss: ISSUER,
    sub: SOURCE_ID,
    aud: "relay-tunnel",
    cnf: {
      jwk: { kty: "OKP", crv: "Ed25519", x: TEST_KEYS.source.publicJwk.x },
    },
    iat,
    exp: iat + 3600,
    jti: jti(),
    ver: 1,
    ...overrides,
  });
}
