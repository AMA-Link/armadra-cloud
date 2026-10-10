/**
 * 维护页 Worker（`armadra-relay-maintenance`，配置在 `wrangler.maintenance.jsonc`）：拉闸时
 * `scripts/kill-switch.mjs maintenance` 把 `relay.armadra.com`（或 staging）的 Custom Domain 切到
 * 它，所有请求答 503 `unavailable`（带 `Retry-After`），不碰任何存储、不调任何对象、不计 DO 用量。
 * 要预先部署好（`wrangler deploy -c wrangler.maintenance.jsonc`），没有路由，平时不收流量。
 *
 * 不依赖运行时包：拉闸要在运行时本身出问题时也能用。错误码与线上形状同注册表（`unavailable`）。
 */
const RETRY_AFTER_S = 300;

const HEADERS: Record<string, string> = {
  "content-type": "application/json; charset=utf-8",
  "cache-control": "no-store",
  "retry-after": String(RETRY_AFTER_S),
  "access-control-allow-origin": "*",
  "access-control-allow-methods": "GET, POST, PATCH, DELETE, OPTIONS",
  "access-control-allow-headers": "authorization, content-type",
};

export function maintenanceResponse(request: Request): Response {
  if (request.method === "OPTIONS")
    return new Response(null, { status: 204, headers: HEADERS });
  if (request.headers.get("upgrade")?.toLowerCase() === "websocket")
    return new Response(null, { status: 503, headers: HEADERS });
  const body =
    request.method === "HEAD"
      ? null
      : JSON.stringify({
          code: "unavailable",
          message: "Relay is under maintenance",
          requestId: crypto.randomUUID(),
          details: { retryAfterMs: RETRY_AFTER_S * 1000 },
        });
  return new Response(body, { status: 503, headers: HEADERS });
}

export default {
  fetch: (request: Request) => maintenanceResponse(request),
} satisfies ExportedHandler;
