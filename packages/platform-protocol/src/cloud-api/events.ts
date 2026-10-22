/**
 * `/v1/me/stream` 的事件（协议包设计 §5.7）：WS 文本帧 JSON。
 * 客户端每 30 秒发 `{ type: "ping" }`，服务端回 `pong`；服务端也可以主动 `ping`。
 */
import { z } from "zod";

import { sourceIdSchema } from "./schemas.ts";
import { spaceSchema, teamIdSchema } from "./saas-schemas.ts";

export const ME_STREAM_PATH = "/v1/me/stream";
export const ME_STREAM_PING_MS = 30_000;

export const meStreamEventSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("sourceOnline"),
    sourceId: sourceIdSchema,
    sinceMs: z.number().int(),
    relayOrigin: z.string(),
  }),
  z.object({
    type: z.literal("sourceOffline"),
    sourceId: sourceIdSchema,
    atMs: z.number().int(),
  }),
  z.object({ type: z.literal("sourceRevoked"), sourceId: sourceIdSchema }),
  z.object({ type: z.literal("accessRevoked"), sourceId: sourceIdSchema }),
  z.object({
    type: z.literal("linkUsed"),
    linkId: z.string(),
    sourceId: sourceIdSchema.optional(),
    teamId: z.string().optional(),
    by: z.object({
      accountId: z.string().optional(),
      guest: z.boolean().optional(),
    }),
    atMs: z.number().int(),
  }),
  // 协议 1.4：SaaS 的团队事件（预留，没有发送方；cloud-api §32）。
  z.object({
    type: z.literal("memberChanged"),
    teamId: teamIdSchema,
    accountId: z.string().optional(),
    change: z.enum(["added", "removed", "roleChanged"]),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("invitationChanged"),
    invitationId: z.string(),
    teamId: teamIdSchema.optional(),
    sourceId: sourceIdSchema.optional(),
    state: z.enum(["pending", "accepted", "declined", "revoked", "expired"]),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("grantChanged"),
    sourceId: sourceIdSchema,
    grantId: z.string().optional(),
    teamId: teamIdSchema.optional(),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("accessRequested"),
    requestId: z.string(),
    sourceId: sourceIdSchema,
    teamId: teamIdSchema.optional(),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("deviceTrustRequested"),
    requestId: z.string(),
    sourceId: sourceIdSchema,
    teamId: teamIdSchema.optional(),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("deviceTrustResolved"),
    requestId: z.string(),
    outcome: z.enum(["trusted", "rejected", "expired"]),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("planChanged"),
    space: spaceSchema,
    plan: z.string(),
    status: z.enum(["active", "trialing", "past_due", "canceled"]),
    atMs: z.number().int(),
  }),
  z.object({ type: z.literal("ping"), ts: z.number() }),
  z.object({ type: z.literal("pong"), ts: z.number().optional() }),
]);

export const meStreamClientMessageSchema = z.object({
  type: z.literal("ping"),
});

export type MeStreamEvent = z.infer<typeof meStreamEventSchema>;
export type MeStreamClientMessage = z.infer<typeof meStreamClientMessageSchema>;
