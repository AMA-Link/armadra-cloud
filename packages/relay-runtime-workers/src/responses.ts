/**
 * 中继自己的 HTTP 答复（Request / Response 版）：错误一律 `{ code, message, requestId? }`，状态码
 * 查协议包注册表，文案取内核的固定英文短句（界面按 `code` 取 i18n）。
 */
import {
  PROTOCOL_HEADER,
  protocolHeaderValue,
} from "@armadra/platform-protocol";
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";
import { errorMessage } from "@armadra/relay-kernel/http/errors";

export interface ErrorOptions {
  retryAfterMs?: number;
  /** 补给跨源页面的 CORS 头（`errorCorsHeaders`）。 */
  cors?: Record<string, string>;
  requestId?: string;
  /** 额外的头（`/v1` 的 `access-control-*`）。 */
  headers?: Record<string, string>;
}

export function jsonResponse(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
      [PROTOCOL_HEADER]: protocolHeaderValue(),
      ...headers,
    },
  });
}

export function relayError(
  code: ErrorCode,
  options: ErrorOptions = {},
): Response {
  const headers: Record<string, string> = {
    ...(options.cors ?? {}),
    ...(options.headers ?? {}),
  };
  if (options.retryAfterMs !== undefined)
    headers["retry-after"] = String(
      Math.max(1, Math.ceil(options.retryAfterMs / 1000)),
    );
  return jsonResponse(
    errorStatus(code),
    {
      code,
      message: errorMessage(code),
      ...(options.requestId === undefined
        ? {}
        : { requestId: options.requestId }),
      ...(options.retryAfterMs === undefined
        ? {}
        : { details: { retryAfterMs: options.retryAfterMs } }),
    },
    headers,
  );
}

/** 是 WebSocket 升级请求（`Upgrade: websocket`）。 */
export function isUpgrade(request: Request): boolean {
  return (request.headers.get("upgrade") ?? "").toLowerCase() === "websocket";
}

/**
 * 升级被拒：浏览器读不到升级失败的状态码，所以先完成升级、再以关闭码结束（与 Node 中继相同）；
 * 回选客户端给的第一个子协议，浏览器才接受这次握手。
 */
export function refuseWithClose(
  request: Request,
  closeCode: number,
  reason: string,
): Response {
  const pair = new WebSocketPair();
  const [client, server] = [pair[0], pair[1]];
  server.accept();
  server.close(closeCode, reason);
  const offered = (request.headers.get("sec-websocket-protocol") ?? "")
    .split(",")
    .map((value) => value.trim())
    .filter(Boolean)[0];
  return new Response(null, {
    status: 101,
    webSocket: client,
    headers: offered ? { "sec-websocket-protocol": offered } : {},
  });
}
