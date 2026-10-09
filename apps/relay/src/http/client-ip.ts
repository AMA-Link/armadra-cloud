/**
 * 客户端 IP 的唯一出处：限流、登录锁定、转给源的 `X-Forwarded-For` 都走 `ClientIp`。
 *
 * 缺省不信任任何代理，只用直连对端地址。配置了受信代理后，只有直连对端落在受信列表内，
 * 才读转发头；不在列表里的对端发来的 `X-Forwarded-For` / `X-Real-IP` 一律忽略（防伪造）。
 * `X-Forwarded-For` 自右向左剔除受信跳数，第一个不受信的地址就是客户端；全部受信取最左一个。
 */
import type { IncomingMessage } from "node:http";
import { BlockList, isIP } from "node:net";

/** 优先读哪个头；另一个作备选。 */
export type TrustedProxyPrefer = "forwarded-for" | "real-ip";

export interface TrustedProxyConfig {
  /** CIDR 或单个 IP（`127.0.0.1/32`、`::1/128`、`10.0.0.1`）。 */
  proxies: readonly string[];
  prefer?: TrustedProxyPrefer;
}

export type ClientIp = (
  req: Pick<IncomingMessage, "headers" | "socket">,
) => string;

/** 去掉方括号、zone id 与 IPv4 映射前缀；不是 IP 返回 null。 */
export function normalizeIp(text: string): string | null {
  let value = text.trim();
  if (value.startsWith("[") && value.endsWith("]")) value = value.slice(1, -1);
  const zone = value.indexOf("%");
  if (zone >= 0) value = value.slice(0, zone);
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(value);
  if (mapped?.[1]) value = mapped[1];
  return isIP(value) === 0 ? null : value.toLowerCase();
}

/** 解析 `--trusted-proxy` 的逗号分隔列表；不合法抛 `Error`（调用方转成用法错误）。 */
export function parseProxyList(text: string): string[] {
  return text
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item.length > 0);
}

function buildList(proxies: readonly string[]): BlockList {
  const list = new BlockList();
  for (const entry of proxies) {
    const [addr = "", prefixText] = entry.split("/");
    const ip = normalizeIp(addr);
    if (ip === null) throw new Error(`受信代理不是 IP 或 CIDR：${entry}`);
    const family = isIP(ip) === 6 ? "ipv6" : "ipv4";
    const max = family === "ipv6" ? 128 : 32;
    if (prefixText === undefined) {
      list.addAddress(ip, family);
      continue;
    }
    const prefix = Number(prefixText);
    if (!/^\d+$/.test(prefixText) || prefix > max)
      throw new Error(`受信代理前缀不合法：${entry}`);
    list.addSubnet(ip, prefix, family);
  }
  return list;
}

function lastHeader(value: string | string[] | undefined): string {
  return Array.isArray(value) ? value.join(",") : (value ?? "");
}

export function createClientIp(config?: TrustedProxyConfig): ClientIp {
  const peerOf = (req: Pick<IncomingMessage, "socket">): string =>
    normalizeIp(req.socket.remoteAddress ?? "") ??
    req.socket.remoteAddress ??
    "";
  if (!config || config.proxies.length === 0) return peerOf;

  const list = buildList(config.proxies);
  const trusted = (ip: string): boolean =>
    list.check(ip, isIP(ip) === 6 ? "ipv6" : "ipv4");
  const prefer = config.prefer ?? "forwarded-for";

  const fromForwardedFor = (header: string): string | null => {
    if (header.trim() === "") return null;
    const hops = header.split(",");
    for (let index = hops.length - 1; index >= 0; index -= 1) {
      const ip = normalizeIp(hops[index] ?? "");
      if (ip === null) return null; // 链里有乱码：整条不可信
      if (!trusted(ip) || index === 0) return ip;
    }
    return null;
  };
  const fromRealIp = (header: string): string | null =>
    header.includes(",") ? null : normalizeIp(header);

  return (req) => {
    const peer = peerOf(req);
    if (!trusted(peer)) return peer;
    const xff = fromForwardedFor(lastHeader(req.headers["x-forwarded-for"]));
    const real = fromRealIp(lastHeader(req.headers["x-real-ip"]));
    return (prefer === "real-ip" ? (real ?? xff) : (xff ?? real)) ?? peer;
  };
}
