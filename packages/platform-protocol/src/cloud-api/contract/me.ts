import { z } from "zod";

import { devicePlatformSchema } from "../../assertion/claims.ts";
import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import { empty, meSchema, sourceSummarySchema } from "../schemas.ts";

const session = { scope: ["session"], since: SINCE_0_1 } as const;

export const meDeviceSchema = z.object({
  deviceId: z.string(),
  platform: devicePlatformSchema,
  name: z.string(),
  lastSeenAtMs: z.number().int().nullable(),
  current: z.boolean(),
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
};
