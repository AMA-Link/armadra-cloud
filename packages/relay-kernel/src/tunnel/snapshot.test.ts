/** 隧道与流的快照 / 还原（Workers 的 DO 休眠后唤醒用）：信用、流号与结束标记原样接上。 */
import { encodeFrame, RST } from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import {
  FakeTransport,
  fakeTunnel,
  OPEN,
  TEST_LIMITS,
} from "../../test/fake-transport.ts";
import {
  StreamCore,
  type StreamEndpoint,
  type StreamFactory,
  type StreamHost,
  type StreamOptions,
} from "./stream.ts";
import { Tunnel } from "./tunnel.ts";

/** 最小的流：交出来的字节记下，`push` 总答 true。 */
class Recorder implements StreamEndpoint {
  readonly streamId: number;
  readonly core: StreamCore<Recorder>;
  readonly received: number[] = [];
  constructor(
    streamId: number,
    host: StreamHost<Recorder>,
    options: StreamOptions,
  ) {
    this.streamId = streamId;
    this.core = new StreamCore(streamId, host, options, this, {
      push: (chunk) => {
        this.received.push(chunk.length);
        return true;
      },
      eof: () => {},
      reset: () => this.core.destroy(RST.clientGone),
    });
    this.core.read();
  }
  receiveData(bytes: Uint8Array): boolean {
    return this.core.receiveData(bytes);
  }
  receiveEnd(): void {
    this.core.receiveEnd();
  }
  receiveRst(code: number): void {
    this.core.receiveRst(code);
  }
  receiveWindow(credit: number): void {
    this.core.receiveWindow(credit);
  }
}

const recorders: StreamFactory<Recorder> = (id, host, options) =>
  new Recorder(id, host, options);

const LIMITS = { ...TEST_LIMITS, streamWindow: 1000, tunnelWindow: 4000 };

describe("快照与还原", () => {
  it("信用、流号与流表原样接上；还原后补窗口的阈值不变", () => {
    const { tunnel, transport, feed } = fakeTunnel<Recorder>(
      LIMITS,
      {},
      recorders,
    );
    const kept = tunnel.openStream(OPEN);
    const dropped = tunnel.openStream(OPEN);
    feed({ type: "data", streamId: kept.streamId, bytes: new Uint8Array(300) });
    kept.core.write(new Uint8Array(200), () => {});
    transport.take();

    const snapshot = tunnel.snapshot();
    expect(snapshot).toMatchObject({
      nextStreamId: 3,
      sendCredit: 3800,
      recvRemaining: 3700,
      recvUnacked: 300,
      streams: [1, 2],
    });
    const state = kept.core.snapshot();
    expect(state).toMatchObject({
      sendCredit: 800,
      recvRemaining: 700,
      unacked: 300,
    });

    const next = new FakeTransport();
    const restored = Tunnel.restore<Recorder>(
      snapshot as NonNullable<typeof snapshot>,
      next,
      LIMITS,
      {},
      recorders,
      (streamId, host, options) => {
        if (streamId !== kept.streamId) return null;
        const stream = new Recorder(streamId, host, options);
        stream.core.restore(state);
        return stream;
      },
    );
    // 没还原的那条：发 RST clientGone 让对端释放。
    expect(next.take()).toEqual([
      { type: "rst", streamId: dropped.streamId, code: RST.clientGone },
    ]);
    expect(restored.streamCount).toBe(1);
    // 再收 200 字节：流已交 500 ≥ 半窗 → WINDOW(1, 500)；隧道已交 500 < 2000 不补。
    restored.receive(
      encodeFrame({
        type: "data",
        streamId: kept.streamId,
        bytes: new Uint8Array(200),
      }),
    );
    expect(next.take()).toEqual([
      { type: "window", streamId: kept.streamId, credit: 500 },
    ]);
    // 下一条流号接着来。
    expect(restored.openStream(OPEN).streamId).toBe(3);
  });

  it("排空或关闭的隧道不存；流还有在途的写时不能存", () => {
    const { tunnel } = fakeTunnel<Recorder>(LIMITS, {}, recorders);
    const stream = tunnel.openStream(OPEN);
    stream.core.write(new Uint8Array(1500), () => {});
    expect(stream.core.idle).toBe(false);
    expect(() => stream.core.snapshot()).toThrow();
    tunnel.goaway("shutdown", 60_000);
    expect(tunnel.snapshot()).toBeNull();
    tunnel.close(4492, "test");
  });
});
