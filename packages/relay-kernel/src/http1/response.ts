/**
 * HTTP/1.1 响应的增量解析（中继设计 §2.4 的 `http1/response`），与运行时无关。源（core）
 * 经隧道流写回的字节由运行时喂进来：状态行、头、体（`Content-Length` / `chunked` / 关闭
 * 定界三种），输出事件。黄金字节在协议包的 `fixtures/http1/`。
 *
 * - `HEAD` 的响应与 `1xx` / `204` / `304` 没有体；`101` 之后的字节原样交出（升级后的
 *   WebSocket 帧），其余 `1xx`（`100 Continue`、`103`）跳过，接着解析最终响应。
 * - 头部（状态行 + 头 + 空行）上限缺省 `openHeadersMaxBytes`；`chunked` 的尾部头同一上限。
 * - 不合法 → 抛 {@link Http1ParseError}：调用方重置流（`RST`）并答 502 `bad_gateway`。
 *   包括：状态行坏、头名不是 token、头名后有空白、折行（obs-fold）、`Content-Length`
 *   不是十进制或多个值不一致、`Transfer-Encoding` 与 `Content-Length` 同时出现（防请求
 *   走私）、分块长度坏、体没收全就 EOF。
 * - 一条流只跑一个请求（`connection: close`）：响应结束后多出来的字节丢弃。
 *
 * 头值按 latin1 解码（与 Node 的 HTTP 解析器相同），保留原始大小写与顺序。
 */
import { LIMITS } from "@armadra/platform-protocol/tunnel";

import type { HeaderPair } from "../edge/forward.ts";

export interface ResponseHead {
  /** `1.0` 或 `1.1`。 */
  version: string;
  status: number;
  statusText: string;
  headers: HeaderPair[];
}

export type ResponseEvent =
  /** 最终响应的头；`upgrade` = `101`，之后只有 `upgraded` 事件。 */
  | { type: "head"; head: ResponseHead; upgrade: boolean }
  | { type: "body"; bytes: Uint8Array }
  | { type: "end" }
  /** `101` 之后的原始字节。 */
  | { type: "upgraded"; bytes: Uint8Array };

export type Http1ParseErrorReason =
  | "bad_status_line"
  | "bad_header"
  | "head_too_large"
  | "bad_content_length"
  | "bad_transfer_encoding"
  | "bad_chunk"
  | "truncated";

export class Http1ParseError extends Error {
  readonly reason: Http1ParseErrorReason;
  constructor(reason: Http1ParseErrorReason) {
    super(`invalid http response: ${reason}`);
    this.reason = reason;
    this.name = "Http1ParseError";
  }
}

export interface ResponseParserOptions {
  /** 请求方法（`HEAD` 的响应没有体）。 */
  method: string;
  /** 头部字节上限；缺省 `LIMITS.openHeadersMaxBytes`。 */
  maxHeadBytes?: number;
}

type Phase =
  | "status"
  | "headers"
  | "length"
  | "chunk-size"
  | "chunk-data"
  | "chunk-crlf"
  | "trailers"
  | "close"
  | "upgraded"
  | "done";

const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const STATUS_LINE = /^HTTP\/1\.([01]) (\d{3})(?: (.*))?$/;
const CHUNK_SIZE = /^([0-9A-Fa-f]{1,16})[ \t]*(?:;.*)?$/;
/** 行里不许有的控制字符（HTAB 以外；裸 CR 也在内）。 */
// eslint-disable-next-line no-control-regex -- 就是要匹配控制字符
const CONTROL = /[\x00-\x08\x0a-\x1f\x7f]/;
/** 分块长度行的上限（含扩展）。 */
const CHUNK_LINE_MAX = 1024;

function latin1(bytes: Uint8Array): string {
  let text = "";
  for (let index = 0; index < bytes.length; index += 0x8000)
    text += String.fromCharCode(...bytes.subarray(index, index + 0x8000));
  return text;
}

function join(parts: readonly Uint8Array[], total: number): Uint8Array {
  if (parts.length === 1) return parts[0] as Uint8Array;
  const out = new Uint8Array(total);
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
}

/** 解析一行头（`name: value`）；不合法抛错。 */
function parseHeaderLine(line: string): HeaderPair {
  if (line.startsWith(" ") || line.startsWith("\t"))
    throw new Http1ParseError("bad_header"); // obs-fold
  const colon = line.indexOf(":");
  if (colon <= 0) throw new Http1ParseError("bad_header");
  const name = line.slice(0, colon);
  if (!TOKEN.test(name)) throw new Http1ParseError("bad_header");
  const value = line.slice(colon + 1).replace(/^[ \t]+|[ \t]+$/g, "");
  if (CONTROL.test(value)) throw new Http1ParseError("bad_header");
  return [name, value];
}

/** 从头里定体的长度：`chunked`、定长或关闭定界。 */
function bodyFraming(
  headers: readonly HeaderPair[],
):
  | { kind: "chunked" }
  | { kind: "length"; length: number }
  | { kind: "close" } {
  const encodings: string[] = [];
  const lengths: string[] = [];
  for (const [name, value] of headers) {
    const lower = name.toLowerCase();
    if (lower === "transfer-encoding")
      encodings.push(
        ...value
          .split(",")
          .map((item) => item.trim().toLowerCase())
          .filter(Boolean),
      );
    else if (lower === "content-length")
      lengths.push(...value.split(",").map((item) => item.trim()));
  }
  if (encodings.length > 0) {
    if (lengths.length > 0) throw new Http1ParseError("bad_transfer_encoding");
    // 最后一个编码是 chunked 才按分块读；否则（RFC 9112 §6.3）读到关闭为止。
    return encodings[encodings.length - 1] === "chunked"
      ? { kind: "chunked" }
      : { kind: "close" };
  }
  if (lengths.length === 0) return { kind: "close" };
  const first = lengths[0] as string;
  if (!/^\d{1,15}$/.test(first) || lengths.some((item) => item !== first))
    throw new Http1ParseError("bad_content_length");
  return { kind: "length", length: Number(first) };
}

/**
 * 增量解析器。体的字节直接切自喂进来的块（不拷贝、不缓冲）；只有状态行、头、分块长度行
 * 与尾部头按行攒在内部缓冲里，每个字节只扫一次。
 */
export class ResponseParser {
  private phase: Phase = "status";
  /** 正在攒的一行（到 LF 为止）。 */
  private line: Uint8Array[] = [];
  private lineBytes = 0;
  /** 这一段头部（状态行 + 头 + 空行，或尾部头）已用的字节。 */
  private headBytes = 0;
  private status: {
    version: string;
    status: number;
    statusText: string;
  } | null = null;
  private headers: HeaderPair[] = [];
  private remaining = 0;
  private readonly method: string;
  private readonly maxHeadBytes: number;

  constructor(options: ResponseParserOptions) {
    this.method = options.method.toUpperCase();
    this.maxHeadBytes = options.maxHeadBytes ?? LIMITS.openHeadersMaxBytes;
  }

  /** 响应已经完整（或已升级）。 */
  get done(): boolean {
    return this.phase === "done" || this.phase === "upgraded";
  }

  /** 吃一段字节（任意切分），答这段里产生的事件。 */
  feed(chunk: Uint8Array): ResponseEvent[] {
    const events: ResponseEvent[] = [];
    let at = 0;
    while (at < chunk.length) at = this.step(chunk, at, events);
    return events;
  }

  /** 流结束（对端 END）：关闭定界的体到此结束；别的阶段说明响应没收全。 */
  finish(): ResponseEvent[] {
    if (this.phase === "close") {
      this.phase = "done";
      return [{ type: "end" }];
    }
    if (this.phase === "done" || this.phase === "upgraded") return [];
    throw new Http1ParseError("truncated");
  }

  /** 从 `at` 起推进一步，答下一个未处理的位置。 */
  private step(chunk: Uint8Array, at: number, events: ResponseEvent[]): number {
    switch (this.phase) {
      case "status":
      case "headers":
      case "trailers":
      case "chunk-size":
      case "chunk-crlf": {
        const { line, next } = this.readLine(chunk, at);
        if (line !== undefined) this.onLine(line, events);
        return next;
      }
      case "length":
      case "chunk-data": {
        const take = Math.min(this.remaining, chunk.length - at);
        events.push({ type: "body", bytes: chunk.subarray(at, at + take) });
        this.remaining -= take;
        if (this.remaining === 0) {
          if (this.phase === "length") this.end(events);
          else this.phase = "chunk-crlf";
        }
        return at + take;
      }
      case "close":
        events.push({ type: "body", bytes: chunk.subarray(at) });
        return chunk.length;
      case "upgraded":
        events.push({ type: "upgraded", bytes: chunk.subarray(at) });
        return chunk.length;
      case "done":
        return chunk.length; // 一条流只跑一个请求：多出来的字节丢弃
    }
  }

  /** 攒到 CRLF 为止；行没完答 `line: undefined`。裸 LF 不合法。 */
  private readLine(
    chunk: Uint8Array,
    at: number,
  ): { line?: string; next: number } {
    const lf = chunk.indexOf(0x0a, at);
    const end = lf < 0 ? chunk.length : lf + 1;
    const piece = chunk.subarray(at, end);
    this.line.push(piece);
    this.lineBytes += piece.length;
    const inHead = this.phase !== "chunk-size" && this.phase !== "chunk-crlf";
    if (inHead) {
      this.headBytes += piece.length;
      if (this.headBytes > this.maxHeadBytes)
        throw new Http1ParseError("head_too_large");
    } else if (this.lineBytes > CHUNK_LINE_MAX)
      throw new Http1ParseError("bad_chunk");
    if (lf < 0) return { next: end };
    const bytes = join(this.line, this.lineBytes);
    this.line = [];
    this.lineBytes = 0;
    if (bytes.length < 2 || bytes[bytes.length - 2] !== 0x0d)
      throw new Http1ParseError(
        this.phase === "status"
          ? "bad_status_line"
          : inHead
            ? "bad_header"
            : "bad_chunk",
      );
    return { line: latin1(bytes.subarray(0, bytes.length - 2)), next: end };
  }

  private onLine(line: string, events: ResponseEvent[]): void {
    switch (this.phase) {
      case "status": {
        const match = STATUS_LINE.exec(line);
        if (!match || CONTROL.test(line))
          throw new Http1ParseError("bad_status_line");
        const status = Number(match[2]);
        if (status < 100) throw new Http1ParseError("bad_status_line");
        this.status = {
          version: `1.${match[1] as string}`,
          status,
          statusText: match[3] ?? "",
        };
        this.headers = [];
        this.phase = "headers";
        return;
      }
      case "headers":
        if (line === "") this.onHead(events);
        else this.headers.push(parseHeaderLine(line));
        return;
      case "chunk-size": {
        const match = CHUNK_SIZE.exec(line);
        if (!match || CONTROL.test(line))
          throw new Http1ParseError("bad_chunk");
        const size = Number.parseInt(match[1] as string, 16);
        if (!Number.isSafeInteger(size)) throw new Http1ParseError("bad_chunk");
        if (size === 0) {
          this.phase = "trailers";
          this.headBytes = 0;
        } else {
          this.remaining = size;
          this.phase = "chunk-data";
        }
        return;
      }
      case "chunk-crlf":
        if (line !== "") throw new Http1ParseError("bad_chunk");
        this.phase = "chunk-size";
        return;
      case "trailers":
        // 尾部头只校验格式、不转发（中继的响应头在头部就已经定了）。
        if (line === "") this.end(events);
        else parseHeaderLine(line);
        return;
      default:
        return;
    }
  }

  private onHead(events: ResponseEvent[]): void {
    const status = this.status as NonNullable<ResponseParser["status"]>;
    const headers = this.headers;
    this.headBytes = 0;
    if (status.status === 101) {
      events.push({
        type: "head",
        head: { ...status, headers },
        upgrade: true,
      });
      this.phase = "upgraded";
      return;
    }
    if (status.status < 200) {
      // 100 / 103：跳过，接着读最终响应。
      this.phase = "status";
      return;
    }
    events.push({ type: "head", head: { ...status, headers }, upgrade: false });
    if (
      this.method === "HEAD" ||
      status.status === 204 ||
      status.status === 304
    ) {
      this.end(events);
      return;
    }
    const framing = bodyFraming(headers);
    if (framing.kind === "chunked") this.phase = "chunk-size";
    else if (framing.kind === "close") this.phase = "close";
    else if (framing.length === 0) this.end(events);
    else {
      this.remaining = framing.length;
      this.phase = "length";
    }
  }

  private end(events: ResponseEvent[]): void {
    this.phase = "done";
    events.push({ type: "end" });
  }
}
