/**
 * 边缘到隧道的请求头处理（中继设计 §3.4）。`OPEN` 只带元数据；`DATA` 里是完整的
 * HTTP/1.1 字节，由源（core）用 Node 自己的 HTTP 解析器解析——中继不理解业务语义。
 * 请求与响应的字节编解码在协议包的 `http1`。
 *
 * 转发的头：保留原顺序与大小写、保留 `Host`；剥掉中继令牌（`Armadra-Relay-Token`、
 * `armadra-relay.*` 子协议）与逐跳头；`X-Forwarded-For` 设为客户端 IP、
 * `X-Forwarded-Proto: https`（客户端带来的同名头与 `Forwarded` / `X-Real-IP` 一律丢掉，
 * 源只信 `OPEN.remoteIp`）。HTTP 请求加 `connection: close`：一条流只跑一个请求。
 */
import { RELAY_TOKEN_HEADER } from "@armadra/platform-protocol";
import { serializeRequestHead as encodeHead } from "@armadra/platform-protocol/http1";

export type HeaderPair = [string, string];

/**
 * 按小写名取值的请求头（Node 的 `IncomingHttpHeaders` 与之结构相同；Workers 由
 * `Headers` 构造）。同名多行时 Node 已用 `, ` 合并，只有 `set-cookie` 是数组。
 */
export type HeaderBag = Readonly<Record<string, string | string[] | undefined>>;

/** 请求方向不转的头（小写）。`connection` / `upgrade` 在 WS 升级时保留。 */
const DROP_REQUEST = new Set([
  RELAY_TOKEN_HEADER,
  "keep-alive",
  "proxy-connection",
  "proxy-authorization",
  "transfer-encoding",
  "te",
  "trailer",
  "expect",
  "x-forwarded-for",
  "x-forwarded-proto",
  "x-forwarded-host",
  "x-real-ip",
  "forwarded",
]);

/** 响应方向不转的逐跳头。 */
const DROP_RESPONSE = new Set([
  "connection",
  "keep-alive",
  "proxy-connection",
  "transfer-encoding",
  "te",
  "trailer",
  "upgrade",
]);

export function pairs(rawHeaders: readonly string[]): HeaderPair[] {
  const out: HeaderPair[] = [];
  for (let index = 0; index + 1 < rawHeaders.length; index += 2)
    out.push([rawHeaders[index] as string, rawHeaders[index + 1] as string]);
  return out;
}

export function flatten(headers: readonly HeaderPair[]): string[] {
  return headers.flat();
}

export interface ForwardOptions {
  remoteIp: string;
  /** WS 升级：保留 `connection` / `upgrade`，子协议换成 `subprotocols`（剥过令牌的）。 */
  upgrade?: { subprotocols: string[] };
}

export function forwardRequestHeaders(
  rawHeaders: readonly string[],
  options: ForwardOptions,
): HeaderPair[] {
  const out: HeaderPair[] = [];
  let protocolsWritten = false;
  for (const [name, value] of pairs(rawHeaders)) {
    const lower = name.toLowerCase();
    if (DROP_REQUEST.has(lower)) continue;
    if (!options.upgrade && (lower === "connection" || lower === "upgrade"))
      continue;
    if (lower === "sec-websocket-protocol") {
      if (!options.upgrade || protocolsWritten) continue;
      protocolsWritten = true;
      if (options.upgrade.subprotocols.length > 0)
        out.push([name, options.upgrade.subprotocols.join(", ")]);
      continue;
    }
    out.push([name, value]);
  }
  out.push(["x-forwarded-for", options.remoteIp]);
  out.push(["x-forwarded-proto", "https"]);
  if (!options.upgrade) out.push(["connection", "close"]);
  return out;
}

export function forwardResponseHeaders(
  rawHeaders: readonly string[],
): HeaderPair[] {
  return pairs(rawHeaders).filter(
    ([name]) => !DROP_RESPONSE.has(name.toLowerCase()),
  );
}

/**
 * 请求行 + 头（WS 升级直接写进流；Node 的 HTTP 请求由 `http.request` 自己序列化，格式相同）。
 * 编码在协议包 `http1`（latin1 原样写回）；这里包成 `Buffer`（全局对象，Node 与 Workers 的
 * `nodejs_compat` 都有，不经 import），直接写给 Node socket。
 */
export function serializeRequestHead(
  method: string,
  path: string,
  headers: readonly HeaderPair[],
): Buffer {
  const bytes = encodeHead(method, path, headers);
  return Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
}

const encoder = new TextEncoder();

/** OPEN 载荷 headers 的 UTF-8 总字节（协议上限 `openHeadersMaxBytes`）。 */
export function headersBytes(headers: readonly HeaderPair[]): number {
  let total = 0;
  for (const [name, value] of headers)
    total += encoder.encode(name).length + encoder.encode(value).length;
  return total;
}
