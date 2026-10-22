import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import {
  groupMemberSchema,
  groupSchema,
  teamIdSchema,
} from "../saas-schemas.ts";

const member = { scope: ["team:member"], since: SINCE_0_10 } as const;
const admin = { scope: ["team:admin"], since: SINCE_0_10 } as const;
/** 组长可以管理本组成员。 */
const lead = {
  scope: ["team:admin", "group:lead"],
  since: SINCE_0_10,
} as const;

const groupErrors = errors.pick("not_found", "team_role_insufficient");
const ids = { teamId: teamIdSchema, groupId: z.string() };
const name = z.string().min(1).max(128);

/**
 * 分组（cloud-api §23）：团队内的一组成员，授予的主体之一。组员必须是团队成员（不含 guest）。
 * 仅 saas，预留（M2）。
 */
export const groups = {
  list: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/groups" })
    .meta(member)
    .errors(groupErrors)
    .input(z.object({ teamId: teamIdSchema }))
    .output(z.object({ groups: z.array(groupSchema) })),
  create: base
    .route({ method: "POST", path: "/v1/teams/{teamId}/groups" })
    .meta(admin)
    .errors({
      ...groupErrors,
      ...errors.pick("limit_reached", "plan_feature_unavailable", "conflict"),
    })
    .input(z.object({ teamId: teamIdSchema, name }))
    .output(groupSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/teams/{teamId}/groups/{groupId}" })
    .meta(admin)
    .errors({ ...groupErrors, ...errors.pick("conflict") })
    .input(z.object({ ...ids, name }))
    .output(groupSchema),
  /** 同时撤销以它为主体的授予。 */
  delete: base
    .route({ method: "DELETE", path: "/v1/teams/{teamId}/groups/{groupId}" })
    .meta(admin)
    .errors(groupErrors)
    .input(z.object(ids))
    .output(empty),
  members: base
    .route({
      method: "GET",
      path: "/v1/teams/{teamId}/groups/{groupId}/members",
    })
    .meta(member)
    .errors(groupErrors)
    .input(z.object(ids))
    .output(z.object({ members: z.array(groupMemberSchema) })),
  memberAdd: base
    .route({
      method: "POST",
      path: "/v1/teams/{teamId}/groups/{groupId}/members",
    })
    .meta(lead)
    .errors({ ...groupErrors, ...errors.pick("bad_request", "conflict") })
    .input(z.object({ ...ids, accountId: z.string() }))
    .output(groupMemberSchema),
  memberRemove: base
    .route({
      method: "DELETE",
      path: "/v1/teams/{teamId}/groups/{groupId}/members/{accountId}",
    })
    .meta(lead)
    .errors(groupErrors)
    .input(z.object({ ...ids, accountId: z.string() }))
    .output(empty),
  setLead: base
    .route({
      method: "PUT",
      path: "/v1/teams/{teamId}/groups/{groupId}/members/{accountId}/lead",
    })
    .meta(admin)
    .errors(groupErrors)
    .input(z.object({ ...ids, accountId: z.string(), isLead: z.boolean() }))
    .output(groupMemberSchema),
};
