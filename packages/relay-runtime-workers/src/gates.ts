/**
 * Worker 入口的闸（中继设计 §4.2，与套餐无关）：Rate Limiting 绑定按 IP、按路径分命名空间，
 * 每条 10 或 60 秒窗、按 Cloudflare 节点各自计数（全局兜底靠区域级限速规则与账号锁定）。
 * 对象里的内存桶（`tunnel.connect.src`、按令牌 / 账号、登录锁定）照常再判一次。
 *
 * 没配的绑定不生效（本地 `wrangler dev` 与测试可以只配一部分）。
 */
import { isPreflight } from "@armadra/relay-kernel/edge/cors";
import { isIssuePath } from "@armadra/relay-kernel/edge/media-tickets";
import {
  RELAY_SUBPROTOCOL_PREFIX,
  RELAY_TOKEN_HEADER,
} from "@armadra/platform-protocol";

import type { RelayEnv } from "./env.ts";

export type GateName =
  | "LIMIT_LOGIN"
  | "LIMIT_LINK_ACCEPT"
  | "LIMIT_REGISTER"
  | "LIMIT_TUNNEL"
  | "LIMIT_MEDIA"
  | "LIMIT_ANONYMOUS"
  | "LIMIT_PREFLIGHT";

/** 各闸的窗口（秒），答 `Retry-After` 用；与 `wrangler.jsonc` 的 `simple.period` 一致。 */
export const GATE_PERIOD_S: Record<GateName, number> = {
  LIMIT_LOGIN: 60,
  LIMIT_LINK_ACCEPT: 60,
  LIMIT_REGISTER: 60,
  LIMIT_TUNNEL: 60,
  LIMIT_MEDIA: 60,
  LIMIT_ANONYMOUS: 60,
  LIMIT_PREFLIGHT: 60,
};

/** `/v1` 的闸：登录、接受链接、注册令牌与登记。 */
export function apiGate(method: string, path: string): GateName | null {
  if (method !== "POST") return null;
  if (path === "/v1/auth/login") return "LIMIT_LOGIN";
  if (/^\/v1\/links\/[^/]+\/accept$/.test(path)) return "LIMIT_LINK_ACCEPT";
  if (
    path === "/v1/sources/registration-tokens" ||
    path === "/v1/sources/register"
  )
    return "LIMIT_REGISTER";
  return null;
}

/** 源地址的闸：预检、签媒体票、没带令牌的请求（`sourcePath` 是转给源的路径）。 */
export function edgeGate(
  request: Request,
  sourcePath: string,
): GateName | null {
  const headers = {
    origin: request.headers.get("origin") ?? undefined,
    "access-control-request-method":
      request.headers.get("access-control-request-method") ?? undefined,
  };
  if (isPreflight({ method: request.method, headers }))
    return "LIMIT_PREFLIGHT";
  if (isIssuePath(sourcePath)) return "LIMIT_MEDIA";
  const token =
    request.headers.has(RELAY_TOKEN_HEADER) ||
    (request.headers.get("sec-websocket-protocol") ?? "").includes(
      RELAY_SUBPROTOCOL_PREFIX,
    );
  // 兑媒体票不带令牌、票就是凭据：也按「没带令牌」计。
  return token ? null : "LIMIT_ANONYMOUS";
}

/** 过闸：没配这个绑定当作放行；绑定出错也放行（对象里还有内存桶）。 */
export async function passGate(
  env: RelayEnv,
  gate: GateName | null,
  key: string,
): Promise<boolean> {
  if (gate === null) return true;
  const binding = env[gate];
  if (!binding) return true;
  try {
    return (await binding.limit({ key })).success;
  } catch {
    return true;
  }
}
