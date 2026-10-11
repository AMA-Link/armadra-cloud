/**
 * 通道建好后 core 发的第一条记录 r0（设计 §4.4）：一个条目，UTF-8 JSON。之后的记录里
 * 才是内层隧道帧。schema 非 strict：更高 minor 加的字段被忽略。
 */
import { z } from "zod";

import { fromUtf8, utf8 } from "../internal/bytes.ts";
import { tunnelLimitsSchema } from "../tunnel/handshake.ts";
import { E2eError } from "./primitives.ts";

export const E2E_DEVICE_STATUSES = ["trusted", "pending", "rejected"] as const;

export const e2eDeviceStatusSchema = z.enum(E2E_DEVICE_STATUSES);
export type E2eDeviceStatus = z.infer<typeof e2eDeviceStatusSchema>;

export const e2eAcceptSchema = z.object({
  type: z.literal("accept"),
  /** `pending` 时客户端只允许发往身份域匿名面与等待批准的长轮询。 */
  deviceStatus: e2eDeviceStatusSchema,
  /** `pending` 时的待确认 id（本机确认、`devices.waitApproval` 用）。 */
  pendingId: z.string().min(1).max(64).optional(),
  /** 通道内复用的限制（与隧道 `ready.limits` 同形）。 */
  limits: tunnelLimitsSchema,
});
export type E2eAccept = z.infer<typeof e2eAcceptSchema>;

/** r0 的条目。 */
export function encodeAccept(accept: E2eAccept): Uint8Array {
  return utf8(JSON.stringify(e2eAcceptSchema.parse(accept)));
}

/** r0 的条目 → accept；必须恰好一个条目。 */
export function decodeAccept(entries: readonly Uint8Array[]): E2eAccept {
  if (entries.length !== 1)
    throw new E2eError("bad_record", "accept 记录须恰好一个条目");
  let value: unknown;
  try {
    value = JSON.parse(fromUtf8(entries[0] as Uint8Array));
  } catch {
    throw new E2eError("bad_record", "accept 不是 UTF-8 JSON");
  }
  const result = e2eAcceptSchema.safeParse(value);
  if (!result.success) throw new E2eError("bad_record", "accept 不符合 schema");
  return result.data;
}
