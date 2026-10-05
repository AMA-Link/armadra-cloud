import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import {
  empty,
  orgMemberSchema,
  orgRoleSchema,
  orgSchema,
  sourceIdSchema,
  sourceSummarySchema,
} from "../schemas.ts";

const session = { scope: ["session"], since: SINCE_0_1 } as const;
const admin = { scope: ["org:admin"], since: SINCE_0_1 } as const;
const orgOwner = { scope: ["org:owner"], since: SINCE_0_1 } as const;
const orgId = z.string();
const notFound = errors.pick("not_found", "org_role_insufficient");

/** 仅 saas。 */
export const orgs = {
  create: base
    .route({ method: "POST", path: "/v1/orgs" })
    .meta(session)
    .errors(errors.pick("org_slug_taken", "bad_request"))
    .input(z.object({ slug: orgSchema.shape.slug, name: orgSchema.shape.name }))
    .output(orgSchema),
  get: base
    .route({ method: "GET", path: "/v1/orgs/{orgId}" })
    .meta(session)
    .errors(errors.pick("not_found"))
    .input(z.object({ orgId }))
    .output(orgSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/orgs/{orgId}" })
    .meta(admin)
    .errors(notFound)
    .input(z.object({ orgId, name: orgSchema.shape.name.optional() }))
    .output(orgSchema),
  delete: base
    .route({ method: "DELETE", path: "/v1/orgs/{orgId}" })
    .meta(orgOwner)
    .errors(notFound)
    .input(z.object({ orgId }))
    .output(empty),
  members: base
    .route({ method: "GET", path: "/v1/orgs/{orgId}/members" })
    .meta(session)
    .errors(notFound)
    .input(z.object({ orgId }))
    .output(z.object({ members: z.array(orgMemberSchema) })),
  memberRole: base
    .route({ method: "PATCH", path: "/v1/orgs/{orgId}/members/{accountId}" })
    .meta(admin)
    .errors(notFound)
    .input(z.object({ orgId, accountId: z.string(), role: orgRoleSchema }))
    .output(orgMemberSchema),
  memberRemove: base
    .route({ method: "DELETE", path: "/v1/orgs/{orgId}/members/{accountId}" })
    .meta(admin)
    .errors(notFound)
    .input(z.object({ orgId, accountId: z.string() }))
    .output(empty),
  sources: base
    .route({ method: "GET", path: "/v1/orgs/{orgId}/sources" })
    .meta(session)
    .errors(notFound)
    .input(z.object({ orgId }))
    .output(z.object({ sources: z.array(sourceSummarySchema) })),
  /** 调用方须是源的 owner 且是组织成员。 */
  sourceAttach: base
    .route({ method: "POST", path: "/v1/orgs/{orgId}/sources" })
    .meta({ scope: ["owner"], since: SINCE_0_1 })
    .errors({ ...notFound, ...errors.pick("source_access_denied") })
    .input(z.object({ orgId, sourceId: sourceIdSchema }))
    .output(empty),
  sourceDetach: base
    .route({ method: "DELETE", path: "/v1/orgs/{orgId}/sources/{sourceId}" })
    .meta({ scope: ["owner", "org:admin"], since: SINCE_0_1 })
    .errors(notFound)
    .input(z.object({ orgId, sourceId: sourceIdSchema }))
    .output(empty),
};
