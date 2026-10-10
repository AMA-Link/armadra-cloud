/**
 * WS 升级之后「源 → 客户端」方向的写出端（中继设计 §3.2、§4.5）。
 *
 * 中继对 WS 是字节透传：不解压、不改帧。但有两件事要在帧边界上做，所以这里只跟踪
 * 帧头（长度，内核 `ws/frames.ts` 的 {@link FrameBoundaryTracker}），从不看载荷：
 *
 * - 对客户端每 `pingMs` 插一个 `ping` 帧（浏览器自动回 `pong`）；`idleMs` 内客户端
 *   一个字节都没发来就断开。客户端回的 `pong` 原样流到源，源的 WS 实现会忽略不请自来的
 *   `pong`（RFC 6455 §5.5.3）。
 * - 源下线（流被 `RST`）时：还没收到源的升级响应 → 答 HTTP 503 `source_offline`；
 *   已经是 WS 且正好在帧边界 → 发关闭帧 4404 再断；在帧中间 → 直接断。
 *
 * 源的响应头不是 `101` 时（比如源的准入答 401），后面按普通 HTTP 字节透传。
 */
import type { Duplex } from "node:stream";
import { Writable } from "node:stream";

import { CLOSE } from "@armadra/platform-protocol/tunnel";
import {
  closeFrame,
  FrameBoundaryTracker,
  PING_FRAME,
} from "@armadra/relay-kernel/ws/frames";

const HEAD_LIMIT = 64 * 1024;

export interface WsDownstreamOptions {
  pingMs: number;
  idleMs: number;
  /** 源下线时、还没收到升级响应的 HTTP 答复（503 `source_offline` 的整段字节）。 */
  offlineResponse: Buffer;
}

export class WsDownstream extends Writable {
  private phase: "head" | "frames" | "http" = "head";
  private head: Buffer[] = [];
  private headLength = 0;
  private wrote = false;
  private readonly tracker = new FrameBoundaryTracker();
  private pending: Uint8Array | null = null;
  private timer: ReturnType<typeof setInterval> | undefined;
  private lastClientMs = Date.now();
  private readonly socket: Duplex;
  private readonly options: WsDownstreamOptions;

  constructor(socket: Duplex, options: WsDownstreamOptions) {
    super();
    this.socket = socket;
    this.options = options;
    socket.on("data", this.onClientData);
    socket.once("close", () => this.stop());
  }

  get upgraded(): boolean {
    return this.phase === "frames";
  }

  override _write(
    chunk: Buffer,
    _encoding: BufferEncoding,
    callback: (error?: Error | null) => void,
  ): void {
    const pieces: Uint8Array[] = [];
    if (this.phase === "head") {
      this.head.push(chunk);
      this.headLength += chunk.length;
      const joined = Buffer.concat(this.head);
      const end = joined.indexOf("\r\n\r\n");
      if (end < 0 && this.headLength < HEAD_LIMIT) {
        callback();
        return;
      }
      this.head = [];
      const headEnd = end < 0 ? joined.length : end + 4;
      pieces.push(joined.subarray(0, headEnd));
      const status = /^HTTP\/1\.[01] (\d{3})/.exec(
        joined.subarray(0, Math.min(headEnd, 32)).toString("latin1"),
      );
      if (status?.[1] === "101") {
        this.phase = "frames";
        this.startKeepalive();
      } else this.phase = "http";
      chunk = joined.subarray(headEnd);
    }
    if (this.phase === "frames" && chunk.length > 0) {
      let from = 0;
      for (const boundary of this.tracker.feed(chunk)) {
        if (!this.pending) continue;
        pieces.push(chunk.subarray(from, boundary), this.pending);
        this.pending = null;
        from = boundary;
      }
      pieces.push(chunk.subarray(from));
    } else if (chunk.length > 0) pieces.push(chunk);
    this.writeOut(pieces, callback);
  }

  override _final(callback: (error?: Error | null) => void): void {
    this.stop();
    this.socket.end();
    callback();
  }

  /** 源下线：按当前状态答 503、关闭帧 4404，或直接断。 */
  sourceGone(): void {
    this.stop();
    if (this.socket.destroyed) return;
    if (this.phase === "head" && !this.wrote) {
      this.socket.end(this.options.offlineResponse);
    } else if (this.phase === "frames" && this.tracker.atBoundary) {
      this.socket.end(closeFrame(CLOSE.sourceOffline, "source_offline"));
    } else {
      this.socket.destroy();
    }
  }

  /** 在下一个帧边界插入一个控制帧。 */
  inject(frame: Uint8Array): void {
    if (this.phase !== "frames") return;
    if (this.tracker.atBoundary) this.writeOut([frame], () => {});
    else this.pending = frame;
  }

  private writeOut(
    pieces: Uint8Array[],
    callback: (error?: Error | null) => void,
  ): void {
    const parts = pieces.filter((piece) => piece.length > 0);
    if (parts.length === 0 || this.socket.destroyed) {
      callback();
      return;
    }
    this.wrote = true;
    let ok = true;
    for (const part of parts) ok = this.socket.write(part);
    if (ok) callback();
    else this.socket.once("drain", () => callback());
  }

  private readonly onClientData = (): void => {
    this.lastClientMs = Date.now();
  };

  private startKeepalive(): void {
    this.lastClientMs = Date.now();
    this.timer = setInterval(() => {
      if (Date.now() - this.lastClientMs >= this.options.idleMs) {
        this.stop();
        this.socket.destroy();
        return;
      }
      this.inject(PING_FRAME);
    }, this.options.pingMs);
    this.timer.unref?.();
  }

  private stop(): void {
    clearInterval(this.timer);
    this.timer = undefined;
  }
}
