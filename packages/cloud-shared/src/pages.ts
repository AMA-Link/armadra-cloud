/**
 * 中继自有的两页（托管页面产物缺失时的错误页与分享落地页）与静态产物的响应头规则，与运行时
 * 无关：Node 中继（`apps/relay` 的 `personal/web.ts`）与 Workers 中继（`relay-runtime-workers`）
 * 共用同一份文案、样式与 CSP。
 *
 * - 页面 CSP 与 core Gateway 的服务器版相同（`connect-src 'self'` 等）；带哈希的资源永久缓存，
 *   `index.html` 不缓存。
 * - 自有页面的样式取 Armadra 设计系统的 token，文案中英两份、跟随页面同一个语言偏好
 *   （`localStorage["armadra.locale"]`），可切换。
 */
import { randomBytes } from "node:crypto";

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

/** Turnstile 组件的来源（script-src、frame-src、connect-src 的最小集合，见 Cloudflare Turnstile 文档 “Content Security Policy”）。 */
export const TURNSTILE_ORIGIN = "https://challenges.cloudflare.com";

/**
 * 客户端内嵌挑战页时的固定受信来源：移动壳（`https://localhost`、`capacitor://localhost`）、
 * 本地开发（`http://localhost[:端口]`）与桌面壳——桌面窗口的页面由本机回环上随机端口的
 * 静态服务给出，来源是 `http://127.0.0.1:<端口>`。只放行回环主机的端口，不通配域名。
 */
export const CHALLENGE_EMBED_ORIGINS = [
  "https://localhost",
  "capacitor://localhost",
  "http://localhost",
  "http://localhost:*",
  "http://127.0.0.1:*",
];

export interface PageChallenge {
  /** 配置里的额外受信来源（Node：状态文件 trustedOrigins；Workers：RELAY_TRUSTED_ORIGINS）。 */
  embedOrigins?: readonly string[];
}

/** `/app/challenge`（含子路径）：唯一允许被客户端内嵌的托管页路径。 */
export function isChallengePath(path: string): boolean {
  return path === "/app/challenge" || path.startsWith("/app/challenge/");
}

/**
 * 托管页（`index.html`）的 CSP。没配 Turnstile（`challenge` 缺省）原样是 `PAGE_CSP`；配了就为
 * Turnstile 放行 `challenges.cloudflare.com`，且只有 `/app/challenge` 的 `frame-ancestors`
 * 列出受信来源（固定的客户端来源、中继自身与额外来源，不用通配），其余路径仍是 `'none'`。
 */
export function pageCsp(path: string, challenge?: PageChallenge): string {
  if (!challenge) return PAGE_CSP;
  const ancestors = ["'self'", ...CHALLENGE_EMBED_ORIGINS];
  for (const origin of challenge.embedOrigins ?? [])
    if (/^[a-z][a-z0-9+.-]*:\/\/[^\s;,'"*]+$/i.test(origin))
      ancestors.push(origin);
  return PAGE_CSP.split("; ")
    .map((directive) => {
      if (directive.startsWith("connect-src "))
        return `${directive} ${TURNSTILE_ORIGIN}`;
      if (directive.startsWith("frame-src "))
        return `${directive} ${TURNSTILE_ORIGIN}`;
      if (directive.startsWith("frame-ancestors "))
        return isChallengePath(path)
          ? `frame-ancestors ${[...new Set(ancestors)].join(" ")}`
          : directive;
      return directive;
    })
    .concat(`script-src 'self' ${TURNSTILE_ORIGIN}`)
    .join("; ");
}

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

export const PAGE_BASE_HEADERS = {
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
};

/** 路径最后一段的扩展名（含点，小写）；没有答空串。 */
function extensionOf(path: string): string {
  const name = path.split(/[\\/]/).pop() ?? "";
  const dot = name.lastIndexOf(".");
  return dot <= 0 ? "" : name.slice(dot).toLowerCase();
}

export function contentTypeFor(path: string): string {
  return CONTENT_TYPES.get(extensionOf(path)) ?? "application/octet-stream";
}

/** Vite 的带哈希文件名（`index-D3fK9x2a.js`）可以永久缓存。 */
export function hashedAsset(name: string): boolean {
  return /-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$/.test(name);
}

// ------------------------------------------------------------ 自有页面

export type PageLocale = "zh-CN" | "en";
export type PageKind = "unavailable" | "join";

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

export function renderPage(
  locale: PageLocale,
  kind: PageKind,
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
