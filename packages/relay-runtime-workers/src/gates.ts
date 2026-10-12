/**
 * Worker 入口的闸（中继设计 §4.2，与套餐无关）：Rate Limiting 绑定按 IP、按路径分命名空间，
 * 每条 10 或 60 秒窗、按 Cloudflare 节点各自计数（全局兜底靠区域级限速规则与账号锁定）。
 * 对象里的内存桶（`tunnel.connect.src`、按令牌 / 账号、登录锁定）照常再判一次。
 *
 * 没配的绑定不生效（本地 `wrangler dev` 与测试可以只配一部分）。
 */
import { RELAY_SUBPROTOCOL_PREFIX } from "@armadra/platform-protocol";

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

/** `/v1` 的闸：登录与改口令（同一个闸）、接受链接、注册令牌与登记。 */
export function apiGate(method: string, path: string): GateName | null {
  if (method !== "POST") return null;
  if (path === "/v1/auth/login" || path === "/v1/auth/password")
    return "LIMIT_LOGIN";
  if (/^\/v1\/links\/[^/]+\/accept$/.test(path)) return "LIMIT_LINK_ACCEPT";
  if (
    path === "/v1/sources/registration-tokens" ||
    path === "/v1/sources/register"
  )
    return "LIMIT_REGISTER";
  return null;
}

/**
 * 源地址的闸：没带中继令牌的请求（0.5.0 起源地址上只有端到端通道的 WS 升级，令牌在子协议
 * 里；预检与媒体票随明文转发删除，`LIMIT_PREFLIGHT` / `LIMIT_MEDIA` 不再用到）。
 */
export function edgeGate(request: Request): GateName | null {
  const token = (request.headers.get("sec-websocket-protocol") ?? "").includes(
    RELAY_SUBPROTOCOL_PREFIX,
  );
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
