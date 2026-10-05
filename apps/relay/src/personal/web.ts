/**
 * 托管页面（中继设计 §6.7，平台设计 §17.3）：`--web-root` 是 Armadra `apps/web` 的构建产物，
 * 本仓不另写前端。
 *
 * - `GET /app/*` 与 `GET /j/<linkId>` 答 `index.html`（SPA；`/j/` 的落地逻辑在页面里），
 *   `/app/<文件>` 存在时答文件本身；构建产物按根路径引用资源（`/assets/…`、`/icon.png`），
 *   所以根下真实存在的文件也直接答。`/` 308 到 `/app/`。
 * - 页面 CSP 与 core Gateway 的服务器版相同（`connect-src 'self'` 等）；带哈希的资源永久缓存，
 *   `index.html` 不缓存。路径先在字符串上收敛到根内，再用 realpath 复核（挡符号链接）。
 * - 没有产物时：`/app/*` 答错误页（503 `unavailable`），`/j/<linkId>` 答最小落地页（一个
 *   「在 Armadra 中打开」深链）。这两页的样式取 Armadra 设计系统的 token，文案中英两份、
 *   跟随页面同一个语言偏好（`localStorage["armadra.locale"]`），可切换。
 * - `GET /ca.crt`：自签模式发 CA（PEM）。
 */
import { randomBytes } from "node:crypto";
import { createReadStream, existsSync, statSync } from "node:fs";
import { realpath } from "node:fs/promises";
import type { IncomingMessage, ServerResponse } from "node:http";
import { extname, join, resolve, sep } from "node:path";

export const PAGE_CSP = [
  "default-src 'self'",
  "connect-src 'self'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data: blob:",
  "font-src 'self' data:",
  "worker-src 'self' blob:",
  "media-src 'self' blob:",
  "frame-src http: https: blob:",
  "frame-ancestors 'none'",
  "object-src 'none'",
  "base-uri 'self'",
  "form-action 'none'",
].join("; ");

const CONTENT_TYPES = new Map<string, string>([
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".mjs", "text/javascript; charset=utf-8"],
  [".css", "text/css; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".webmanifest", "application/manifest+json"],
  [".svg", "image/svg+xml"],
  [".png", "image/png"],
  [".jpg", "image/jpeg"],
  [".jpeg", "image/jpeg"],
  [".gif", "image/gif"],
  [".webp", "image/webp"],
  [".ico", "image/x-icon"],
  [".woff", "font/woff"],
  [".woff2", "font/woff2"],
  [".ttf", "font/ttf"],
  [".map", "application/json; charset=utf-8"],
  [".wasm", "application/wasm"],
  [".txt", "text/plain; charset=utf-8"],
]);

const BASE_HEADERS = {
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
};

export function contentTypeFor(path: string): string {
  return (
    CONTENT_TYPES.get(extname(path).toLowerCase()) ?? "application/octet-stream"
  );
}

/** Vite 的带哈希文件名（`index-D3fK9x2a.js`）可以永久缓存。 */
export function hashedAsset(name: string): boolean {
  return /-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$/.test(name);
}

// ------------------------------------------------------------ 自有页面

export type PageLocale = "zh-CN" | "en";

/** 自有页面的文案（中英同步，`web.test.ts` 守住两边键一致）。 */
export const PAGE_MESSAGES: Record<PageLocale, Record<string, string>> = {
  "zh-CN": {
    "relay.unavailable.title": "页面不可用",
    "relay.join.title": "加入共享",
    "relay.join.open": "在 Armadra 中打开",
    "relay.locale.switch": "English",
  },
  en: {
    "relay.unavailable.title": "Page unavailable",
    "relay.join.title": "Join shared source",
    "relay.join.open": "Open in Armadra",
    "relay.locale.switch": "简体中文",
  },
};

/** 按 `Accept-Language` 选初始语言（页面脚本再按存下来的偏好覆盖）。 */
export function pickLocale(acceptLanguage: string | undefined): PageLocale {
  const first =
    (acceptLanguage ?? "").split(",")[0]?.trim().toLowerCase() ?? "";
  if (first.startsWith("en")) return "en";
  return "zh-CN";
}

const escapeHtml = (value: string) =>
  value.replace(
    /[&<>"']/g,
    (char) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        char
      ] as string,
  );

/** 设计系统 §2 的 token（深色缺省，浅色覆盖）；只取这两页用得到的几项。 */
const PAGE_STYLE = `
:root{--bg:#1c1c1c;--card:#2c2c2c;--text:rgb(255 255 255/85%);--muted-foreground:rgb(255 255 255/55%);--border:rgb(255 255 255/8%);--brand-solid:#0a6fd6;--on-accent:#fff;--focus-ring:#b8dbff;--hover:rgb(255 255 255/7%);
--font-ui:-apple-system,"SF Pro Text","PingFang SC",system-ui,"Segoe UI","Microsoft YaHei",sans-serif;color-scheme:dark}
:root[data-theme="light"]{--bg:#f7f7f7;--card:#fff;--text:rgb(32 32 32/90%);--muted-foreground:rgb(32 32 32/70%);--border:rgb(32 32 32/10%);--brand-solid:#0066cc;--focus-ring:#082340;--hover:rgb(32 32 32/6%);color-scheme:light}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%;background:var(--bg);color:var(--text);font-family:var(--font-ui);-webkit-font-smoothing:antialiased}
main{min-height:100dvh;display:grid;place-items:center;padding:24px}
section{display:flex;flex-direction:column;align-items:center;gap:16px;text-align:center;max-width:28rem}
h1{margin:0;font-size:20px;line-height:28px;font-weight:600;text-wrap:balance}
a.primary{display:inline-flex;align-items:center;justify-content:center;min-height:44px;padding:0 16px;border-radius:8px;background:var(--brand-solid);color:var(--on-accent);font-size:14px;font-weight:500;text-decoration:none}
button.locale{position:fixed;top:12px;right:12px;min-height:44px;padding:0 12px;border:0;border-radius:8px;background:transparent;color:var(--muted-foreground);font:inherit;font-size:13px;cursor:pointer}
button.locale:hover{background:var(--hover)}
a.primary:focus-visible,button.locale:focus-visible{outline:3px solid color-mix(in srgb,var(--focus-ring) 50%,transparent);outline-offset:2px}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}
@media (forced-colors:active){a.primary{border:1px solid ButtonText}}
`;

/** 页面脚本：语言与主题跟随 `apps/web` 存的偏好；`#` 片段只在浏览器里拼进深链。 */
const PAGE_SCRIPT = `
(function(){
var M=JSON.parse(document.getElementById("relay-messages").textContent);
var root=document.documentElement;
function get(k){try{return localStorage.getItem(k)}catch(e){return null}}
function put(k,v){try{localStorage.setItem(k,v)}catch(e){}}
var theme=get("armadra.theme");
var dark=theme==="dark"||(theme!=="light"&&!(window.matchMedia&&matchMedia("(prefers-color-scheme: light)").matches));
root.dataset.theme=dark?"dark":"light";
var stored=get("armadra.locale");
var locale=stored==="en"||stored==="zh-CN"?stored:root.lang;
function apply(){
  root.lang=locale;
  var t=M[locale];
  document.querySelectorAll("[data-i18n]").forEach(function(el){el.textContent=t[el.getAttribute("data-i18n")]});
  document.title=t[document.body.getAttribute("data-title")];
}
document.getElementById("relay-locale").addEventListener("click",function(){
  locale=locale==="en"?"zh-CN":"en";put("armadra.locale",locale);apply();
});
var open=document.getElementById("relay-open");
if(open){
  var link=document.body.getAttribute("data-link");
  var q="link="+encodeURIComponent(link)+"&issuer="+encodeURIComponent(location.origin);
  if(location.hash.length>1)q+="&s="+encodeURIComponent(location.hash.slice(1));
  open.href="armadra://join?"+q;
}
apply();
})();
`;

function renderPage(
  locale: PageLocale,
  kind: "unavailable" | "join",
  linkId?: string,
): { html: string; csp: string } {
  const nonce = randomBytes(16).toString("base64");
  const t = PAGE_MESSAGES[locale];
  const titleKey =
    kind === "join" ? "relay.join.title" : "relay.unavailable.title";
  const body =
    kind === "join"
      ? `<h1 data-i18n="relay.join.title">${escapeHtml(t["relay.join.title"] ?? "")}</h1>
<a class="primary" id="relay-open" href="armadra://join" data-i18n="relay.join.open">${escapeHtml(t["relay.join.open"] ?? "")}</a>`
      : `<h1 data-i18n="relay.unavailable.title">${escapeHtml(t["relay.unavailable.title"] ?? "")}</h1>`;
  const html = `<!doctype html>
<html lang="${locale}" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>${escapeHtml(t[titleKey] ?? "")}</title>
<style nonce="${nonce}">${PAGE_STYLE}</style>
</head>
<body data-title="${titleKey}"${linkId ? ` data-link="${escapeHtml(linkId)}"` : ""}>
<button class="locale" id="relay-locale" type="button" data-i18n="relay.locale.switch">${escapeHtml(t["relay.locale.switch"] ?? "")}</button>
<main><section>
${body}
</section></main>
<script type="application/json" id="relay-messages">${JSON.stringify(PAGE_MESSAGES).replace(/</g, "\\u003c")}</script>
<script nonce="${nonce}">${PAGE_SCRIPT}</script>
</body>
</html>
`;
  const csp = [
    "default-src 'none'",
    `style-src 'nonce-${nonce}'`,
    `script-src 'nonce-${nonce}'`,
    "img-src 'self'",
    "base-uri 'none'",
    "form-action 'none'",
    "frame-ancestors 'none'",
  ].join("; ");
  return { html, csp };
}

function sendPage(
  req: IncomingMessage,
  res: ServerResponse,
  status: number,
  kind: "unavailable" | "join",
  linkId?: string,
): void {
  const { html, csp } = renderPage(
    pickLocale(req.headers["accept-language"]),
    kind,
    linkId,
  );
  res.writeHead(status, {
    ...BASE_HEADERS,
    "content-type": "text/html; charset=utf-8",
    "content-length": Buffer.byteLength(html),
    "cache-control": "no-store",
    "content-security-policy": csp,
  });
  res.end(req.method === "HEAD" ? undefined : html);
}

// ------------------------------------------------------------ 静态产物

export interface WebPagesOptions {
  /** Armadra `apps/web` 的构建产物目录；没有就是 null。 */
  webRoot: string | null;
  /** `/ca.crt` 发的 PEM（自签模式的 CA）；没有答 undefined。 */
  caPem: () => string | undefined;
}

export interface WebPages {
  /** `extra` 钩子：认领了返回 true。 */
  handle(req: IncomingMessage, res: ServerResponse): boolean;
  readonly available: boolean;
}

const LINK_PATH = /^\/j\/([0-9a-f]{16})\/?$/;

/** 构建产物目录在且有 `index.html` 才算有。 */
export function usableWebRoot(dir: string | undefined | null): string | null {
  if (!dir) return null;
  const root = resolve(dir);
  return existsSync(join(root, "index.html")) ? root : null;
}

export function createWebPages(options: WebPagesOptions): WebPages {
  const root = options.webRoot;
  let rootReal: Promise<string> | undefined;

  /** 请求路径 → 根内的文件；不在根内、不是文件都答 null。 */
  async function locate(relative: string): Promise<string | null> {
    if (!root) return null;
    let decoded: string;
    try {
      decoded = decodeURIComponent(relative);
    } catch {
      return null;
    }
    if (decoded.includes("\u0000")) return null;
    const candidate = resolve(root, `.${sep}${decoded}`);
    if (candidate !== root && !candidate.startsWith(root + sep)) return null;
    try {
      if (!statSync(candidate).isFile()) return null;
      rootReal ??= realpath(root);
      const [real, base] = await Promise.all([realpath(candidate), rootReal]);
      if (!real.startsWith(base + sep)) return null;
      return real;
    } catch {
      return null;
    }
  }

  function serveFile(
    req: IncomingMessage,
    res: ServerResponse,
    file: string,
  ): void {
    const isHtml = extname(file).toLowerCase() === ".html";
    const name = file.split(sep).pop() ?? "";
    const size = statSync(file).size;
    res.writeHead(200, {
      ...BASE_HEADERS,
      "content-type": contentTypeFor(file),
      "content-length": size,
      "cache-control": isHtml
        ? "no-store"
        : hashedAsset(name)
          ? "public, max-age=31536000, immutable"
          : "no-cache",
      ...(isHtml ? { "content-security-policy": PAGE_CSP } : {}),
    });
    if (req.method === "HEAD") {
      res.end();
      return;
    }
    const stream = createReadStream(file);
    stream.on("error", () => res.destroy());
    stream.pipe(res);
  }

  function index(req: IncomingMessage, res: ServerResponse): void {
    serveFile(req, res, join(root as string, "index.html"));
  }

  async function route(
    req: IncomingMessage,
    res: ServerResponse,
    path: string,
  ): Promise<boolean> {
    if (path === "/ca.crt") {
      const pem = options.caPem();
      if (!pem) return false;
      res.writeHead(200, {
        ...BASE_HEADERS,
        "content-type": "application/x-pem-file",
        "content-disposition": 'attachment; filename="armadra-relay-ca.crt"',
        "content-length": Buffer.byteLength(pem),
        "cache-control": "no-store",
      });
      res.end(req.method === "HEAD" ? undefined : pem);
      return true;
    }
    if (path === "/" || path === "/app") {
      res.writeHead(308, { ...BASE_HEADERS, location: "/app/" });
      res.end();
      return true;
    }
    const link = LINK_PATH.exec(path);
    if (link?.[1]) {
      if (root) index(req, res);
      else sendPage(req, res, 200, "join", link[1]);
      return true;
    }
    if (path.startsWith("/app/")) {
      if (!root) {
        sendPage(req, res, 503, "unavailable");
        return true;
      }
      const file = await locate(path.slice("/app/".length));
      if (file) serveFile(req, res, file);
      else index(req, res);
      return true;
    }
    if (!root || path.endsWith("/")) return false;
    const file = await locate(path.slice(1));
    if (!file) return false;
    serveFile(req, res, file);
    return true;
  }

  return {
    available: root !== null,
    handle(req, res) {
      if (req.method !== "GET" && req.method !== "HEAD") return false;
      const path = (req.url ?? "/").split("?")[0] ?? "/";
      // 根下的文件是否存在要异步查，所以先同步判「可能是我的」再认领。
      const mine =
        path === "/ca.crt"
          ? options.caPem() !== undefined
          : path === "/" ||
            path === "/app" ||
            path.startsWith("/app/") ||
            LINK_PATH.test(path) ||
            (root !== null &&
              !path.endsWith("/") &&
              existsSync(join(root, path)));
      if (!mine) return false;
      route(req, res, path).then(
        (handled) => {
          if (!handled && !res.headersSent) {
            res.writeHead(404, {
              ...BASE_HEADERS,
              "content-type": "application/json; charset=utf-8",
              "cache-control": "no-store",
            });
            res.end(
              JSON.stringify({ code: "not_found", message: "Not found" }),
            );
          }
        },
        () => {
          if (!res.headersSent) res.writeHead(500);
          res.end();
        },
      );
      return true;
    },
  };
}
