import { once } from "node:events";

import {
  readFixture,
  splitSequence,
} from "@armadra/platform-protocol/fixtures";
import { decodeFrame, LIMITS, RST } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { fakeTunnel, OPEN } from "../test/fake-transport.ts";
import { StreamResetError } from "./stream.ts";

const tick = () => new Promise((resolve) => setImmediate(resolve));

function dataFrames(
  frames: ReturnType<ReturnType<typeof fakeTunnel>["transport"]["take"]>,
) {
  return frames.filter((frame) => frame.type === "data") as {
    type: "data";
    streamId: number;
    bytes: Uint8Array;
  }[];
}

describe("写：切块与信用", () => {
  it("按 64 KiB 切块", async () => {
    const { tunnel, transport } = fakeTunnel();
    const stream = tunnel.openStream(OPEN);
    transport.take();
    stream.write(Buffer.alloc(LIMITS.maxDataChunk * 2 + 10, 1));
    await tick();
    expect(dataFrames(transport.take()).map((f) => f.bytes.length)).toEqual([
      65_536, 65_536, 10,
    ]);
  });

  it("流窗口耗尽时 DATA 停发，WINDOW 后继续", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    let done = false;
    stream.write(Buffer.alloc(2500, 2), () => (done = true));
    await tick();
    expect(dataFrames(transport.take()).map((f) => f.bytes.length)).toEqual([
      1000,
    ]);
    expect(done).toBe(false);
    feed({ type: "window", streamId: stream.streamId, credit: 1000 });
    await tick();
    expect(dataFrames(transport.take()).map((f) => f.bytes.length)).toEqual([
      1000,
    ]);
    feed({ type: "window", streamId: stream.streamId, credit: 1000 });
    await tick();
    expect(dataFrames(transport.take()).map((f) => f.bytes.length)).toEqual([
      500,
    ]);
    expect(done).toBe(true);
  });

  it("隧道级窗口由所有流共享，WINDOW(0) 叫醒等待的流", async () => {
    const { tunnel, transport, feed } = fakeTunnel({
      streamWindow: 10_000,
      tunnelWindow: 1500,
    });
    const a = tunnel.openStream(OPEN);
    const b = tunnel.openStream(OPEN);
    transport.take();
    a.write(Buffer.alloc(1000));
    b.write(Buffer.alloc(1000));
    await tick();
    const first = dataFrames(transport.take());
    expect(first.map((f) => [f.streamId, f.bytes.length])).toEqual([
      [a.streamId, 1000],
      [b.streamId, 500],
    ]);
    feed({ type: "window", streamId: 0, credit: 500 });
    await tick();
    expect(
      dataFrames(transport.take()).map((f) => [f.streamId, f.bytes.length]),
    ).toEqual([[b.streamId, 500]]);
  });
});

describe("读：消费后补信用", () => {
  it("交给消费者的字节累计到窗口一半时发 WINDOW，隧道级同理", async () => {
    const { tunnel, transport, feed } = fakeTunnel({
      streamWindow: 1000,
      tunnelWindow: 4000,
    });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    const received: Buffer[] = [];
    stream.on("data", (chunk: Buffer) => received.push(chunk));
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(400),
    });
    await tick();
    expect(transport.take()).toEqual([]);
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(400),
    });
    await tick();
    expect(transport.take()).toEqual([
      { type: "window", streamId: stream.streamId, credit: 800 },
    ]);
    for (let index = 0; index < 3; index += 1)
      feed({
        type: "data",
        streamId: stream.streamId,
        bytes: new Uint8Array(400),
      });
    await tick();
    // 第 4 次 400 字节后流级又补 800；第 5 次后隧道级累计 2000 = 4000/2，补 2000。
    expect(transport.take()).toEqual([
      { type: "window", streamId: stream.streamId, credit: 800 },
      { type: "window", streamId: 0, credit: 2000 },
    ]);
    expect(Buffer.concat(received).length).toBe(2000);
  });

  it("消费者暂停时不补信用（背压传到对端）", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    stream.pause();
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(1000),
    });
    await tick();
    // 没人读：Readable 没调 _read 之前字节留在队列里。
    expect(transport.take().filter((f) => f.type === "window")).toEqual([]);
  });

  it("对端超出流信用 → RST 5 + 关隧道 4400", () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    stream.on("error", () => {});
    transport.take();
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(1001),
    });
    expect(transport.take()).toEqual([
      { type: "rst", streamId: stream.streamId, code: RST.flowControl },
    ]);
    expect(transport.closedWith).toEqual({
      code: 4400,
      reason: "flow_control",
    });
    expect(tunnel.state).toBe("closed");
  });

  it("对端超出隧道信用 → RST 5 + 4400", () => {
    const { tunnel, transport, feed } = fakeTunnel({
      streamWindow: 10_000,
      tunnelWindow: 500,
    });
    const stream = tunnel.openStream(OPEN);
    stream.on("error", () => {});
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(501),
    });
    expect(transport.closedWith?.code).toBe(4400);
  });
});

describe("END / RST 双向", () => {
  it("收 END → 读端结束；本地 end() → 发 END；两端都结束后摘出流表", async () => {
    const { tunnel, transport, feed } = fakeTunnel();
    const stream = tunnel.openStream(OPEN);
    transport.take();
    stream.resume();
    const ended = once(stream, "end");
    feed({ type: "end", streamId: stream.streamId });
    await ended;
    expect(tunnel.streamCount).toBe(1);
    stream.end();
    await once(stream, "finish");
    expect(transport.take()).toEqual([
      { type: "end", streamId: stream.streamId },
    ]);
    expect(tunnel.streamCount).toBe(0);
  });

  it("收 RST → destroy 带码；本地 destroy() → 发 RST clientGone", async () => {
    const { tunnel, transport, feed } = fakeTunnel();
    const a = tunnel.openStream(OPEN);
    const errored = once(a, "error");
    feed({ type: "rst", streamId: a.streamId, code: RST.refused });
    const [error] = await errored;
    expect(error).toBeInstanceOf(StreamResetError);
    expect((error as StreamResetError).rstCode).toBe(RST.refused);
    const b = tunnel.openStream(OPEN);
    transport.take();
    b.destroy();
    expect(transport.take()).toEqual([
      { type: "rst", streamId: b.streamId, code: RST.clientGone },
    ]);
    expect(tunnel.streamCount).toBe(0);
  });

  it("销毁时丢掉的未读字节照样还隧道级信用", async () => {
    const { tunnel, transport, feed } = fakeTunnel({
      streamWindow: 10_000,
      tunnelWindow: 1000,
    });
    const stream = tunnel.openStream(OPEN);
    stream.pause();
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(600),
    });
    transport.take();
    stream.destroy();
    expect(transport.take()).toContainEqual({
      type: "window",
      streamId: 0,
      credit: 600,
    });
  });
});

describe("黄金夹具", () => {
  it("HTTP 往返序列：中继发出的帧与夹具逐字节相同，源的响应原样读出", async () => {
    const sequence = splitSequence(
      readFixture("tunnel/sequence-http-roundtrip.bin"),
    );
    const frames = sequence.map((bytes) => decodeFrame(bytes));
    const open = frames[0];
    if (open?.type !== "open") throw new Error("序列第一帧应是 OPEN");
    const { tunnel, transport } = fakeTunnel();
    const stream = tunnel.openStream(open.open);
    const response: Buffer[] = [];
    stream.on("data", (chunk: Buffer) => response.push(chunk));
    for (const frame of frames.slice(1, 3))
      if (frame.type === "data") stream.write(frame.bytes);
    stream.end();
    await once(stream, "finish");
    expect(transport.sent.map((bytes) => Buffer.from(bytes))).toEqual(
      sequence.slice(0, 4).map((bytes) => Buffer.from(bytes)),
    );
    const ended = once(stream, "end");
    for (const bytes of sequence.slice(4)) tunnel.receive(bytes);
    await ended;
    const last = frames[4];
    if (last?.type !== "data") throw new Error("第 5 帧应是 DATA");
    expect(Buffer.concat(response)).toEqual(Buffer.from(last.bytes));
    expect(tunnel.streamCount).toBe(0);
  });

  it("OPEN 帧与 open-http.bin 相同", () => {
    const fixture = decodeFrame(readFixture("tunnel/open-http.bin"));
    if (fixture.type !== "open") throw new Error("不是 OPEN");
    const { tunnel, transport } = fakeTunnel();
    tunnel.openStream(fixture.open);
    expect(Buffer.from(transport.sent[0] ?? [])).toEqual(
      Buffer.from(readFixture("tunnel/open-http.bin")),
    );
  });
});
