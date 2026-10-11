import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1, SINCE_0_2 } from "../meta.ts";
import {
  challengeTokenSchema,
  deviceInputSchema,
  linkKindSchema,
  linkScopeSchema,
  linkSummarySchema,
  sessionSchema,
  sourceIdSchema,
} from "../schemas.ts";

export const createLinkInputSchema = z.discriminatedUnion("kind", [
  z.object({
    kind: z.literal("source_invite"),
    sourceId: sourceIdSchema,
    /** core 邀请的 id（真相在 core）。 */
    invitationId: z.string(),
    label: z.string().max(128),
    role: z.string(),
    expiresAtMs: z.number().int(),
    maxUses: z.number().int().positive().optional(),
    /** 0.3.0 起（能力 `links.scope`）；缺省 = 整台、按角色。 */
    scope: linkScopeSchema.optional(),
  }),
  z.object({
    kind: z.literal("org_join"),
    orgId: z.string(),
    label: z.string().max(128),
    expiresAtMs: z.number().int(),
    maxUses: z.number().int().positive().optional(),
    sources: z.array(sourceIdSchema).optional(),
  }),
]);

export const links = {
  create: base
    .route({ method: "POST", path: "/v1/links" })
    .meta({ scope: ["owner", "org:admin"], since: SINCE_0_1 })
    .errors(
      errors.pick(
        "source_access_denied",
        "org_role_insufficient",
        "bad_request",
      ),
    )
    .input(createLinkInputSchema)
    .output(
      z.object({
        linkId: z.string(),
        url: z.string(),
        /** 只有 org_join 与 personal 的 source_invite 返回。 */
        secret: z.string().optional(),
        expiresAtMs: z.number().int(),
      }),
    ),
  get: base
    .route({ method: "GET", path: "/v1/links/{linkId}" })
    .meta({ scope: ["anonymous"], since: SINCE_0_1 })
    .errors(errors.pick("link_invalid"))
    .input(z.object({ linkId: z.string() }))
    .output(
      z.object({
        kind: linkKindSchema,
        label: z.string(),
        role: z.string().optional(),
        sourceName: z.string().optional(),
        orgName: z.string().optional(),
        /** 0.3.0 起；整台时不出现。 */
        scope: linkScopeSchema.optional(),
        expiresAtMs: z.number().int(),
        exhausted: z.boolean(),
        requiresAccount: z.boolean(),
      }),
    ),
  /** saas 要云会话；personal 的访客匿名调用。 */
  accept: base
    .route({ method: "POST", path: "/v1/links/{linkId}/accept" })
    .meta({ scope: ["session", "anonymous"], since: SINCE_0_1 })
    .errors(
      errors.pick(
        "link_invalid",
        "link_expired",
        "link_exhausted",
        "link_secret_invalid",
        "rate_limited",
        "challenge_required",
        "challenge_invalid",
      ),
    )
    .input(
      z.object({
        linkId: z.string(),
        secret: z.string().optional(),
        device: deviceInputSchema.optional(),
        /** 挑战令牌（cloud-api §16，0.3.5 起）：`platform.info.challenge.scope` 含 `links.accept` 时必带。 */
        challenge: challengeTokenSchema.optional(),
      }),
    )
    .output(
      z.object({
        sourceId: sourceIdSchema.optional(),
        relayOrigin: z.string().optional(),
        relayBaseUrl: z.string().optional(),
        assertion: z.string().optional(),
        relayToken: z.string().optional(),
        orgId: z.string().optional(),
        /**
         * 源端到端通道的身份公钥指纹（`base32(SHA-256(pub))`，52 字符；cloud-api §20，
         * 0.5.0 起）。只是提示：访客按链接片段第三段（源自己给的指纹前缀）钉扎。
         */
        identityFp: z.string().optional(),
        guestSession: sessionSchema.optional(),
      }),
    ),
  list: base
    .route({ method: "GET", path: "/v1/links" })
    .meta({ scope: ["owner", "org:admin"], since: SINCE_0_1 })
    .input(
      z.object({
        sourceId: sourceIdSchema.optional(),
        orgId: z.string().optional(),
      }),
    )
    .output(z.object({ links: z.array(linkSummarySchema) })),
  /** 改备注（`label`）；已撤销的链接也能改，别的字段不动。 */
  update: base
    .route({ method: "PATCH", path: "/v1/links/{linkId}" })
    .meta({ scope: ["owner", "org:admin"], since: SINCE_0_2 })
    .errors(errors.pick("not_found"))
    .input(z.object({ linkId: z.string(), label: z.string().max(128) }))
    .output(linkSummarySchema),
  revoke: base
    .route({ method: "DELETE", path: "/v1/links/{linkId}" })
    .meta({ scope: ["owner", "org:admin"], since: SINCE_0_1 })
    .errors(errors.pick("not_found"))
    .input(z.object({ linkId: z.string() }))
    .output(z.object({})),
};
