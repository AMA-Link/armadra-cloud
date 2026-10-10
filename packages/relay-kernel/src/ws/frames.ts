/**
 * WebSocket 帧的纯编解码（RFC 6455 §5），与运行时无关。黄金字节在协议包的
 * `fixtures/ws/`（含 RFC §5.7 的示例帧）。
 *
 * - {@link FrameBoundaryTracker}：只读帧头、数出帧边界，不看载荷。Node 边缘对 WS 是字节
 *   透传，在帧边界上插 `ping`、发关闭帧时用它。
 * - {@link encodeWsFrame} / {@link WsFrameDecoder}：运行时自己终止客户端 WS（Workers）时，
 *   在「隧道流里的字节」与「消息」之间转换：下行切帧、合并分片、去掩码；上行打成带掩码的帧
 *   （客户端帧必须带掩码，§5.1）。
 *
 * 不协商扩展：RSV 位一律视为协议错误（中继与 core 都关了 `permessage-deflate`）。
 */

export const OPCODE = {
  continuation: 0x0,
  text: 0x1,
  binary: 0x2,
  close: 0x8,
  ping: 0x9,
  pong: 0xa,
} as const;

/** 不带掩码、空载荷的 `ping`（中继在帧边界插给客户端）。 */
export const PING_FRAME: Uint8Array = Uint8Array.of(0x89, 0x00);

const encoder = new TextEncoder();

/**
 * 不带掩码的关闭帧（服务端 → 客户端）：状态码 + UTF-8 原因；原因须短于 124 字节
 * （控制帧载荷上限 125）。答 `Buffer`（全局对象，Workers 的 `nodejs_compat` 也有），
 * 直接写给 Node socket。
 */
export function closeFrame(code: number, reason: string): Buffer {
  return Buffer.from(
    encodeWsFrame({
      opcode: OPCODE.close,
      payload: closePayload(code, reason),
    }),
  );
}

/** 只读帧头、数出帧边界的跟踪器。 */
export class FrameBoundaryTracker {
  private header: number[] = [];
  private headerNeed = 2;
  private remaining = 0;

  get atBoundary(): boolean {
    return this.header.length === 0 && this.remaining === 0;
  }

  /** 吃一段字节，返回这段里每个帧结束的位置（相对本段的偏移）。 */
  feed(chunk: Uint8Array): number[] {
    const boundaries: number[] = [];
    let index = 0;
    while (index < chunk.length) {
      if (this.remaining > 0) {
        const take = Math.min(this.remaining, chunk.length - index);
        index += take;
        this.remaining -= take;
        if (this.remaining === 0) boundaries.push(index);
        continue;
      }
      this.header.push(chunk[index] as number);
      index += 1;
      if (this.header.length === 2) {
        const second = this.header[1] as number;
        const length7 = second & 0x7f;
        this.headerNeed =
          2 +
          (length7 === 126 ? 2 : length7 === 127 ? 8 : 0) +
          (second & 0x80 ? 4 : 0);
      }
      if (this.header.length >= 2 && this.header.length === this.headerNeed) {
        const payload = this.payloadLength();
        this.header = [];
        this.headerNeed = 2;
        this.remaining = payload;
        if (payload === 0) boundaries.push(index);
      }
    }
    return boundaries;
  }

  private payloadLength(): number {
    const length7 = (this.header[1] as number) & 0x7f;
    if (length7 < 126) return length7;
    const view = new DataView(Uint8Array.from(this.header).buffer);
    if (length7 === 126) return view.getUint16(2);
    return view.getUint32(2) * 2 ** 32 + view.getUint32(6);
  }
}

export interface WsFrameInput {
  opcode: number;
  payload: Uint8Array;
  /** 最后一片（缺省 true）。 */
  fin?: boolean;
  /** 4 字节掩码键：给了就带掩码（客户端 → 服务端必须带）。 */
  mask?: Uint8Array;
}

/** 编一帧：长度按 7 / 16 / 64 位最短写法。 */
export function encodeWsFrame(input: WsFrameInput): Uint8Array {
  const { opcode, payload, fin = true, mask } = input;
  if (mask && mask.length !== 4) throw new RangeError("掩码键必须是 4 字节");
  const length = payload.length;
  const extended = length < 126 ? 0 : length < 65_536 ? 2 : 8;
  const headerLength = 2 + extended + (mask ? 4 : 0);
  const frame = new Uint8Array(headerLength + length);
  const view = new DataView(frame.buffer);
  frame[0] = (fin ? 0x80 : 0) | (opcode & 0x0f);
  const maskBit = mask ? 0x80 : 0;
  if (extended === 0) frame[1] = maskBit | length;
  else if (extended === 2) {
    frame[1] = maskBit | 126;
    view.setUint16(2, length);
  } else {
    frame[1] = maskBit | 127;
    view.setUint32(2, Math.floor(length / 2 ** 32));
    view.setUint32(6, length >>> 0);
  }
  if (mask) {
    frame.set(mask, 2 + extended);
    for (let index = 0; index < length; index += 1)
      frame[headerLength + index] =
        (payload[index] as number) ^ (mask[index & 3] as number);
  } else frame.set(payload, headerLength);
  return frame;
}

/** 关闭帧的载荷：状态码 + UTF-8 原因（`code` 缺省时空载荷）。 */
export function closePayload(code?: number, reason = ""): Uint8Array {
  if (code === undefined) return new Uint8Array(0);
  const text = encoder.encode(reason);
  const payload = new Uint8Array(2 + text.length);
  payload[0] = (code >> 8) & 0xff;
  payload[1] = code & 0xff;
  payload.set(text, 2);
  return payload;
}

export type WsEvent =
  | { type: "message"; binary: boolean; data: Uint8Array }
  | { type: "close"; code?: number; reason: string }
  | { type: "ping"; data: Uint8Array }
  | { type: "pong"; data: Uint8Array };

export type WsFrameErrorReason =
  | "reserved_bits"
  | "bad_opcode"
  | "bad_control_frame"
  | "bad_continuation"
  | "mask_required"
  | "mask_forbidden"
  | "too_large"
  | "bad_close_payload";

export class WsFrameError extends Error {
  readonly reason: WsFrameErrorReason;
  constructor(reason: WsFrameErrorReason) {
    super(`websocket frame error: ${reason}`);
    this.reason = reason;
    this.name = "WsFrameError";
  }
}

export interface WsFrameDecoderOptions {
  /** `required`：对端是客户端；`forbidden`：对端是服务端（core）。 */
  masked: "required" | "forbidden";
  /** 一条消息（合并分片后）的上限；缺省 16 MiB。 */
  maxMessageBytes?: number;
}

const DEFAULT_MAX_MESSAGE = 16 * 1024 * 1024;
const utf8 = new TextDecoder("utf-8", { ignoreBOM: true });

function concat(parts: readonly Uint8Array[], total: number): Uint8Array {
  if (parts.length === 1) return parts[0] as Uint8Array;
  const out = new Uint8Array(total);
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
}

/**
 * 增量解帧：`feed` 吃任意切分的字节，吐出完整的消息与控制帧；分片的数据帧合并成一条
 * 消息（中间可以夹控制帧）。违规抛 {@link WsFrameError}（调用方以 1002 关闭）。
 */
export class WsFrameDecoder {
  private buffered: Uint8Array = new Uint8Array(0);
  private fragments: Uint8Array[] = [];
  private fragmentBytes = 0;
  private fragmentBinary: boolean | null = null;
  private readonly options: Required<WsFrameDecoderOptions>;

  constructor(options: WsFrameDecoderOptions) {
    this.options = {
      maxMessageBytes: DEFAULT_MAX_MESSAGE,
      ...options,
    };
  }

  feed(chunk: Uint8Array): WsEvent[] {
    this.buffered =
      this.buffered.length === 0
        ? chunk
        : concat([this.buffered, chunk], this.buffered.length + chunk.length);
    const events: WsEvent[] = [];
    for (;;) {
      const consumed = this.next(events);
      if (consumed === 0) break;
      this.buffered = this.buffered.subarray(consumed);
    }
    return events;
  }

  /** 解出一帧，答吃掉的字节数；不够一帧答 0。 */
  private next(events: WsEvent[]): number {
    const bytes = this.buffered;
    if (bytes.length < 2) return 0;
    const first = bytes[0] as number;
    const second = bytes[1] as number;
    if (first & 0x70) throw new WsFrameError("reserved_bits");
    const fin = (first & 0x80) !== 0;
    const opcode = first & 0x0f;
    const masked = (second & 0x80) !== 0;
    if (masked && this.options.masked === "forbidden")
      throw new WsFrameError("mask_forbidden");
    if (!masked && this.options.masked === "required")
      throw new WsFrameError("mask_required");
    const length7 = second & 0x7f;
    const extended = length7 === 126 ? 2 : length7 === 127 ? 8 : 0;
    const headerLength = 2 + extended + (masked ? 4 : 0);
    if (bytes.length < headerLength) return 0;
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.length);
    let length = length7;
    if (extended === 2) length = view.getUint16(2);
    else if (extended === 8) {
      const high = view.getUint32(2);
      if (high & 0x80000000) throw new WsFrameError("too_large");
      length = high * 2 ** 32 + view.getUint32(6);
    }
    const control = opcode >= 0x8;
    if (control && (!fin || length > 125))
      throw new WsFrameError("bad_control_frame");
    if (!control && this.fragmentBytes + length > this.options.maxMessageBytes)
      throw new WsFrameError("too_large");
    if (bytes.length < headerLength + length) return 0;
    const payload = bytes.slice(headerLength, headerLength + length);
    if (masked) {
      const key = bytes.subarray(headerLength - 4, headerLength);
      for (let index = 0; index < payload.length; index += 1)
        payload[index] =
          (payload[index] as number) ^ (key[index & 3] as number);
    }
    this.frame(events, opcode, fin, payload);
    return headerLength + length;
  }

  private frame(
    events: WsEvent[],
    opcode: number,
    fin: boolean,
    payload: Uint8Array,
  ): void {
    switch (opcode) {
      case OPCODE.ping:
        events.push({ type: "ping", data: payload });
        return;
      case OPCODE.pong:
        events.push({ type: "pong", data: payload });
        return;
      case OPCODE.close: {
        if (payload.length === 0) {
          events.push({ type: "close", reason: "" });
          return;
        }
        if (payload.length === 1) throw new WsFrameError("bad_close_payload");
        const code = ((payload[0] as number) << 8) | (payload[1] as number);
        events.push({
          type: "close",
          code,
          reason: utf8.decode(payload.subarray(2)),
        });
        return;
      }
      case OPCODE.text:
      case OPCODE.binary: {
        if (this.fragmentBinary !== null)
          throw new WsFrameError("bad_continuation");
        const binary = opcode === OPCODE.binary;
        if (fin) {
          events.push({ type: "message", binary, data: payload });
          return;
        }
        this.fragmentBinary = binary;
        this.fragments = [payload];
        this.fragmentBytes = payload.length;
        return;
      }
      case OPCODE.continuation: {
        if (this.fragmentBinary === null)
          throw new WsFrameError("bad_continuation");
        this.fragments.push(payload);
        this.fragmentBytes += payload.length;
        if (!fin) return;
        events.push({
          type: "message",
          binary: this.fragmentBinary,
          data: concat(this.fragments, this.fragmentBytes),
        });
        this.fragments = [];
        this.fragmentBytes = 0;
        this.fragmentBinary = null;
        return;
      }
      default:
        throw new WsFrameError("bad_opcode");
    }
  }
}
