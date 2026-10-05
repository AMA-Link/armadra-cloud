/**
 * 隧道二进制帧（协议包设计 §3.2）。
 *
 * ```text
 * offset  size  字段
 * 0       1     type (u8)
 * 1       4     streamId (u32 BE)；0 = 隧道级（只允许 WINDOW / PING / PONG / GOAWAY）
 * 5       n     payload
 * ```
 *
 * 纯函数、无 I/O；编码与解码对同一组约束对称：任何 `decodeFrame` 会拒绝的帧，
 * `encodeFrame` 也拒绝产出。
 */
import { z } from "zod";

import { fromUtf8, utf8 } from "../internal/bytes.ts";
import { FRAME_TYPE, type FrameTypeName } from "./codes.ts";
import { LIMITS } from "./limits.ts";

export const FRAME_HEADER_BYTES = 5;
const U32_MAX = 0xffff_ffff;

export type FrameErrorCode = "bad_frame" | "frame_too_large" | "bad_open";

export class FrameError extends Error {
  readonly code: FrameErrorCode;
  constructor(code: FrameErrorCode, message: string) {
    super(message);
    this.name = "FrameError";
    this.code = code;
  }
}

const headerPairSchema = z.tuple([z.string().min(1), z.string()]);

export const openPayloadSchema = z.object({
  kind: z.enum(["http", "ws"]),
  method: z.string().min(1).max(32),
  /** 含 query。 */
  path: z.string().startsWith("/"),
  /** 已剥掉中继令牌与 `armadra-relay.*` 子协议。 */
  headers: z.array(headerPairSchema),
  clientOrigin: z.string().nullable(),
  remoteIp: z.string(),
  relayNode: z.string(),
});

export type OpenPayload = z.infer<typeof openPayloadSchema>;

export const goawayPayloadSchema = z.object({
  reason: z.enum(["shutdown", "rebalance", "replaced"]),
  graceMs: z.number().int().nonnegative(),
});

export type GoawayReason = z.infer<typeof goawayPayloadSchema>["reason"];

export type Frame =
  | { type: "open"; streamId: number; open: OpenPayload }
  | { type: "data"; streamId: number; bytes: Uint8Array }
  | { type: "end"; streamId: number }
  | { type: "rst"; streamId: number; code: number }
  | { type: "window"; streamId: number; credit: number }
  | { type: "ping"; ts: number }
  | { type: "pong"; ts: number }
  | { type: "goaway"; reason: GoawayReason; graceMs: number };

const TYPE_NAMES = new Map<number, FrameTypeName>(
  Object.entries(FRAME_TYPE).map(([name, value]) => [
    value,
    name as FrameTypeName,
  ]),
);

function bad(message: string): never {
  throw new FrameError("bad_frame", message);
}

function checkStreamId(type: FrameTypeName, streamId: number): void {
  if (!Number.isInteger(streamId) || streamId < 0 || streamId > U32_MAX)
    bad(`streamId 不是 u32：${streamId}`);
  const tunnelLevel = type === "ping" || type === "pong" || type === "goaway";
  if (tunnelLevel && streamId !== 0) bad(`${type} 只能是隧道级（streamId 0）`);
  const streamLevel =
    type === "open" || type === "data" || type === "end" || type === "rst";
  if (streamLevel && streamId === 0) bad(`${type} 不能用 streamId 0`);
}

function headersBytes(headers: readonly (readonly [string, string])[]): number {
  let total = 0;
  for (const [name, value] of headers)
    total += utf8(name).length + utf8(value).length;
  return total;
}

function parseOpen(value: unknown): OpenPayload {
  const result = openPayloadSchema.safeParse(value);
  if (!result.success)
    throw new FrameError("bad_open", "OPEN 载荷不符合 schema");
  if (headersBytes(result.data.headers) > LIMITS.openHeadersMaxBytes) {
    throw new FrameError("bad_open", "OPEN 的 headers 超过上限");
  }
  return result.data;
}

function parseGoaway(value: unknown): {
  reason: GoawayReason;
  graceMs: number;
} {
  const result = goawayPayloadSchema.safeParse(value);
  if (!result.success) bad("GOAWAY 载荷不符合 schema");
  return result.data;
}

function frame(
  type: FrameTypeName,
  streamId: number,
  payload: Uint8Array,
): Uint8Array {
  const total = FRAME_HEADER_BYTES + payload.length;
  if (total > LIMITS.maxFrameBytes)
    throw new FrameError("frame_too_large", `帧 ${total} 字节超过上限`);
  const out = new Uint8Array(total);
  const view = new DataView(out.buffer);
  view.setUint8(0, FRAME_TYPE[type]);
  view.setUint32(1, streamId, false);
  out.set(payload, FRAME_HEADER_BYTES);
  return out;
}

function u16(value: number): Uint8Array {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff)
    bad(`不是 u16：${value}`);
  const out = new Uint8Array(2);
  new DataView(out.buffer).setUint16(0, value, false);
  return out;
}

function u32(value: number): Uint8Array {
  if (!Number.isInteger(value) || value < 0 || value > U32_MAX)
    bad(`不是 u32：${value}`);
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, value, false);
  return out;
}

function f64(value: number): Uint8Array {
  if (!Number.isFinite(value)) bad(`ts 不是有限数：${value}`);
  const out = new Uint8Array(8);
  new DataView(out.buffer).setFloat64(0, value, false);
  return out;
}

export function encodeFrame(input: Frame): Uint8Array {
  switch (input.type) {
    case "open": {
      checkStreamId("open", input.streamId);
      const open = parseOpen(input.open);
      return frame("open", input.streamId, utf8(JSON.stringify(open)));
    }
    case "data":
      checkStreamId("data", input.streamId);
      if (input.bytes.length > LIMITS.maxDataChunk) {
        throw new FrameError(
          "frame_too_large",
          `DATA ${input.bytes.length} 字节超过 maxDataChunk`,
        );
      }
      return frame("data", input.streamId, input.bytes);
    case "end":
      checkStreamId("end", input.streamId);
      return frame("end", input.streamId, new Uint8Array(0));
    case "rst":
      checkStreamId("rst", input.streamId);
      return frame("rst", input.streamId, u16(input.code));
    case "window":
      checkStreamId("window", input.streamId);
      if (input.credit === 0) bad("WINDOW 的 credit 不能为 0");
      return frame("window", input.streamId, u32(input.credit));
    case "ping":
      return frame("ping", 0, f64(input.ts));
    case "pong":
      return frame("pong", 0, f64(input.ts));
    case "goaway": {
      const payload = parseGoaway({
        reason: input.reason,
        graceMs: input.graceMs,
      });
      return frame("goaway", 0, utf8(JSON.stringify(payload)));
    }
  }
}

function decodeJson(payload: Uint8Array, code: FrameErrorCode): unknown {
  try {
    return JSON.parse(fromUtf8(payload));
  } catch {
    throw new FrameError(code, "载荷不是 UTF-8 JSON");
  }
}

export function decodeFrame(bytes: Uint8Array): Frame {
  if (bytes.length < FRAME_HEADER_BYTES) bad(`帧只有 ${bytes.length} 字节`);
  if (bytes.length > LIMITS.maxFrameBytes)
    throw new FrameError("frame_too_large", `帧 ${bytes.length} 字节超过上限`);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const type = TYPE_NAMES.get(view.getUint8(0));
  if (type === undefined) bad(`未知帧类型 ${view.getUint8(0)}`);
  const streamId = view.getUint32(1, false);
  checkStreamId(type, streamId);
  const payload = bytes.subarray(FRAME_HEADER_BYTES);
  const payloadView = new DataView(
    payload.buffer,
    payload.byteOffset,
    payload.byteLength,
  );
  const expectLength = (length: number) => {
    if (payload.length !== length)
      bad(`${type} 载荷应为 ${length} 字节，实为 ${payload.length}`);
  };
  switch (type) {
    case "open":
      return {
        type,
        streamId,
        open: parseOpen(decodeJson(payload, "bad_open")),
      };
    case "data":
      if (payload.length > LIMITS.maxDataChunk) {
        throw new FrameError(
          "frame_too_large",
          `DATA ${payload.length} 字节超过 maxDataChunk`,
        );
      }
      return { type, streamId, bytes: payload.slice() };
    case "end":
      expectLength(0);
      return { type, streamId };
    case "rst":
      expectLength(2);
      return { type, streamId, code: payloadView.getUint16(0, false) };
    case "window": {
      expectLength(4);
      const credit = payloadView.getUint32(0, false);
      if (credit === 0) bad("WINDOW 的 credit 不能为 0");
      return { type, streamId, credit };
    }
    case "ping":
    case "pong": {
      expectLength(8);
      const ts = payloadView.getFloat64(0, false);
      if (!Number.isFinite(ts)) bad("ts 不是有限数");
      return { type, ts };
    }
    case "goaway":
      return { type, ...parseGoaway(decodeJson(payload, "bad_frame")) };
  }
}
