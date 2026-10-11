import { describe, expect, it } from "vitest";

import {
  listFixtures,
  readFixture,
  splitSequence,
} from "../src/fixtures/index.ts";
import { CLOSE, FRAME_TYPE, RST } from "../src/tunnel/codes.ts";
import {
  decodeFrame,
  encodeFrame,
  FrameError,
  type Frame,
} from "../src/tunnel/frames.ts";
import { LIMITS } from "../src/tunnel/limits.ts";

const open: Frame = {
  type: "open",
  streamId: 7,
  open: {
    kind: "http",
    method: "GET",
    path: "/health?x=1",
    headers: [["host", "a"]],
    clientOrigin: null,
    remoteIp: "198.51.100.1",
    relayNode: "n1",
  },
};

const ROUND_TRIP: Frame[] = [
  open,
  { type: "data", streamId: 7, bytes: new Uint8Array([0, 1, 2, 255]) },
  { type: "data", streamId: 7, bytes: new Uint8Array(0) },
  { type: "end", streamId: 7 },
  { type: "rst", streamId: 7, code: RST.flowControl },
  { type: "window", streamId: 7, credit: 1 },
  { type: "window", streamId: 0, credit: 0xffff_ffff },
  { type: "ping", ts: 1_760_000_000_000.5 },
  { type: "pong", ts: 0 },
  { type: "goaway", reason: "rebalance", graceMs: 0 },
];

function frameError(fn: () => unknown): FrameError {
  try {
    fn();
  } catch (error) {
    if (error instanceof FrameError) return error;
    throw error;
  }
  throw new Error("没有抛 FrameError");
}

function raw(
  type: number,
  streamId: number,
  payload: number[] | Uint8Array = [],
): Uint8Array {
  const bytes = new Uint8Array(5 + payload.length);
  const view = new DataView(bytes.buffer);
  view.setUint8(0, type);
  view.setUint32(1, streamId, false);
  bytes.set(payload, 5);
  return bytes;
}

describe("往返", () => {
  it.each(ROUND_TRIP.map((frame) => [frame.type, frame] as const))(
    "%s",
    (_, frame) => {
      expect(decodeFrame(encodeFrame(frame))).toEqual(frame);
    },
  );

  it("帧头：type u8 + streamId u32 BE", () => {
    const bytes = encodeFrame({ type: "end", streamId: 0x01020304 });
    expect([...bytes]).toEqual([FRAME_TYPE.end, 1, 2, 3, 4]);
  });

  it("从更大的缓冲区的切片里解码", () => {
    const encoded = encodeFrame({ type: "rst", streamId: 9, code: 2 });
    const big = new Uint8Array(encoded.length + 10);
    big.set(encoded, 3);
    expect(decodeFrame(big.subarray(3, 3 + encoded.length))).toEqual({
      type: "rst",
      streamId: 9,
      code: 2,
    });
  });

  it("OPEN 丢掉未知字段（非 strict，向前兼容）", () => {
    const json = new TextEncoder().encode(
      JSON.stringify({ ...open.open, extra: 1 }),
    );
    const decoded = decodeFrame(raw(FRAME_TYPE.open, 7, json));
    expect(decoded).toEqual(open);
  });
});

describe("黄金字节", () => {
  const expected: Record<string, Frame> = {
    "tunnel/end.bin": { type: "end", streamId: 1 },
    "tunnel/rst-3.bin": { type: "rst", streamId: 1, code: RST.sourceGone },
    "tunnel/window-stream.bin": {
      type: "window",
      streamId: 1,
      credit: 131_072,
    },
    "tunnel/window-tunnel.bin": {
      type: "window",
      streamId: 0,
      credit: 4_194_304,
    },
    "tunnel/ping.bin": { type: "ping", ts: 1_760_000_000_000 },
    "tunnel/pong.bin": { type: "pong", ts: 1_760_000_000_000 },
    "tunnel/goaway.bin": {
      type: "goaway",
      reason: "shutdown",
      graceMs: LIMITS.goawayGraceMs,
    },
    "tunnel/open-e2e.bin": {
      type: "open",
      streamId: 5,
      open: {
        kind: "e2e",
        method: "GET",
        path: "/_relay/e2e",
        headers: [],
        clientOrigin: "https://app.armadra.test",
        remoteIp: "203.0.113.7",
        relayNode: "relay-1",
      },
    },
  };

  it.each(Object.entries(expected))(
    "%s 解码与重编码逐字节相等",
    (name, frame) => {
      const bytes = readFixture(name);
      expect(decodeFrame(bytes)).toEqual(frame);
      expect(encodeFrame(frame)).toEqual(bytes);
    },
  );

  it("每个单帧夹具解码后重编码逐字节相等", () => {
    const names = listFixtures("tunnel").filter(
      (name) => name.endsWith(".bin") && !name.includes("sequence"),
    );
    expect(names.length).toBe(11);
    for (const name of names) {
      const bytes = readFixture(name);
      expect(encodeFrame(decodeFrame(bytes)), name).toEqual(bytes);
    }
  });

  it("ping.bin 的字节布局", () => {
    expect([...readFixture("tunnel/ping.bin")]).toEqual([
      6, 0, 0, 0, 0, 0x42, 0x79, 0x9c, 0x82, 0xcc, 0x00, 0x00, 0x00,
    ]);
  });

  it("data-64k.bin 正好是 maxDataChunk", () => {
    const frame = decodeFrame(readFixture("tunnel/data-64k.bin"));
    expect(frame.type === "data" && frame.bytes.length).toBe(
      LIMITS.maxDataChunk,
    );
  });

  it("HTTP 往返序列：OPEN、DATA×2、END、DATA、END", () => {
    const frames = splitSequence(
      readFixture("tunnel/sequence-http-roundtrip.bin"),
    ).map(decodeFrame);
    expect(frames.map((frame) => frame.type)).toEqual([
      "open",
      "data",
      "data",
      "end",
      "data",
      "end",
    ]);
    const decoder = new TextDecoder();
    const [, head, body, , response] = frames;
    expect(head?.type === "data" && decoder.decode(head.bytes)).toMatch(
      /^POST \/api\/workspaces\?limit=10 HTTP\/1\.1\r\n/,
    );
    expect(body?.type === "data" && decoder.decode(body.bytes)).toBe(
      '{"name":"a"}\n',
    );
    expect(response?.type === "data" && decoder.decode(response.bytes)).toMatch(
      /^HTTP\/1\.1 200 OK/,
    );
  });
});

describe("拒绝", () => {
  it("短于帧头 → bad_frame", () => {
    expect(frameError(() => decodeFrame(new Uint8Array(4))).code).toBe(
      "bad_frame",
    );
  });

  it("未知类型 → bad_frame", () => {
    expect(frameError(() => decodeFrame(raw(9, 1))).code).toBe("bad_frame");
    expect(frameError(() => decodeFrame(raw(0, 1))).code).toBe("bad_frame");
  });

  it("streamId 0 上的流级帧 → bad_frame", () => {
    for (const type of [
      FRAME_TYPE.open,
      FRAME_TYPE.data,
      FRAME_TYPE.end,
      FRAME_TYPE.rst,
    ]) {
      expect(
        frameError(() =>
          decodeFrame(raw(type, 0, type === FRAME_TYPE.rst ? [0, 1] : [])),
        ).code,
      ).toBe("bad_frame");
    }
    expect(
      frameError(() =>
        encodeFrame({ type: "data", streamId: 0, bytes: new Uint8Array(1) }),
      ).code,
    ).toBe("bad_frame");
  });

  it("隧道级帧带非零 streamId → bad_frame", () => {
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.ping, 1, new Uint8Array(8))))
        .code,
    ).toBe("bad_frame");
  });

  it("载荷长度不对 → bad_frame", () => {
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.end, 1, [0]))).code,
    ).toBe("bad_frame");
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.rst, 1, [0]))).code,
    ).toBe("bad_frame");
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.window, 1, [0, 0, 1]))).code,
    ).toBe("bad_frame");
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.ping, 0, new Uint8Array(7))))
        .code,
    ).toBe("bad_frame");
  });

  it("WINDOW 信用为 0 → bad_frame", () => {
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.window, 1, [0, 0, 0, 0])))
        .code,
    ).toBe("bad_frame");
    expect(
      frameError(() => encodeFrame({ type: "window", streamId: 1, credit: 0 }))
        .code,
    ).toBe("bad_frame");
  });

  it("超过 maxDataChunk 或 maxFrameBytes → frame_too_large", () => {
    const big = new Uint8Array(LIMITS.maxDataChunk + 1);
    expect(
      frameError(() => encodeFrame({ type: "data", streamId: 1, bytes: big }))
        .code,
    ).toBe("frame_too_large");
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.data, 1, big))).code,
    ).toBe("frame_too_large");
    expect(
      frameError(() => decodeFrame(new Uint8Array(LIMITS.maxFrameBytes + 1)))
        .code,
    ).toBe("frame_too_large");
  });

  it("OPEN 不是 JSON、缺字段、headers 超限 → bad_open", () => {
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.open, 1, [0x7b]))).code,
    ).toBe("bad_open");
    const missing = new TextEncoder().encode(JSON.stringify({ kind: "http" }));
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.open, 1, missing))).code,
    ).toBe("bad_open");
    const headers: [string, string][] = [
      ["x", "y".repeat(LIMITS.openHeadersMaxBytes)],
    ];
    expect(
      frameError(() =>
        encodeFrame({ ...open, open: { ...open.open, headers } }),
      ).code,
    ).toBe("bad_open");
  });

  it("协议 1.3：e2e 的 OPEN 只能是 GET /_relay/e2e、不带头 → 否则 bad_open", () => {
    const e2e: Frame = {
      type: "open",
      streamId: 9,
      open: { ...open.open, kind: "e2e", path: "/_relay/e2e", headers: [] },
    };
    expect(decodeFrame(encodeFrame(e2e))).toEqual(e2e);
    for (const change of [
      { headers: [["host", "a"]] as [string, string][] },
      { method: "POST" },
      { path: "/api/x" },
    ])
      expect(
        frameError(() =>
          encodeFrame({ ...e2e, open: { ...e2e.open, ...change } }),
        ).code,
        JSON.stringify(change),
      ).toBe("bad_open");
  });

  it("GOAWAY 原因未知 → bad_frame；PING 的 ts 不是有限数 → bad_frame", () => {
    const goaway = new TextEncoder().encode(
      JSON.stringify({ reason: "bored", graceMs: 1 }),
    );
    expect(
      frameError(() => decodeFrame(raw(FRAME_TYPE.goaway, 0, goaway))).code,
    ).toBe("bad_frame");
    expect(
      frameError(() => encodeFrame({ type: "ping", ts: Number.NaN })).code,
    ).toBe("bad_frame");
  });

  it("RST 码超出 u16、streamId 超出 u32 → bad_frame", () => {
    expect(
      frameError(() => encodeFrame({ type: "rst", streamId: 1, code: 70_000 }))
        .code,
    ).toBe("bad_frame");
    expect(
      frameError(() => encodeFrame({ type: "end", streamId: 2 ** 32 })).code,
    ).toBe("bad_frame");
  });
});

it("关闭码与 RST 码是常量表", () => {
  expect(CLOSE.tunnelReplaced).toBe(4491);
  expect(CLOSE.sourceOffline).toBe(4404);
  expect(RST.timeout).toBe(7);
});
