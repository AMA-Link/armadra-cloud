import { FIXED_NOW_MS, readFixture } from "@armadra/platform-protocol/fixtures";
import { CLOSE, RST } from "@armadra/platform-protocol/tunnel";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeTunnel, OPEN } from "../test/fake-transport.ts";
import {
  TunnelUnavailableError,
  type TunnelCloseInfo,
} from "@armadra/relay-kernel/tunnel/tunnel";

afterEach(() => {
  vi.useRealTimers();
});

describe("心跳", () => {
  it("每 heartbeatMs 发 PING；连续两次未答关闭隧道", () => {
    vi.useFakeTimers();
    const closes: TunnelCloseInfo[] = [];
    let misses = 0;
    const { tunnel, transport } = fakeTunnel(
      { heartbeatMs: 1000 },
      {
        onClose: (info) => closes.push(info),
        onHeartbeatMiss: () => (misses += 1),
      },
    );
    tunnel.start();
    vi.advanceTimersByTime(1000);
    vi.advanceTimersByTime(1000);
    expect(transport.take().map((f) => f.type)).toEqual(["ping", "ping"]);
    expect(tunnel.state).toBe("open");
    vi.advanceTimersByTime(1000);
    expect(transport.terminated).toBe(true);
    expect(tunnel.state).toBe("closed");
    expect(misses).toBe(1);
    expect(closes).toEqual([
      { code: CLOSE.goingAway, reason: "heartbeat_timeout", replaced: false },
    ]);
  });

  it("PONG 清零未答计数并回调（路由续期）", () => {
    vi.useFakeTimers();
    let pongs = 0;
    const { tunnel, transport, feed } = fakeTunnel(
      { heartbeatMs: 1000 },
      { onPong: () => (pongs += 1) },
    );
    tunnel.start();
    for (let index = 0; index < 5; index += 1) {
      vi.advanceTimersByTime(1000);
      feed({ type: "pong", ts: 1 });
    }
    expect(pongs).toBe(5);
    expect(tunnel.state).toBe("open");
    expect(transport.terminated).toBe(false);
    tunnel.close(CLOSE.normal, "done");
  });

  it("源发来 PING 立即答 PONG（原样回 ts）", () => {
    const { tunnel, transport } = fakeTunnel();
    tunnel.receive(readFixture("tunnel/ping.bin"));
    expect(Buffer.from(transport.sent[0] ?? [])).toEqual(
      Buffer.from(readFixture("tunnel/pong.bin")),
    );
    expect(transport.take()).toEqual([{ type: "pong", ts: FIXED_NOW_MS }]);
  });
});

describe("GOAWAY", () => {
  it("发 GOAWAY 后拒绝新流，旧流自然结束后以 4492 关闭", async () => {
    const closes: TunnelCloseInfo[] = [];
    const { tunnel, transport, feed } = fakeTunnel(
      {},
      { onClose: (info) => closes.push(info) },
    );
    const stream = tunnel.openStream(OPEN);
    transport.take();
    tunnel.goaway("shutdown", 30_000);
    expect(transport.take()).toEqual([
      { type: "goaway", reason: "shutdown", graceMs: 30_000 },
    ]);
    expect(() => tunnel.openStream(OPEN)).toThrow(TunnelUnavailableError);
    expect(tunnel.state).toBe("draining");
    stream.resume();
    stream.end();
    feed({ type: "end", streamId: stream.streamId });
    await new Promise((resolve) => setImmediate(resolve));
    expect(tunnel.state).toBe("closed");
    expect(transport.closedWith).toEqual({
      code: CLOSE.tunnelGoaway,
      reason: "goaway",
    });
    expect(closes[0]?.replaced).toBe(false);
  });

  it("grace 到了强关，剩下的流以 sourceGone 结束", () => {
    vi.useFakeTimers();
    const { tunnel, transport } = fakeTunnel();
    const stream = tunnel.openStream(OPEN);
    const errors: unknown[] = [];
    stream.on("error", (error) => errors.push(error));
    tunnel.goaway("rebalance", 500);
    vi.advanceTimersByTime(499);
    expect(tunnel.state).toBe("draining");
    vi.advanceTimersByTime(1);
    expect(tunnel.state).toBe("closed");
    expect(transport.closedWith?.code).toBe(CLOSE.tunnelGoaway);
    expect(stream.destroyed).toBe(true);
  });

  it("同源替换：GOAWAY replaced，关闭码 4491，标记 replaced", () => {
    const closes: TunnelCloseInfo[] = [];
    const { tunnel, transport } = fakeTunnel(
      {},
      { onClose: (info) => closes.push(info) },
    );
    tunnel.goaway("replaced");
    expect(transport.take()[0]).toMatchObject({
      type: "goaway",
      reason: "replaced",
    });
    expect(transport.closedWith?.code).toBe(CLOSE.tunnelReplaced);
    expect(closes[0]?.replaced).toBe(true);
  });

  it("源发来 GOAWAY：不再开新流并回调", () => {
    let reason = "";
    const { tunnel } = fakeTunnel({}, { onPeerGoaway: (r) => (reason = r) });
    const stream = tunnel.openStream(OPEN);
    tunnel.receive(readFixture("tunnel/goaway.bin"));
    expect(reason).toBe("shutdown");
    expect(tunnel.accepting).toBe(false);
    expect(tunnel.state).toBe("draining");
    stream.destroy();
    expect(tunnel.state).toBe("closed");
  });
});

describe("帧与流表", () => {
  it("坏帧关 4400；超大帧关 4413", () => {
    const a = fakeTunnel();
    a.tunnel.receive(new Uint8Array([99, 0, 0, 0, 0]));
    expect(a.transport.closedWith?.code).toBe(CLOSE.badFrame);
    const b = fakeTunnel();
    b.tunnel.receive(new Uint8Array(1_048_577));
    expect(b.transport.closedWith?.code).toBe(CLOSE.frameTooLarge);
  });

  it("源发 OPEN 是协议错误：答 RST protocol", () => {
    const { tunnel, transport } = fakeTunnel();
    tunnel.receive(readFixture("tunnel/open-http.bin"));
    expect(transport.take()).toEqual([
      { type: "rst", streamId: 1, code: RST.protocol },
    ]);
  });

  it("超过 maxStreams 拒绝开流", () => {
    const { tunnel } = fakeTunnel({ maxStreams: 2 });
    tunnel.openStream(OPEN);
    tunnel.openStream(OPEN);
    expect(() => tunnel.openStream(OPEN)).toThrow("max_streams");
  });

  it("未知流的 DATA 丢弃但还隧道信用；关隧道时流以 sourceGone 结束", () => {
    const { tunnel, transport } = fakeTunnel({ tunnelWindow: 100_000 });
    tunnel.receive(readFixture("tunnel/data-64k.bin"));
    expect(transport.take()).toEqual([
      { type: "window", streamId: 0, credit: 65_536 },
    ]);
    const stream = tunnel.openStream(OPEN);
    let code = -1;
    stream.on("error", (error: { rstCode: number }) => (code = error.rstCode));
    tunnel.transportClosed(1006);
    expect(stream.destroyed).toBe(true);
    return new Promise<void>((resolve) =>
      setImmediate(() => {
        expect(code).toBe(RST.sourceGone);
        resolve();
      }),
    );
  });
});
