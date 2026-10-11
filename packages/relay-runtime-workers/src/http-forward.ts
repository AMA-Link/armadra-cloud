/**
 * DO 里的客户端 HTTP 转发（中继设计 §2.4）：Workers 没有可以挂到自定义连接上的 HTTP 客户端，
 * 请求由协议包的 `http1` 写成 HTTP/1.1 字节进隧道流，源的响应由同一模块的 `ResponseParser` 增量解析
 * 成 `Response`。判定（地址、令牌、限流、开流）在内核的 `EdgePolicy`，这里只管字节。
 *
 * - 请求头：`forwardRequestHeaders`（剥令牌与逐跳头、`X-Forwarded-*`、`connection: close`），再剥掉
 *   Cloudflare 自己加的 `cf-*` / `cdn-loop` / `true-client-ip`（客户端 IP 已在 `OPEN.remoteIp`）。
 * - 请求体：有合法 `Content-Length` 就按长度写，否则 `chunked`；按流信用背压，不预读整个体。
 *   我们这一侧的 END 等响应收全才发（源不一定允许半关闭）。
 * - 响应：头解析出来才答 `Response`；之前流被重置答 503 `source_offline`，响应不合法重置流答 502
 *   `bad_gateway`。`HEAD` 与 `204` / `304` 没有体；有 `Content-Length` 的体经 `FixedLengthStream`，
 *   客户端照样拿到长度（`<video>` 的 `Range` 播放要它）。
 * - 客户端中途断开：响应体被取消 → 流 `RST clientGone`。
 *
 * 不解析正文、不缓存、不记路径与头。
 */
import {
  encodeChunk,
  encodeRequestHead,
  Http1ParseError,
  Http1RequestError,
  LAST_CHUNK,
  requestFraming,
  ResponseParser,
  type ResponseEvent,
  type ResponseHead,
} from "@armadra/platform-protocol/http1";
import { RST } from "@armadra/platform-protocol/tunnel";
import type { AddressTarget } from "@armadra/relay-kernel/edge/addressing";
import {
  flatten,
  forwardRequestHeaders,
  forwardResponseHeaders,
  type HeaderPair,
} from "@armadra/relay-kernel/edge/forward";
import type {
  EdgePolicy,
  EdgeRequest,
} from "@armadra/relay-kernel/edge/policy";
import { errorName, type RelayLog } from "@armadra/relay-kernel/log";
import type { Tunnel } from "@armadra/relay-kernel/tunnel/tunnel";

import type { TunnelEndpoint } from "./endpoint.ts";
import { relayError } from "./responses.ts";

/** Cloudflare 边缘加给 Worker 的头：不转给源。 */
const CLOUDFLARE_HEADERS = /^(?:cf-|cdn-loop$|true-client-ip$)/;

/** `Request.headers` → 原始头数组（名字已是小写；同名多行已被合并）。 */
export function rawHeadersOf(
  headers: Headers,
  drop: (name: string) => boolean = () => false,
): string[] {
  const raw: string[] = [];
  for (const [name, value] of headers) {
    if (CLOUDFLARE_HEADERS.test(name) || drop(name)) continue;
    raw.push(name, value);
  }
  return raw;
}

/** 响应缓冲：一条流的窗口（256 KiB），读走才补信用。 */
const RESPONSE_HIGH_WATER = 256 * 1024;

export interface ParsedResponse {
  head: ResponseHead;
  /** 解析器接下来交出的体（`body` / `end`），或升级后的字节（`upgraded`）。 */
  body: ReadableStream<Uint8Array> | null;
  /** `101` 之后的字节由调用方接管（WS）：先到的那部分。 */
  upgraded: Uint8Array[];
}

/**
 * 在流上读源的响应：答响应头（以及体的流）。流被重置或在头之前结束 → `source_offline`；
 * 响应不合法 → 重置流、`bad_gateway`。`upgrade` 时不建体流，`101` 之后的字节交给调用方。
 */
export function readResponse(
  endpoint: TunnelEndpoint,
  method: string,
  options: { upgrade: boolean },
): Promise<
  | { ok: true; response: ParsedResponse }
  | { ok: false; code: "source_offline" | "bad_gateway" }
> {
  const parser = new ResponseParser({ method });
  let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
  let settled = false;
  let finished = false;
  const upgraded: Uint8Array[] = [];
  return new Promise((resolve) => {
    const bodyStream = options.upgrade
      ? null
      : new ReadableStream<Uint8Array>(
          {
            start: (c) => {
              controller = c;
            },
            pull: () => endpoint.read(),
            cancel: () => endpoint.destroy(RST.clientGone),
          },
          new ByteLengthQueuingStrategy({ highWaterMark: RESPONSE_HIGH_WATER }),
        );
    const fail = (code: "source_offline" | "bad_gateway", error?: unknown) => {
      if (code === "bad_gateway") endpoint.destroy(RST.protocol);
      if (!settled) {
        settled = true;
        resolve({ ok: false, code });
        return;
      }
      if (!finished) {
        finished = true;
        try {
          controller?.error(error ?? new Error(code));
        } catch {
          // 已经关了。
        }
      }
    };
    const apply = (events: ResponseEvent[]): boolean => {
      let wantMore = true;
      for (const event of events) {
        switch (event.type) {
          case "head": {
            settled = true;
            const noBody =
              event.upgrade ||
              method.toUpperCase() === "HEAD" ||
              event.head.status === 204 ||
              event.head.status === 304;
            resolve({
              ok: true,
              response: {
                head: event.head,
                body: noBody ? null : bodyStream,
                upgraded,
              },
            });
            if (noBody && !event.upgrade) {
              finished = true;
              controller?.close();
              void endpoint.finish().catch(() => {});
            }
            break;
          }
          case "body":
            if (controller && !finished) {
              controller.enqueue(event.bytes);
              wantMore = (controller.desiredSize ?? 0) > 0;
            }
            break;
          case "end":
            if (!finished) {
              finished = true;
              controller?.close();
            }
            // 响应收全了才结束我们这一侧：源若不允许半关闭（Node 的 http.Server），先发 END
            // 会让它提前关掉还没写完的响应。与 Node 中继的 http.request 行为一致。
            void endpoint.finish().catch(() => {});
            break;
          case "upgraded":
            upgraded.push(event.bytes);
            break;
        }
      }
      return wantMore;
    };
    endpoint.attach({
      data(chunk) {
        try {
          return apply(parser.feed(chunk));
        } catch (error) {
          fail(
            error instanceof Http1ParseError ? "bad_gateway" : "source_offline",
            error,
          );
          return false;
        }
      },
      end() {
        try {
          apply(parser.finish());
        } catch (error) {
          fail(
            error instanceof Http1ParseError && settled
              ? "bad_gateway"
              : "source_offline",
            error,
          );
        }
      },
      reset(error) {
        fail("source_offline", error);
      },
    });
  });
}

/** 源的响应头 → Workers 的 `Headers`（剥逐跳头，`set-cookie` 等多行原样 append）。 */
export function responseHeaders(head: ResponseHead): Headers {
  const headers = new Headers();
  for (const [name, value] of forwardResponseHeaders(flatten(head.headers)))
    headers.append(name, value);
  return headers;
}

function contentLengthOf(pairs: readonly HeaderPair[]): number | null {
  for (const [name, value] of pairs)
    if (
      name.toLowerCase() === "content-length" &&
      /^\d{1,15}$/.test(value.trim())
    )
      return Number(value.trim());
  return null;
}

/** 解析好的响应 → `Response`（有长度的体经 `FixedLengthStream` 保住 `Content-Length`）。 */
export function toResponse(parsed: ParsedResponse): Response {
  const { head } = parsed;
  const headers = responseHeaders(head);
  let body: ReadableStream<Uint8Array> | null = parsed.body;
  const length = contentLengthOf(head.headers);
  if (body && length !== null) {
    const fixed = new FixedLengthStream(length);
    void body.pipeTo(fixed.writable).catch(() => {});
    body = fixed.readable;
  }
  return new Response(body, {
    status: head.status,
    statusText: head.statusText,
    headers,
  });
}

/**
 * 请求体按定界写进流（不发 END：请求的结束由定界说明，END 等响应收全再发）；出错（客户端
 * 上传中断）就重置流。
 */
async function pumpBody(
  endpoint: TunnelEndpoint,
  body: ReadableStream<Uint8Array> | null,
  chunked: boolean,
): Promise<void> {
  if (!body) return;
  const reader = body.getReader();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    if (value.length === 0) continue;
    await endpoint.write(chunked ? encodeChunk(value) : value);
  }
  if (chunked) await endpoint.write(LAST_CHUNK);
}

export interface HttpForwardInput {
  request: Request;
  edgeRequest: EdgeRequest;
  target: AddressTarget;
  tunnel: Tunnel<TunnelEndpoint>;
  remoteIp: string;
  policy: EdgePolicy<TunnelEndpoint, WebSocket>;
  log: RelayLog;
}

export async function forwardHttp(input: HttpForwardInput): Promise<Response> {
  const { request, edgeRequest, target, tunnel, remoteIp, policy, log } = input;
  const cors = policy.errorCors(edgeRequest, target.sourceId);
  const headers = forwardRequestHeaders(rawHeadersOf(request.headers), {
    remoteIp,
  });
  const hasBody =
    request.body !== null &&
    request.method !== "GET" &&
    request.method !== "HEAD";
  let framing;
  try {
    framing = requestFraming(headers, hasBody);
  } catch (error) {
    if (error instanceof Http1RequestError)
      return relayError("bad_request", { cors });
    throw error;
  }
  const opened = policy.open(
    tunnel,
    "http",
    edgeRequest,
    target,
    headers,
    remoteIp,
  );
  if (!opened.ok) return relayError(opened.code, { cors });
  const endpoint = opened.stream;
  const response = readResponse(endpoint, request.method, { upgrade: false });
  void endpoint
    .write(encodeRequestHead(request.method, target.path, headers, framing))
    .then(() =>
      pumpBody(
        endpoint,
        hasBody ? request.body : null,
        framing.kind === "chunked",
      ),
    )
    .catch((error: unknown) => {
      endpoint.destroy(RST.clientGone);
      log.debug("边缘流结束", {
        sourceId: target.sourceId,
        streamId: endpoint.streamId,
        error: errorName(error),
      });
    });
  const result = await response;
  if (!result.ok) return relayError(result.code, { cors });
  if (result.response.head.status === 101) {
    endpoint.destroy(RST.protocol);
    return relayError("bad_gateway", { cors });
  }
  try {
    return toResponse(result.response);
  } catch {
    // 状态码不在 200–599、原因短语带非法字符……：Workers 造不出这个响应。
    endpoint.destroy(RST.protocol);
    return relayError("bad_gateway", { cors });
  }
}
