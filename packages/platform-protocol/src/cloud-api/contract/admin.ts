import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import {
  nextCursorSchema,
  pageInputShape,
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
});

export const adminRelayNodeSchema = z.object({
  node: relayNodeIdSchema,
  region: z.string().optional(),
  online: z.boolean(),
  /** 在线隧道数。 */
  tunnels: z.number().int().nonnegative(),
  draining: z.boolean(),
  version: z.string(),
  lastSeenAtMs: z.number().int().nullable(),
});

/** 事故开关：一键停签断言、停注册；中继侧拒绝新连接、保留在线连接。 */
export const adminIncidentSchema = z.object({
  pauseAssertions: z.boolean(),
  pauseRegistrations: z.boolean(),
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
    .input(z.object({ node: relayNodeIdSchema, drain: z.boolean() }))
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
        reason: statusReason,
      }),
    )
    .output(adminIncidentSchema),
};
