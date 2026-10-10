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
import { createReadStream, existsSync, statSync } from "node:fs";
import { realpath } from "node:fs/promises";
import type { IncomingMessage, ServerResponse } from "node:http";
import { extname, join, resolve, sep } from "node:path";

import {
  contentTypeFor,
  hashedAsset,
  PAGE_BASE_HEADERS as BASE_HEADERS,
  PAGE_CSP,
  pickLocale,
  renderPage,
} from "@armadra/cloud-shared/pages";

export {
  contentTypeFor,
  hashedAsset,
  PAGE_CSP,
  PAGE_MESSAGES,
  pickLocale,
  type PageLocale,
} from "@armadra/cloud-shared/pages";

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
