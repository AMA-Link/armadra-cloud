import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import {
  apiTokenSchema,
  apiTokenScopeSchema,
  teamIdSchema,
} from "../saas-schemas.ts";

/** 个人令牌本人建；团队令牌要团队 admin（`teamId` 在体里，所以两个 scope 满足其一）。 */
const maker = { scope: ["session", "team:admin"], since: SINCE_0_10 } as const;

/**
 * API 令牌（cloud-api §27）：个人访问令牌或属于团队的令牌（不随某个人离开而失效），可限定
 * 范围与到期。令牌明文只在创建时返回一次，库里只存哈希。API 令牌本身不能再建 API 令牌。
 * 仅 saas，预留（M2）。
 */
export const apiTokens = {
  create: base
    .route({ method: "POST", path: "/v1/api-tokens" })
    .meta(maker)
    .errors(
      errors.pick("team_role_insufficient", "limit_reached", "bad_request"),
    )
    .input(
      z.object({
        name: z.string().min(1).max(128),
        /** 缺省 = 个人令牌。 */
        teamId: teamIdSchema.optional(),
        scopes: z.array(apiTokenScopeSchema).min(1),
        expiresAtMs: z.number().int().optional(),
      }),
    )
    .output(z.object({ token: z.string(), apiToken: apiTokenSchema })),
  list: base
    .route({ method: "GET", path: "/v1/api-tokens" })
    .meta(maker)
    .errors(errors.pick("team_role_insufficient"))
    .input(z.object({ teamId: teamIdSchema.optional() }))
    .output(z.object({ apiTokens: z.array(apiTokenSchema) })),
  revoke: base
    .route({ method: "DELETE", path: "/v1/api-tokens/{tokenId}" })
    .meta(maker)
    .errors(errors.pick("not_found", "team_role_insufficient"))
    .input(z.object({ tokenId: z.string() }))
    .output(empty),
};
