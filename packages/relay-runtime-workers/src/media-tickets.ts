/**
 * 媒体票的 DO 存储版（cloud-api §12，内核的 {@link MediaTicketStore}）：票落在 SourceDO 的同步
 * KV（`ctx.storage.kv`）里，对象休眠、唤醒后照样认（播放器的一串 `Range` 请求之间常有空闲）。
 *
 * 规则与进程内存版相同：32 字节随机、base64url；绑源、单个路径与签票令牌；闲置 5 分钟或签出
 * 30 分钟作废。存储里**没有**可用的东西：键是票的派生哈希，签票时的路径（里面有 core 的媒体票）
 * 与令牌标识用票本身派生的密钥做 AES-256-GCM 封起来，只有拿着票来兑的那次请求解得开；清扫
 * 只看明文的两个时间戳。闲置期一分钟内只续一次，少写几次存储。
 */
import {
  createCipheriv,
  createDecipheriv,
  createHash,
  randomBytes,
} from "node:crypto";

import {
  MEDIA_TICKET_IDLE_MS,
  MEDIA_TICKET_MAX_MS,
  type MediaGrant,
  type MediaTicketStore,
} from "@armadra/relay-kernel/edge/media-tickets";

const PREFIX = "mt:";
/** 闲置期的续期粒度：一分钟内重复使用不写存储。 */
const TOUCH_GRANULARITY_MS = 60_000;

interface StoredTicket {
  issuedAtMs: number;
  lastUsedAtMs: number;
  /** base64(IV ‖ 密文 ‖ 标签)，明文是 {@link MediaGrant} 的 JSON。 */
  sealed: string;
}

const derive = (label: string, ticket: string) =>
  createHash("sha256")
    .update(`armadra-media-ticket ${label}\0${ticket}`)
    .digest();

const keyOf = (ticket: string) =>
  `${PREFIX}${derive("id", ticket).toString("base64url")}`;

function seal(ticket: string, grant: MediaGrant): string {
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", derive("key", ticket), iv);
  const body = Buffer.concat([
    cipher.update(Buffer.from(JSON.stringify(grant), "utf8")),
    cipher.final(),
  ]);
  return Buffer.concat([iv, body, cipher.getAuthTag()]).toString("base64");
}

function open(ticket: string, sealed: string): MediaGrant | undefined {
  try {
    const bytes = Buffer.from(sealed, "base64");
    const decipher = createDecipheriv(
      "aes-256-gcm",
      derive("key", ticket),
      bytes.subarray(0, 12),
    );
    decipher.setAuthTag(bytes.subarray(bytes.length - 16));
    const plain = Buffer.concat([
      decipher.update(bytes.subarray(12, bytes.length - 16)),
      decipher.final(),
    ]).toString("utf8");
    const grant = JSON.parse(plain) as MediaGrant;
    return {
      sourceId: grant.sourceId,
      path: grant.path,
      subject: grant.subject,
      jti: grant.jti,
    };
  } catch {
    return undefined;
  }
}

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
      issuedAtMs: at,
      lastUsedAtMs: at,
      sealed: seal(ticket, grant),
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
    const grant = open(ticket, found.sealed);
    if (!grant) return undefined;
    if (at - found.lastUsedAtMs >= TOUCH_GRANULARITY_MS)
      this.kv.put(key, { ...found, lastUsedAtMs: at });
    return grant;
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
