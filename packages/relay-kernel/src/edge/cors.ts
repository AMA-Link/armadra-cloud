/**
 * 中继自己答的错误（404 / 401 / 403 / 429 / 503）的 CORS 头（契约 §7）。
 *
 * 规则与 core 隧道准入的 CORS 一致：只对名单里的来源按 Origin 回显（从不用 `*`、
 * 不发 `allow-credentials`），带 `Vary: Origin`。名单 = 登记时发给源的可信来源 ∪ 原生
 * App 的两个来源。0.5.0 起中继不转明文的应用请求，也就不再答 CORS 预检（源地址上的
 * `OPTIONS` 同样 404）。
 */
/** 原生 App 的 WebView 来源（iOS / Android），与 core 的名单相同。 */
export const NATIVE_APP_ORIGINS: readonly string[] = [
  "capacitor://localhost",
  "https://localhost",
];

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
