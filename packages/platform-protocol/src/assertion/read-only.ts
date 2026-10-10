/**
 * 只读中继令牌（`ro: true`，0.3.0 起）在边缘放行哪些请求（SaaS 设计 §3.5）。纵深防御：
 * core 仍是最终判定点，这里只挡住明显的写。
 *
 * 放行：
 * - 安全方法 `GET` / `HEAD` / `OPTIONS`（含 WebSocket 升级，升级请求总是 `GET`）；
 * - 身份域路径（任意方法）：`/api/identity/` 与 `/api/rpc/identity/` 之下——登录、刷新、
 *   WS 票、登出都在这里，只读访客也要用。
 *
 * 其余答 `403 forbidden`。路径按转给源的那一段算（不含 `/s/<id>` 前缀），只看 `?` 之前；
 * 含 `.` / `..` 段、`//`、`%`、`\` 的路径不算身份域（避免规范化前后理解不一致）。
 */

export const READ_ONLY_SAFE_METHODS = ["GET", "HEAD", "OPTIONS"] as const;

export const READ_ONLY_IDENTITY_PREFIXES = [
  "/api/identity/",
  "/api/rpc/identity/",
] as const;

function pathnameOf(path: string): string {
  const cut = path.search(/[?#]/);
  return cut === -1 ? path : path.slice(0, cut);
}

/** 身份域路径（规范、无歧义的写法才算）。 */
export function isIdentityPath(path: string): boolean {
  const pathname = pathnameOf(path);
  if (/[%\\]/.test(pathname) || pathname.includes("//")) return false;
  if (
    pathname.split("/").some((segment) => segment === "." || segment === "..")
  )
    return false;
  return READ_ONLY_IDENTITY_PREFIXES.some((prefix) =>
    pathname.startsWith(prefix),
  );
}

/** 只读令牌下这个请求能不能过边缘。 */
export function isReadOnlyAllowed(method: string, path: string): boolean {
  const upper = method.toUpperCase();
  if ((READ_ONLY_SAFE_METHODS as readonly string[]).includes(upper))
    return true;
  return isIdentityPath(path);
}
