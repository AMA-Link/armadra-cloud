/**
 * DO 里的客户端 WebSocket（中继设计 §2.5）：客户端 WS 由 Workers 运行时终止（拿到的是消息，
 * 不是帧），所以中继在「隧道流里的字节」与「消息」之间转换（内核 `ws/frames`）：
 *
 * - 下行（core → 客户端）：解帧（服务端帧不带掩码）、合并分片，`ws.send`；`close` 帧 →
 *   `ws.close(code, reason)`；core 发来的 `ping` 由中继代答带掩码的 `pong`（客户端那边的活性由
 *   Cloudflare 边缘与检查周期维持），`pong` 吞掉。解帧出错 → 客户端 1002、流重置。
 * - 上行（客户端 → core）：每条消息打成一帧（客户端帧必须带 4 字节掩码，RFC 6455 §5.1）写进流，
 *   超过 1 MiB 以 1009 关闭；客户端关闭 → 发关闭帧并 END。
 * - 源下线（流被重置）：客户端以 4404 `source_offline` 关闭。
 *
 * 一条桥的状态（流号、信用、账号、最近有字节的时间）存在这条 WS 的附件里，对象休眠后照样接上。
 */
import { randomBytes } from "node:crypto";

import { CLOSE, RST } from "@armadra/platform-protocol/tunnel";
import {
  closePayload,
  encodeWsFrame,
  OPCODE,
  WsFrameDecoder,
  WsFrameError,
} from "@armadra/relay-kernel/ws/frames";

import type { EndpointHandler, TunnelEndpoint } from "./endpoint.ts";

/** 客户端单条消息的上限（§4.2）。 */
export const MAX_CLIENT_MESSAGE_BYTES = 1024 * 1024;
/** 源发给客户端的单条消息上限（合并分片后）。 */
export const MAX_SOURCE_MESSAGE_BYTES = 16 * 1024 * 1024;

const encoder = new TextEncoder();

/** 发给客户端的关闭码：`ws.close` 只收 1000 与 3000–4999，其余换 1000。 */
function clientCloseCode(code: number | undefined): number {
  if (code === undefined) return 1000;
  return code === 1000 || (code >= 3000 && code <= 4999) ? code : 1000;
}

/** 关闭原因的 UTF-8 不超过 123 字节。 */
function shortReason(reason: string): string {
  let out = reason;
  while (encoder.encode(out).length > 123) out = out.slice(0, -1);
  return out;
}

export interface BridgeHooks {
  /** 有字节经过（任一方向）：记最近活动时间、存附件。 */
  activity(): void;
  /** 桥结束（客户端已关或已断）：摘掉、还并发名额。 */
  closed(): void;
}

export class WsBridge implements EndpointHandler {
  readonly ws: WebSocket;
  readonly endpoint: TunnelEndpoint;
  private readonly decoder = new WsFrameDecoder({
    masked: "forbidden",
    maxMessageBytes: MAX_SOURCE_MESSAGE_BYTES,
  });
  private readonly hooks: BridgeHooks;
  /** 客户端那条 WS 已经关了（我们关的或它关的）。 */
  private clientClosed = false;
  /** 往 core 的方向已经结束（发过关闭帧 + END，或流已重置）。 */
  private upstreamClosed = false;
  private done = false;

  constructor(ws: WebSocket, endpoint: TunnelEndpoint, hooks: BridgeHooks) {
    this.ws = ws;
    this.endpoint = endpoint;
    this.hooks = hooks;
  }

  /** 接管流（握手之后，或休眠唤醒后还原）；`pending` 是 `101` 之后已经到的字节。 */
  start(pending: readonly Uint8Array[] = []): void {
    for (const chunk of pending) this.data(chunk);
    this.endpoint.attach(this);
  }

  /** 没有半帧、流也没有排队的字节：可以休眠。 */
  get idle(): boolean {
    return this.decoder.idle && this.endpoint.idle;
  }

  get finished(): boolean {
    return this.done;
  }

  // ------------------------------------------------------------ 下行

  data(chunk: Uint8Array): boolean {
    if (this.done) return true;
    this.hooks.activity();
    let events;
    try {
      events = this.decoder.feed(chunk);
    } catch (error) {
      const tooLarge =
        error instanceof WsFrameError && error.reason === "too_large";
      this.closeClient(
        tooLarge ? 1009 : 1002,
        tooLarge ? "message_too_large" : "protocol_error",
      );
      this.destroyUpstream(RST.protocol);
      this.markDone();
      return true;
    }
    for (const event of events) {
      if (this.done) break;
      switch (event.type) {
        case "message":
          try {
            this.ws.send(
              event.binary ? event.data : new TextDecoder().decode(event.data),
            );
          } catch {
            this.clientGone();
          }
          break;
        case "close":
          // core 先关：转给客户端，并回一个关闭帧完成 core 那边的握手。
          this.closeClient(
            clientCloseCode(event.code),
            shortReason(event.reason),
          );
          this.sendClose(event.code, "");
          this.markDone();
          break;
        case "ping":
          // 代客户端答 pong：core 的活性检查照常通过。
          void this.endpoint
            .write(
              encodeWsFrame({
                opcode: OPCODE.pong,
                payload: event.data,
                mask: randomBytes(4),
              }),
            )
            .catch(() => {});
          break;
        case "pong":
          break;
      }
    }
    return true;
  }

  end(): void {
    // core 结束了流（没发关闭帧也按正常结束）：客户端 1000，我们这边也 END。
    this.closeClient(1000, "");
    if (!this.upstreamClosed) {
      this.upstreamClosed = true;
      void this.endpoint.finish().catch(() => {});
    }
    this.markDone();
  }

  /** 流被对端重置（源下线、隧道关闭）：客户端 4404。 */
  reset(): void {
    this.closeClient(CLOSE.sourceOffline, "source_offline");
    this.upstreamClosed = true;
    this.markDone();
  }

  // ------------------------------------------------------------ 上行

  /** 客户端发来一条消息。 */
  clientMessage(message: string | ArrayBuffer): void {
    if (this.done) return;
    this.hooks.activity();
    const payload =
      typeof message === "string"
        ? encoder.encode(message)
        : new Uint8Array(message);
    if (payload.length > MAX_CLIENT_MESSAGE_BYTES) {
      this.closeClient(1009, "message_too_large");
      this.sendClose(1009, "message_too_large");
      this.markDone();
      return;
    }
    if (this.upstreamClosed) return;
    void this.endpoint
      .write(
        encodeWsFrame({
          opcode: typeof message === "string" ? OPCODE.text : OPCODE.binary,
          payload,
          mask: randomBytes(4),
        }),
      )
      .catch(() => {});
  }

  /** 客户端关闭（`webSocketClose`）：转一个关闭帧给 core，然后 END；掉线（1006）直接重置。 */
  clientClose(code: number, reason: string): void {
    this.clientClosed = true;
    if (code === 1006 || code === 1015) this.destroyUpstream(RST.clientGone);
    else this.sendClose(code === 1005 ? undefined : code, reason);
    this.markDone();
  }

  /** 客户端出错（`webSocketError`）：当作掉线。 */
  clientGone(): void {
    this.clientClosed = true;
    this.destroyUpstream(RST.clientGone);
    this.markDone();
  }

  /** 中继要断开这条连接（撤销、空闲、关隧道）。 */
  disconnect(code: number, reason: string): void {
    this.closeClient(code, reason);
    this.destroyUpstream(RST.clientGone);
    this.markDone();
  }

  private sendClose(code: number | undefined, reason: string): void {
    if (this.upstreamClosed) return;
    this.upstreamClosed = true;
    void this.endpoint
      .write(
        encodeWsFrame({
          opcode: OPCODE.close,
          payload: closePayload(code, shortReason(reason)),
          mask: randomBytes(4),
        }),
      )
      .then(() => this.endpoint.finish())
      .catch(() => {});
  }

  private destroyUpstream(code: number): void {
    if (this.upstreamClosed) return;
    this.upstreamClosed = true;
    this.endpoint.destroy(code);
  }

  private closeClient(code: number, reason: string): void {
    if (this.clientClosed) return;
    this.clientClosed = true;
    try {
      this.ws.close(code, reason);
    } catch {
      // 已经关了。
    }
  }

  private markDone(): void {
    if (this.done) return;
    this.done = true;
    this.hooks.closed();
  }
}
