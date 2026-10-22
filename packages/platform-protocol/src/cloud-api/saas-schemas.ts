/**
 * SaaS 能力（协议 1.4，cloud-api §21 起）的共用形状：空间、团队、邀请、分组、授予、访问申请、
 * 审计、API 令牌、转移。这些 procedure 现在全是预留的（personal 答 404、cloud 答 501），形状为
 * 预留，实现时只加字段。schema 非 strict。
 */
import { z } from "zod";

import {
  devicePlatformSchema,
  grantRoleSchema,
  teamRoleSchema,
} from "../assertion/claims.ts";
import { hex32Schema } from "../tunnel/handshake.ts";

const sourceIdSchema = hex32Schema;

export { grantRoleSchema, teamRoleSchema };

/** query 里是字符串：毫秒整数按数字收。 */
export const msQuery = z.coerce.number().int().nonnegative();

/** 列表的游标分页：`limit` 缺省 100、最多 500；`cursor` 原样回传上一页的 `nextCursor`。 */
export const PAGE_LIMIT_DEFAULT = 100;
export const PAGE_LIMIT_MAX = 500;
export const pageInputShape = {
  limit: z.coerce
    .number()
    .int()
    .positive()
    .max(PAGE_LIMIT_MAX)
    .default(PAGE_LIMIT_DEFAULT),
  cursor: z.string().max(512).optional(),
};
export const nextCursorSchema = z.string().nullable();

/** 空间：个人空间（账号自带、不可删）或某个团队；主机、链接、配额、计费都挂在空间上。 */
export const spaceSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("personal") }),
  z.object({ kind: z.literal("team"), teamId: z.string().min(1) }),
]);
export type Space = z.infer<typeof spaceSchema>;

export const teamIdSchema = z.string().min(1);
export const teamSlugSchema = z
  .string()
  .regex(/^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$/);
export const teamNameSchema = z.string().min(1).max(128);
/** 团队邀请、加入链接能给的角色：owner 只能经转移，billing 与 guest 由 admin 改。 */
export const teamInviteRoleSchema = z.enum([
  "admin",
  "member",
  "billing",
  "guest",
]);

/** 新设备背书人（cloud-api §21，设计 §11.4）。 */
export const deviceEndorsersSchema = z.enum(["owner", "owner_admin"]);

export const teamSettingsSchema = z.object({
  /** member 能否把主机登记到团队。 */
  membersCanRegisterHosts: z.boolean(),
  /** 新授予缺省给的主机角色。 */
  defaultHostRole: grantRoleSchema,
  allowGuests: z.boolean(),
  /** 是否允许匿名（不登录云账号）接受分享链接。 */
  allowAnonymousLinks: z.boolean(),
  deviceEndorsers: deviceEndorsersSchema,
  /** 成员能否从自己另一台已受信的设备给新设备背书。 */
  memberSelfEndorse: z.boolean(),
  /** 以下三项 M2 起生效。 */
  requireMfa: z.boolean(),
  sessionMaxAgeMs: z.number().int().positive().nullable(),
  allowedEmailDomains: z.array(z.string().min(1).max(253)).max(32),
});

export const teamSchema = z.object({
  teamId: teamIdSchema,
  slug: teamSlugSchema,
  name: teamNameSchema,
  avatarUrl: z.string().nullable(),
  /** 调用者在这个团队里的角色。 */
  role: teamRoleSchema,
  /** 计划代号：`free`、`team`、`enterprise`…。 */
  plan: z.string(),
  createdAtMs: z.number().int(),
  /** 删除后的恢复期内非空：到 `purgeAtMs` 清除。 */
  deletedAtMs: z.number().int().nullable(),
  purgeAtMs: z.number().int().nullable(),
});

export const teamMemberSchema = z.object({
  accountId: z.string(),
  email: z.string().nullable(),
  displayName: z.string(),
  role: teamRoleSchema,
  joinedAtMs: z.number().int(),
  /** 1.4 补充：所在分组（M2；guest 恒空）。实现时无分组也必须返回 []。 */
  groups: z.array(z.object({ groupId: z.string(), name: z.string() })),
  /** 1.4 补充：只对 guest 有值——被直接授予、能到达的主机数。 */
  reachableHosts: z.number().int().nonnegative().optional(),
});

/** 一项用量：`metric` 如 `hosts`、`members`、`guests`、`connections`、`relay.bytes`。 */
export const usageEntrySchema = z.object({
  metric: z.string(),
  value: z.number().nonnegative(),
  /** `null` = 不限。 */
  limit: z.number().nullable(),
});
export const periodSchema = z.string().regex(/^\d{4}-\d{2}$/);

/** `billing.usage` 的按天流量序列（1.4 补充）：一天一条，`startMs` 是当天 UTC 零点。 */
export const usageSeriesPointSchema = z.object({
  startMs: z.number().int(),
  relayBytes: z.number().nonnegative(),
});

export const teamUsageSchema = z.object({
  teamId: teamIdSchema,
  period: periodSchema,
  usage: z.array(usageEntrySchema),
});

/** `me.spaces`：个人空间 + 所属的每个团队。 */
export const meSpaceSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("personal"), plan: z.string() }),
  z.object({
    kind: z.literal("team"),
    teamId: teamIdSchema,
    slug: teamSlugSchema,
    name: teamNameSchema,
    role: teamRoleSchema,
    plan: z.string(),
  }),
]);

export const teamMembershipSchema = z.object({
  teamId: teamIdSchema,
  slug: teamSlugSchema,
  name: teamNameSchema,
  role: teamRoleSchema,
});

// 邀请与加入 -------------------------------------------------------------

export const invitationKindSchema = z.enum(["team", "host"]);
export const invitationStateSchema = z.enum([
  "pending",
  "accepted",
  "declined",
  "revoked",
  "expired",
]);

export const invitationSchema = z.object({
  invitationId: z.string(),
  kind: invitationKindSchema,
  email: z.email(),
  state: invitationStateSchema,
  /** `kind = "team"`。 */
  teamId: teamIdSchema.optional(),
  teamRole: teamInviteRoleSchema.optional(),
  /** `kind = "host"`：接受后成为该主机的 guest 授予。 */
  sourceId: sourceIdSchema.optional(),
  hostRole: grantRoleSchema.optional(),
  workspaceIds: z.array(z.string()).optional(),
  invitedBy: z.string(),
  createdAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
  decidedAtMs: z.number().int().nullable(),
});

export const createInvitationInputSchema = z.discriminatedUnion("kind", [
  z.object({
    kind: z.literal("team"),
    teamId: teamIdSchema,
    email: z.email(),
    role: teamInviteRoleSchema,
  }),
  z.object({
    kind: z.literal("host"),
    sourceId: sourceIdSchema,
    email: z.email(),
    role: grantRoleSchema,
    /** 工作空间 id 列表；缺省 = 整台主机。 */
    workspaceIds: z.array(z.string().min(1).max(128)).max(64).optional(),
    /** 缺省 7 天，最长 30 天。 */
    expiresAtMs: z.number().int().optional(),
  }),
]);

/** 匿名可见的摘要（`invitations.get`）：不含对方邮箱全文。 */
export const invitationPreviewSchema = z.object({
  kind: invitationKindSchema,
  state: invitationStateSchema,
  teamName: z.string().optional(),
  teamRole: teamInviteRoleSchema.optional(),
  sourceName: z.string().optional(),
  hostRole: grantRoleSchema.optional(),
  inviterName: z.string(),
  /** 被邀请邮箱的遮掩形式，如 `a***@example.com`。 */
  emailHint: z.string(),
  expiresAtMs: z.number().int(),
});

// 分组 -------------------------------------------------------------------

export const groupSchema = z.object({
  groupId: z.string(),
  teamId: teamIdSchema,
  name: z.string().min(1).max(128),
  memberCount: z.number().int().nonnegative(),
  createdAtMs: z.number().int(),
});

export const groupMemberSchema = z.object({
  accountId: z.string(),
  email: z.string().nullable(),
  displayName: z.string(),
  isLead: z.boolean(),
  addedAtMs: z.number().int(),
});

// 授予与访问申请 ---------------------------------------------------------

/** 授予的主体：团队全员、单个成员、分组（M2）、guest（团队里的外部协作者）。 */
export const grantSubjectSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("team"), teamId: teamIdSchema }),
  z.object({ kind: z.literal("member"), accountId: z.string() }),
  z.object({ kind: z.literal("group"), groupId: z.string() }),
  z.object({ kind: z.literal("guest"), accountId: z.string() }),
]);

export const grantSchema = z.object({
  grantId: z.string(),
  sourceId: sourceIdSchema,
  subject: grantSubjectSchema,
  role: grantRoleSchema,
  /** 工作空间 id；空数组 = 整台主机。 */
  workspaceIds: z.array(z.string()),
  expiresAtMs: z.number().int().nullable(),
  grantedBy: z.string(),
  createdAtMs: z.number().int(),
  revokedAtMs: z.number().int().nullable(),
});

/** 工作空间 id 列表；空 = 整台主机。 */
export const workspaceIdsSchema = z.array(z.string().min(1).max(128)).max(64);
const workspaceIdsInput = workspaceIdsSchema;

export const createGrantInputSchema = z.object({
  sourceId: sourceIdSchema,
  subject: grantSubjectSchema,
  role: grantRoleSchema,
  workspaceIds: workspaceIdsInput.optional(),
  expiresAtMs: z.number().int().optional(),
});

/** 有效权限的一个来源：同一个人从多条授予里取最高角色、范围取并集。 */
export const effectiveSourceSchema = z.object({
  via: z.enum(["owner", "team", "member", "group", "guest"]),
  grantId: z.string().nullable(),
  role: grantRoleSchema,
  workspaceIds: z.array(z.string()),
  expiresAtMs: z.number().int().nullable(),
});

export const effectiveAccessSchema = z.object({
  sourceId: sourceIdSchema,
  accountId: z.string(),
  /** `null` = 没有任何有效授予。 */
  role: grantRoleSchema.nullable(),
  workspaceIds: z.array(z.string()),
  expiresAtMs: z.number().int().nullable(),
  from: z.array(effectiveSourceSchema),
});

export const accessRequestStatusSchema = z.enum([
  "pending",
  "approved",
  "denied",
  "canceled",
]);

export const accessRequestSchema = z.object({
  requestId: z.string(),
  sourceId: sourceIdSchema,
  sourceName: z.string(),
  requester: z.object({
    accountId: z.string(),
    email: z.string().nullable(),
    displayName: z.string(),
  }),
  role: grantRoleSchema,
  workspaceIds: z.array(z.string()),
  reason: z.string().max(500),
  status: accessRequestStatusSchema,
  decidedBy: z.string().nullable(),
  decidedAtMs: z.number().int().nullable(),
  createdAtMs: z.number().int(),
});

// 审计 -------------------------------------------------------------------

/** 审计只记主体、动作、对象、时间与来源 IP 的前缀，不记内容与令牌。 */
export const auditEventSchema = z.object({
  eventId: z.string(),
  teamId: teamIdSchema.nullable(),
  actor: z.object({
    kind: z.enum(["account", "api_token", "system", "operator"]),
    id: z.string().nullable(),
  }),
  /** 点分动作名，如 `member.add`、`grant.create`、`assertion.issue`。 */
  action: z.string().min(1).max(64),
  target: z.object({ kind: z.string(), id: z.string() }).nullable(),
  /** 断言签发事件带 `jti`，可与 core 审计对账。 */
  jti: z.string().nullable(),
  ipPrefix: z.string().nullable(),
  atMs: z.number().int(),
});

export const AUDIT_EXPORT_FORMATS = ["csv", "json"] as const;
/** 导出一次最多这么多行，超出答 `400 bad_request`（缩小时间范围）。 */
export const AUDIT_EXPORT_MAX_ROWS = 10_000;

// API 令牌 ---------------------------------------------------------------

/** 个人 / 团队 API 令牌能带的范围：只读目录、管理授予、管理主机。 */
export const API_TOKEN_SCOPES = [
  "directory:read",
  "grants:manage",
  "hosts:manage",
] as const;
export const apiTokenScopeSchema = z.enum(API_TOKEN_SCOPES);

export const apiTokenSchema = z.object({
  tokenId: z.string(),
  name: z.string().min(1).max(128),
  /** `null` = 属于创建者本人的个人令牌。 */
  teamId: teamIdSchema.nullable(),
  scopes: z.array(apiTokenScopeSchema).min(1),
  /** 令牌明文的前 8 位，只用来辨认。 */
  prefix: z.string(),
  expiresAtMs: z.number().int().nullable(),
  lastUsedAtMs: z.number().int().nullable(),
  createdAtMs: z.number().int(),
  revokedAtMs: z.number().int().nullable(),
});

// 主机补充 ---------------------------------------------------------------

export const hostTagSchema = z.string().regex(/^[a-z0-9][a-z0-9._-]{0,31}$/);
export const hostTagsSchema = z.array(hostTagSchema).max(16);

/** 谁能签这台主机的分享链接。 */
export const linkCreatorsSchema = z.enum(["owner", "owner_admin", "driver"]);
export const hostSettingsSchema = z.object({
  linkCreators: linkCreatorsSchema,
  allowGuests: z.boolean(),
  /** 新授予缺省给的角色。 */
  defaultRole: grantRoleSchema,
});

export const transferStatusSchema = z.enum([
  "pending",
  "completed",
  "canceled",
  "expired",
]);

/** 主机转移空间：两边都要确认才生效（个人 → 团队：团队 admin；团队 → 团队：两边 admin）。 */
export const sourceTransferSchema = z.object({
  transferId: z.string(),
  sourceId: sourceIdSchema,
  from: spaceSchema,
  to: spaceSchema,
  status: transferStatusSchema,
  /** 来源方、目标方是否已确认。 */
  confirmedBy: z.object({ from: z.boolean(), to: z.boolean() }),
  createdAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
});

// 控制台补充（cloud-api §28、§29、§31、§32）-----------------------------

/** 通知偏好：安全类邮件恒开、不可关。 */
export const notificationPrefsSchema = z.object({
  invitations: z.boolean(),
  accessRequests: z.boolean(),
  hostPresence: z.boolean(),
  security: z.literal(true),
});

/** 设备授权页要显示的发起设备（设计 §11.5：显示平台与名字）。 */
export const deviceRequestSchema = z.object({
  platform: devicePlatformSchema,
  name: z.string(),
  requestedAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
});

export const invoiceStatusSchema = z.enum([
  "draft",
  "open",
  "paid",
  "void",
  "uncollectible",
]);
export const invoiceSchema = z.object({
  invoiceId: z.string(),
  number: z.string(),
  status: invoiceStatusSchema,
  /** 最小货币单位（分）。 */
  amount: z.number().int(),
  currency: z.string().length(3),
  periodStartMs: z.number().int(),
  periodEndMs: z.number().int(),
  issuedAtMs: z.number().int(),
  hostedUrl: z.string().nullable(),
  pdfUrl: z.string().nullable(),
});

/**
 * 新设备待信任（设计 §11.4）：团队成员第一次从新设备连团队主机，由主机 owner 或团队 admin
 * 从自己已受信的设备背书。**批准走端到端通道发到主机，不经云端**；云端只转发这条通知并记
 * 拒绝，看不到也改不了背书内容。
 */
export const trustRequestSchema = z.object({
  requestId: z.string(),
  sourceId: sourceIdSchema,
  sourceName: z.string(),
  teamId: teamIdSchema.nullable(),
  member: z.object({
    accountId: z.string(),
    email: z.string().nullable(),
    displayName: z.string(),
  }),
  device: z.object({ name: z.string(), platform: devicePlatformSchema }),
  requestedAtMs: z.number().int(),
  expiresAtMs: z.number().int(),
});

export const statusComponentStateSchema = z.enum([
  "operational",
  "degraded",
  "outage",
]);
/** 对外状态页的数据：组件状态 + 公告。 */
export const publicStatusSchema = z.object({
  components: z.array(
    z.object({
      id: z.string().min(1).max(64),
      name: z.string().min(1).max(128),
      state: statusComponentStateSchema,
    }),
  ),
  notice: z.string().max(1000).nullable(),
  updatedAtMs: z.number().int().nullable(),
});

export const abuseSignalSchema = z.enum(["traffic", "report"]);
export const abuseReportSchema = z.object({
  reportId: z.string(),
  target: z.object({ kind: z.enum(["link", "host"]), id: z.string() }),
  teamId: teamIdSchema.nullable(),
  signal: abuseSignalSchema,
  /** 本月流量字节数（流量异常信号）。 */
  relayBytesMonth: z.number().int().nonnegative().nullable(),
  frozen: z.boolean(),
  createdAtMs: z.number().int(),
});

export const featureFlagSchema = z.object({
  flag: z.string().regex(/^[a-z][a-z0-9._-]{0,63}$/),
  /** 对全部团队开。 */
  all: z.boolean(),
  /** 灰度的团队 id（`all` 为 `false` 时生效）。 */
  teamIds: z.array(teamIdSchema).max(1000),
});

/** `space` 在 GET 里按 query 的方括号写法：`space[kind]=team&space[teamId]=…`。 */
export type Team = z.infer<typeof teamSchema>;
export type TeamMember = z.infer<typeof teamMemberSchema>;
export type TeamSettings = z.infer<typeof teamSettingsSchema>;
export type Invitation = z.infer<typeof invitationSchema>;
export type Group = z.infer<typeof groupSchema>;
export type Grant = z.infer<typeof grantSchema>;
export type GrantSubject = z.infer<typeof grantSubjectSchema>;
export type AccessRequest = z.infer<typeof accessRequestSchema>;
export type AuditEvent = z.infer<typeof auditEventSchema>;
export type ApiToken = z.infer<typeof apiTokenSchema>;
export type SourceTransfer = z.infer<typeof sourceTransferSchema>;
