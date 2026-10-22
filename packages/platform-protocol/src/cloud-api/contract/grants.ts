import { z } from "zod";

import { grantRoleSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty, sourceIdSchema } from "../schemas.ts";
import {
  accessRequestSchema,
  accessRequestStatusSchema,
  createGrantInputSchema,
  effectiveAccessSchema,
  grantSchema,
  teamIdSchema,
} from "../saas-schemas.ts";

const session = { scope: ["session"], since: SINCE_0_10 } as const;
/** 主机 owner 或（团队主机的）团队 admin。 */
const manager = { scope: ["owner", "team:admin"], since: SINCE_0_10 } as const;

const grantErrors = errors.pick(
  "not_found",
  "source_access_denied",
  "team_role_insufficient",
);

/**
 * 主机授予（cloud-api §24）：谁以什么角色、什么范围到达哪台主机。云端只合成意图，断言把结果
 * 带进 core，最终判定仍在 core。撤销即生效：写撤销表（`jti`）、踢在线连接。仅 saas，预留。
 */
export const grants = {
  /** 按主机（`sourceId`）或按主体（`accountId` / `groupId`）列；至少给一个，否则 `400 bad_request`。 */
  list: base
    .route({ method: "GET", path: "/v1/grants" })
    .meta(manager)
    .errors({ ...grantErrors, ...errors.pick("bad_request") })
    .input(
      z.object({
        sourceId: sourceIdSchema.optional(),
        accountId: z.string().optional(),
        groupId: z.string().optional(),
      }),
    )
    .output(z.object({ grants: z.array(grantSchema) })),
  create: base
    .route({ method: "POST", path: "/v1/grants" })
    .meta(manager)
    .errors({
      ...grantErrors,
      ...errors.pick(
        "team_policy_denied",
        "email_unverified",
        "limit_reached",
        "conflict",
        "bad_request",
      ),
    })
    .input(createGrantInputSchema)
    .output(grantSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/grants/{grantId}" })
    .meta(manager)
    .errors({ ...grantErrors, ...errors.pick("bad_request") })
    .input(
      z.object({
        grantId: z.string(),
        role: grantRoleSchema.optional(),
        workspaceIds: z.array(z.string().min(1).max(128)).max(64).optional(),
        /** `null` = 清除到期。 */
        expiresAtMs: z.number().int().nullable().optional(),
      }),
    )
    .output(grantSchema),
  revoke: base
    .route({ method: "DELETE", path: "/v1/grants/{grantId}" })
    .meta(manager)
    .errors(grantErrors)
    .input(z.object({ grantId: z.string() }))
    .output(empty),
  /** 某人对某主机的有效权限：多条授予取最高角色、范围取并集，并列出各来源。 */
  effective: base
    .route({ method: "GET", path: "/v1/grants/effective" })
    .meta(manager)
    .errors(grantErrors)
    .input(z.object({ sourceId: sourceIdSchema, accountId: z.string() }))
    .output(effectiveAccessSchema),
};

/**
 * 访问申请（cloud-api §24）：成员对看得到但不可达的主机申请访问，主机 owner 或团队 admin
 * 批准或拒绝。仅 saas，预留（M2）。
 */
export const accessRequests = {
  create: base
    .route({ method: "POST", path: "/v1/access-requests" })
    .meta(session)
    .errors(
      errors.pick(
        "not_found",
        "conflict",
        "limit_reached",
        "email_unverified",
        "rate_limited",
        "bad_request",
      ),
    )
    .input(
      z.object({
        sourceId: sourceIdSchema,
        role: grantRoleSchema,
        workspaceIds: z.array(z.string().min(1).max(128)).max(64).optional(),
        reason: z.string().max(500).default(""),
      }),
    )
    .output(accessRequestSchema),
  /** `box = inbox`（缺省）是自己能处理的，`mine` 是自己提的。 */
  list: base
    .route({ method: "GET", path: "/v1/access-requests" })
    .meta(session)
    .input(
      z.object({
        box: z.enum(["inbox", "mine"]).default("inbox"),
        teamId: teamIdSchema.optional(),
        sourceId: sourceIdSchema.optional(),
        status: accessRequestStatusSchema.optional(),
      }),
    )
    .output(z.object({ requests: z.array(accessRequestSchema) })),
  /** 批准时可以改小角色 / 范围、设到期；答出新建的授予。 */
  approve: base
    .route({ method: "POST", path: "/v1/access-requests/{requestId}/approve" })
    .meta(manager)
    .errors({
      ...grantErrors,
      ...errors.pick("conflict", "team_policy_denied", "limit_reached"),
    })
    .input(
      z.object({
        requestId: z.string(),
        role: grantRoleSchema.optional(),
        workspaceIds: z.array(z.string().min(1).max(128)).max(64).optional(),
        expiresAtMs: z.number().int().optional(),
      }),
    )
    .output(z.object({ request: accessRequestSchema, grant: grantSchema })),
  deny: base
    .route({ method: "POST", path: "/v1/access-requests/{requestId}/deny" })
    .meta(manager)
    .errors({ ...grantErrors, ...errors.pick("conflict") })
    .input(
      z.object({
        requestId: z.string(),
        reason: z.string().max(500).optional(),
      }),
    )
    .output(accessRequestSchema),
};
