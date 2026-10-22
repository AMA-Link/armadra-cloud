// 用 TypeScript 的令牌校验（cloud-shared 的 verifyJwt + 协议包的 zod schema）验 Go 签的令牌：
//
//   echo '{"issuer":"…","nowMs":…,"jwks":{"keys":[…]},"tokens":[{"kind":"access","token":"…","aud":"cloud"}]}' \
//     | node go/testdata/compat/verify-tokens.mjs
//
// 输出 JSON 数组，每个令牌一项：{ ok: true, claims } 或 { ok: false, failure }。令牌不进日志，只经标准输入传入。
import { createPublicKey } from "node:crypto";
import { readFileSync } from "node:fs";

import { verifyJwt } from "../../../packages/cloud-shared/src/jwt.ts";
import {
  accessClaimsSchema,
  assertionClaimsSchema,
  relayTokenClaimsSchema,
  TOKEN_TYPES,
  tunnelTokenClaimsSchema,
} from "../../../packages/platform-protocol/src/assertion/index.ts";

const input = JSON.parse(readFileSync(0, "utf8"));
const kinds = {
  access: [TOKEN_TYPES.access, accessClaimsSchema],
  assertion: [TOKEN_TYPES.assertion, assertionClaimsSchema],
  relay: [TOKEN_TYPES.relay, relayTokenClaimsSchema],
  tunnel: [TOKEN_TYPES.tunnel, tunnelTokenClaimsSchema],
};

const keys = new Map(
  input.jwks.keys.map((jwk) => [
    jwk.kid,
    createPublicKey({
      key: { kty: jwk.kty, crv: jwk.crv, x: jwk.x },
      format: "jwk",
    }),
  ]),
);

const results = [];
for (const { kind, token, aud } of input.tokens) {
  const [typ, schema] = kinds[kind];
  results.push(
    await verifyJwt(token, {
      typ,
      schema,
      expectedAud: aud,
      expectedIss: input.issuer,
      nowMs: input.nowMs,
      key: async (kid) => keys.get(kid) ?? null,
      isRevoked: async () => false,
    }),
  );
}
process.stdout.write(JSON.stringify(results));
