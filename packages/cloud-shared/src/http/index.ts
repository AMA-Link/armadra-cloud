/**
 * 极小 HTTP 工具：JSON 输出、`{ code, message, requestId? }` 错误、按「方法 + 精确路径」
 * 分派的路由与监听。`/v1/*` 的契约路由由 oRPC 门面负责，这里只服务 `/health`
 * 一类的固定端点与兜底 404。
 */
import { randomUUID } from "node:crypto";
import {
  createServer,
  type IncomingMessage,
  type Server,
  type ServerResponse,
} from "node:http";
import {
  createServer as createHttpsServer,
  type Server as HttpsServer,
} from "node:https";
import type { AddressInfo } from "node:net";

import type { ErrorCode } from "@armadra/platform-protocol/errors";
import { protocolHeaderValue } from "@armadra/platform-protocol";

import type { ListenAddress } from "../config.ts";

/** `Armadra-Protocol` 头的值（协议包的 PROTOCOL_VERSION）。 */
export const PROTOCOL_HEADER_VALUE = protocolHeaderValue();

export function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": Buffer.byteLength(payload),
    "cache-control": "no-store",
    ...headers,
  });
  res.end(payload);
}

/** 线上错误形状；`code` 只能取协议包注册表里的码。 */
export interface WireError {
  code: ErrorCode;
  message: string;
  requestId?: string;
  details?: unknown;
}

export function sendError(
  res: ServerResponse,
  status: number,
  error: WireError,
): void {
  sendJson(res, status, error);
}

export type Handler = (
  req: IncomingMessage,
  res: ServerResponse,
  context: { requestId: string; url: URL },
) => void | Promise<void>;

export interface Router {
  get(path: string, handler: Handler): Router;
  handle(req: IncomingMessage, res: ServerResponse): Promise<void>;
}

export interface RouterOptions {
  onError?: (error: unknown) => void;
  /** 精确路由都没命中时调用；返回 false 表示也不认识，落到 404。 */
  fallback?: (
    req: IncomingMessage,
    res: ServerResponse,
    context: { requestId: string; url: URL; method: string },
  ) => boolean;
}

/** 方法 + 精确路径；HEAD 走 GET 的处理器。都没命中且 `fallback` 不认 → 404 `not_found`。 */
export function createRouter(options: RouterOptions = {}): Router {
  const routes = new Map<string, Handler>();
  const router: Router = {
    get(path, handler) {
      routes.set(`GET ${path}`, handler);
      return router;
    },
    async handle(req, res) {
      const requestId = randomUUID();
      const url = new URL(req.url ?? "/", "http://localhost");
      const method = req.method === "HEAD" ? "GET" : (req.method ?? "GET");
      const handler = routes.get(`${method} ${url.pathname}`);
      if (!handler) {
        if (options.fallback?.(req, res, { requestId, url, method })) return;
        sendError(res, 404, {
          code: "not_found",
          message: "Not found",
          requestId,
        });
        return;
      }
      try {
        await handler(req, res, { requestId, url });
      } catch (error) {
        options.onError?.(error);
        if (!res.headersSent) {
          sendError(res, 500, {
            code: "internal",
            message: "Internal error",
            requestId,
          });
        } else {
          res.destroy();
        }
      }
    },
  };
  return router;
}

export interface RunningServer {
  server: Server;
  address: ListenAddress;
  url: string;
  /** 只有 HTTPS 监听有：换证书（续期、文件热加载），已建立的连接不受影响。 */
  setTls?(tls: TlsPair): void;
  close(): Promise<void>;
}

/** PEM 证书链与私钥。 */
export interface TlsPair {
  cert: string;
  key: string;
}

export interface ListenOptions {
  /** 给了就是 HTTPS（`node:https`，HTTP/1.1）。 */
  tls?: TlsPair;
}

export async function listen(
  address: ListenAddress,
  handler: (req: IncomingMessage, res: ServerResponse) => void | Promise<void>,
  options: ListenOptions = {},
): Promise<RunningServer> {
  const onRequest = (req: IncomingMessage, res: ServerResponse) => {
    void handler(req, res);
  };
  const tls = options.tls;
  const server: Server = tls
    ? createHttpsServer(
        { cert: tls.cert, key: tls.key, minVersion: "TLSv1.2" },
        onRequest,
      )
    : createServer(onRequest);
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(address.port, address.host, () => {
      server.off("error", reject);
      resolve();
    });
  });
  const bound = server.address() as AddressInfo;
  const host = bound.family === "IPv6" ? `[${bound.address}]` : bound.address;
  return {
    server,
    address: { host: bound.address, port: bound.port },
    url: `${tls ? "https" : "http"}://${host}:${bound.port}`,
    ...(tls
      ? {
          setTls(pair: TlsPair) {
            (server as HttpsServer).setSecureContext({
              cert: pair.cert,
              key: pair.key,
            });
          },
        }
      : {}),
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
        server.closeAllConnections();
      }),
  };
}
