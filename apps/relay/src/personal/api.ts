/**
 * personal 的 `/v1/*` 与 `/.well-known/*`（平台设计 §17.3 的子集表，契约 §10）的 HTTP 外壳。
 *
 * 路由与处理器在控制核：`createCloudRouter` + `createPersonalProcedures`，形状与 saas 完全相同；
 * 没实现的 procedure 与契约外的路径答 `404 not_found`。输出形状由测试逐条对契约的 output
 * schema 校验。
 *
 * - 输入：路径参数 + （GET / DELETE 的 query | 其余的 JSON 体），按契约的 input schema 校验，
 *   不过答 `400 bad_request`；JSON 体上限 64 KiB。
 * - 认证：`session`（`Authorization: Bearer <访问令牌>`）、`source`（`Authorization: Source <jws>`）、
 *   `anonymous`。
 * - 错误：`{ code, message, requestId, details? }`，状态码查注册表；`account_locked` /
 *   `rate_limited` 带 `details.retryAfterMs` 与 `Retry-After` 头。
 * - 跨源：`/v1/*` 与 `/.well-known/*` 允许任意来源（只认 Bearer / Source 头，不用 Cookie），
 *   页面、手机与第二台桌面都能直接调。
 */
import { randomUUID } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";

import { ControlError } from "@armadra/control-core/errors";
import type { MeStreamCore } from "@armadra/control-core/me-stream";
import { createPersonalProcedures } from "@armadra/control-core/procedures";
import { createCloudRouter } from "@armadra/control-core/router";
import type { Authenticator, Caller } from "@armadra/control-core/sessions";
import {
  PLATFORM_WELL_KNOWN,
  PROTOCOL_HEADER,
  protocolHeaderValue,
} from "@armadra/platform-protocol";
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";
import {
  createClientIp,
  type ClientIp,
} from "@armadra/relay-kernel/http/client-ip";
import { errorMessage } from "@armadra/relay-kernel/http/errors";

import type { PersonalContext } from "./context.ts";

const MAX_BODY_BYTES = 64 * 1024;

const CORS_HEADERS: Record<string, string> = {
  "access-control-allow-origin": "*",
  "access-control-allow-methods": "GET, POST, PATCH, DELETE, OPTIONS",
  "access-control-allow-headers": "authorization, content-type",
  "access-control-max-age": "7200",
};

function send(
  res: ServerResponse,
  status: number,
  body: unknown,
  extra: Record<string, string> = {},
): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": Buffer.byteLength(payload),
    "cache-control": "no-store",
    [PROTOCOL_HEADER]: protocolHeaderValue(),
    ...CORS_HEADERS,
    ...extra,
  });
  res.end(payload);
}

export function sendPersonalError(
  res: ServerResponse,
  code: ErrorCode,
  requestId: string,
  retryAfterMs?: number,
): void {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  const extra: Record<string, string> =
    retryAfterMs === undefined
      ? {}
      : { "retry-after": String(Math.max(1, Math.ceil(retryAfterMs / 1000))) };
  send(
    res,
    errorStatus(code),
    {
      code,
      message: errorMessage(code),
      requestId,
      ...(retryAfterMs === undefined ? {} : { details: { retryAfterMs } }),
    },
    extra,
  );
}

async function readBody(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    size += (chunk as Buffer).length;
    if (size > MAX_BODY_BYTES) throw new ControlError("bad_request");
    chunks.push(chunk as Buffer);
  }
  if (size === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw new ControlError("bad_request");
  }
}

export interface PersonalApi {
  /** `extra` 钩子：认领 `/v1/*`、`/.well-known/*`。 */
  handle(req: IncomingMessage, res: ServerResponse): boolean;
  authenticator: Authenticator;
}

export function createPersonalApi(
  context: PersonalContext,
  stream: MeStreamCore,
  options: {
    webApp: () => string | null;
    /** 客户端 IP 的取法；缺省直连对端地址。 */
    clientIp?: ClientIp;
    /** 处理器意外出错时记一笔（只给错误类名）。 */
    onError?: (error: unknown) => void;
  },
): PersonalApi {
  const clientIp = options.clientIp ?? createClientIp();
  const { handlers, authenticator } = createPersonalProcedures(context, {
    stream,
    webApp: options.webApp,
  });
  const router = createCloudRouter(handlers);

  async function run(
    req: IncomingMessage,
    res: ServerResponse,
    url: URL,
    requestId: string,
  ): Promise<void> {
    const method = req.method === "HEAD" ? "GET" : (req.method ?? "GET");
    const hit = router.match(method, url.pathname);
    if (!hit?.handler) {
      req.resume();
      sendPersonalError(res, "not_found", requestId);
      return;
    }
    const raw =
      method === "GET" || method === "DELETE"
        ? Object.fromEntries(url.searchParams)
        : await readBody(req);
    if (raw === null || typeof raw !== "object" || Array.isArray(raw))
      throw new ControlError("bad_request");
    const parsed = hit.route.input.safeParse({ ...raw, ...hit.params });
    if (!parsed.success) throw new ControlError("bad_request");
    let cached: Promise<Caller> | undefined;
    const caller = () => {
      cached ??= authenticator.authenticate(req.headers.authorization);
      return cached;
    };
    const output = await hit.handler({
      input: parsed.data as Record<string, unknown>,
      ip: clientIp(req),
      authorization: req.headers.authorization,
      caller,
    });
    send(res, 200, output);
  }

  return {
    authenticator,
    handle(req, res) {
      const url = new URL(req.url ?? "/", "http://relay");
      const path = url.pathname;
      if (!path.startsWith("/v1/") && !path.startsWith("/.well-known/"))
        return false;
      if (path !== PLATFORM_WELL_KNOWN && path.startsWith("/.well-known/")) {
        if (path !== "/.well-known/jwks.json") return false;
      }
      if (req.method === "OPTIONS") {
        res.writeHead(204, CORS_HEADERS);
        res.end();
        return true;
      }
      const requestId = randomUUID();
      run(req, res, url, requestId).catch((error: unknown) => {
        if (error instanceof ControlError) {
          sendPersonalError(res, error.code, requestId, error.retryAfterMs);
          return;
        }
        options.onError?.(error);
        sendPersonalError(res, "internal", requestId);
      });
      return true;
    },
  };
}
