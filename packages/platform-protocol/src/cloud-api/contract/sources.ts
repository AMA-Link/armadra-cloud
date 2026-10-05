import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { tunnelLimitsSchema } from "../../tunnel/handshake.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import {
  deviceInputSchema,
  ed25519PublicJwkSchema,
  empty,
  protocolVersionSchema,
  sourceDetailSchema,
  sourceIdSchema,
  sourceKindSchema,
  sourceSummarySchema,
  sourceViaSchema,
} from "../schemas.ts";

const session = { scope: ["session"], since: SINCE_0_1 } as const;
const owner = { scope: ["owner"], since: SINCE_0_1 } as const;

export const relayNodeSchema = z.object({
  url: z.string().regex(/^wss?:\/\//),
  region: z.string().optional(),
  weight: z.number().int().positive(),
});

export const sourceAssertionOutputSchema = z.object({
  assertion: z.string(),
  assertionExpiresAtMs: z.number().int(),
  relayToken: z.string(),
  relayTokenExpiresAtMs: z.number().int(),
  relayOrigin: z.string(),
  /** saas：`https://<sourceId>.src.<relay 域>`；personal：`https://<relay>/s/<sourceId>`。客户端只用它，不自己拼。 */
  relayBaseUrl: z.string(),
  online: z.boolean(),
});

export const sourceAccessSchema = z.object({
  accountId: z.string(),
  email: z.string().nullable(),
  via: sourceViaSchema,
  linkId: z.string().nullable(),
  grantedAtMs: z.number().int(),
});

export const sources = {
  registrationToken: base
    .route({ method: "POST", path: "/v1/sources/registration-tokens" })
    .meta(session)
    .errors(
      errors.pick("email_unverified", "limit_reached", "org_role_insufficient"),
    )
    .input(z.object({ orgId: z.string().optional() }))
    .output(
      z.object({
        registrationToken: z.string(),
        expiresAtMs: z.number().int(),
        issuer: z.string(),
      }),
    ),
  /** 匿名 + 一次性注册令牌（10 分钟）；core 调。 */
  register: base
    .route({ method: "POST", path: "/v1/sources/register" })
    .meta({ scope: ["anonymous"], since: SINCE_0_1 })
    .errors(
      errors.pick(
        "registration_token_invalid",
        "source_taken",
        "protocol_unsupported",
        "rate_limited",
      ),
    )
    .input(
      z.object({
        registrationToken: z.string(),
        sourceId: sourceIdSchema,
        publicKey: ed25519PublicJwkSchema,
        name: z.string().min(1).max(128),
        kind: sourceKindSchema,
        coreVersion: z.string(),
        capabilities: z.array(z.string()),
        protocol: protocolVersionSchema,
      }),
    )
    .output(
      z.object({
        issuer: z.string(),
        jwksUrl: z.string(),
        relayOrigins: z.array(z.string()),
        trustedOrigins: z.array(z.string()),
        sourceId: sourceIdSchema,
        ownerAccountId: z.string(),
      }),
    ),
  /** 源 JWS（`Authorization: Source <jws>`）换隧道令牌与节点列表。 */
  heartbeatHint: base
    .route({ method: "GET", path: "/v1/sources/me/relay" })
    .meta({ scope: ["source"], since: SINCE_0_1 })
    .errors(errors.pick("source_revoked", "unauthenticated"))
    .input(empty)
    .output(
      z.object({
        tunnelToken: z.string(),
        expiresAtMs: z.number().int(),
        nodes: z.array(relayNodeSchema).min(1),
        limits: tunnelLimitsSchema,
      }),
    ),
  list: base
    .route({ method: "GET", path: "/v1/sources" })
    .meta(session)
    .input(z.object({ orgId: z.string().optional() }))
    .output(z.object({ sources: z.array(sourceSummarySchema) })),
  get: base
    .route({ method: "GET", path: "/v1/sources/{sourceId}" })
    .meta(session)
    .errors(errors.pick("not_found"))
    .input(z.object({ sourceId: sourceIdSchema }))
    .output(sourceDetailSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/sources/{sourceId}" })
    .meta(owner)
    .errors(errors.pick("not_found", "bad_request"))
    .input(
      z.object({
        sourceId: sourceIdSchema,
        name: z.string().min(1).max(128).optional(),
        trustedOrigins: z.array(z.string()).optional(),
      }),
    )
    .output(sourceDetailSchema),
  /** 踢隧道、撤令牌。 */
  revoke: base
    .route({ method: "DELETE", path: "/v1/sources/{sourceId}" })
    .meta(owner)
    .errors(errors.pick("not_found"))
    .input(z.object({ sourceId: sourceIdSchema }))
    .output(empty),
  assertion: base
    .route({ method: "POST", path: "/v1/sources/{sourceId}/assertion" })
    .meta(session)
    .errors(
      errors.pick("source_access_denied", "source_revoked", "rate_limited"),
    )
    .input(
      z.object({
        sourceId: sourceIdSchema,
        device: deviceInputSchema.optional(),
      }),
    )
    .output(sourceAssertionOutputSchema),
  accessList: base
    .route({ method: "GET", path: "/v1/sources/{sourceId}/access" })
    .meta(owner)
    .errors(errors.pick("not_found"))
    .input(z.object({ sourceId: sourceIdSchema }))
    .output(z.object({ access: z.array(sourceAccessSchema) })),
  accessRevoke: base
    .route({
      method: "DELETE",
      path: "/v1/sources/{sourceId}/access/{accountId}",
    })
    .meta(owner)
    .errors(errors.pick("not_found"))
    .input(z.object({ sourceId: sourceIdSchema, accountId: z.string() }))
    .output(empty),
};
