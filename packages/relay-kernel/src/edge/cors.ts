/**
 * 源地址上的 CORS 预检由中继自己回答（契约 §7）。
 *
 * 浏览器的预检不带自定义头，带不了中继令牌；而真正的请求要带 `Armadra-Relay-Token`，
 * 这个头只有中继认识，core 的预检答案里不会列它。所以预检不进隧道：中继按来源名单
 * 答 204，不验令牌、不找隧道、不开流——答案与源在不在线、登没登记都无关，借预检
 * 探不出源的状态，也不花源的任何资源。
 *
 * 规则与 core 隧道准入的 CORS 一致：只对名单里的来源按 Origin 回显（从不用 `*`、
 * 不发 `allow-credentials`），带 `Vary: Origin`；方法与头取 core 放行的那几个，
 * 外加中继令牌头。名单 = 登记时发给源的可信来源 ∪ 原生 App 的两个来源。
 */
import type { IncomingMessage } from "node:http";

import { RELAY_TOKEN_HEADER } from "@armadra/platform-protocol";

/** 原生 App 的 WebView 来源（iOS / Android），与 core 的名单相同。 */
export const NATIVE_APP_ORIGINS: readonly string[] = [
  "capacitor://localhost",
  "https://localhost",
];

/** 与 core 隧道准入答的方法相同。 */
export const PREFLIGHT_METHODS = "GET,POST,PUT,PATCH,DELETE,OPTIONS";
/** core 隧道准入放行的头，加上中继令牌头。 */
export const PREFLIGHT_HEADERS = `authorization, content-type, x-armadra-csrf, ${RELAY_TOKEN_HEADER}`;
/**
 * 预检缓存秒数：浏览器各自封顶（WebKit 10 分钟、Chromium 2 小时），给到 2 小时让
 * 能缓存久的尽量少预检。
 */
export const PREFLIGHT_MAX_AGE_S = "7200";

/** CORS 预检：`OPTIONS` + 单个 `Origin` + `Access-Control-Request-Method`。 */
export function isPreflight(req: IncomingMessage): boolean {
  return (
    req.method === "OPTIONS" &&
    typeof req.headers.origin === "string" &&
    typeof req.headers["access-control-request-method"] === "string"
  );
}

/** http(s) 来源的规范拼法；不是来源（带路径、凭据、别的协议）答 undefined。 */
function canonical(value: string): string | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") return undefined;
  if (url.username || url.password) return undefined;
  if (url.pathname !== "/" || url.search || url.hash) return undefined;
  return url.origin;
}

/** 来源在不在名单里：原生 App 逐字比，其余按规范拼法比；浏览器发来的 Origin 必须已是规范拼法。 */
export function originAllowed(
  origin: string,
  trusted: readonly string[],
): boolean {
  if (NATIVE_APP_ORIGINS.includes(origin)) return true;
  if (canonical(origin) !== origin) return false;
  return trusted.some((entry) => canonical(entry) === origin);
}

export function preflightHeaders(origin: string): Record<string, string> {
  return {
    "access-control-allow-origin": origin,
    "access-control-allow-methods": PREFLIGHT_METHODS,
    "access-control-allow-headers": PREFLIGHT_HEADERS,
    "access-control-max-age": PREFLIGHT_MAX_AGE_S,
    vary: "origin",
    "cache-control": "no-store",
  };
}

/** 页面需要读的中继响应头（限流的重试等待）。 */
export const EXPOSED_HEADERS = "retry-after";

/**
 * 中继自己返回的错误答复（401 / 403 / 429 / 503）的 CORS 头：来源在名单里才有，
 * 按 Origin 回显、带 `Vary: Origin`、暴露 `retry-after`；名单外（或没有 Origin）不加。
 * 只加头，不影响任何鉴权判定。
 */
export function errorCorsHeaders(
  origin: string | null,
  trusted: readonly string[],
): Record<string, string> | undefined {
  if (origin === null || !originAllowed(origin, trusted)) return undefined;
  return {
    "access-control-allow-origin": origin,
    "access-control-expose-headers": EXPOSED_HEADERS,
    vary: "origin",
  };
}
