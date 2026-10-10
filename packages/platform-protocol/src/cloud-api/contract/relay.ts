/**
 * 中继节点对控制面的回报（cloud-api §17，0.3.5 起）：不和控制面共用 Redis 的节点（Workers
 * 中继）把隧道上下线经 HTTP 告诉控制面，控制面据此答 `sources.assertion` 的 `online` 与
 * `me/stream` 的在线事件。认证是节点令牌（`Authorization: Bearer <节点令牌>`，运营方给每个
 * 节点发一枚，scope `relay-node`）。同一事件可以重复报（幂等）；`ttlMs` 内没再报 `sourceOnline`
 * 的路由由控制面当作过期。
 */
import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_3_5 } from "../meta.ts";
import { sourceIdSchema } from "../schemas.ts";

/** 节点名：小写字母数字与连字符，1–63 个字符（Workers 中继固定是 `cf`）。 */
export const relayNodeIdSchema = z.string().regex(/^[a-z0-9][a-z0-9-]{0,62}$/);

const tunnelIdSchema = z.string().regex(/^[0-9a-f]{32}$/);

export const relayPresenceEventSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("sourceOnline"),
    sourceId: sourceIdSchema,
    tunnelId: tunnelIdSchema,
    coreVersion: z.string().min(1).max(64),
    atMs: z.number().int(),
  }),
  z.object({
    type: z.literal("sourceOffline"),
    sourceId: sourceIdSchema,
    tunnelId: tunnelIdSchema,
    atMs: z.number().int(),
  }),
]);

export const relay = {
  nodePresence: base
    .route({ method: "POST", path: "/v1/relay/nodes/{node}/presence" })
    .meta({ scope: ["relay-node"], since: SINCE_0_3_5 })
    .errors(errors.pick("unauthenticated", "forbidden"))
    .input(
      z.object({
        node: relayNodeIdSchema,
        /** 路由的有效期：这段时间里没再报 `sourceOnline` 就过期（10 秒到 10 分钟）。 */
        ttlMs: z.number().int().min(10_000).max(600_000),
        events: z.array(relayPresenceEventSchema).min(1).max(256),
      }),
    )
    .output(z.object({ accepted: z.number().int().nonnegative() })),
};

export type RelayPresenceEvent = z.infer<typeof relayPresenceEventSchema>;
