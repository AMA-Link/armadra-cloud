import { z } from "zod";

import { devicePlatformSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1, SINCE_0_4 } from "../meta.ts";
import {
  empty,
  meSchema,
  sourceIdSchema,
  sourceSummarySchema,
} from "../schemas.ts";

const session = { scope: ["session"], since: SINCE_0_1 } as const;

export const meDeviceSchema = z.object({
  deviceId: z.string(),
  platform: devicePlatformSchema,
  name: z.string(),
  lastSeenAtMs: z.number().int().nullable(),
  current: z.boolean(),
});

/** 流量统计的类别（cloud-api §19）：端到端通道、明文 HTTP、明文 WebSocket、媒体票。 */
export const TRAFFIC_KINDS = ["e2e", "http", "ws", "media"] as const;
export const trafficKindSchema = z.enum(TRAFFIC_KINDS);
export const TRAFFIC_GRANULARITIES = ["hour", "day"] as const;
/** `me.traffic` 一次最多答这么多行，超出答 `400 bad_request`（缩小时间范围或按源过滤）。 */
export const TRAFFIC_MAX_ROWS = 10_000;

/** 一个时间桶 × 节点 × 源 × 令牌 `sub` × `dev` × 类别的累计。 */
export const trafficBucketSchema = z.object({
  /** 桶起点（UTC 整小时或整天，毫秒）。 */
  bucketStartMs: z.number().int(),
  node: z.string(),
  sourceId: z.string(),
  /** 中继令牌的 `sub`（账号或 `guest:<linkId>:<随机>`）。 */
  subject: z.string(),
  /** 中继令牌的 `dev`；没带时为空串。 */
  deviceId: z.string(),
  kind: trafficKindSchema,
  connections: z.number().int().nonnegative(),
  /** 客户端 → 源。 */
  bytesToSource: z.number().int().nonnegative(),
  /** 源 → 客户端。 */
  bytesToClient: z.number().int().nonnegative(),
});

/** query 里是字符串：毫秒整数按数字收。 */
const msQuery = z.coerce.number().int().nonnegative();

export const trafficInputSchema = z.object({
  /** 含：桶起点 ≥ fromMs。 */
  fromMs: msQuery,
  /** 不含：桶起点 < toMs。 */
  toMs: msQuery,
  sourceId: sourceIdSchema.optional(),
  granularity: z.enum(TRAFFIC_GRANULARITIES).default("hour"),
});

export const me = {
  get: base
    .route({ method: "GET", path: "/v1/me" })
    .meta(session)
    .input(empty)
    .output(meSchema),
  update: base
    .route({ method: "PATCH", path: "/v1/me" })
    .meta(session)
    .errors(errors.pick("bad_request"))
    .input(z.object({ displayName: z.string().min(1).max(128).optional() }))
    .output(meSchema),
  sources: base
    .route({ method: "GET", path: "/v1/me/sources" })
    .meta(session)
    .input(empty)
    .output(z.object({ sources: z.array(sourceSummarySchema) })),
  devices: base
    .route({ method: "GET", path: "/v1/me/devices" })
    .meta(session)
    .input(empty)
    .output(z.object({ devices: z.array(meDeviceSchema) })),
  deviceRevoke: base
    .route({ method: "DELETE", path: "/v1/me/devices/{deviceId}" })
    .meta(session)
    .errors(errors.pick("not_found"))
    .input(z.object({ deviceId: z.string() }))
    .output(empty),
  /** 30 秒一次性；WS 升级 `GET /v1/me/stream` 用 `Sec-WebSocket-Protocol: armadra-ticket.<t>`。 */
  streamTicket: base
    .route({ method: "POST", path: "/v1/me/stream/ticket" })
    .meta(session)
    .input(empty)
    .output(z.object({ ticket: z.string(), expiresAtMs: z.number().int() })),
  /**
   * 中继流量统计（cloud-api §19，0.4.0 起）：账号看自己能访问的源，访客只看自己的 `subject`。
   * 能力 `me.traffic`。
   */
  traffic: base
    .route({ method: "GET", path: "/v1/me/traffic" })
    .meta({ scope: ["session"], since: SINCE_0_4 })
    .errors(errors.pick("bad_request"))
    .input(trafficInputSchema)
    .output(z.object({ buckets: z.array(trafficBucketSchema) })),
};
