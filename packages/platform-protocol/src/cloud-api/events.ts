/**
 * `/v1/me/stream` 的事件（协议包设计 §5.7）：WS 文本帧 JSON。
 * 客户端每 30 秒发 `{ type: "ping" }`，服务端回 `pong`；服务端也可以主动 `ping`。
 */
import { z } from "zod";

import { sourceIdSchema } from "./schemas.ts";

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
  z.object({ type: z.literal("ping"), ts: z.number() }),
  z.object({ type: z.literal("pong"), ts: z.number().optional() }),
]);

export const meStreamClientMessageSchema = z.object({
  type: z.literal("ping"),
});

export type MeStreamEvent = z.infer<typeof meStreamEventSchema>;
export type MeStreamClientMessage = z.infer<typeof meStreamClientMessageSchema>;
