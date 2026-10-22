import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import {
  periodSchema,
  teamIdSchema,
  teamMemberSchema,
  teamNameSchema,
  teamSchema,
  teamSettingsSchema,
  teamSlugSchema,
  teamUsageSchema,
  teamRoleSchema,
} from "../saas-schemas.ts";

const session = { scope: ["session"], since: SINCE_0_10 } as const;
const member = { scope: ["team:member"], since: SINCE_0_10 } as const;
const admin = { scope: ["team:admin"], since: SINCE_0_10 } as const;
const owner = { scope: ["team:owner"], since: SINCE_0_10 } as const;

const teamErrors = errors.pick("not_found", "team_role_insufficient");
const teamId = { teamId: teamIdSchema };

/**
 * 团队（cloud-api §21）：租户。仅 saas，预留。角色表见设计 `saas-design.md` §2.1：`scope` 写的是
 * 调用者在路径里那个团队的最低角色。
 */
export const teams = {
  /** 创建者是 owner；新团队用 Free 计划。 */
  create: base
    .route({ method: "POST", path: "/v1/teams" })
    .meta(session)
    .errors(
      errors.pick(
        "team_slug_taken",
        "bad_request",
        "email_unverified",
        "limit_reached",
      ),
    )
    .input(z.object({ slug: teamSlugSchema, name: teamNameSchema }))
    .output(teamSchema),
  list: base
    .route({ method: "GET", path: "/v1/teams" })
    .meta(session)
    .input(empty)
    .output(z.object({ teams: z.array(teamSchema) })),
  get: base
    .route({ method: "GET", path: "/v1/teams/{teamId}" })
    .meta(member)
    .errors(errors.pick("not_found"))
    .input(z.object(teamId))
    .output(teamSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/teams/{teamId}" })
    .meta(admin)
    .errors(teamErrors)
    .input(
      z.object({
        ...teamId,
        name: teamNameSchema.optional(),
        avatarUrl: z.string().max(2048).nullable().optional(),
      }),
    )
    .output(teamSchema),
  /** 先撤销全部主机或把主机转走；进 30 天恢复期，之后清除。 */
  delete: base
    .route({ method: "DELETE", path: "/v1/teams/{teamId}" })
    .meta(owner)
    .errors({ ...teamErrors, ...errors.pick("conflict") })
    .input(z.object(teamId))
    .output(z.object({ purgeAtMs: z.number().int() })),
  restore: base
    .route({ method: "POST", path: "/v1/teams/{teamId}/restore" })
    .meta(owner)
    .errors(teamErrors)
    .input(z.object(teamId))
    .output(teamSchema),
  /** 目标必须是 admin；原 owner 降为 admin。 */
  transferOwnership: base
    .route({ method: "POST", path: "/v1/teams/{teamId}/transfer-ownership" })
    .meta(owner)
    .errors({ ...teamErrors, ...errors.pick("bad_request") })
    .input(z.object({ ...teamId, accountId: z.string() }))
    .output(empty),
  members: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/members" })
    .meta(member)
    .errors(teamErrors)
    .input(z.object(teamId))
    .output(z.object({ members: z.array(teamMemberSchema) })),
  /** 不能动 owner；最后一个 owner 不能降级。 */
  memberRole: base
    .route({ method: "PATCH", path: "/v1/teams/{teamId}/members/{accountId}" })
    .meta(admin)
    .errors({ ...teamErrors, ...errors.pick("team_owner_required") })
    .input(
      z.object({
        ...teamId,
        accountId: z.string(),
        role: teamRoleSchema.exclude(["owner"]),
      }),
    )
    .output(teamMemberSchema),
  /** 同时撤销此人的全部团队授予并踢掉在线连接。 */
  memberRemove: base
    .route({ method: "DELETE", path: "/v1/teams/{teamId}/members/{accountId}" })
    .meta(admin)
    .errors({ ...teamErrors, ...errors.pick("team_owner_required") })
    .input(z.object({ ...teamId, accountId: z.string() }))
    .output(empty),
  leave: base
    .route({ method: "POST", path: "/v1/teams/{teamId}/leave" })
    .meta(member)
    .errors({ ...teamErrors, ...errors.pick("team_owner_required") })
    .input(z.object(teamId))
    .output(empty),
  settings: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/settings" })
    .meta(member)
    .errors(teamErrors)
    .input(z.object(teamId))
    .output(teamSettingsSchema),
  settingsUpdate: base
    .route({ method: "PATCH", path: "/v1/teams/{teamId}/settings" })
    .meta(admin)
    .errors({ ...teamErrors, ...errors.pick("bad_request") })
    .input(z.object({ ...teamId, ...teamSettingsSchema.partial().shape }))
    .output(teamSettingsSchema),
  /** 主机数、成员数、guest 数、同时连接数、本月流量，对照当前计划的上限。 */
  usage: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/usage" })
    .meta(member)
    .errors(teamErrors)
    .input(z.object({ ...teamId, period: periodSchema.optional() }))
    .output(teamUsageSchema),
};
