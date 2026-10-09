/** 两种令牌校验共用的上下文：控制面（issuer、撤销表）、JWKS 缓存与时钟。 */
import type { ControlPlane } from "../control/types.ts";
import { JwksCache } from "./jwks.ts";

export interface AuthContext {
  control: ControlPlane;
  jwks: JwksCache;
  now: () => number;
}

export function createAuthContext(
  control: ControlPlane,
  now: () => number = Date.now,
): AuthContext {
  return { control, jwks: new JwksCache(() => control.jwks(), { now }), now };
}

export function jwtOptions(context: AuthContext) {
  return {
    expectedIss: context.control.issuer,
    nowMs: context.now(),
    key: (kid: string) => context.jwks.key(kid),
    isRevoked: (jti: string) => context.control.tokens.isRevoked(jti),
  };
}
