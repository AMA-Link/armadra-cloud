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

/** 团队里的角色（0.3.0 起，断言 `team.role`）。 */
export const teamRoleSchema = z.enum(["lead", "member"]);
export type TeamRole = z.infer<typeof teamRoleSchema>;

/** 主机授予的四档（core `roleScopes`；0.3.0 起，断言 `role`）。 */
export const grantRoleSchema = z.enum([
  "viewer",
  "editor",
  "operator",
  "driver",
]);
export type GrantRole = z.infer<typeof grantRoleSchema>;

/** 资源 id（工作空间、会话）：可打印、无空白，最长 128。 */
const scopeIdPattern = "[A-Za-z0-9_.:-]{1,128}";

/** `scp` 的前缀与只读标记（0.3.0 起）。 */
export const SCOPE_PREFIX = { workspace: "ws:", session: "sess:" } as const;
export const SCOPE_READ_ONLY = "ro";

/**
 * 断言范围的一项：`ws:<workspaceId>`、`sess:<sessionId>` 或 `ro`（只读）。
 * `scp` 缺省或为空 = 整台主机、按角色可写（0.2.x 的行为逐字不变）。
 */
export const scopeEntrySchema = z
  .string()
  .regex(new RegExp(`^(?:ws:${scopeIdPattern}|sess:${scopeIdPattern}|ro)$`));
export type ScopeEntry = z.infer<typeof scopeEntrySchema>;

/** 分享范围（`links.create` 的 `scope`，0.3.0 起）。都缺省 = 整台、按角色。 */
export interface ShareScope {
  workspaceId?: string;
  /** 给了会话就必须给它所在的工作空间；会话范围总是只读。 */
  sessionId?: string;
  readOnly?: boolean;
}

/** 分享范围 → `scp`（顺序固定：`ws:`、`sess:`、`ro`）。整台可写答空数组。 */
export function scopeClaims(scope: ShareScope | undefined): ScopeEntry[] {
  if (!scope) return [];
  const out: ScopeEntry[] = [];
  if (scope.workspaceId !== undefined)
    out.push(`${SCOPE_PREFIX.workspace}${scope.workspaceId}`);
  if (scope.sessionId !== undefined)
    out.push(`${SCOPE_PREFIX.session}${scope.sessionId}`);
  if (scope.readOnly === true || scope.sessionId !== undefined)
    out.push(SCOPE_READ_ONLY);
  return out;
}

/** `scp` → 分享范围（未知项忽略；多个 `ws:` / `sess:` 时取第一个）。 */
export function parseScopeClaims(scp: readonly string[] | undefined): {
  workspaceIds: string[];
  sessionIds: string[];
  readOnly: boolean;
} {
  const workspaceIds: string[] = [];
  const sessionIds: string[] = [];
  let readOnly = false;
  for (const entry of scp ?? []) {
    if (entry === SCOPE_READ_ONLY) readOnly = true;
    else if (entry.startsWith(SCOPE_PREFIX.workspace))
      workspaceIds.push(entry.slice(SCOPE_PREFIX.workspace.length));
    else if (entry.startsWith(SCOPE_PREFIX.session))
      sessionIds.push(entry.slice(SCOPE_PREFIX.session.length));
  }
  return { workspaceIds, sessionIds, readOnly };
}

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
  /** 0.3.0 起：经团队授予时的团队。 */
  team: z
    .object({ teamId: z.string().min(1), role: teamRoleSchema })
    .optional(),
  /** 0.3.0 起：主机授予的角色；缺省时 core 按邀请 / 本地设置决定（旧行为）。 */
  role: grantRoleSchema.optional(),
  /** 0.3.0 起：范围（`ws:<id>`、`sess:<id>`、`ro`）；缺省或空 = 整台（旧行为）。 */
  scp: z.array(scopeEntrySchema).max(64).optional(),
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
  /** 0.3.0 起：源所属的组织（限流与计量）；personal 不发。 */
  org: z.string().min(1).optional(),
  /** 0.3.0 起：只读——边缘只放行安全方法与身份域路径（{@link isReadOnlyAllowed}）。 */
  ro: z.boolean().optional(),
});

export const tunnelTokenClaimsSchema = z.object({
  ...registered,
  /** sourceId。 */
  sub: hex32Schema,
  aud: z.literal(AUDIENCES.tunnel),
  /** 握手时 relay 用它验 auth 的签名（§4.4）。 */
  cnf: z.object({ jwk: ed25519PublicJwkSchema }),
  ver: z.literal(CLAIMS_VERSION),
  /** 0.3.0 起：源所属的组织（`tunnel.connect` 按组织计量）；personal 不发。 */
  org: z.string().min(1).optional(),
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
