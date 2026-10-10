/**
 * 托管页面（中继设计 §2.1）：`/app/*` 与 `/j/<linkId>` 答 Armadra `apps/web` 的构建产物（Workers
 * Static Assets，`run_worker_first`，由这里决定答什么），规则与 Node 中继 `personal/web.ts` 相同：
 *
 * - `/` 与 `/app` 308 到 `/app/`；`/app/<文件>` 存在答文件，否则答 `index.html`（SPA）；产物按根路径
 *   引用资源，根下真实存在的文件也直接答；
 * - `index.html` 不缓存、带页面 CSP；带哈希的资源永久缓存；
 * - 没有产物时 `/app/*` 答错误页（503），`/j/<linkId>` 答「在 Armadra 中打开」的最小落地页——
 *   两页的文案、样式与 CSP 取 `@armadra/cloud-shared/pages`，与 Node 中继同一份。
 */
import {
  hashedAsset,
  PAGE_BASE_HEADERS,
  PAGE_CSP,
  pickLocale,
  renderPage,
  type PageKind,
} from "@armadra/cloud-shared/pages";

const LINK_PATH = /^\/j\/([0-9a-f]{16})\/?$/;

/** 这个路径可能归托管页面（GET / HEAD 才答）。 */
export function isPagePath(path: string): boolean {
  return (
    path === "/" ||
    path === "/app" ||
    path.startsWith("/app/") ||
    LINK_PATH.test(path)
  );
}

function ownPage(
  request: Request,
  status: number,
  kind: PageKind,
  linkId?: string,
): Response {
  const { html, csp } = renderPage(
    pickLocale(request.headers.get("accept-language") ?? undefined),
    kind,
    linkId,
  );
  return new Response(request.method === "HEAD" ? null : html, {
    status,
    headers: {
      ...PAGE_BASE_HEADERS,
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      "content-security-policy": csp,
    },
  });
}

/** 静态产物的响应加上与 Node 中继相同的头。 */
function decorate(response: Response, path: string): Response {
  const headers = new Headers(response.headers);
  for (const [name, value] of Object.entries(PAGE_BASE_HEADERS))
    headers.set(name, value);
  const isHtml =
    (headers.get("content-type") ?? "").startsWith("text/html") ||
    path.endsWith(".html");
  const name = path.split("/").pop() ?? "";
  headers.set(
    "cache-control",
    isHtml
      ? "no-store"
      : hashedAsset(name)
        ? "public, max-age=31536000, immutable"
        : "no-cache",
  );
  if (isHtml) headers.set("content-security-policy", PAGE_CSP);
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

async function asset(
  assets: Fetcher,
  request: Request,
  path: string,
): Promise<Response | null> {
  const url = new URL(request.url);
  url.pathname = path;
  url.search = "";
  const response = await assets.fetch(
    new Request(url.toString(), {
      method: request.method,
      headers: request.headers,
    }),
  );
  if (response.status === 404) return null;
  // 产物服务自己会把 `/x/index.html` 307 到 `/x/`：按目标再取一次。
  if (response.status >= 300 && response.status < 400) {
    const location = response.headers.get("location");
    if (location) {
      const next = new URL(location, url).pathname;
      if (next !== path) return asset(assets, request, next);
    }
    return null;
  }
  return decorate(response, path);
}

/** 托管页面与根下的产物文件；不归这里答 null。 */
export async function servePages(
  request: Request,
  assets: Fetcher | undefined,
): Promise<Response | null> {
  if (request.method !== "GET" && request.method !== "HEAD") return null;
  const path = new URL(request.url).pathname;
  if (path === "/" || path === "/app")
    return new Response(null, {
      status: 308,
      headers: { ...PAGE_BASE_HEADERS, location: "/app/" },
    });
  const index = async () =>
    assets ? await asset(assets, request, "/index.html") : null;
  const link = LINK_PATH.exec(path);
  if (link?.[1])
    return (await index()) ?? ownPage(request, 200, "join", link[1]);
  if (path.startsWith("/app/")) {
    if (!assets) return ownPage(request, 503, "unavailable");
    const relative = path.slice("/app".length);
    if (relative !== "/" && !relative.endsWith("/")) {
      const file = await asset(assets, request, relative);
      if (file) return file;
    }
    return (await index()) ?? ownPage(request, 503, "unavailable");
  }
  if (!assets || path.endsWith("/")) return null;
  return asset(assets, request, path);
}
