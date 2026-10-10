/** 隧道流的 Web Streams 版（运行时无关）：信用背压、补窗口、END / RST 与流表。 */
import { RST } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import { fakeTunnel, OPEN } from "../../test/fake-transport.ts";
import { StreamResetError } from "./stream.ts";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

function dataLengths(
  frames: ReturnType<ReturnType<typeof fakeTunnel>["transport"]["take"]>,
): number[] {
  return frames.flatMap((frame) =>
    frame.type === "data" ? [frame.bytes.length] : [],
  );
}

describe("写", () => {
  it("按 maxDataChunk 切块，流窗口耗尽时写挂起、WINDOW 后继续", async () => {
    const { tunnel, transport, feed } = fakeTunnel({
      streamWindow: 1000,
      maxDataChunk: 400,
    });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    const writer = stream.writable.getWriter();
    let done = false;
    void writer.write(new Uint8Array(1500)).then(() => (done = true));
    await tick();
    expect(dataLengths(transport.take())).toEqual([400, 400, 200]);
    expect(done).toBe(false);
    feed({ type: "window", streamId: stream.streamId, credit: 1000 });
    await tick();
    expect(dataLengths(transport.take())).toEqual([400, 100]);
    expect(done).toBe(true);
    expect(stream.bytesOut).toBe(1500);
  });

  it("close 发 END；对端也 END 后从流表摘掉", async () => {
    const { tunnel, transport, feed } = fakeTunnel();
    const stream = tunnel.openStream(OPEN);
    transport.take();
    await stream.writable.getWriter().close();
    expect(transport.take()).toEqual([
      { type: "end", streamId: stream.streamId },
    ]);
    expect(tunnel.streamCount).toBe(1);
    feed({ type: "end", streamId: stream.streamId });
    expect(tunnel.streamCount).toBe(0);
  });
});

describe("读", () => {
  it("消费者读走一半窗口时补 WINDOW；EOF 后读完", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    const reader = stream.readable.getReader();
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(300),
    });
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(300),
    });
    const first = await reader.read();
    const second = await reader.read();
    expect([first.value?.length, second.value?.length]).toEqual([300, 300]);
    expect(transport.take()).toContainEqual({
      type: "window",
      streamId: stream.streamId,
      credit: 600,
    });
    feed({ type: "end", streamId: stream.streamId });
    expect((await reader.read()).done).toBe(true);
    expect(stream.bytesIn).toBe(600);
  });

  it("消费者不读时不交出（队列背压），也不补窗口", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    for (let index = 0; index < 3; index += 1)
      feed({
        type: "data",
        streamId: stream.streamId,
        bytes: new Uint8Array(300),
      });
    await tick();
    // ReadableStream 缺省高水位 1 块：交出一块后停，剩下两块留在内核队列。
    expect(transport.take()).toEqual([]);
  });

  it("超发：答 RST flowControl 并关隧道", () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 100 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    feed({
      type: "data",
      streamId: stream.streamId,
      bytes: new Uint8Array(101),
    });
    expect(transport.take()).toContainEqual({
      type: "rst",
      streamId: stream.streamId,
      code: RST.flowControl,
    });
    expect(tunnel.state).toBe("closed");
  });
});

describe("重置", () => {
  it("对端 RST：读与在途的写都以 StreamResetError 结束，不回 RST", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ streamWindow: 10 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    const writer = stream.writable.getWriter();
    const pending = writer.write(new Uint8Array(100));
    const reader = stream.readable.getReader();
    const read = reader.read();
    feed({ type: "rst", streamId: stream.streamId, code: RST.refused });
    await expect(pending).rejects.toBeInstanceOf(StreamResetError);
    await expect(read).rejects.toMatchObject({ rstCode: RST.refused });
    expect(stream.destroyed).toBe(true);
    expect(transport.take().filter((frame) => frame.type === "rst")).toEqual(
      [],
    );
    expect(tunnel.streamCount).toBe(0);
  });

  it("本地 cancel：发 RST clientGone，队列里的字节还回隧道信用", async () => {
    const { tunnel, transport, feed } = fakeTunnel({ tunnelWindow: 1000 });
    const stream = tunnel.openStream(OPEN);
    transport.take();
    for (let index = 0; index < 3; index += 1)
      feed({
        type: "data",
        streamId: stream.streamId,
        bytes: new Uint8Array(200),
      });
    await stream.readable.cancel();
    expect(transport.take()).toEqual([
      { type: "rst", streamId: stream.streamId, code: RST.clientGone },
      { type: "window", streamId: 0, credit: 600 },
    ]);
    expect(tunnel.streamCount).toBe(0);
  });

  it("隧道关闭：流以 sourceGone 结束", async () => {
    const { tunnel } = fakeTunnel();
    const stream = tunnel.openStream(OPEN);
    const read = stream.readable.getReader().read();
    tunnel.transportClosed(1006);
    await expect(read).rejects.toMatchObject({ rstCode: RST.sourceGone });
  });
});
