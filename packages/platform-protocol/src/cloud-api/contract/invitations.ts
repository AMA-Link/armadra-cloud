import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10, SINCE_0_11 } from "../meta.ts";
import { empty, sourceIdSchema } from "../schemas.ts";
import {
  createInvitationInputSchema,
  invitationPreviewSchema,
  invitationSchema,
  teamIdSchema,
} from "../saas-schemas.ts";

const inviter = { scope: ["team:admin", "owner"], since: SINCE_0_10 } as const;
const session = { scope: ["session"], since: SINCE_0_10 } as const;

const invitationErrors = errors.pick(
  "not_found",
  "invitation_expired",
  "invitation_revoked",
);

/**
 * 邀请（cloud-api §22）：团队邀请（按邮箱、指定角色、7 天）与主机邀请（接受后成为该主机的
 * guest 授予）。仅 saas，预留。邮件里的链接带一次性 `token`，匿名 `get` 看摘要，接受要登录且
 * 邮箱一致。
 */
export const invitations = {
  /** 团队邀请要团队 admin；主机邀请要主机 owner（团队主机也可以由团队 admin 发）。 */
  create: base
    .route({ method: "POST", path: "/v1/invitations" })
    .meta(inviter)
    .errors(
      errors.pick(
        "team_role_insufficient",
        "source_access_denied",
        "team_policy_denied",
        "guests_disabled",
        "member_exists",
        "email_unverified",
        "limit_reached",
        "conflict",
        "rate_limited",
        "bad_request",
      ),
    )
    .input(createInvitationInputSchema)
    .output(invitationSchema),
  /**
   * `box = sent`（缺省）列自己能管的邀请，可按 `teamId` / `sourceId` 过滤；`received` 列发给
   * 自己邮箱的待处理邀请。
   */
  list: base
    .route({ method: "GET", path: "/v1/invitations" })
    .meta(session)
    .errors(
      errors.pick(
        "team_role_insufficient",
        "source_access_denied",
        "not_found",
      ),
    )
    .input(
      z.object({
        box: z.enum(["sent", "received"]).default("sent"),
        teamId: teamIdSchema.optional(),
        sourceId: sourceIdSchema.optional(),
      }),
    )
    .output(z.object({ invitations: z.array(invitationSchema) })),
  revoke: base
    .route({ method: "DELETE", path: "/v1/invitations/{invitationId}" })
    .meta(inviter)
    .errors(
      errors.pick(
        "not_found",
        "team_role_insufficient",
        "source_access_denied",
      ),
    )
    .input(z.object({ invitationId: z.string() }))
    .output(empty),
  /** 重发邀请邮件：换新令牌、重新计 7 天，旧链接作废；按邀请限流。 */
  resend: base
    .route({ method: "POST", path: "/v1/invitations/{invitationId}/resend" })
    .meta(inviter)
    .errors(
      errors.pick(
        "not_found",
        "team_role_insufficient",
        "invitation_revoked",
        "source_access_denied",
        "bad_request",
        "conflict",
        "rate_limited",
      ),
    )
    .input(z.object({ invitationId: z.string() }))
    .output(invitationSchema),
  /**
   * 已弃用，保留兼容：`?token=` 会进代理与访问日志，客户端改用 `preview`（令牌放请求体）。
   */
  get: base
    .route({ method: "GET", path: "/v1/invitations/{invitationId}" })
    .meta({ scope: ["anonymous"], since: SINCE_0_10 })
    .errors({ ...invitationErrors, ...errors.pick("rate_limited") })
    .input(z.object({ invitationId: z.string(), token: z.string().min(1) }))
    .output(invitationPreviewSchema),
  /** 同 `get`，令牌放请求体、不进 URL（0.11.0）。 */
  preview: base
    .route({ method: "POST", path: "/v1/invitations/{invitationId}/preview" })
    .meta({ scope: ["anonymous"], since: SINCE_0_11 })
    .errors({ ...invitationErrors, ...errors.pick("rate_limited") })
    .input(z.object({ invitationId: z.string(), token: z.string().min(1) }))
    .output(invitationPreviewSchema),
  accept: base
    .route({ method: "POST", path: "/v1/invitations/{invitationId}/accept" })
    .meta(session)
    .errors({
      ...invitationErrors,
      ...errors.pick(
        "invitation_email_mismatch",
        "email_unverified",
        "limit_reached",
        "team_policy_denied",
        "member_exists",
        "guests_disabled",
        "team_deleted",
        "conflict",
        "rate_limited",
      ),
    })
    .input(z.object({ invitationId: z.string(), token: z.string().min(1) }))
    .output(
      z.object({
        teamId: teamIdSchema.optional(),
        sourceId: sourceIdSchema.optional(),
      }),
    ),
  decline: base
    .route({ method: "POST", path: "/v1/invitations/{invitationId}/decline" })
    .meta(session)
    .errors({
      ...invitationErrors,
      ...errors.pick("invitation_email_mismatch"),
    })
    .input(z.object({ invitationId: z.string(), token: z.string().min(1) }))
    .output(empty),
};
