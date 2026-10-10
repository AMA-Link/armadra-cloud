/**
 * 假 core（中继设计 §9）：一个最小 `http.Server` + `ws`，经协议包的隧道帧与握手 schema
 * 自己实现的「隧道客户端」连上中继。每条 `OPEN` 变成一个 `Duplex`，`emit("connection")`
 * 给 `http.Server`——与 core 的做法相同，由 Node 解析 HTTP 与升级。
 *
 * 流控用中继同一份 `TunnelStream`（对角色无感）；帧的分派、握手、隧道级窗口、心跳应答
 * 是这里独立写的。路由：
 *
 * - `GET /echo-request`：答 `{ method, url, headers }`（看中继剥了什么、加了什么）。
 * - `POST /echo`：原样回请求体（流式）。
 * - `GET /big?bytes=N`：写 N 字节的确定序列（背压测试）。
 * - `GET /hold`：先写头，等 `release()` 才结束（GOAWAY 排空测试）。
 * - WS 任意路径：回显（文本 / 二进制），选 `armadra-ticket.*` 子协议。
 */
import { sign, type KeyObject } from "node:crypto";
import { once } from "node:events";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { Socket } from "node:net";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import {
  challengeSchema,
  challengeSigningInput,
  decodeFrame,
  encodeFrame,
  readySchema,
  rejectSchema,
  type Frame,
  type Ready,
  type Reject,
} from "@armadra/platform-protocol/tunnel";
import { WebSocket, WebSocketServer, type RawData } from "ws";

import { CreditPool, TunnelStream, type StreamHost } from "../src/stream.ts";
import {
  sourceKey as defaultSourceKey,
  SOURCE_ID,
  tunnelToken,
} from "@armadra/relay-kernel/testing/tokens";

export function bigByte(index: number): number {
  return (index * 31 + 7) & 0xff;
}

export interface FakeCoreOptions {
  /** `ws://127.0.0.1:<port>/t/v1` */
  tunnelUrl: string;
  sourceId?: string;
  sourceKey?: KeyObject;
  token?: () => string;
  /** 连 `wss://` 时信任的 CA（personal 的自签 CA）。 */
  ca?: string;
}

export interface Connected {
  ready: Ready;
}

export class FakeCore {
  readonly http: Server;
  private readonly wss = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    handleProtocols: (protocols) =>
      [...protocols].find((p) => p.startsWith("armadra-ticket.")) ?? false,
  });
  private ws: WebSocket | undefined;
  private readonly streams = new Map<number, TunnelStream>();
  private sendPool = new CreditPool(0);
  private recvUnacked = 0;
  private ready: Ready | undefined;
  private holds: (() => void)[] = [];
  /** 不答 PING（心跳测试）。 */
  answerPings = true;
  /** 所有流发出的 DATA 字节（含已关闭的流）。 */
  bytesSent = 0;
  goaways: string[] = [];
  /** 收到的 HTTP 请求数（预检不该进隧道）。 */
  requests = 0;
  closeCode: number | undefined;
  readonly options: FakeCoreOptions;

  constructor(options: FakeCoreOptions) {
    this.options = options;
    this.http = createServer((req, res) => {
      this.requests += 1;
      const url = new URL(req.url ?? "/", "http://core");
      if (url.pathname === "/echo-request") {
        const body = JSON.stringify({
          method: req.method,
          url: req.url,
          headers: req.headers,
          remote: req.socket.remoteAddress ?? null,
        });
        res.writeHead(200, { "content-type": "application/json" });
        res.end(body);
        return;
      }
      if (url.pathname === "/echo" && req.method === "POST") {
        res.writeHead(200, {
          "content-type":
            req.headers["content-type"] ?? "application/octet-stream",
        });
        req.pipe(res);
        return;
      }
      if (url.pathname === "/big") {
        void this.writeBig(res, Number(url.searchParams.get("bytes") ?? 0));
        return;
      }
      if (url.pathname === "/hold") {
        res.writeHead(200, { "content-type": "text/plain" });
        res.write("held:");
        this.holds.push(() => res.end("released"));
        return;
      }
      res.writeHead(404);
      res.end();
    });
    this.http.on(
      "upgrade",
      (req: IncomingMessage, socket: Socket, head: Buffer) => {
        this.wss.handleUpgrade(req, socket, head, (ws) => {
          ws.on("message", (data: RawData, isBinary: boolean) => {
            if (!isBinary && data.toString() === "whoami") {
              ws.send(
                JSON.stringify({
                  url: req.url,
                  protocol: ws.protocol,
                  headers: req.headers,
                }),
              );
              return;
            }
            if (!isBinary && data.toString() === "bye") {
              ws.close(4000, "bye");
              return;
            }
            ws.send(data, { binary: isBinary });
          });
        });
      },
    );
  }

  get connected(): boolean {
    return this.ws?.readyState === WebSocket.OPEN && this.ready !== undefined;
  }

  get openStreams(): number {
    return this.streams.size;
  }

  /** 握手到 ready；被拒时答 reject 消息与关闭码。 */
  async connect(): Promise<{
    ready?: Ready;
    reject?: Reject;
    closeCode?: number;
  }> {
    const ws = new WebSocket(this.options.tunnelUrl, {
      perMessageDeflate: false,
      ...(this.options.ca ? { ca: this.options.ca } : {}),
    });
    this.ws = ws;
    this.ready = undefined;
    this.closeCode = undefined;
    const sourceId = this.options.sourceId ?? SOURCE_ID;
    const nonce = Buffer.alloc(16, 0xa1).toString("base64url");
    const outcome = new Promise<{
      ready?: Ready;
      reject?: Reject;
      closeCode?: number;
    }>((resolve) => {
      let reject: Reject | undefined;
      ws.on("message", (data: RawData, isBinary: boolean) => {
        if (isBinary) {
          this.receive(
            Buffer.isBuffer(data) ? data : Buffer.from(data as ArrayBuffer),
          );
          return;
        }
        const message = JSON.parse(data.toString()) as unknown;
        const challenge = challengeSchema.safeParse(message);
        if (challenge.success) {
          const sig = sign(
            null,
            challengeSigningInput({
              sourceId,
              nonce,
              nonce2: challenge.data.nonce2,
              relayNode: challenge.data.relayNode,
            }),
            this.options.sourceKey ?? defaultSourceKey,
          ).toString("base64url");
          ws.send(JSON.stringify({ type: "auth", sig }));
          return;
        }
        const ready = readySchema.safeParse(message);
        if (ready.success) {
          this.ready = ready.data;
          this.sendPool = new CreditPool(ready.data.limits.tunnelWindow);
          this.recvUnacked = 0;
          resolve({ ready: ready.data });
          return;
        }
        const rejected = rejectSchema.safeParse(message);
        if (rejected.success) reject = rejected.data;
      });
      ws.on("close", (code: number) => {
        this.closeCode = code;
        for (const stream of [...this.streams.values()]) stream.destroy();
        this.streams.clear();
        resolve({ reject, closeCode: code });
      });
      ws.on("error", () => {});
    });
    await once(ws, "open");
    ws.send(
      JSON.stringify({
        type: "hello",
        protocol: PROTOCOL_VERSION,
        sourceId,
        coreVersion: "0.2.0-test",
        capabilities: [],
        tunnelToken: (this.options.token ?? (() => tunnelToken()))(),
        nonce,
      }),
    );
    return outcome;
  }

  /** 等隧道被中继关掉，答关闭码。 */
  async closed(): Promise<number> {
    if (!this.ws || this.ws.readyState === WebSocket.CLOSED)
      return this.closeCode ?? 0;
    const [code] = (await once(this.ws, "close")) as [number];
    return code;
  }

  sendGoaway(): void {
    this.send({ type: "goaway", reason: "shutdown", graceMs: 1000 });
  }

  terminate(): void {
    this.ws?.terminate();
  }

  release(): void {
    for (const release of this.holds.splice(0)) release();
  }

  async stop(): Promise<void> {
    this.ws?.terminate();
    for (const client of this.wss.clients) client.terminate();
    this.http.closeAllConnections();
  }

  // ------------------------------------------------------------ 帧

  private readonly host: StreamHost = this.createHost();

  private createHost(): StreamHost {
    // eslint-disable-next-line @typescript-eslint/no-this-alias
    const self = this;
    return {
      sendFrame: (frame) => self.send(frame),
      // 每次握手换新的 CreditPool：用 getter 读当前那个。
      get sendCredit() {
        return self.sendPool;
      },
      consumed: (bytes) => {
        self.recvUnacked += bytes;
        const window = self.ready?.limits.tunnelWindow ?? 0;
        if (self.recvUnacked >= window / 2) {
          self.send({ type: "window", streamId: 0, credit: self.recvUnacked });
          self.recvUnacked = 0;
        }
      },
      closed: (stream) => {
        if (self.streams.get(stream.streamId) === stream)
          self.streams.delete(stream.streamId);
      },
    };
  }

  private send(frame: Frame): void {
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    if (frame.type === "data") this.bytesSent += frame.bytes.length;
    this.ws.send(encodeFrame(frame), { binary: true });
  }

  private receive(bytes: Uint8Array): void {
    const frame = decodeFrame(bytes);
    switch (frame.type) {
      case "open": {
        const stream = new TunnelStream(frame.streamId, this.host, {
          streamWindow: this.ready?.limits.streamWindow ?? 0,
          maxDataChunk: 65_536,
        });
        stream.on("error", () => {});
        this.streams.set(frame.streamId, stream);
        this.http.emit("connection", stream);
        return;
      }
      case "data": {
        const stream = this.streams.get(frame.streamId);
        if (!stream) this.host.consumed(frame.bytes.length);
        else stream.receiveData(frame.bytes);
        return;
      }
      case "end":
        this.streams.get(frame.streamId)?.receiveEnd();
        return;
      case "rst":
        this.streams.get(frame.streamId)?.receiveRst(frame.code);
        return;
      case "window":
        if (frame.streamId === 0) this.sendPool.add(frame.credit);
        else this.streams.get(frame.streamId)?.receiveWindow(frame.credit);
        return;
      case "ping":
        if (this.answerPings) this.send({ type: "pong", ts: frame.ts });
        return;
      case "pong":
        return;
      case "goaway":
        this.goaways.push(frame.reason);
        return;
    }
  }

  private async writeBig(
    res: import("node:http").ServerResponse,
    total: number,
  ): Promise<void> {
    res.writeHead(200, {
      "content-type": "application/octet-stream",
      "content-length": String(total),
    });
    const chunkSize = 64 * 1024;
    for (let offset = 0; offset < total; offset += chunkSize) {
      const size = Math.min(chunkSize, total - offset);
      const chunk = Buffer.alloc(size);
      for (let index = 0; index < size; index += 1)
        chunk[index] = bigByte(offset + index);
      if (!res.write(chunk)) {
        await new Promise<void>((resolve) => {
          const done = () => {
            res.off("drain", done);
            res.off("close", done);
            resolve();
          };
          res.on("drain", done);
          res.on("close", done);
        });
        if (res.destroyed) return;
      }
    }
    res.end();
  }
}
