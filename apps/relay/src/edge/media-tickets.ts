/**
 * 媒体票（cloud-api §12）：`<video src>`、`<img src>`、`<a href download>` 带不了
 * `Armadra-Relay-Token` 头，经中继的源上的媒体就放不了。客户端先用带头的请求在源
 * 地址下换一张票（`POST <源地址>/_relay/media-tickets`，体 `{ path }`），浏览器再取
 * `<源地址>/_relay/m/<票>`，边缘按票把请求转到源上的那一条路径。
 *
 * 票只在这个中继进程的内存里：32 字节随机、base64url；绑源、单个路径（含 query）与
 * 签票的中继令牌（`sub`、`jti`）；只认 GET / HEAD。可多次使用（播放器拖动是一串
 * `Range` 请求），闲置 {@link MEDIA_TICKET_IDLE_MS} 或签出 {@link MEDIA_TICKET_MAX_MS}
 * 之后作废；签票的令牌被撤销后也作废。地址里只有票，路径不出现在 URL 上；日志与指标
 * 照旧不记路径（票也在路径里）。
 */
import { randomBytes } from "node:crypto";

export const MEDIA_ISSUE_PATH = "/_relay/media-tickets";
export const MEDIA_REDEEM_PREFIX = "/_relay/m/";
export const MEDIA_TICKET_IDLE_MS = 5 * 60_000;
export const MEDIA_TICKET_MAX_MS = 30 * 60_000;
/** 签票时体的上限：只有一条路径。 */
export const MEDIA_ISSUE_BODY_MAX = 4096;
const MEDIA_PATH_MAX = 2048;
const LIMIT = 4096;

export interface MediaGrant {
  readonly sourceId: string;
  /** 转给源的路径（含 query）。 */
  readonly path: string;
  /** 签票的中继令牌：限流按它与账号计，撤销了票跟着失效。 */
  readonly subject: string;
  readonly jti: string;
}

interface Entry extends MediaGrant {
  readonly issuedAtMs: number;
  lastUsedAtMs: number;
}

export class MediaTickets {
  private readonly tickets = new Map<string, Entry>();
  private readonly now: () => number;

  constructor(now: () => number = Date.now) {
    this.now = now;
  }

  issue(grant: MediaGrant): { ticket: string; expiresAtMs: number } {
    this.sweep();
    if (this.tickets.size >= LIMIT) {
      const oldest = this.tickets.keys().next().value as string;
      this.tickets.delete(oldest);
    }
    const ticket = randomBytes(32).toString("base64url");
    const at = this.now();
    this.tickets.set(ticket, { ...grant, issuedAtMs: at, lastUsedAtMs: at });
    return { ticket, expiresAtMs: at + MEDIA_TICKET_IDLE_MS };
  }

  /** 认一张票并续上闲置期；过期或不认识答 `undefined`。 */
  use(ticket: string): MediaGrant | undefined {
    const found = this.tickets.get(ticket);
    if (found === undefined) return undefined;
    if (this.expired(found)) {
      this.tickets.delete(ticket);
      return undefined;
    }
    found.lastUsedAtMs = this.now();
    return found;
  }

  forget(ticket: string): void {
    this.tickets.delete(ticket);
  }

  get size(): number {
    return this.tickets.size;
  }

  private expired(entry: Entry): boolean {
    const now = this.now();
    return (
      now - entry.lastUsedAtMs >= MEDIA_TICKET_IDLE_MS ||
      now - entry.issuedAtMs >= MEDIA_TICKET_MAX_MS
    );
  }

  private sweep(): void {
    for (const [ticket, entry] of this.tickets)
      if (this.expired(entry)) this.tickets.delete(ticket);
  }
}

/** 源路径 `/_relay/m/<票>` 里的票；形状不对答 `null`。 */
export function redeemTicketOf(path: string): string | null {
  if (!path.startsWith(MEDIA_REDEEM_PREFIX)) return null;
  const ticket = path.slice(MEDIA_REDEEM_PREFIX.length);
  return /^[A-Za-z0-9_-]{43}$/.test(ticket) ? ticket : null;
}

/** 签票的路径（去掉 query 比）。 */
export function isIssuePath(path: string): boolean {
  const bare = path.split("?", 1)[0];
  return bare === MEDIA_ISSUE_PATH;
}

/**
 * 体里要转的那条源路径：以 `/` 开头、不含片段与控制字符、不指向中继自己的
 * `/_relay/`、不超过 2048 字节。不合格答 `null`。
 */
export function mediaTargetPath(body: unknown): string | null {
  if (typeof body !== "object" || body === null) return null;
  const path = (body as { path?: unknown }).path;
  if (typeof path !== "string") return null;
  if (!path.startsWith("/") || path.startsWith("//")) return null;
  if (path.length > MEDIA_PATH_MAX) return null;
  // eslint-disable-next-line no-control-regex
  if (/[\u0000- \u007f#\\]/.test(path)) return null;
  if (path.startsWith("/_relay/")) return null;
  const bare = path.split("?", 1)[0] ?? "";
  if (bare.split("/").some((segment) => segment === ".." || segment === "."))
    return null;
  return path;
}
