import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { sourceIdSchema } from "../schemas.ts";
import {
  AUDIT_EXPORT_FORMATS,
  auditEventSchema,
  msQuery,
  nextCursorSchema,
  pageInputShape,
  teamIdSchema,
} from "../saas-schemas.ts";

const admin = { scope: ["team:admin"], since: SINCE_0_10 } as const;
const auditErrors = errors.pick(
  "not_found",
  "team_role_insufficient",
  "bad_request",
);

const filters = {
  teamId: teamIdSchema,
  /** 含：事件时间 ≥ fromMs。 */
  fromMs: msQuery.optional(),
  /** 不含：事件时间 < toMs。 */
  toMs: msQuery.optional(),
  actor: z.string().optional(),
  sourceId: sourceIdSchema.optional(),
  /** 动作名前缀，如 `grant.`。 */
  action: z.string().max(64).optional(),
};

/**
 * 团队审计（cloud-api §25）：成员、分组、主机、授予、链接、设置、计划变更，以及每次断言签发
 * （按 `jti`）。只记主体、动作、对象、时间与来源 IP 的前缀，不记内容与令牌。账号本人的安全
 * 日志是 `me.securityLog`（§29）。仅 saas，预留。
 */
export const audit = {
  list: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/audit" })
    .meta(admin)
    .errors(auditErrors)
    .input(z.object({ ...filters, ...pageInputShape }))
    .output(
      z.object({
        events: z.array(auditEventSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  /** 同样的筛选，一次最多 `AUDIT_EXPORT_MAX_ROWS` 行；`content` 是序列化后的文本。 */
  export: base
    .route({ method: "GET", path: "/v1/teams/{teamId}/audit/export" })
    .meta(admin)
    .errors(auditErrors)
    .input(
      z.object({
        ...filters,
        format: z.enum(AUDIT_EXPORT_FORMATS).default("json"),
      }),
    )
    .output(
      z.object({
        format: z.enum(AUDIT_EXPORT_FORMATS),
        rows: z.number().int().nonnegative(),
        content: z.string(),
      }),
    ),
};
