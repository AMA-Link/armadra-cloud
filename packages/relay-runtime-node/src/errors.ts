/**
 * 中继错误答复的 Node 写法：写进 `ServerResponse`，或把裸 HTTP/1.1 字节写给升级前的
 * socket。文案、状态行与字节在内核的 `http/errors.ts`。
 */
import type { ServerResponse } from "node:http";
import type { Duplex } from "node:stream";

import { sendError } from "@armadra/cloud-shared/http";
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";
import {
  errorMessage,
  rawErrorResponse,
} from "@armadra/relay-kernel/http/errors";

export function sendRelayError(
  res: ServerResponse,
  code: ErrorCode,
  options: {
    requestId?: string;
    retryAfterMs?: number;
    /** 名单内来源的 CORS 头（边缘算好），让跨源页面读得到错误码。 */
    cors?: Record<string, string>;
  } = {},
): void {
  for (const [name, value] of Object.entries(options.cors ?? {}))
    res.setHeader(name, value);
  if (options.retryAfterMs !== undefined)
    res.setHeader(
      "retry-after",
      String(Math.max(1, Math.ceil(options.retryAfterMs / 1000))),
    );
  sendError(res, errorStatus(code), {
    code,
    message: errorMessage(code),
    ...(options.requestId ? { requestId: options.requestId } : {}),
  });
}

/** {@link rawErrorResponse} 的 `Buffer`（裸 socket 写）。 */
export function rawErrorBytes(
  code: ErrorCode,
  cors?: Record<string, string>,
): Buffer {
  const bytes = rawErrorResponse(code, cors);
  return Buffer.from(bytes.buffer, bytes.byteOffset, bytes.length);
}

export function rejectUpgrade(
  socket: Duplex,
  code: ErrorCode,
  cors?: Record<string, string>,
): void {
  if (socket.destroyed) return;
  socket.end(rawErrorBytes(code, cors));
}
