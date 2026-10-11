/**
 * workerd 里的假 core：经 Worker（`SELF`）连 `/t/v1`、握手，按隧道帧收发；每条 `OPEN` 用内核的
 * `StreamCore` 管信用（对角色无感），HTTP 请求由一个极小的 HTTP/1.1 解析器读出来再答，WS 升级
 * 答 `101` 后回显。路由：
 *
 * - `GET /echo-request`：答 `{ method, url, headers }`（看中继剥了什么、加了什么）。
 * - `POST /echo`：原样回请求体（请求带长度就答长度，否则 chunked）。
 * - `GET /big?bytes=N`：定长的确定序列（背压与大体）。
 * - `GET /chunked`：分块响应。
 * - `GET /media`：1000 字节的资源，认 `Range: bytes=a-b`（206）。
 * - `GET /bad`：不合法的响应（中继应答 502）。
 * - 其余 `POST` / `PUT` / `DELETE`：`{ ok: true }`（只读令牌透传的测试）。
 * - WS 任意路径：选 `armadra-ticket.*` 子协议；`whoami` 答 `{ url, protocol }`；`bye` 以 4000 关闭；
 *   `ping-me` 发一个 ping 并记下中继代答的 pong；其余回显。
 */
import { createHash, randomBytes, sign } from "node:crypto";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import {
  challengeSchema,
  challengeSigningInput,
  decodeFrame,
  encodeFrame,
  readySchema,
  rejectSchema,
  type Frame,
  type OpenPayload,
  type Ready,
  type Reject,
} from "@armadra/platform-protocol/tunnel";
import {
  CreditPool,
  StreamCore,
  type StreamHost,
} from "@armadra/relay-kernel/tunnel/stream";
import {
  encodeWsFrame,
  OPCODE,
  WsFrameDecoder,
} from "@armadra/relay-kernel/ws/frames";
import { SELF } from "cloudflare:test";

import type { TestSource } from "./api.ts";

const encoder = new TextEncoder();
const decoder = new TextDecoder();
const WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

export function bigByte(index: number): number {
  return (index * 31 + 7) & 0xff;
}

export const MEDIA = Uint8Array.from({ length: 1000 }, (_, index) =>
  bigByte(index),
);

interface ParsedRequest {
  method: string;
  path: string;
  headers: Record<string, string>;
  body: Uint8Array;
}

/** 极小的 HTTP/1.1 请求读取器：头 + `Content-Length` 或 `chunked` 体。 */
class RequestReader {
  private buffer = new Uint8Array(0);
  head: {
    method: string;
    path: string;
    headers: Record<string, string>;
  } | null = null;
  private bodyParts: Uint8Array[] = [];
  done = false;

  feed(chunk: Uint8Array): ParsedRequest | null {
    const merged = new Uint8Array(this.buffer.length + chunk.length);
    merged.set(this.buffer);
    merged.set(chunk, this.buffer.length);
    this.buffer = merged;
    if (!this.head) {
      const text = decoder.decode(this.buffer);
      const end = text.indexOf("\r\n\r\n");
      if (end < 0) return null;
      const [line, ...rest] = text.slice(0, end).split("\r\n");
      const [method, path] = (line ?? "").split(" ");
      const headers: Record<string, string> = {};
      for (const row of rest) {
        const at = row.indexOf(":");
        headers[row.slice(0, at).trim().toLowerCase()] = row
          .slice(at + 1)
          .trim();
      }
      this.head = { method: method ?? "", path: path ?? "", headers };
      this.buffer = this.buffer.subarray(
        encoder.encode(text.slice(0, end + 4)).length,
      );
    }
    return this.tryBody();
  }

  private tryBody(): ParsedRequest | null {
    const head = this.head;
    if (!head || this.done) return null;
    if (head.headers["upgrade"]) {
      this.done = true;
      return { ...head, body: this.buffer };
    }
    const length = head.headers["content-length"];
    if (length !== undefined) {
      if (this.buffer.length < Number(length)) return null;
      this.done = true;
      return { ...head, body: this.buffer.subarray(0, Number(length)) };
    }
    if (head.headers["transfer-encoding"] === "chunked") {
      for (;;) {
        const text = decoder.decode(this.buffer);
        const lineEnd = text.indexOf("\r\n");
        if (lineEnd < 0) return null;
        const size = Number.parseInt(text.slice(0, lineEnd), 16);
        const start = lineEnd + 2;
        if (size === 0) {
          if (this.buffer.length < start + 2) return null;
          this.done = true;
          const total = this.bodyParts.reduce(
            (sum, part) => sum + part.length,
            0,
          );
          const body = new Uint8Array(total);
          let at = 0;
          for (const part of this.bodyParts) {
            body.set(part, at);
            at += part.length;
          }
          return { ...head, body };
        }
        if (this.buffer.length < start + size + 2) return null;
        this.bodyParts.push(this.buffer.slice(start, start + size));
        this.buffer = this.buffer.subarray(start + size + 2);
      }
    }
    this.done = true;
    return { ...head, body: new Uint8Array(0) };
  }
}

interface CoreStream {
  core: StreamCore<CoreStream>;
  remoteEnded: boolean;
  streamId: number;
  open: OpenPayload;
  reader: RequestReader;
  ws: WsFrameDecoder | null;
  /** 写一块（拿到信用后完成）。 */
  write(bytes: Uint8Array): Promise<void>;
}

export class FakeCore {
  ws: WebSocket | null = null;
  ready: Ready | null = null;
  reject: Reject | null = null;
  closeInfo: { code: number; reason: string } | null = null;
  readonly requests: ParsedRequest[] = [];
  readonly opens: OpenPayload[] = [];
  /** 中继代客户端答的 pong 次数。 */
  pongsFromRelay = 0;
  /** 响应写完之前就收到中继 END 的次数（源不允许半关闭时会截断响应，应为 0）。 */
  endedBeforeResponse = 0;
  private readonly streams = new Map<number, CoreStream>();
  private sendPool = new CreditPool(0);
  private recvUnacked = 0;
  private readonly pongWaiters: (() => void)[] = [];
  private closedWaiters: ((info: { code: number; reason: string }) => void)[] =
    [];
  readonly source: TestSource;
  private readonly host: StreamHost<CoreStream>;

  constructor(source: TestSource) {
    this.source = source;
    // eslint-disable-next-line @typescript-eslint/no-this-alias
    const self = this;
    this.host = {
      sendFrame: (frame) => self.send(frame),
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

  get openStreams(): number {
    return this.streams.size;
  }

  /** 连 `nodeUrl`（`wss://…/t/v1?src=…`），握手到 ready 或被拒。 */
  async connect(
    nodeUrl: string,
    tunnelToken: string,
    options: { sourceId?: string } = {},
  ): Promise<{ ready?: Ready; reject?: Reject; closeCode?: number }> {
    const response = await SELF.fetch(nodeUrl.replace(/^ws/, "http"), {
      headers: { upgrade: "websocket", "cf-connecting-ip": "192.0.2.10" },
    });
    const ws = response.webSocket;
    if (!ws) return { closeCode: response.status };
    ws.accept();
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    const sourceId = options.sourceId ?? this.source.sourceId;
    const nonce = randomBytes(16).toString("base64url");
    const outcome = new Promise<{
      ready?: Ready;
      reject?: Reject;
      closeCode?: number;
    }>((resolve) => {
      ws.addEventListener("message", (event) => {
        if (typeof event.data !== "string") {
          this.receive(new Uint8Array(event.data as ArrayBuffer));
          return;
        }
        const message = JSON.parse(event.data) as unknown;
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
            this.source.privateKey,
          ).toString("base64url");
          ws.send(JSON.stringify({ type: "auth", sig }));
          return;
        }
        const ready = readySchema.safeParse(message);
        if (ready.success) {
          this.ready = ready.data;
          this.sendPool = new CreditPool(ready.data.limits.tunnelWindow);
          resolve({ ready: ready.data });
          return;
        }
        const rejected = rejectSchema.safeParse(message);
        if (rejected.success) this.reject = rejected.data;
      });
      ws.addEventListener("close", (event) => {
        this.closeInfo = { code: event.code, reason: event.reason };
        for (const stream of [...this.streams.values()]) stream.core.destroy(3);
        this.streams.clear();
        for (const waiter of this.closedWaiters.splice(0))
          waiter(this.closeInfo);
        resolve({ reject: this.reject ?? undefined, closeCode: event.code });
      });
    });
    ws.send(
      JSON.stringify({
        type: "hello",
        protocol: PROTOCOL_VERSION,
        sourceId,
        coreVersion: "0.2.0-test",
        capabilities: [],
        tunnelToken,
        nonce,
      }),
    );
    return outcome;
  }

  /** 等隧道被关。 */
  closed(): Promise<{ code: number; reason: string }> {
    if (this.closeInfo) return Promise.resolve(this.closeInfo);
    return new Promise((resolve) => this.closedWaiters.push(resolve));
  }

  /** 发一个 PING，等中继的 PONG。 */
  ping(): Promise<void> {
    return new Promise((resolve) => {
      this.pongWaiters.push(resolve);
      this.send({ type: "ping", ts: Date.now() });
    });
  }

  /** 源主动断开（不发 GOAWAY）。 */
  disconnect(): void {
    this.ws?.close(1000, "bye");
  }

  // ------------------------------------------------------------ 帧

  private send(frame: Frame): void {
    try {
      this.ws?.send(encodeFrame(frame));
    } catch {
      // 已经关了。
    }
  }

  private receive(bytes: Uint8Array): void {
    const frame = decodeFrame(bytes);
    switch (frame.type) {
      case "open":
        this.opens.push(frame.open);
        this.openStream(frame.streamId, frame.open);
        return;
      case "data": {
        const stream = this.streams.get(frame.streamId);
        if (!stream) this.host.consumed(frame.bytes.length);
        else stream.core.receiveData(frame.bytes);
        return;
      }
      case "end":
        this.streams.get(frame.streamId)?.core.receiveEnd();
        return;
      case "rst":
        this.streams.get(frame.streamId)?.core.receiveRst(frame.code);
        return;
      case "window":
        if (frame.streamId === 0) this.sendPool.add(frame.credit);
        else this.streams.get(frame.streamId)?.core.receiveWindow(frame.credit);
        return;
      case "ping":
        this.send({ type: "pong", ts: frame.ts });
        return;
      case "pong":
        this.pongWaiters.shift()?.();
        return;
      case "goaway":
        return;
    }
  }

  private openStream(streamId: number, open: OpenPayload): void {
    const stream = {
      streamId,
      open,
      reader: new RequestReader(),
      ws: null,
    } as unknown as CoreStream;
    const core = new StreamCore<CoreStream>(
      streamId,
      this.host,
      {
        streamWindow: this.ready?.limits.streamWindow ?? 262_144,
        maxDataChunk: 65_536,
      },
      stream,
      {
        push: (chunk) => {
          this.onBytes(stream, chunk);
          return true;
        },
        eof: () => {
          stream.remoteEnded = true;
        },
        reset: () => core.destroy(6),
      },
    );
    stream.core = core;
    stream.write = (bytes) =>
      new Promise<void>((resolve) => core.write(bytes, resolve));
    this.streams.set(streamId, stream);
    core.read();
  }

  private onBytes(stream: CoreStream, chunk: Uint8Array): void {
    if (stream.ws) {
      void this.onWsBytes(stream, chunk);
      return;
    }
    const request = stream.reader.feed(chunk);
    if (!request) return;
    this.requests.push(request);
    if (stream.open.kind === "ws") void this.upgrade(stream, request);
    else void this.respond(stream, request);
  }

  private async finish(stream: CoreStream): Promise<void> {
    if (stream.remoteEnded && stream.open.kind === "http")
      this.endedBeforeResponse += 1;
    await new Promise<void>((resolve) => stream.core.finish(resolve));
  }

  private async respond(
    stream: CoreStream,
    request: ParsedRequest,
  ): Promise<void> {
    const url = new URL(request.path, "http://core");
    const send = async (
      status: number,
      headers: Record<string, string>,
      body: Uint8Array,
      chunked = false,
    ) => {
      let head = `HTTP/1.1 ${status} X\r\n`;
      for (const [name, value] of Object.entries(headers))
        head += `${name}: ${value}\r\n`;
      head += chunked
        ? "transfer-encoding: chunked\r\n"
        : `content-length: ${body.length}\r\n`;
      head += "connection: close\r\n\r\n";
      await stream.write(encoder.encode(head));
      if (chunked) {
        for (let at = 0; at < body.length; at += 7) {
          const piece = body.subarray(at, at + 7);
          await stream.write(
            encoder.encode(`${piece.length.toString(16)}\r\n`),
          );
          await stream.write(piece);
          await stream.write(encoder.encode("\r\n"));
        }
        await stream.write(encoder.encode("0\r\n\r\n"));
      } else if (request.method !== "HEAD" && body.length > 0) {
        for (let at = 0; at < body.length; at += 65_536)
          await stream.write(body.subarray(at, at + 65_536));
      }
      await this.finish(stream);
    };
    if (url.pathname === "/echo-request")
      return send(
        200,
        { "content-type": "application/json", "set-cookie": "a=1" },
        encoder.encode(
          JSON.stringify({
            method: request.method,
            url: request.path,
            headers: request.headers,
          }),
        ),
      );
    if (url.pathname === "/echo" && request.method === "POST")
      return send(
        200,
        {
          "content-type":
            request.headers["content-type"] ?? "application/octet-stream",
        },
        request.body,
        request.headers["transfer-encoding"] === "chunked",
      );
    if (url.pathname === "/big") {
      const total = Number(url.searchParams.get("bytes") ?? 0);
      return send(
        200,
        { "content-type": "application/octet-stream" },
        Uint8Array.from({ length: total }, (_, index) => bigByte(index)),
      );
    }
    if (url.pathname === "/chunked")
      return send(
        200,
        { "content-type": "text/plain" },
        encoder.encode("chunked body here"),
        true,
      );
    if (url.pathname === "/media") {
      const range = /^bytes=(\d+)-(\d+)$/.exec(request.headers["range"] ?? "");
      if (range) {
        const from = Number(range[1]);
        const to = Math.min(Number(range[2]), MEDIA.length - 1);
        return send(
          206,
          {
            "content-type": "video/mp4",
            "content-range": `bytes ${from}-${to}/${MEDIA.length}`,
            "accept-ranges": "bytes",
          },
          MEDIA.subarray(from, to + 1),
        );
      }
      return send(
        200,
        { "content-type": "video/mp4", "accept-ranges": "bytes" },
        MEDIA,
      );
    }
    if (url.pathname === "/bad") {
      await stream.write(encoder.encode("HTTP/1.1 nope\r\n\r\n"));
      await this.finish(stream);
      return;
    }
    if (request.method !== "GET" && request.method !== "HEAD")
      return send(
        200,
        { "content-type": "application/json" },
        encoder.encode('{"ok":true}'),
      );
    return send(404, {}, new Uint8Array(0));
  }

  private async upgrade(
    stream: CoreStream,
    request: ParsedRequest,
  ): Promise<void> {
    const key = request.headers["sec-websocket-key"] ?? "";
    const accept = createHash("sha1")
      .update(`${key}${WS_GUID}`)
      .digest("base64");
    const protocol = (request.headers["sec-websocket-protocol"] ?? "")
      .split(",")
      .map((value) => value.trim())
      .find((value) => value.startsWith("armadra-ticket."));
    let head =
      "HTTP/1.1 101 Switching Protocols\r\nupgrade: websocket\r\nconnection: Upgrade\r\n" +
      `sec-websocket-accept: ${accept}\r\n`;
    if (protocol) head += `sec-websocket-protocol: ${protocol}\r\n`;
    stream.ws = new WsFrameDecoder({ masked: "required" });
    await stream.write(encoder.encode(`${head}\r\n`));
    if (request.body.length > 0) await this.onWsBytes(stream, request.body);
  }

  private async onWsBytes(
    stream: CoreStream,
    chunk: Uint8Array,
  ): Promise<void> {
    const events = (stream.ws as WsFrameDecoder).feed(chunk);
    for (const event of events) {
      if (event.type === "message") {
        const text = event.binary ? null : decoder.decode(event.data);
        if (text === "whoami") {
          await stream.write(
            encodeWsFrame({
              opcode: OPCODE.text,
              payload: encoder.encode(
                JSON.stringify({
                  url: stream.open.path,
                  protocol:
                    this.requests.at(-1)?.headers["sec-websocket-protocol"] ??
                    null,
                }),
              ),
            }),
          );
          continue;
        }
        if (text === "bye") {
          await stream.write(
            encodeWsFrame({
              opcode: OPCODE.close,
              payload: Uint8Array.of(0x0f, 0xa0, ...encoder.encode("bye")),
            }),
          );
          continue;
        }
        if (text === "ping-me") {
          await stream.write(
            encodeWsFrame({
              opcode: OPCODE.ping,
              payload: encoder.encode("p"),
            }),
          );
          continue;
        }
        await stream.write(
          encodeWsFrame({
            opcode: event.binary ? OPCODE.binary : OPCODE.text,
            payload: event.data,
          }),
        );
      } else if (event.type === "pong") {
        this.pongsFromRelay += 1;
      } else if (event.type === "close") {
        await this.finish(stream);
      }
    }
  }
}
