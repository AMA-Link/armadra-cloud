/**
 * personal 的 `/v1/*` 与 `/.well-known/*`（平台设计 §17.3 的子集表，契约 §10）。
 *
 * 路由表直接取协议包的 `cloudContract`：方法、路径与输入 schema 都来自契约，形状与 saas
 * 完全相同；没实现的 procedure 与契约外的路径答 `404 not_found`。输出形状由测试逐条对契约的
 * output schema 校验。
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

import {
  PLATFORM_WELL_KNOWN,
  PROTOCOL_HEADER,
  PROTOCOL_VERSION,
  protocolHeaderValue,
} from "@armadra/platform-protocol";
import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";
import { errorStatus, type ErrorCode } from "@armadra/platform-protocol/errors";
import type { z } from "zod";

import { errorMessage } from "../http/errors.ts";
import { createAccounts } from "./accounts.ts";
import { PersonalError, type PersonalContext } from "./context.ts";
import { createLinks } from "./links.ts";
import {
  createAuthenticator,
  refresh,
  revokeDevice,
  type Authenticator,
  type Caller,
} from "./sessions.ts";
import { createSources } from "./sources.ts";
import type { MeStream } from "./stream.ts";

const MAX_BODY_BYTES = 64 * 1024;

/** personal 报的能力（契约 `PLATFORM_CAPABILITIES` 的子集）。 */
export const PERSONAL_CAPABILITIES = [
  "auth.password",
  "links.source-invite",
  "links.update",
  "me.stream",
] as const;

/** personal 实现的 procedure（平台设计 §17.3 的子集表）；其余答 404。 */
export const PERSONAL_PROCEDURES = [
  "platform.info",
  "platform.jwks",
  "auth.login",
  "auth.refresh",
  "auth.logout",
  "me.get",
  "me.sources",
  "me.devices",
  "me.deviceRevoke",
  "me.streamTicket",
  "sources.registrationToken",
  "sources.register",
  "sources.heartbeatHint",
  "sources.list",
  "sources.get",
  "sources.revoke",
  "sources.assertion",
  "links.create",
  "links.get",
  "links.accept",
  "links.list",
  "links.update",
  "links.revoke",
] as const;
type PersonalProcedure = (typeof PERSONAL_PROCEDURES)[number];

interface Call {
  input: Record<string, unknown>;
  ip: string;
  req: IncomingMessage;
  caller(): Promise<Caller>;
}

type Handler = (call: Call) => unknown;

interface CompiledRoute {
  key: string;
  method: string;
  regex: RegExp;
  params: string[];
  statics: number;
  input: z.ZodType;
  handler: Handler;
}

function compile(path: string): { regex: RegExp; params: string[] } {
  const params: string[] = [];
  const pattern = path
    .split("/")
    .map((segment) => {
      const match = /^\{(\w+)\}$/.exec(segment);
      if (match?.[1]) {
        params.push(match[1]);
        return "([^/]+)";
      }
      return segment.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    })
    .join("/");
  return { regex: new RegExp(`^${pattern}$`), params };
}

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
    if (size > MAX_BODY_BYTES) throw new PersonalError("bad_request");
    chunks.push(chunk as Buffer);
  }
  if (size === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw new PersonalError("bad_request");
  }
}

function remoteIp(req: IncomingMessage): string {
  return req.socket.remoteAddress ?? "";
}

export interface PersonalApi {
  /** `extra` 钩子：认领 `/v1/*`、`/.well-known/*`。 */
  handle(req: IncomingMessage, res: ServerResponse): boolean;
  authenticator: Authenticator;
}

export function createPersonalApi(
  context: PersonalContext,
  stream: MeStream,
  options: {
    webApp: () => string | null;
    /** 处理器意外出错时记一笔（只给错误类名）。 */
    onError?: (error: unknown) => void;
  },
): PersonalApi {
  const authenticator = createAuthenticator(context);
  const accounts = createAccounts(context);
  const sources = createSources(context);
  const links = createLinks(context, sources);

  const handlers: Record<PersonalProcedure, Handler> = {
    "platform.info": () => {
      const issuer = context.signer.issuer;
      return {
        mode: "personal",
        issuer,
        protocol: { ...PROTOCOL_VERSION },
        capabilities: [...PERSONAL_CAPABILITIES],
        relay: { addressing: "path" },
        webApp: options.webApp(),
      };
    },
    "platform.jwks": () => context.signer.jwks(),
    "auth.login": ({ input, ip }) =>
      accounts
        .login(input as unknown as Parameters<typeof accounts.login>[0], ip)
        .then((session) => ({ session })),
    "auth.refresh": ({ input }) => ({
      session: refresh(context, input.refreshToken as string),
    }),
    "auth.logout": async ({ caller }) => {
      revokeDevice(context, (await caller()).deviceId);
      return {};
    },
    "me.get": async ({ caller }) => {
      const who = await caller();
      const session = context.store.current;
      return {
        accountId: who.subject,
        email: null,
        displayName:
          who.role === "owner"
            ? session.account.name
            : (session.links.find((row) => row.linkId === who.linkId)?.label ??
              "guest"),
        emailVerified: false,
        orgs: [],
        mfaEnabled: false,
      };
    },
    "me.sources": async ({ caller }) => ({
      sources: await sources.list(await caller()),
    }),
    "me.devices": async ({ caller }) => {
      const who = await caller();
      const nowMs = context.now();
      return {
        devices: context.store.current.devices
          .filter(
            (row) =>
              row.subject === who.subject &&
              row.revokedAtMs === null &&
              row.expiresAtMs > nowMs,
          )
          .map((row) => ({
            deviceId: row.deviceId,
            platform: row.platform,
            name: row.name,
            lastSeenAtMs: row.lastSeenAtMs,
            current: row.deviceId === who.deviceId,
          })),
      };
    },
    "me.deviceRevoke": async ({ input, caller }) => {
      const who = await caller();
      const device = context.store.current.devices.find(
        (row) => row.deviceId === input.deviceId && row.subject === who.subject,
      );
      if (!device || !revokeDevice(context, device.deviceId))
        throw new PersonalError("not_found");
      return {};
    },
    "me.streamTicket": async ({ caller }) => stream.ticket(await caller()),
    "sources.registrationToken": async ({ caller }) =>
      sources.registrationToken(await caller()),
    "sources.register": ({ input, ip }) =>
      sources.register(
        input as unknown as Parameters<typeof sources.register>[0],
        ip,
      ),
    "sources.heartbeatHint": ({ req }) =>
      sources.relayFor(sources.authenticateSource(req.headers.authorization)),
    "sources.list": async ({ caller }) => ({
      sources: await sources.list(await caller()),
    }),
    "sources.get": async ({ input, caller }) =>
      sources.get(await caller(), input.sourceId as string),
    "sources.revoke": async ({ input, caller }) => {
      sources.revoke(await caller(), input.sourceId as string);
      return {};
    },
    "sources.assertion": async ({ input, caller }) =>
      sources.assertion(
        await caller(),
        input.sourceId as string,
        input.device as Parameters<typeof sources.assertion>[2],
      ),
    "links.create": async ({ input, caller }) =>
      links.create(
        await caller(),
        input as unknown as Parameters<typeof links.create>[1],
      ),
    "links.get": ({ input }) => links.get(input.linkId as string),
    "links.accept": ({ input, ip }) =>
      links.accept(input as unknown as Parameters<typeof links.accept>[0], ip),
    "links.list": async ({ input, caller }) => ({
      links: links.list(await caller(), input.sourceId as string | undefined),
    }),
    "links.update": async ({ input, caller }) =>
      links.update(await caller(), {
        linkId: input.linkId as string,
        label: input.label as string,
      }),
    "links.revoke": async ({ input, caller }) => {
      links.revoke(await caller(), input.linkId as string);
      return {};
    },
  };

  const routes: CompiledRoute[] = listProcedures(cloudContract)
    .filter((entry) =>
      (PERSONAL_PROCEDURES as readonly string[]).includes(entry.key),
    )
    .map((entry) => {
      const { regex, params } = compile(entry.path);
      return {
        key: entry.key,
        method: entry.method,
        regex,
        params,
        statics: entry.path.split("/").length - params.length,
        input: entry.procedure["~orpc"].inputSchema as z.ZodType,
        handler: handlers[entry.key as PersonalProcedure],
      };
    })
    // 静态段多的优先：`/v1/sources/me/relay` 先于 `/v1/sources/{sourceId}`。
    .sort((left, right) => right.statics - left.statics);

  const match = (method: string, pathname: string) => {
    for (const route of routes) {
      if (route.method !== method) continue;
      const found = route.regex.exec(pathname);
      if (!found) continue;
      const params: Record<string, string> = {};
      route.params.forEach((name, index) => {
        params[name] = decodeURIComponent(found[index + 1] ?? "");
      });
      return { route, params };
    }
    return null;
  };

  async function run(
    req: IncomingMessage,
    res: ServerResponse,
    url: URL,
    requestId: string,
  ): Promise<void> {
    const method = req.method === "HEAD" ? "GET" : (req.method ?? "GET");
    const hit = match(method, url.pathname);
    if (!hit) {
      req.resume();
      sendPersonalError(res, "not_found", requestId);
      return;
    }
    const raw =
      method === "GET" || method === "DELETE"
        ? Object.fromEntries(url.searchParams)
        : await readBody(req);
    if (raw === null || typeof raw !== "object" || Array.isArray(raw))
      throw new PersonalError("bad_request");
    const parsed = hit.route.input.safeParse({ ...raw, ...hit.params });
    if (!parsed.success) throw new PersonalError("bad_request");
    let cached: Promise<Caller> | undefined;
    const caller = () => {
      cached ??= authenticator.authenticate(req.headers.authorization);
      return cached;
    };
    const output = await hit.route.handler({
      input: parsed.data as Record<string, unknown>,
      ip: remoteIp(req),
      req,
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
        if (error instanceof PersonalError) {
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
