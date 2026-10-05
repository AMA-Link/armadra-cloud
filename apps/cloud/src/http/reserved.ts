/**
 * 预留的 `/v1/*` 与 `/.well-known/*` 路由（平台落地总计划 §12.2：SaaS 服务端先只
 * 预留接口）。路由表直接取协议包的 `cloudContract`，统一答
 * 501 `{ code: "not_implemented", message, requestId }`；实现落地时把 procedure 加进
 * {@link IMPLEMENTED}，经 `http/rpc.ts`（oRPC 门面）挂上去。
 */
import type { IncomingMessage, ServerResponse } from "node:http";

import { sendError } from "@armadra/cloud-shared/http";
import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";

export interface ReservedRoute {
  /** 契约里的 procedure 名，例如 `auth.login`。 */
  key: string;
  method: string;
  /** 路径模板，参数写成 `{name}`。 */
  path: string;
}

/** 已经实现、不走预留的 procedure。 */
export const IMPLEMENTED = new Set(["platform.health"]);

/** 契约里除 {@link IMPLEMENTED} 之外的全部 procedure，按声明顺序。 */
export const RESERVED_ROUTES: readonly ReservedRoute[] = listProcedures(
  cloudContract,
)
  .filter((entry) => !IMPLEMENTED.has(entry.key))
  .map(({ key, method, path }) => ({ key, method, path }));

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
