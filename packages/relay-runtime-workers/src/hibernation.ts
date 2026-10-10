/**
 * Hibernation 的粘合（中继设计 §2.3）：对象可以在没有事件时被换出内存，WebSocket 照样连着；
 * 下一个事件把对象唤醒，构造函数从 WS 的附件（`serializeAttachment`，随 WS 存放、不计存储写）
 * 还原隧道与客户端桥。热状态（信用、流号、结束标记）每次动过就写进附件，所以休眠前总是最新的。
 *
 * 不能存的状态（流里还有排队或在途的字节、半个 WS 帧）出现时，对象用一个短定时器把自己留在
 * 内存里，直到这些字节走完——有定时器在，对象不会休眠。
 */
import type { StreamCoreState } from "@armadra/relay-kernel/tunnel/stream";
import type { TunnelSnapshot } from "@armadra/relay-kernel/tunnel/tunnel";

/** 隧道 WS 的附件。 */
export interface TunnelAttachment {
  kind: "tunnel";
  /** `handshake`：握手中（不能跨休眠，唤醒后关掉）；`ready`：已就绪。 */
  phase: "handshake" | "ready";
  sourceId: string;
  /** 最近一次收到源的任何帧（检查周期据此判死）。 */
  lastHeardAtMs: number;
  readyAtMs?: number;
  /** 只在 `open` 时有；排空中的旧隧道是 null（唤醒后关掉）。 */
  snapshot: TunnelSnapshot | null;
}

/** 客户端 WS 的附件（一条桥）。 */
export interface ClientAttachment {
  kind: "client";
  sourceId: string;
  tunnelId: string;
  streamId: number;
  /** 中继令牌的 `sub`（按账号并发、按访客前缀断开）。 */
  subject: string;
  /** 最近一次任一方向有字节。 */
  lastActivityMs: number;
  /** 流的信用；有排队或在途字节时是 null（那时对象不休眠）。 */
  core: StreamCoreState | null;
}

export type SourceAttachment = TunnelAttachment | ClientAttachment;

export function attachmentOf(ws: WebSocket): SourceAttachment | null {
  try {
    const value = ws.deserializeAttachment() as SourceAttachment | null;
    return value && typeof value === "object" && "kind" in value ? value : null;
  } catch {
    return null;
  }
}

/** 把对象留在内存里的短定时器（有它在就不休眠）；`check` 答 true 就继续留。 */
export class KeepAwake {
  private timer: ReturnType<typeof setTimeout> | undefined;
  private readonly intervalMs: number;

  constructor(intervalMs = 1_000) {
    this.intervalMs = intervalMs;
  }

  hold(check: () => boolean): void {
    if (this.timer !== undefined) return;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      if (check()) this.hold(check);
    }, this.intervalMs);
  }

  get holding(): boolean {
    return this.timer !== undefined;
  }

  release(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
  }
}
