/**
 * 预留的 `/v1/*` 与 `/.well-known/*` 路由（平台落地总计划 §12.2：SaaS 服务端先只
 * 预留接口）。路由表与协议包设计 §5 一一对应，统一答
 * 501 `{ code: "not_implemented", message, requestId }`；实现落地时逐条从表里拿掉。
 */
import type { IncomingMessage, ServerResponse } from "node:http";

import { sendError } from "@armadra/cloud-shared/http";

export interface ReservedRoute {
  /** 契约里的 procedure 名，例如 `auth.login`。 */
  key: string;
  method: "GET" | "POST" | "PATCH" | "PUT" | "DELETE";
  /** 路径模板，参数写成 `{name}`。 */
  path: string;
}

export const RESERVED_ROUTES: readonly ReservedRoute[] = [
  {
    key: "platform.info",
    method: "GET",
    path: "/.well-known/armadra-platform",
  },
  { key: "platform.jwks", method: "GET", path: "/.well-known/jwks.json" },
  { key: "auth.register", method: "POST", path: "/v1/auth/register" },
  { key: "auth.login", method: "POST", path: "/v1/auth/login" },
  { key: "auth.mfaVerify", method: "POST", path: "/v1/auth/mfa/verify" },
  { key: "auth.refresh", method: "POST", path: "/v1/auth/refresh" },
  { key: "auth.logout", method: "POST", path: "/v1/auth/logout" },
  { key: "auth.deviceStart", method: "POST", path: "/v1/auth/device/start" },
  { key: "auth.devicePoll", method: "POST", path: "/v1/auth/device/poll" },
  {
    key: "auth.deviceApprove",
    method: "POST",
    path: "/v1/auth/device/approve",
  },
  {
    key: "auth.passkeyRegisterOptions",
    method: "POST",
    path: "/v1/auth/passkey/register/options",
  },
  {
    key: "auth.passkeyRegister",
    method: "POST",
    path: "/v1/auth/passkey/register",
  },
  {
    key: "auth.passkeyLoginOptions",
    method: "POST",
    path: "/v1/auth/passkey/login/options",
  },
  { key: "auth.passkeyLogin", method: "POST", path: "/v1/auth/passkey/login" },
  { key: "auth.totpEnroll", method: "POST", path: "/v1/auth/totp/enroll" },
  { key: "auth.totpConfirm", method: "POST", path: "/v1/auth/totp/confirm" },
  { key: "auth.totpDisable", method: "POST", path: "/v1/auth/totp/disable" },
  {
    key: "auth.recoveryCodesRotate",
    method: "POST",
    path: "/v1/auth/recovery-codes/rotate",
  },
  { key: "me.get", method: "GET", path: "/v1/me" },
  { key: "me.update", method: "PATCH", path: "/v1/me" },
  { key: "me.sources", method: "GET", path: "/v1/me/sources" },
  { key: "me.devices", method: "GET", path: "/v1/me/devices" },
  {
    key: "me.deviceRevoke",
    method: "DELETE",
    path: "/v1/me/devices/{deviceId}",
  },
  { key: "me.streamTicket", method: "POST", path: "/v1/me/stream/ticket" },
  {
    key: "sources.registrationToken",
    method: "POST",
    path: "/v1/sources/registration-tokens",
  },
  { key: "sources.register", method: "POST", path: "/v1/sources/register" },
  { key: "sources.heartbeatHint", method: "GET", path: "/v1/sources/me/relay" },
  { key: "sources.list", method: "GET", path: "/v1/sources" },
  { key: "sources.get", method: "GET", path: "/v1/sources/{sourceId}" },
  { key: "sources.update", method: "PATCH", path: "/v1/sources/{sourceId}" },
  { key: "sources.revoke", method: "DELETE", path: "/v1/sources/{sourceId}" },
  {
    key: "sources.assertion",
    method: "POST",
    path: "/v1/sources/{sourceId}/assertion",
  },
  {
    key: "sources.accessList",
    method: "GET",
    path: "/v1/sources/{sourceId}/access",
  },
  {
    key: "sources.accessRevoke",
    method: "DELETE",
    path: "/v1/sources/{sourceId}/access/{accountId}",
  },
  { key: "links.create", method: "POST", path: "/v1/links" },
  { key: "links.get", method: "GET", path: "/v1/links/{linkId}" },
  { key: "links.accept", method: "POST", path: "/v1/links/{linkId}/accept" },
  { key: "links.list", method: "GET", path: "/v1/links" },
  { key: "links.revoke", method: "DELETE", path: "/v1/links/{linkId}" },
  { key: "orgs.create", method: "POST", path: "/v1/orgs" },
  { key: "orgs.get", method: "GET", path: "/v1/orgs/{orgId}" },
  { key: "orgs.update", method: "PATCH", path: "/v1/orgs/{orgId}" },
  { key: "orgs.delete", method: "DELETE", path: "/v1/orgs/{orgId}" },
  { key: "orgs.members", method: "GET", path: "/v1/orgs/{orgId}/members" },
  {
    key: "orgs.memberRole",
    method: "PATCH",
    path: "/v1/orgs/{orgId}/members/{accountId}",
  },
  {
    key: "orgs.memberRemove",
    method: "DELETE",
    path: "/v1/orgs/{orgId}/members/{accountId}",
  },
  { key: "orgs.sources", method: "GET", path: "/v1/orgs/{orgId}/sources" },
  {
    key: "orgs.sourceAttach",
    method: "POST",
    path: "/v1/orgs/{orgId}/sources",
  },
  {
    key: "orgs.sourceDetach",
    method: "DELETE",
    path: "/v1/orgs/{orgId}/sources/{sourceId}",
  },
  { key: "billing.plan", method: "GET", path: "/v1/orgs/{orgId}/billing" },
  { key: "billing.usage", method: "GET", path: "/v1/orgs/{orgId}/usage" },
];

/** `/v1/me/stream` 的 WS 升级同样预留（C3-3）。 */
export const RESERVED_UPGRADE_PATH = "/v1/me/stream";

function compile(path: string): RegExp {
  const pattern = path
    .split("/")
    .map((segment) =>
      /^\{\w+\}$/.test(segment)
        ? "[^/]+"
        : segment.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
    )
    .join("/");
  return new RegExp(`^${pattern}$`);
}

export interface ReservedMatcher {
  match(method: string, pathname: string): ReservedRoute | null;
}

export function createReservedMatcher(
  routes: readonly ReservedRoute[] = RESERVED_ROUTES,
): ReservedMatcher {
  const compiled = routes.map((route) => ({
    route,
    regex: compile(route.path),
  }));
  return {
    match(method, pathname) {
      // 静态段优先（`/v1/sources/me/relay` 先于 `/v1/sources/{sourceId}`），与契约路由器一致。
      const hits = compiled.filter(
        (entry) => entry.route.method === method && entry.regex.test(pathname),
      );
      hits.sort(
        (left, right) =>
          left.route.path.split("{").length -
          right.route.path.split("{").length,
      );
      return hits[0]?.route ?? null;
    },
  };
}

/** 命中预留路由就答 501 并返回 true。 */
export function answerReserved(
  matcher: ReservedMatcher,
  req: IncomingMessage,
  res: ServerResponse,
  context: { requestId: string; url: URL; method: string },
): boolean {
  const route = matcher.match(context.method, context.url.pathname);
  if (!route) return false;
  // 请求体不读：501 之后连接照常关闭或复用由 Node 处理。
  req.resume();
  sendError(res, 501, {
    code: "not_implemented",
    message: `${route.key} 尚未实现`,
    requestId: context.requestId,
  });
  return true;
}
