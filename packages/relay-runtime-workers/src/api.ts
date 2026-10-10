/**
 * `/v1/*` 与 `/.well-known/*` 的 HTTP 外壳（Request / Response 版，Node 中继 `personal/api.ts` 的
 * 同一套规则）：路由与处理器在控制核（`createCloudRouter` + `createPersonalProcedures`），没实现
 * 的 procedure 与契约外的路径答 `404 not_found`。
 *
 * - 输入：路径参数 + （GET / DELETE 的 query | 其余的 JSON 体），按契约的 input schema 校验，
 *   不过答 `400 bad_request`；JSON 体上限 64 KiB。
 * - 错误：`{ code, message, requestId, details? }`；`account_locked` / `rate_limited` 带
 *   `details.retryAfterMs` 与 `Retry-After` 头。
 * - 跨源：`/v1/*` 与 `/.well-known/*` 允许任意来源（只认 `Authorization` 头，不用 Cookie）。
 */
import { ControlError } from "@armadra/control-core/errors";
import type { MeStreamCore } from "@armadra/control-core/me-stream";
import type { ControlContext } from "@armadra/control-core/ports";
import { createPersonalProcedures } from "@armadra/control-core/procedures";
import { createCloudRouter } from "@armadra/control-core/router";
import type { Authenticator, Caller } from "@armadra/control-core/sessions";
import {
  JWKS_WELL_KNOWN,
  PLATFORM_WELL_KNOWN,
} from "@armadra/platform-protocol";

import { jsonResponse, relayError } from "./responses.ts";

const MAX_BODY_BYTES = 64 * 1024;

export const API_CORS_HEADERS: Record<string, string> = {
  "access-control-allow-origin": "*",
  "access-control-allow-methods": "GET, POST, PATCH, DELETE, OPTIONS",
  "access-control-allow-headers": "authorization, content-type",
  "access-control-max-age": "7200",
};

const utf8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

async function readBody(request: Request): Promise<unknown> {
  if (!request.body) return {};
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    size += value.length;
    if (size > MAX_BODY_BYTES) {
      await reader.cancel().catch(() => {});
      throw new ControlError("bad_request");
    }
    chunks.push(value);
  }
  if (size === 0) return {};
  try {
    return JSON.parse(utf8.decode(Buffer.concat(chunks)));
  } catch {
    throw new ControlError("bad_request");
  }
}

/** 这个路径归 `/v1` 外壳（`/.well-known/` 只认平台信息与 JWKS）。 */
export function isApiPath(path: string): boolean {
  return (
    path.startsWith("/v1/") ||
    path === PLATFORM_WELL_KNOWN ||
    path === JWKS_WELL_KNOWN
  );
}

export interface WorkersApi {
  handle(request: Request, clientIp: string): Promise<Response>;
  authenticator: Authenticator;
}

export function createWorkersApi(
  context: ControlContext,
  stream: MeStreamCore,
  options: {
    webApp: () => string | null;
    name: () => string | null;
    onError: (error: unknown) => void;
  },
): WorkersApi {
  const { handlers, authenticator } = createPersonalProcedures(context, {
    stream,
    webApp: options.webApp,
    name: options.name,
  });
  const router = createCloudRouter(handlers);

  const fail = (
    code: Parameters<typeof relayError>[0],
    requestId: string,
    retryAfterMs?: number,
  ) =>
    relayError(code, {
      requestId,
      retryAfterMs,
      headers: API_CORS_HEADERS,
    });

  async function run(
    request: Request,
    url: URL,
    clientIp: string,
    requestId: string,
  ): Promise<Response> {
    const method = request.method === "HEAD" ? "GET" : request.method;
    const hit = router.match(method, url.pathname);
    if (!hit?.handler) return fail("not_found", requestId);
    const raw =
      method === "GET" || method === "DELETE"
        ? Object.fromEntries(url.searchParams)
        : await readBody(request);
    if (raw === null || typeof raw !== "object" || Array.isArray(raw))
      throw new ControlError("bad_request");
    const parsed = hit.route.input.safeParse({ ...raw, ...hit.params });
    if (!parsed.success) throw new ControlError("bad_request");
    const authorization = request.headers.get("authorization") ?? undefined;
    let cached: Promise<Caller> | undefined;
    const caller = () => {
      cached ??= authenticator.authenticate(authorization);
      return cached;
    };
    const output = await hit.handler({
      input: parsed.data as Record<string, unknown>,
      ip: clientIp,
      authorization,
      caller,
    });
    return jsonResponse(200, output, API_CORS_HEADERS);
  }

  return {
    authenticator,
    async handle(request, clientIp) {
      const url = new URL(request.url);
      if (request.method === "OPTIONS")
        return new Response(null, { status: 204, headers: API_CORS_HEADERS });
      const requestId = crypto.randomUUID();
      try {
        return await run(request, url, clientIp, requestId);
      } catch (error) {
        if (error instanceof ControlError)
          return fail(error.code, requestId, error.retryAfterMs);
        options.onError(error);
        return fail("internal", requestId);
      }
    },
  };
}
