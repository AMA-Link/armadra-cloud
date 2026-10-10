/**
 * 隧道终端 `/t/v1` 的 Node 外壳（中继设计 §3.1）：`ws` 完成升级，每条 WebSocket 交给
 * 内核的 {@link TunnelHub}（握手、隧道表、同源替换、路由与在线事件都在那里）。
 */
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import { LIMITS } from "@armadra/platform-protocol/tunnel";
import type { TunnelHub } from "@armadra/relay-kernel/tunnel/hub";
import type { Tunnel } from "@armadra/relay-kernel/tunnel/tunnel";
import { WebSocketServer, type RawData, type WebSocket } from "ws";

import type { TunnelStream } from "./stream.ts";

function toBytes(data: RawData): Buffer {
  if (Buffer.isBuffer(data)) return data;
  if (Array.isArray(data)) return Buffer.concat(data);
  return Buffer.from(data);
}

export class TunnelServer {
  private readonly wss = new WebSocketServer({
    noServer: true,
    maxPayload: LIMITS.maxFrameBytes + 16,
    perMessageDeflate: false,
  });
  private readonly hub: TunnelHub<TunnelStream>;

  constructor(hub: TunnelHub<TunnelStream>) {
    this.hub = hub;
  }

  counts(): { tunnels: number; streams: number } {
    return this.hub.counts();
  }

  /** 边缘用：本节点上持有这个源、且路由指向它、还能开流的隧道。 */
  lookup(sourceId: string): Promise<Tunnel<TunnelStream> | null> {
    return this.hub.lookup(sourceId);
  }

  handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): void {
    if (!this.hub.accepting) {
      socket.destroy();
      return;
    }
    this.wss.handleUpgrade(req, socket, head, (ws) => this.accept(ws));
  }

  /** 所有隧道发 GOAWAY，等它们排空（或 grace 到），再断开剩下的 WebSocket。 */
  async shutdown(graceMs: number): Promise<void> {
    await this.hub.shutdown(graceMs);
    for (const client of this.wss.clients) client.terminate();
    await new Promise<void>((resolve) => this.wss.close(() => resolve()));
  }

  private accept(ws: WebSocket): void {
    const connection = this.hub.accept({
      sendText: (text) => ws.send(text),
      sendBinary: (bytes) => ws.send(bytes, { binary: true }),
      close: (code, reason) => ws.close(code, reason),
      terminate: () => ws.terminate(),
      get isOpen() {
        return ws.readyState === ws.OPEN;
      },
    });
    ws.on("message", (data: RawData, isBinary: boolean) =>
      connection.message(toBytes(data), isBinary),
    );
    ws.on("close", (code: number) => connection.closed(code));
    ws.on("error", () => {
      // close 随后到达。
    });
  }
}
