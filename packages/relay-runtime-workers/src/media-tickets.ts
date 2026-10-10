/**
 * 媒体票的 DO 存储版（cloud-api §12，内核的 {@link MediaTicketStore}）：票落在 SourceDO 的同步
 * KV（`ctx.storage.kv`）里，对象休眠、唤醒后照样认（播放器的一串 `Range` 请求之间常有空闲）。
 *
 * 规则与进程内存版相同：32 字节随机、base64url；绑源、单个路径与签票令牌；闲置 5 分钟或签出
 * 30 分钟作废。存的是票的 SHA-256（键 `mt:<哈希>`），库里没有可直接用的票；闲置期一分钟内只
 * 续一次，少写几次存储。过期的由 alarm 清掉。
 */
import { createHash, randomBytes } from "node:crypto";

import {
  MEDIA_TICKET_IDLE_MS,
  MEDIA_TICKET_MAX_MS,
  type MediaGrant,
  type MediaTicketStore,
} from "@armadra/relay-kernel/edge/media-tickets";

const PREFIX = "mt:";
/** 闲置期的续期粒度：一分钟内重复使用不写存储。 */
const TOUCH_GRANULARITY_MS = 60_000;

interface StoredTicket extends MediaGrant {
  issuedAtMs: number;
  lastUsedAtMs: number;
}

const keyOf = (ticket: string) =>
  `${PREFIX}${createHash("sha256").update(ticket).digest("base64url")}`;

export class KvMediaTickets implements MediaTicketStore {
  private readonly kv: SyncKvStorage;
  private readonly now: () => number;

  constructor(kv: SyncKvStorage, now: () => number = Date.now) {
    this.kv = kv;
    this.now = now;
  }

  issue(grant: MediaGrant): { ticket: string; expiresAtMs: number } {
    const ticket = randomBytes(32).toString("base64url");
    const at = this.now();
    const stored: StoredTicket = {
      sourceId: grant.sourceId,
      path: grant.path,
      subject: grant.subject,
      jti: grant.jti,
      issuedAtMs: at,
      lastUsedAtMs: at,
    };
    this.kv.put(keyOf(ticket), stored);
    return { ticket, expiresAtMs: at + MEDIA_TICKET_IDLE_MS };
  }

  use(ticket: string): MediaGrant | undefined {
    const key = keyOf(ticket);
    const found = this.kv.get<StoredTicket>(key);
    if (found === undefined) return undefined;
    const at = this.now();
    if (this.expired(found, at)) {
      this.kv.delete(key);
      return undefined;
    }
    if (at - found.lastUsedAtMs >= TOUCH_GRANULARITY_MS)
      this.kv.put(key, { ...found, lastUsedAtMs: at });
    return {
      sourceId: found.sourceId,
      path: found.path,
      subject: found.subject,
      jti: found.jti,
    };
  }

  forget(ticket: string): void {
    this.kv.delete(keyOf(ticket));
  }

  /** 清掉过期的票（alarm 调）；答还剩几张。 */
  sweep(): number {
    const at = this.now();
    let left = 0;
    const stale: string[] = [];
    for (const [key, value] of this.kv.list<StoredTicket>({ prefix: PREFIX })) {
      if (this.expired(value, at)) stale.push(key);
      else left += 1;
    }
    for (const key of stale) this.kv.delete(key);
    return left;
  }

  private expired(entry: StoredTicket, at: number): boolean {
    return (
      at - entry.lastUsedAtMs >= MEDIA_TICKET_IDLE_MS ||
      at - entry.issuedAtMs >= MEDIA_TICKET_MAX_MS
    );
  }
}
