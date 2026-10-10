/**
 * HTTP/1.1 请求的字节编码（中继设计 §2.4 的 `http1/request`），与运行时无关。运行时自己
 * 终止客户端 HTTP（Workers）时用它把请求写进隧道流：请求行 + 头（{@link serializeRequestHead}，
 * 头已经过 `forwardRequestHeaders` 剥掉逐跳头并加了 `connection: close`）+ 体。
 *
 * 体的定界：请求带合法的 `Content-Length` 就原样按长度写；有体而没有长度时加
 * `transfer-encoding: chunked`，每块用 {@link encodeChunk} 包一层，最后写 {@link LAST_CHUNK}；
 * 没有体什么都不加。切成 64 KiB `DATA` 与信用背压由隧道流负责。黄金字节在协议包的
 * `fixtures/http1/`。
 */
import { serializeRequestHead, type HeaderPair } from "../edge/forward.ts";

export type BodyFraming =
  | { kind: "none" }
  | { kind: "length"; length: number }
  | { kind: "chunked" };

export class Http1RequestError extends Error {
  constructor() {
    super("invalid content-length");
    this.name = "Http1RequestError";
  }
}

/**
 * 选体的定界。`Content-Length` 不是十进制或多个值不一致抛 {@link Http1RequestError}
 * （调用方答 400 `bad_request`）。
 */
export function requestFraming(
  headers: readonly HeaderPair[],
  hasBody: boolean,
): BodyFraming {
  const lengths: string[] = [];
  for (const [name, value] of headers)
    if (name.toLowerCase() === "content-length")
      lengths.push(...value.split(",").map((item) => item.trim()));
  if (lengths.length > 0) {
    const first = lengths[0] as string;
    if (!/^\d{1,15}$/.test(first) || lengths.some((item) => item !== first))
      throw new Http1RequestError();
    return { kind: "length", length: Number(first) };
  }
  return hasBody ? { kind: "chunked" } : { kind: "none" };
}

/** 请求行 + 头；`chunked` 时在末尾补 `transfer-encoding: chunked`。 */
export function encodeRequestHead(
  method: string,
  path: string,
  headers: readonly HeaderPair[],
  framing: BodyFraming,
): Uint8Array {
  return serializeRequestHead(
    method,
    path,
    framing.kind === "chunked"
      ? [...headers, ["transfer-encoding", "chunked"]]
      : headers,
  );
}

const encoder = new TextEncoder();

/** 一块分块体：`<十六进制长度>\r\n<字节>\r\n`；空块答空（空块会被当成结束）。 */
export function encodeChunk(bytes: Uint8Array): Uint8Array {
  if (bytes.length === 0) return new Uint8Array(0);
  const size = encoder.encode(`${bytes.length.toString(16)}\r\n`);
  const out = new Uint8Array(size.length + bytes.length + 2);
  out.set(size, 0);
  out.set(bytes, size.length);
  out[out.length - 2] = 0x0d;
  out[out.length - 1] = 0x0a;
  return out;
}

/** 分块体的结束：`0\r\n\r\n`（不带尾部头）。 */
export const LAST_CHUNK: Uint8Array = encoder.encode("0\r\n\r\n");
