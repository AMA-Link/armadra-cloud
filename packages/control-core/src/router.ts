/**
 * `createCloudRouter`：`/v1/*` 路由表的唯一实现，个人中转与 SaaS 控制面共用。
 *
 * 路由表直接取协议包的 `cloudContract`：方法、路径与输入 schema 都来自契约，形状两边完全相同。
 * 给了处理器的 procedure 是「启用」的；契约里其余的 procedure 是「预留」的，由装配方决定怎么答
 * （personal：`404 not_found`；saas：`501 not_implemented`）。
 *
 * 匹配先只看启用的路由，都不中再看预留的——预留路由不会遮住启用的路由。同一组里静态段多的
 * 优先：`/v1/sources/me/relay` 先于 `/v1/sources/{sourceId}`。路由器不碰 HTTP：读体、认证头与
 * 写响应都在装配方。
 */
import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";
import type { z } from "zod";

import type { Caller } from "./sessions.ts";

/** 一次 procedure 调用（与传输无关）。 */
export interface ProcedureCall {
  /** 路径参数 + query / JSON 体，已按契约的 input schema 校验。 */
  input: Record<string, unknown>;
  /** 客户端 IP（装配方按受信代理规则取）。 */
  ip: string;
  /** `Authorization` 头原文（`sources.heartbeatHint` 用 `Source <jws>`）。 */
  authorization: string | undefined;
  /** 按 `Authorization: Bearer` 认证调用方（惰性、只认证一次）。 */
  caller(): Promise<Caller>;
}

export type ProcedureHandler = (call: ProcedureCall) => unknown;

export interface CloudRoute {
  /** 契约里的 procedure 名，例如 `auth.login`。 */
  key: string;
  method: string;
  /** 路径模板，参数写成 `{name}`。 */
  path: string;
  input: z.ZodType;
}

export type RouteMatch =
  | {
      route: CloudRoute;
      params: Record<string, string>;
      handler: ProcedureHandler;
    }
  | { route: CloudRoute; params: Record<string, string>; handler: null };

export interface CloudRouter {
  /** 启用的路由（按契约声明顺序）。 */
  readonly enabled: readonly CloudRoute[];
  /** 预留的路由（按契约声明顺序）。 */
  readonly reserved: readonly CloudRoute[];
  /** 启用的命中答处理器；只命中预留的答 `handler: null`；都不中答 null。 */
  match(method: string, pathname: string): RouteMatch | null;
}

interface Compiled {
  route: CloudRoute;
  regex: RegExp;
  params: string[];
  statics: number;
}

function compile(route: CloudRoute): Compiled {
  const params: string[] = [];
  const pattern = route.path
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
  return {
    route,
    regex: new RegExp(`^${pattern}$`),
    params,
    statics: route.path.split("/").length - params.length,
  };
}

function find(
  table: readonly Compiled[],
  method: string,
  pathname: string,
): { route: CloudRoute; params: Record<string, string> } | null {
  for (const entry of table) {
    if (entry.route.method !== method) continue;
    const found = entry.regex.exec(pathname);
    if (!found) continue;
    const params: Record<string, string> = {};
    entry.params.forEach((name, index) => {
      params[name] = decodeURIComponent(found[index + 1] ?? "");
    });
    return { route: entry.route, params };
  }
  return null;
}

/** 静态段多的优先；同样多的保持契约声明顺序。 */
const byStatics = (left: Compiled, right: Compiled) =>
  right.statics - left.statics;

export function createCloudRouter(
  handlers: Readonly<Record<string, ProcedureHandler>>,
  options: {
    /** 不进路由表的 procedure（例如 saas 的 `platform.health` 由外层直接答）。 */
    exclude?: readonly string[];
  } = {},
): CloudRouter {
  const exclude = new Set(options.exclude ?? []);
  const all: CloudRoute[] = listProcedures(cloudContract)
    .filter((entry) => !exclude.has(entry.key))
    .map((entry) => ({
      key: entry.key,
      method: entry.method,
      path: entry.path,
      input: entry.procedure["~orpc"].inputSchema as z.ZodType,
    }));
  const known = new Set(all.map((route) => route.key));
  for (const key of Object.keys(handlers))
    if (!known.has(key)) throw new Error(`契约里没有这个 procedure：${key}`);
  const enabled = all.filter((route) => Object.hasOwn(handlers, route.key));
  const reserved = all.filter((route) => !Object.hasOwn(handlers, route.key));
  const enabledTable = enabled.map(compile).sort(byStatics);
  const reservedTable = reserved.map(compile).sort(byStatics);
  return {
    enabled,
    reserved,
    match(method, pathname) {
      const hit = find(enabledTable, method, pathname);
      if (hit) {
        const handler = handlers[hit.route.key];
        if (handler) return { ...hit, handler };
      }
      const held = find(reservedTable, method, pathname);
      return held ? { ...held, handler: null } : null;
    },
  };
}
