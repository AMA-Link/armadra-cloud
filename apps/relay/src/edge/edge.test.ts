import { PassThrough } from "node:stream";

import {
  readFixture,
  splitSequence,
} from "@armadra/platform-protocol/fixtures";
import { decodeFrame } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { hostAddressing, pathAddressing } from "./addressing.ts";
import {
  forwardRequestHeaders,
  forwardResponseHeaders,
  serializeRequestHead,
} from "./forward.ts";
import {
  closeFrame,
  FrameBoundaryTracker,
  WsDownstream,
} from "./ws-downstream.ts";

const ID = "0123456789abcdef0123456789abcdef";

describe("addressing", () => {
  it("personal：/s/<id> 前缀剥掉后转发，query 保留", () => {
    const a = pathAddressing();
    const at = (url: string) => a.resolve({ url, host: "relay" });
    expect(at(`/s/${ID}/api/x?y=1`)).toEqual({
      sourceId: ID,
      path: "/api/x?y=1",
    });
    expect(at(`/s/${ID}`)).toEqual({ sourceId: ID, path: "/" });
    expect(at(`/s/${ID}?t=1`)).toEqual({ sourceId: ID, path: "/?t=1" });
    expect(at(`/s/${ID}x/api`)).toBeNull();
    expect(at(`/s/${ID.toUpperCase()}/api`)).toBeNull();
    expect(at("/s/abc/api")).toBeNull();
    expect(at("/health")).toBeNull();
  });

  it("saas：<id>.src.<域>，路径原样", () => {
    const a = hostAddressing("relay.armadra.test");
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relay.armadra.test:443` }),
    ).toEqual({ sourceId: ID, path: "/api/x" });
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relay.armadra.test` }),
    ).not.toBeNull();
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relayXarmadra.test` }),
    ).toBeNull();
    expect(a.resolve({ url: "/api/x", host: "relay.armadra.test" })).toBeNull();
    expect(a.resolve({ url: "/api/x", host: undefined })).toBeNull();
  });
});

describe("forward", () => {
  it("请求头与黄金序列的请求行逐字节相同：剥令牌、加 X-Forwarded-*、保留 Host", () => {
    const frames = splitSequence(
      readFixture("tunnel/sequence-http-roundtrip.bin"),
    ).map((bytes) => decodeFrame(bytes));
    const open = frames[0];
    const head = frames[1];
    if (open?.type !== "open" || head?.type !== "data")
      throw new Error("夹具形状不对");
    const raw = [
      ...open.open.headers.flat(),
      "Armadra-Relay-Token",
      "secret.jwt",
      "X-Forwarded-For",
      "6.6.6.6",
      "Forwarded",
      "for=6.6.6.6",
    ];
    const headers = forwardRequestHeaders(raw, {
      remoteIp: "203.0.113.7",
      upgrade: { subprotocols: [] },
    });
    expect(
      serializeRequestHead(open.open.method, open.open.path, headers),
    ).toEqual(Buffer.from(head.bytes));
  });

  it("HTTP：去掉逐跳头与 Expect，末尾加 connection: close", () => {
    const headers = forwardRequestHeaders(
      [
        "Host",
        "h",
        "Connection",
        "keep-alive",
        "Transfer-Encoding",
        "chunked",
        "Expect",
        "100-continue",
        "Upgrade",
        "h2c",
        "X-Real-IP",
        "1.1.1.1",
        "Authorization",
        "Bearer core-token",
      ],
      { remoteIp: "10.0.0.1" },
    );
    expect(headers).toEqual([
      ["Host", "h"],
      ["Authorization", "Bearer core-token"],
      ["x-forwarded-for", "10.0.0.1"],
      ["x-forwarded-proto", "https"],
      ["connection", "close"],
    ]);
  });

  it("WS：保留 armadra-ticket.*，剥 armadra-relay.*；子协议空了就不带", () => {
    const raw = [
      "Host",
      "h",
      "Connection",
      "Upgrade",
      "Upgrade",
      "websocket",
      "Sec-WebSocket-Protocol",
      "armadra-ticket.t1, armadra-relay.jwt",
    ];
    expect(
      forwardRequestHeaders(raw, {
        remoteIp: "1",
        upgrade: { subprotocols: ["armadra-ticket.t1"] },
      }),
    ).toEqual([
      ["Host", "h"],
      ["Connection", "Upgrade"],
      ["Upgrade", "websocket"],
      ["Sec-WebSocket-Protocol", "armadra-ticket.t1"],
      ["x-forwarded-for", "1"],
      ["x-forwarded-proto", "https"],
    ]);
    expect(
      forwardRequestHeaders(raw, {
        remoteIp: "1",
        upgrade: { subprotocols: [] },
      }).map(([name]) => name.toLowerCase()),
    ).not.toContain("sec-websocket-protocol");
  });

  it("响应去掉逐跳头", () => {
    expect(
      forwardResponseHeaders([
        "Content-Type",
        "text/plain",
        "Connection",
        "close",
        "Transfer-Encoding",
        "chunked",
        "Set-Cookie",
        "a=1",
      ]),
    ).toEqual([
      ["Content-Type", "text/plain"],
      ["Set-Cookie", "a=1"],
    ]);
  });
});

describe("WS 帧边界", () => {
  const frame = (payload: number, masked = false) => {
    const header =
      payload < 126
        ? [0x82, payload | (masked ? 0x80 : 0)]
        : payload < 65_536
          ? [0x82, 126 | (masked ? 0x80 : 0), payload >> 8, payload & 0xff]
          : [
              0x82,
              127 | (masked ? 0x80 : 0),
              0,
              0,
              0,
              0,
              ...[24, 16, 8, 0].map((s) => (payload >>> s) & 0xff),
            ];
    return Buffer.concat([
      Buffer.from(header),
      Buffer.alloc(masked ? 4 : 0),
      Buffer.alloc(payload, 7),
    ]);
  };

  it("各种长度编码都能数出边界，跨块也行", () => {
    const tracker = new FrameBoundaryTracker();
    const bytes = Buffer.concat([
      frame(0),
      frame(5),
      frame(300),
      frame(70_000),
      frame(3, true),
    ]);
    const ends: number[] = [];
    let offset = 0;
    for (let at = 0; at < bytes.length; at += 7) {
      const piece = bytes.subarray(at, at + 7);
      for (const end of tracker.feed(piece)) ends.push(offset + end);
      offset += piece.length;
    }
    expect(ends).toEqual([
      2,
      2 + 7,
      2 + 7 + 304,
      2 + 7 + 304 + 70_010,
      bytes.length,
    ]);
    expect(tracker.atBoundary).toBe(true);
    tracker.feed(frame(10).subarray(0, 4));
    expect(tracker.atBoundary).toBe(false);
  });

  it("ping 在帧边界插入；源下线在边界上发 4404 关闭帧", async () => {
    const socket = new PassThrough();
    const written: Buffer[] = [];
    socket.on("data", (chunk: Buffer) => written.push(chunk));
    const down = new WsDownstream(socket, {
      pingMs: 60_000,
      idleMs: 120_000,
      offlineResponse: Buffer.from("503"),
    });
    const write = (chunk: Buffer) =>
      new Promise<void>((resolve) => down.write(chunk, () => resolve()));
    await write(
      Buffer.from(
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n",
      ),
    );
    expect(down.upgraded).toBe(true);
    const big = frame(10);
    await write(big.subarray(0, 5));
    down.inject(Buffer.from([0x89, 0x00]));
    await write(big.subarray(5));
    const all = Buffer.concat(written);
    expect(all.subarray(all.length - 2)).toEqual(Buffer.from([0x89, 0x00]));
    expect(all.subarray(all.length - 2 - big.length, all.length - 2)).toEqual(
      big,
    );
    down.sourceGone();
    await new Promise((resolve) => setImmediate(resolve));
    expect(
      Buffer.concat(written).subarray(
        -closeFrame(4404, "source_offline").length,
      ),
    ).toEqual(closeFrame(4404, "source_offline"));
  });

  it("还没收到升级响应时源下线：答 503", async () => {
    const socket = new PassThrough();
    const written: Buffer[] = [];
    socket.on("data", (chunk: Buffer) => written.push(chunk));
    const down = new WsDownstream(socket, {
      pingMs: 60_000,
      idleMs: 120_000,
      offlineResponse: Buffer.from("HTTP/1.1 503"),
    });
    down.sourceGone();
    await new Promise((resolve) => setImmediate(resolve));
    expect(Buffer.concat(written).toString()).toBe("HTTP/1.1 503");
  });

  it("源答的不是 101：之后按 HTTP 字节透传，不插 ping", async () => {
    const socket = new PassThrough();
    const written: Buffer[] = [];
    socket.on("data", (chunk: Buffer) => written.push(chunk));
    const down = new WsDownstream(socket, {
      pingMs: 60_000,
      idleMs: 120_000,
      offlineResponse: Buffer.alloc(0),
    });
    await new Promise<void>((resolve) =>
      down.write(
        Buffer.from("HTTP/1.1 401 Unauthorized\r\ncontent-length: 2\r\n\r\n{}"),
        () => resolve(),
      ),
    );
    down.inject(Buffer.from([0x89, 0x00]));
    expect(down.upgraded).toBe(false);
    expect(Buffer.concat(written).toString()).toBe(
      "HTTP/1.1 401 Unauthorized\r\ncontent-length: 2\r\n\r\n{}",
    );
  });
});
