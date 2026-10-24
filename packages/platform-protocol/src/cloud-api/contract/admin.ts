import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import {
  abuseReportSchema,
  auditEventSchema,
  featureFlagSchema,
  nextCursorSchema,
  pageInputShape,
  publicStatusSchema,
  statusComponentStateSchema,
  teamIdSchema,
  teamSlugSchema,
} from "../saas-schemas.ts";
import { relayNodeIdSchema } from "./relay.ts";

const operator = { scope: ["operator"], since: SINCE_0_10 } as const;
const adminErrors = errors.pick("not_found", "forbidden");

const statusReason = z.string().min(1).max(500);

export const adminAccountSchema = z.object({
  accountId: z.string(),
  email: z.string().nullable(),
  displayName: z.string(),
  emailVerified: z.boolean(),
  status: z.enum(["active", "disabled"]),
  plan: z.string(),
  hostCount: z.number().int().nonnegative(),
  teamCount: z.number().int().nonnegative(),
  createdAtMs: z.number().int(),
  lastSeenAtMs: z.number().int().nullable(),
  /** 详情用：席位、本月流量与最近的运营动作（审计最近 5 条）。 */
  seats: z.number().int().nonnegative().optional(),
  relayBytesMonth: z.number().int().nonnegative().optional(),
  recentActions: z.array(auditEventSchema).max(20).optional(),
});

export const adminTeamSchema = z.object({
  teamId: teamIdSchema,
  slug: teamSlugSchema,
  name: z.string(),
  status: z.enum(["active", "suspended"]),
  plan: z.string(),
  memberCount: z.number().int().nonnegative(),
  hostCount: z.number().int().nonnegative(),
  createdAtMs: z.number().int(),
  deletedAtMs: z.number().int().nullable(),
  /** 详情用：席位、本月流量与最近的运营动作（审计最近 5 条）。 */
  seats: z.number().int().nonnegative().optional(),
  relayBytesMonth: z.number().int().nonnegative().optional(),
  recentActions: z.array(auditEventSchema).max(20).optional(),
});

/** 搜索结果里账号与团队混排的一行（`admin.tenants`）。 */
export const adminTenantSchema = z.object({
  kind: z.enum(["account", "team"]),
  id: z.string(),
  name: z.string(),
  plan: z.string(),
  hostCount: z.number().int().nonnegative(),
  status: z.enum(["active", "disabled", "suspended"]),
  createdAtMs: z.number().int(),
});

export const adminRelayNodeSchema = z.object({
  node: relayNodeIdSchema,
  region: z.string().optional(),
  online: z.boolean(),
  /** 在线隧道数。 */
  tunnels: z.number().int().nonnegative(),
  /** 当前经这个节点的客户端连接数。 */
  connections: z.number().int().nonnegative(),
  draining: z.boolean(),
  version: z.string(),
  lastSeenAtMs: z.number().int().nullable(),
});

/**
 * 事故开关：一键停签断言、停注册；中继侧拒绝新连接、保留在线连接。
 * `rejectNewConnections` 是协议 1.4 发布后的只加不改补充（0.11.0 起）：缺省 = 关；
 * 旧控制面不带它，客户端按关处理。
 */
export const adminIncidentSchema = z.object({
  pauseAssertions: z.boolean(),
  pauseRegistrations: z.boolean(),
  rejectNewConnections: z.boolean().optional(),
  updatedAtMs: z.number().int().nullable(),
});

const searchShape = {
  q: z.string().max(128).optional(),
  ...pageInputShape,
};

/**
 * 运营后台（cloud-api §28）：单独的 scope `operator`（运营令牌），不对外公开。只读元数据——
 * 看不到任何内容与令牌，也没有「以用户身份登录」。停用 / 恢复、节点排空与事故开关都写审计。
 * 仅 saas，预留（M1 最小集；滥用处理、功能开关、状态页 M2）。
 */
export const admin = {
  /** 控制台的租户搜索：账号与团队混排，按名称 / 邮箱 / slug 模糊查；详情与停用按 `kind` 走下面的 account* / team*。 */
  tenants: base
    .route({ method: "GET", path: "/v1/admin/tenants" })
    .meta(operator)
    .errors(adminErrors)
    .input(z.object({ ...searchShape }))
    .output(
      z.object({
        tenants: z.array(adminTenantSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  accounts: base
    .route({ method: "GET", path: "/v1/admin/accounts" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        ...searchShape,
        status: z.enum(["active", "disabled"]).optional(),
      }),
    )
    .output(
      z.object({
        accounts: z.array(adminAccountSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  account: base
    .route({ method: "GET", path: "/v1/admin/accounts/{accountId}" })
    .meta(operator)
    .errors(adminErrors)
    .input(z.object({ accountId: z.string() }))
    .output(adminAccountSchema),
  /** 停用：拒绝登录与断言、撤销全部设备、踢掉连接。 */
  accountStatus: base
    .route({ method: "PUT", path: "/v1/admin/accounts/{accountId}/status" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        accountId: z.string(),
        status: z.enum(["active", "disabled"]),
        reason: statusReason,
      }),
    )
    .output(adminAccountSchema),
  teams: base
    .route({ method: "GET", path: "/v1/admin/teams" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        ...searchShape,
        status: z.enum(["active", "suspended"]).optional(),
      }),
    )
    .output(
      z.object({
        teams: z.array(adminTeamSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  team: base
    .route({ method: "GET", path: "/v1/admin/teams/{teamId}" })
    .meta(operator)
    .errors(adminErrors)
    .input(z.object({ teamId: teamIdSchema }))
    .output(adminTeamSchema),
  /** 停用：拒绝签断言与登记，踢掉连接。 */
  teamStatus: base
    .route({ method: "PUT", path: "/v1/admin/teams/{teamId}/status" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        teamId: teamIdSchema,
        status: z.enum(["active", "suspended"]),
        reason: statusReason,
      }),
    )
    .output(adminTeamSchema),
  relayNodes: base
    .route({ method: "GET", path: "/v1/admin/relay-nodes" })
    .meta(operator)
    .errors(adminErrors)
    .input(empty)
    .output(z.object({ nodes: z.array(adminRelayNodeSchema) })),
  /** 排空：隧道收到 GOAWAY rebalance，源重连到别的节点。 */
  relayNodeDrain: base
    .route({ method: "PUT", path: "/v1/admin/relay-nodes/{node}/drain" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        node: relayNodeIdSchema,
        drain: z.boolean(),
        /** 排空理由，写进审计（0.11.0 起，只加不改）。 */
        reason: statusReason.optional(),
      }),
    )
    .output(adminRelayNodeSchema),
  incident: base
    .route({ method: "GET", path: "/v1/admin/incident" })
    .meta(operator)
    .errors(adminErrors)
    .input(empty)
    .output(adminIncidentSchema),
  incidentSet: base
    .route({ method: "PUT", path: "/v1/admin/incident" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        pauseAssertions: z.boolean().optional(),
        pauseRegistrations: z.boolean().optional(),
        /** 0.11.0 起：中继节点拒绝新的隧道与端到端升级，在线连接保留。 */
        rejectNewConnections: z.boolean().optional(),
        reason: statusReason,
      }),
    )
    .output(adminIncidentSchema),
  /** 对外状态页的发布（M2）：改组件状态与公告，`status.get` 立即反映。 */
  statusPublish: base
    .route({ method: "PUT", path: "/v1/admin/status" })
    .meta(operator)
    .errors({ ...adminErrors, ...errors.pick("bad_request") })
    .input(
      z.object({
        components: z
          .array(
            z.object({
              id: z.string().min(1).max(64),
              state: statusComponentStateSchema,
            }),
          )
          .min(1),
        notice: z.string().max(1000).nullable().optional(),
        reason: statusReason,
      }),
    )
    .output(publicStatusSchema),
  /** 滥用信号（流量异常、举报）：链接或主机，可按是否已冻结过滤（M2）。 */
  abuseList: base
    .route({ method: "GET", path: "/v1/admin/abuse" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        signal: z.enum(["traffic", "report"]).optional(),
        frozen: z.enum(["true", "false"]).optional(),
        ...pageInputShape,
      }),
    )
    .output(
      z.object({
        reports: z.array(abuseReportSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  /** 冻结链接：之后 `links.accept` 答 `403 link_frozen`，已进来的访客被踢（M2）。 */
  linkFreeze: base
    .route({ method: "PUT", path: "/v1/admin/links/{linkId}/frozen" })
    .meta(operator)
    .errors(adminErrors)
    .input(
      z.object({
        linkId: z.string(),
        frozen: z.boolean(),
        reason: statusReason,
      }),
    )
    .output(empty),
  /** 功能开关：全部团队开，或灰度到指定团队（M2）。 */
  flagsList: base
    .route({ method: "GET", path: "/v1/admin/flags" })
    .meta(operator)
    .errors(adminErrors)
    .input(empty)
    .output(z.object({ flags: z.array(featureFlagSchema) })),
  flagsSet: base
    .route({ method: "PUT", path: "/v1/admin/flags/{flag}" })
    .meta(operator)
    .errors({ ...adminErrors, ...errors.pick("bad_request") })
    .input(featureFlagSchema.extend({ reason: statusReason }))
    .output(featureFlagSchema),
};

/** 对外服务状态（cloud-api §28，M2）：匿名可读，状态页与控制台都用它。 */
export const status = {
  get: base
    .route({ method: "GET", path: "/v1/status" })
    .meta({ scope: ["anonymous"], since: SINCE_0_10 })
    .errors(errors.pick("rate_limited"))
    .input(empty)
    .output(publicStatusSchema),
};
