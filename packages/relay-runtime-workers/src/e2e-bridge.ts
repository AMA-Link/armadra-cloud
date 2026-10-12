/**
 * DO 里的端到端通道（cloud-api §20）：客户端 WS 由 Workers 运行时终止，二进制消息原样写进
 * `OPEN kind: "e2e"` 的流，流的字节原样作为二进制消息发回——不解帧，里面是
 * `armadra-e2e/1` 的密文单元，中继看不懂也不看。消息边界不保证（通道按长度自己切）。
 *
 * - 文本消息以 4400 关闭并重置流；单条消息超过 1 MiB 以 1009 关闭。
 * - 客户端关闭 → `END`；掉线（1006 / 1015）→ `RST clientGone`。源 `END` → 1000；源 `RST` / 隧道
 *   没了 → 4404 `source_offline`。
 *
 * 附件里 `mode: "e2e"`；0.5.0 起这是唯一的客户端桥（明文 WS 的桥随明文转发删除）。
 */
import { CLOSE, RST } from "@armadra/platform-protocol/tunnel";

import type { EndpointHandler, TunnelEndpoint } from "./endpoint.ts";
/** 客户端单条消息的上限。 */
export const MAX_CLIENT_MESSAGE_BYTES = 1024 * 1024;

export interface BridgeHooks {
  /** 有字节经过（任一方向）：记最近活动时间、存附件。 */
  activity(): void;
  /** 桥结束（客户端已关或已断）：摘掉、还并发名额。 */
  closed(): void;
}

export class E2eBridge implements EndpointHandler {
  readonly ws: WebSocket;
  readonly endpoint: TunnelEndpoint;
  private readonly hooks: BridgeHooks;
  private clientClosed = false;
  private upstreamClosed = false;
  private done = false;

  constructor(ws: WebSocket, endpoint: TunnelEndpoint, hooks: BridgeHooks) {
    this.ws = ws;
    this.endpoint = endpoint;
    this.hooks = hooks;
  }

  /** 接管流（开流之后，或休眠唤醒后还原）。 */
  start(): void {
    this.endpoint.attach(this);
  }

  /** 流没有排队或在途的字节：可以休眠（这里不缓存任何半截数据）。 */
  get idle(): boolean {
    return this.endpoint.idle;
  }

  get finished(): boolean {
    return this.done;
  }

  // ------------------------------------------------------------ 下行

  data(chunk: Uint8Array): boolean {
    if (this.done) return true;
    this.hooks.activity();
    try {
      this.ws.send(chunk);
    } catch {
      this.clientGone();
    }
    return true;
  }

  end(): void {
    this.closeClient(CLOSE.normal, "");
    if (!this.upstreamClosed) {
      this.upstreamClosed = true;
      void this.endpoint.finish().catch(() => {});
    }
    this.markDone();
  }

  reset(): void {
    this.closeClient(CLOSE.sourceOffline, "source_offline");
    this.upstreamClosed = true;
    this.markDone();
  }

  // ------------------------------------------------------------ 上行

  clientMessage(message: string | ArrayBuffer): void {
    if (this.done) return;
    this.hooks.activity();
    if (typeof message === "string") {
      this.closeClient(CLOSE.badFrame, "binary_only");
      this.destroyUpstream(RST.protocol);
      this.markDone();
      return;
    }
    if (message.byteLength > MAX_CLIENT_MESSAGE_BYTES) {
      this.closeClient(1009, "message_too_large");
      this.destroyUpstream(RST.protocol);
      this.markDone();
      return;
    }
    if (this.upstreamClosed || message.byteLength === 0) return;
    void this.endpoint.write(new Uint8Array(message)).catch(() => {});
  }

  clientClose(code: number): void {
    this.clientClosed = true;
    if (code === 1006 || code === 1015) this.destroyUpstream(RST.clientGone);
    else if (!this.upstreamClosed) {
      this.upstreamClosed = true;
      void this.endpoint.finish().catch(() => {});
    }
    this.markDone();
  }

  clientGone(): void {
    this.clientClosed = true;
    this.destroyUpstream(RST.clientGone);
    this.markDone();
  }

  disconnect(code: number, reason: string): void {
    this.closeClient(code, reason);
    this.destroyUpstream(RST.clientGone);
    this.markDone();
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
