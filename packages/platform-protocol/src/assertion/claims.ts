/**
 * 五种令牌的声明（协议包设计 §4.2）。schema 非 strict（更高 minor 加的声明被忽略）。
 *
 * 时间声明 `iat` / `exp` / `nbf` 是 JWT 的 NumericDate：**整数秒**。校验辅助
 * （`check.ts`）收毫秒的 `nowMs`，内部换算。
 */
import { z } from "zod";

import { hex32Schema } from "../tunnel/handshake.ts";

/** 头里的 `typ`。 */
export const TOKEN_TYPES = {
  access: "armadra-access",
  assertion: "armadra-assertion",
  relay: "armadra-relay",
  tunnel: "armadra-tunnel",
  source: "armadra-source",
} as const;

export type TokenType = (typeof TOKEN_TYPES)[keyof typeof TOKEN_TYPES];

/** 固定 `aud`。源访问断言的 `aud` 是 sourceId，源 JWS 的 `aud` 是 issuer。 */
export const AUDIENCES = {
  access: "cloud",
  relay: "relay",
  tunnel: "relay-tunnel",
} as const;

/** 断言、令牌的 `ver`：协议 major。 */
export const CLAIMS_VERSION = 1;

export const devicePlatformSchema = z.enum([
  "ios",
  "android",
  "desktop",
  "browser",
  "server",
]);
export type DevicePlatform = z.infer<typeof devicePlatformSchema>;

export const orgRoleSchema = z.enum(["owner", "admin", "member"]);
export type OrgRole = z.infer<typeof orgRoleSchema>;

/** Ed25519 公钥 JWK（RFC 8037）。`x` 是 32 字节的 base64url。 */
export const ed25519PublicJwkSchema = z.object({
  kty: z.literal("OKP"),
  crv: z.literal("Ed25519"),
  x: z.string().regex(/^[A-Za-z0-9_-]{43}$/, "须为 32 字节 base64url"),
  kid: z.string().optional(),
  alg: z.literal("EdDSA").optional(),
  use: z.literal("sig").optional(),
});

export type Ed25519PublicJwk = z.infer<typeof ed25519PublicJwkSchema>;

export const jwkSetSchema = z.object({ keys: z.array(ed25519PublicJwkSchema) });
export type JwkSet = z.infer<typeof jwkSetSchema>;

const numericDate = z.number().int().nonnegative();

const registered = {
  iss: z.string().min(1),
  iat: numericDate,
  exp: numericDate,
  nbf: numericDate.optional(),
  jti: z.string().min(8).max(128),
};

/** `guest:<linkId>:<随机>`：经链接进来的访客。 */
export const guestSubjectSchema = z
  .string()
  .regex(/^guest:[A-Za-z0-9_-]+:[A-Za-z0-9_-]+$/);

export const accessClaimsSchema = z.object({
  ...registered,
  sub: z.string().min(1),
  aud: z.literal(AUDIENCES.access),
  sid: z.string().min(1),
  ver: z.literal(CLAIMS_VERSION),
});

export const assertionClaimsSchema = z.object({
  ...registered,
  /** accountId 或访客 `guest:<linkId>:<随机>`。 */
  sub: z.string().min(1),
  /** 目标源的 sourceId。 */
  aud: hex32Schema,
  ver: z.literal(CLAIMS_VERSION),
  name: z.string().optional(),
  email: z.string().optional(),
  emailVerified: z.boolean().optional(),
  org: z.object({ orgId: z.string().min(1), role: orgRoleSchema }).optional(),
  link: z
    .object({ linkId: z.string().min(1), invitationId: z.string().min(1) })
    .optional(),
  device: z.object({
    deviceId: z.string().min(1),
    platform: devicePlatformSchema,
    name: z.string(),
  }),
});

export const relayTokenClaimsSchema = z.object({
  ...registered,
  sub: z.string().min(1),
  aud: z.literal(AUDIENCES.relay),
  /** 只能经中继访问这个源。 */
  src: hex32Schema,
  ver: z.literal(CLAIMS_VERSION),
});

export const tunnelTokenClaimsSchema = z.object({
  ...registered,
  /** sourceId。 */
  sub: hex32Schema,
  aud: z.literal(AUDIENCES.tunnel),
  /** 握手时 relay 用它验 auth 的签名（§4.4）。 */
  cnf: z.object({ jwk: ed25519PublicJwkSchema }),
  ver: z.literal(CLAIMS_VERSION),
});

/** core → 控制面 / personal 中继：`Authorization: Source <jws>`，头 `kid = sourceId`，60 秒。 */
export const sourceJwsClaimsSchema = z.object({
  ...registered,
  /** sourceId。 */
  iss: hex32Schema,
  /** 控制面的 issuer。 */
  aud: z.string().min(1),
});

export type AccessClaims = z.infer<typeof accessClaimsSchema>;
export type AssertionClaims = z.infer<typeof assertionClaimsSchema>;
export type RelayTokenClaims = z.infer<typeof relayTokenClaimsSchema>;
export type TunnelTokenClaims = z.infer<typeof tunnelTokenClaimsSchema>;
export type SourceJwsClaims = z.infer<typeof sourceJwsClaimsSchema>;
