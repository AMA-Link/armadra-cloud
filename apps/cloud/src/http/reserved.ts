/**
 * 预留的 `/v1/*` 与 `/.well-known/*` 路由（平台落地总计划 §12.2：SaaS 服务端先只
 * 预留接口）。路由表来自控制核的 `createCloudRouter`（与个人中转同一份），这里没有给任何
 * 处理器，契约里的 procedure 全部是预留的，统一答
 * 501 `{ code: "not_implemented", message, requestId }`；实现落地时把处理器交给路由工厂，
 * 并把 procedure 从预留里拿掉。
 */
import type { IncomingMessage, ServerResponse } from "node:http";

import { sendError } from "@armadra/cloud-shared/http";
import { createCloudRouter } from "@armadra/control-core/router";

export interface ReservedRoute {
  /** 契约里的 procedure 名，例如 `auth.login`。 */
  key: string;
  method: string;
  /** 路径模板，参数写成 `{name}`。 */
  path: string;
}

/** 已经实现、不走预留的 procedure（由外层路由直接答）。 */
export const IMPLEMENTED = new Set(["platform.health"]);

const router = createCloudRouter({}, { exclude: [...IMPLEMENTED] });

/** 契约里除 {@link IMPLEMENTED} 之外的全部 procedure，按声明顺序。 */
export const RESERVED_ROUTES: readonly ReservedRoute[] = router.reserved.map(
  ({ key, method, path }) => ({ key, method, path }),
);

/** `/v1/me/stream` 的 WS 升级同样预留（C3-3）。 */
export const RESERVED_UPGRADE_PATH = "/v1/me/stream";

export interface ReservedMatcher {
  match(method: string, pathname: string): ReservedRoute | null;
}

export function createReservedMatcher(): ReservedMatcher {
  return {
    match(method, pathname) {
      // 静态段优先（`/v1/sources/me/relay` 先于 `/v1/sources/{sourceId}`），与契约路由器一致。
      const hit = router.match(method, pathname);
      if (!hit) return null;
      const { key, path } = hit.route;
      return { key, method: hit.route.method, path };
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
