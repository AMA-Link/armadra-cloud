/**
 * 中继的错误输出：`{ code, message, requestId? }`，状态码查协议包注册表；code 只能是
 * 注册表里的码。message 是固定的英文短句——界面按 code 取 i18n 文案，不展示 message。
 */
import type { Duplex } from "node:stream";
import type { ServerResponse } from "node:http";
import { STATUS_CODES } from "node:http";

import { sendError } from "@armadra/cloud-shared/http";
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";

const MESSAGES: Partial<Record<ErrorCode, string>> = {
  not_found: "Not found",
  unauthenticated: "Unauthenticated",
  rate_limited: "Too many requests",
  relay_token_invalid: "Relay token missing or invalid",
  relay_source_mismatch: "Relay token is for another source",
  source_offline: "Source is offline",
  limit_reached: "Too many concurrent streams",
  bad_request: "Bad request",
  internal: "Internal error",
};

export function errorMessage(code: ErrorCode): string {
  return MESSAGES[code] ?? code;
}

export function sendRelayError(
  res: ServerResponse,
  code: ErrorCode,
  options: { requestId?: string; retryAfterMs?: number } = {},
): void {
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

/** 原始 HTTP/1.1 错误响应的字节（升级请求被拒、或升级途中源下线时写给裸 socket）。 */
export function rawErrorResponse(code: ErrorCode): Buffer {
  const status = errorStatus(code);
  const body = JSON.stringify({ code, message: errorMessage(code) });
  return Buffer.from(
    `HTTP/1.1 ${status} ${STATUS_CODES[status] ?? ""}\r\n` +
      "content-type: application/json; charset=utf-8\r\n" +
      `content-length: ${Buffer.byteLength(body)}\r\n` +
      "cache-control: no-store\r\nconnection: close\r\n\r\n" +
      body,
    "utf8",
  );
}

export function rejectUpgrade(socket: Duplex, code: ErrorCode): void {
  if (socket.destroyed) return;
  socket.end(rawErrorResponse(code));
}
