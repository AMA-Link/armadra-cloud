/** `/v1/*` 契约里多处共用的形状（协议包设计 §5）。schema 非 strict。 */
import { z } from "zod";

import {
  devicePlatformSchema,
  ed25519PublicJwkSchema,
  orgRoleSchema,
} from "../assertion/claims.ts";
import { hex32Schema, protocolVersionSchema } from "../tunnel/handshake.ts";
import {
  hostSettingsSchema,
  hostTagsSchema,
  spaceSchema,
  teamMembershipSchema,
} from "./saas-schemas.ts";

export {
  devicePlatformSchema,
  ed25519PublicJwkSchema,
  orgRoleSchema,
  protocolVersionSchema,
};

export const sourceIdSchema = hex32Schema;
export const empty = z.object({});

/** 挑战的提供方（cloud-api §16，0.3.5 起）。 */
export const challengeProviderSchema = z.enum(["turnstile"]);
/** 要挑战的 procedure（`platform.info.challenge.scope`；`auth.changePassword` 0.3.10 起，cloud-api §18）。 */
export const challengeScopeSchema = z.enum([
  "auth.login",
  "links.accept",
  "auth.changePassword",
]);

/**
 * 请求里带的挑战令牌（`auth.login` / `links.accept` 的可选 `challenge`，0.3.5 起）：页面或
 * 手机的挑战组件交回的一次性令牌。服务端在 `platform.info.challenge.scope` 列了这条 procedure
 * 时才要求；缺失答 `challenge_required`，无效 / 过期 / 用过答 `challenge_invalid`。
 */
export const challengeTokenSchema = z.object({
  provider: challengeProviderSchema,
  token: z.string().min(1).max(2048),
});

export const deviceInputSchema = z.object({
  platform: devicePlatformSchema,
  name: z.string().min(1).max(128),
  pushPublicKey: z.string().optional(),
});

/**
 * 云会话。浏览器来源（`app.<cloud>` 自己的页面）上登录类答案不带 `refreshToken`，
 * 改设 `__Host-armadra-cloud` Cookie + `X-Armadra-CSRF`，所以 `refreshToken` 可缺。
 */
export const sessionSchema = z.object({
  accessToken: z.string(),
  refreshToken: z.string().optional(),
  accessExpiresAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
  account: z.object({
    accountId: z.string(),
    email: z.string().nullable(),
    displayName: z.string(),
    emailVerified: z.boolean(),
  }),
  device: z.object({ deviceId: z.string() }),
});

export const sessionResultSchema = z.object({ session: sessionSchema });

export const mfaChallengeResultSchema = z.object({
  mfa: z.object({
    challengeId: z.string(),
    methods: z.array(z.enum(["totp", "recovery"])),
  }),
});

export const sourceKindSchema = z.enum(["desktop", "server", "hosted"]);
/** `team` = 经团队授予（1.4 起）；`org` 保留给以后的企业组织层。 */
export const sourceViaSchema = z.enum(["owner", "link", "team", "org"]);

export const sourceSummarySchema = z.object({
  sourceId: sourceIdSchema,
  name: z.string(),
  kind: sourceKindSchema,
  online: z.boolean(),
  lastSeenAtMs: z.number().int().nullable(),
  owner: z.boolean(),
  via: sourceViaSchema,
  relayOrigin: z.string(),
  coreVersion: z.string(),
  /** 1.4 起：主机所属的空间；缺省 = 个人空间（personal 不报）。 */
  space: spaceSchema.optional(),
  /** 1.4 起：标签（筛选与成组授予）。 */
  tags: hostTagsSchema.optional(),
});

export const sourceDetailSchema = sourceSummarySchema.extend({
  trustedOrigins: z.array(z.string()),
  capabilities: z.array(z.string()),
  registeredAtMs: z.number().int(),
  /** 1.4 起：主机设置；personal 不报。 */
  settings: hostSettingsSchema.optional(),
});

export const orgMembershipSchema = z.object({
  orgId: z.string(),
  slug: z.string(),
  name: z.string(),
  role: orgRoleSchema,
});

export const meSchema = z.object({
  accountId: z.string(),
  email: z.string().nullable(),
  displayName: z.string(),
  emailVerified: z.boolean(),
  /** 保留给以后的企业组织层；1.4 里恒为空。 */
  orgs: z.array(orgMembershipSchema),
  /** 1.4 起：所属的团队；personal 不报。 */
  teams: z.array(teamMembershipSchema).optional(),
  mfaEnabled: z.boolean(),
});

/** `team_join` = 团队加入链接（1.4 起；M2）；`org_join` 从未发出，保留枚举值只增不删。 */
export const linkKindSchema = z.enum([
  "source_invite",
  "org_join",
  "team_join",
]);

const scopeIdSchema = z.string().regex(/^[A-Za-z0-9_.:-]{1,128}$/);

/**
 * 分享范围（0.3.0 起，`source_invite` 专用）：都缺省 = 整台、按角色可写。
 * `sessionId` 必须同时给 `workspaceId`，会话范围总是只读（`readOnly: false` 答 `bad_request`）。
 * 签出的断言带 `scp`（`ws:` / `sess:` / `ro`），只读时中继令牌带 `ro: true`。
 */
export const linkScopeSchema = z.object({
  workspaceId: scopeIdSchema.optional(),
  sessionId: scopeIdSchema.optional(),
  readOnly: z.boolean().optional(),
});

export const linkSummarySchema = z.object({
  linkId: z.string(),
  kind: linkKindSchema,
  label: z.string(),
  role: z.string().optional(),
  sourceId: sourceIdSchema.optional(),
  /** `team_join` 链接的团队（1.4 起，取代从未发出的 `orgId`）。 */
  teamId: z.string().optional(),
  /** 0.3.0 起；整台时不出现。 */
  scope: linkScopeSchema.optional(),
  url: z.string(),
  uses: z.number().int().nonnegative(),
  maxUses: z.number().int().positive().nullable(),
  expiresAtMs: z.number().int(),
  createdAtMs: z.number().int(),
  revokedAtMs: z.number().int().nullable(),
});

export type DeviceInput = z.infer<typeof deviceInputSchema>;
export type ChallengeToken = z.infer<typeof challengeTokenSchema>;
export type Session = z.infer<typeof sessionSchema>;
export type SessionResult = z.infer<typeof sessionResultSchema>;
export type SourceSummary = z.infer<typeof sourceSummarySchema>;
export type SourceDetail = z.infer<typeof sourceDetailSchema>;
export type Me = z.infer<typeof meSchema>;
export type LinkSummary = z.infer<typeof linkSummarySchema>;
export type LinkScope = z.infer<typeof linkScopeSchema>;
