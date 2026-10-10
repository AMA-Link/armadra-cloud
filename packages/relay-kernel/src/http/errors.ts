/**
 * 中继的错误答复：`{ code, message, requestId? }`，状态码查协议包注册表；code 只能是
 * 注册表里的码。message 是固定的英文短句——界面按 code 取 i18n 文案，不展示 message。
 *
 * 这里只有与运行时无关的部分（文案、状态行、裸 HTTP/1.1 字节）；写进 Node
 * `ServerResponse` / 裸 socket 的在 `@armadra/relay-runtime-node` 的 `errors.ts`。
 */
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";

const MESSAGES: Partial<Record<ErrorCode, string>> = {
  not_found: "Not found",
  unauthenticated: "Unauthenticated",
  rate_limited: "Too many requests",
  relay_token_invalid: "Relay token missing or invalid",
  relay_source_mismatch: "Relay token is for another source",
  source_offline: "Source is offline",
  limit_reached: "Too many concurrent streams",
  bad_gateway: "Source sent an invalid response",
  bad_request: "Bad request",
  internal: "Internal error",
  forbidden: "Forbidden",
  unavailable: "Unavailable",
  credentials_invalid: "Account or password is incorrect",
  account_locked: "Too many failed sign-in attempts; try again later",
  session_expired: "Session expired",
  session_revoked: "Session revoked",
  password_too_short: "Password is shorter than the minimum",
  password_too_long: "Password is too long",
  password_contains_name: "Password must not contain the account name",
  password_too_common: "Password is on the list of common passwords",
  registration_token_invalid: "Registration token missing, used or expired",
  source_taken: "Source id is registered with another key",
  source_revoked: "Source revoked",
  source_access_denied: "No access to this source",
  protocol_unsupported: "Protocol version not supported",
  link_invalid: "Link not found or revoked",
  link_expired: "Link expired",
  link_exhausted: "Link has no uses left",
  link_secret_invalid: "Link secret missing or wrong",
};

export function errorMessage(code: ErrorCode): string {
  return MESSAGES[code] ?? code;
}

/**
 * 状态行的原因短语（与 Node `http.STATUS_CODES` 逐字相同，覆盖注册表与边缘用到的状态码；
 * 单测对着 Node 的表核对）。不认识的答空串。
 */
const STATUS_TEXT: Readonly<Record<number, string>> = {
  101: "Switching Protocols",
  200: "OK",
  204: "No Content",
  400: "Bad Request",
  401: "Unauthorized",
  403: "Forbidden",
  404: "Not Found",
  409: "Conflict",
  410: "Gone",
  426: "Upgrade Required",
  428: "Precondition Required",
  429: "Too Many Requests",
  500: "Internal Server Error",
  501: "Not Implemented",
  502: "Bad Gateway",
  503: "Service Unavailable",
};

export function statusText(status: number): string {
  return STATUS_TEXT[status] ?? "";
}

const encoder = new TextEncoder();

/** 原始 HTTP/1.1 错误响应的字节（升级请求被拒、或升级途中源下线时写给裸连接）。 */
export function rawErrorResponse(
  code: ErrorCode,
  cors?: Record<string, string>,
): Uint8Array {
  const status = errorStatus(code);
  const body = encoder.encode(
    JSON.stringify({ code, message: errorMessage(code) }),
  );
  const head = encoder.encode(
    `HTTP/1.1 ${status} ${statusText(status)}\r\n` +
      "content-type: application/json; charset=utf-8\r\n" +
      `content-length: ${body.length}\r\n` +
      "cache-control: no-store\r\nconnection: close\r\n" +
      Object.entries(cors ?? {})
        .map(([name, value]) => `${name}: ${value}\r\n`)
        .join("") +
      "\r\n",
  );
  const out = new Uint8Array(head.length + body.length);
  out.set(head, 0);
  out.set(body, head.length);
  return out;
}
