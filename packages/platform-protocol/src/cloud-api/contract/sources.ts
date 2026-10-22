import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { tunnelLimitsSchema } from "../../tunnel/handshake.ts";
import { base, SINCE_0_1, SINCE_0_10 } from "../meta.ts";
import {
  grantRoleSchema,
  hostSettingsSchema,
  hostTagsSchema,
  spaceSchema,
  sourceTransferSchema,
  workspaceIdsSchema,
} from "../saas-schemas.ts";
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
/** 主机 owner，或团队主机的团队 admin（1.4 起）。 */
const manager = { scope: ["owner", "team:admin"], since: SINCE_0_10 } as const;

/**
 * 隧道节点（cloud-api §17）：`url` 是完整的 WebSocket 地址，可以带路径与 query（Workers 中继
 * 答 `wss://<域>/t/v1?src=<sourceId>`，靠 query 在握手前把隧道路由到这个源的对象），客户端原样
 * 连接；没有路径时补 `/t/v1`。
 */
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
  /** 1.4 起：来自哪条授予、角色、范围与到期（访问清单）；personal 不报。 */
  grantId: z.string().nullable().optional(),
  role: grantRoleSchema.optional(),
  workspaceIds: workspaceIdsSchema.optional(),
  expiresAtMs: z.number().int().nullable().optional(),
});

export const sources = {
  registrationToken: base
    .route({ method: "POST", path: "/v1/sources/registration-tokens" })
    .meta(session)
    .errors(
      errors.pick(
        "email_unverified",
        "limit_reached",
        "team_role_insufficient",
        "team_policy_denied",
        "host_registration_disabled",
        "team_suspended",
        "team_deleted",
      ),
    )
    // 1.4 起：令牌由某个空间签发，主机就归那个空间；缺省 = 个人空间。
    .input(z.object({ space: spaceSchema.optional() }))
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
    // 1.4 起：给 `space` 只列那个空间的主机；缺省列调用者能到达的全部（个人空间 + 各团队）。
    // GET 里写成 `space[kind]=team&space[teamId]=…`。
    // 1.4 补充：筛选与游标分页；`tag` 是「且」（主机要带齐所有给出的标签）。GET 里 `tag` 重复写。
    .input(
      z.object({
        space: spaceSchema.optional(),
        status: z.enum(["online", "offline", "revoked"]).optional(),
        tag: z.array(z.string()).optional(),
        q: z.string().max(128).optional(),
        cursor: z.string().optional(),
        limit: z.coerce.number().int().min(1).max(200).optional(),
      }),
    )
    .output(
      z.object({
        sources: z.array(sourceSummarySchema),
        /** `null` 或缺省 = 没有下一页（已部署的中继不报这个字段）。 */
        nextCursor: z.string().nullable().optional(),
      }),
    ),
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
        /** 1.4 起：标签（整组替换）与主机设置（逐项改）。 */
        tags: hostTagsSchema.optional(),
        settings: hostSettingsSchema.partial().optional(),
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
      errors.pick(
        "source_access_denied",
        "source_revoked",
        "rate_limited",
        "team_suspended",
      ),
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
  /**
   * 转移空间（1.4 起，cloud-api §4）：个人 → 团队由主机 owner 发起、团队 admin 接受；团队 →
   * 团队两边的 admin 都确认。转移后原空间签过的断言与链接全部作废。
   */
  transferStart: base
    .route({ method: "POST", path: "/v1/sources/{sourceId}/transfer" })
    .meta(manager)
    .errors(
      errors.pick(
        "not_found",
        "source_access_denied",
        "team_role_insufficient",
        "transfer_pending",
        "limit_reached",
        "bad_request",
      ),
    )
    .input(z.object({ sourceId: sourceIdSchema, to: spaceSchema }))
    .output(sourceTransferSchema),
  transferList: base
    .route({ method: "GET", path: "/v1/source-transfers" })
    .meta({ scope: ["session"], since: SINCE_0_10 })
    .input(
      z.object({
        sourceId: sourceIdSchema.optional(),
        space: spaceSchema.optional(),
      }),
    )
    .output(z.object({ transfers: z.array(sourceTransferSchema) })),
  /** 以调用者能代表的一方（主机 owner 或团队 admin）确认；两边都确认后生效。 */
  transferAccept: base
    .route({ method: "POST", path: "/v1/source-transfers/{transferId}/accept" })
    .meta(manager)
    .errors(
      errors.pick(
        "not_found",
        "team_role_insufficient",
        "transfer_expired",
        "limit_reached",
        "conflict",
      ),
    )
    .input(z.object({ transferId: z.string() }))
    .output(sourceTransferSchema),
  /** 任何一方都可以取消或拒绝。 */
  transferCancel: base
    .route({ method: "DELETE", path: "/v1/source-transfers/{transferId}" })
    .meta(manager)
    .errors(errors.pick("not_found", "team_role_insufficient", "conflict"))
    .input(z.object({ transferId: z.string() }))
    .output(sourceTransferSchema),
};
