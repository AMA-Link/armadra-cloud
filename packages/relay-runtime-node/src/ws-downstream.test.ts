import { PassThrough } from "node:stream";

import { closeFrame } from "@armadra/platform-protocol/ws-frames";
import { describe, expect, it } from "vitest";

import { WsDownstream } from "./ws-downstream.ts";

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
    ).toEqual(Buffer.from(closeFrame(4404, "source_offline")));
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
