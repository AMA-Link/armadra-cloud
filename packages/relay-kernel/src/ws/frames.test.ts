import {
  readFixture,
  readJsonFixture,
} from "@armadra/platform-protocol/fixtures";
import { describe, expect, it } from "vitest";

import { fixtureBytes, joinBytes, type FixtureBytes } from "../../test/wire.ts";
import {
  closeFrame,
  closePayload,
  encodeWsFrame,
  FrameBoundaryTracker,
  OPCODE,
  PING_FRAME,
  WsFrameDecoder,
  WsFrameError,
  type WsEvent,
} from "./frames.ts";

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
});

interface WsCase {
  name: string;
  file: string;
  from: "client" | "server";
  events?: (
    | { type: "message"; binary: boolean; data: FixtureBytes }
    | { type: "close"; code?: number; reason: string }
    | { type: "ping" | "pong"; data: FixtureBytes }
  )[];
  encode?: {
    opcode: number;
    fin: boolean;
    payload: FixtureBytes;
    mask?: string;
  }[];
  error?: string;
}

const wsCases = readJsonFixture<{ frames: WsCase[] }>("ws/cases.json").frames;

function decodeAll(bytes: Uint8Array, from: WsCase["from"], step: number) {
  const decoder = new WsFrameDecoder({
    masked: from === "client" ? "required" : "forbidden",
  });
  const events: WsEvent[] = [];
  const size = step === 0 ? bytes.length : step;
  for (let at = 0; at < bytes.length; at += size)
    events.push(...decoder.feed(bytes.subarray(at, at + size)));
  return events;
}

function expectedEvents(entry: WsCase): WsEvent[] {
  return (entry.events ?? []).map((event) =>
    event.type === "close"
      ? {
          type: "close",
          ...(event.code === undefined ? {} : { code: event.code }),
          reason: event.reason,
        }
      : event.type === "message"
        ? {
            type: "message",
            binary: event.binary,
            data: fixtureBytes(event.data),
          }
        : { type: event.type, data: fixtureBytes(event.data) },
  );
}

describe("ws/frames：黄金夹具", () => {
  it("夹具齐全（含 RFC 6455 §5.7 示例）", () => {
    expect(
      wsCases.filter((entry) => entry.name.startsWith("rfc-")).length,
    ).toBe(7);
    expect(wsCases.filter((entry) => entry.error).length).toBeGreaterThan(5);
  });

  for (const entry of wsCases) {
    const bytes = readFixture(entry.file);
    if (entry.error) {
      it(`${entry.name}：违规（${entry.error}）`, () => {
        for (const step of [0, 1]) {
          let reason: string | undefined;
          try {
            decodeAll(bytes, entry.from, step);
          } catch (error) {
            expect(error).toBeInstanceOf(WsFrameError);
            reason = (error as WsFrameError).reason;
          }
          expect(reason, `step ${step}`).toBe(entry.error);
        }
      });
      continue;
    }
    it(`${entry.name}：解码（整段与逐字节）与编码都对上`, () => {
      for (const step of [0, 1, 5]) {
        const events = decodeAll(bytes, entry.from, step).map((event) =>
          "data" in event ? { ...event, data: event.data.slice() } : event,
        );
        expect(events, `step ${step}`).toEqual(expectedEvents(entry));
      }
      const encoded = joinBytes(
        (entry.encode ?? []).map((frame) =>
          encodeWsFrame({
            opcode: frame.opcode,
            fin: frame.fin,
            payload: fixtureBytes(frame.payload),
            mask: frame.mask ? fixtureBytes({ hex: frame.mask }) : undefined,
          }),
        ),
      );
      expect(Buffer.from(encoded)).toEqual(Buffer.from(bytes));
    });
  }

  it("closeFrame 与关闭帧夹具逐字节相同；PING_FRAME 是空 ping", () => {
    expect(closeFrame(4404, "source_offline")).toEqual(
      Buffer.from(readFixture("ws/close-source-offline.bin")),
    );
    expect(
      encodeWsFrame({ opcode: OPCODE.ping, payload: new Uint8Array(0) }),
    ).toEqual(PING_FRAME);
    expect(closePayload()).toEqual(new Uint8Array(0));
  });

  it("消息上限：合并分片后超过就报 too_large", () => {
    const decoder = new WsFrameDecoder({
      masked: "forbidden",
      maxMessageBytes: 4,
    });
    decoder.feed(
      encodeWsFrame({
        opcode: OPCODE.text,
        fin: false,
        payload: new Uint8Array(3),
      }),
    );
    expect(() =>
      decoder.feed(
        encodeWsFrame({
          opcode: OPCODE.continuation,
          payload: new Uint8Array(2),
        }),
      ),
    ).toThrow("too_large");
  });

  it("掩码键必须 4 字节", () => {
    expect(() =>
      encodeWsFrame({
        opcode: 1,
        payload: new Uint8Array(1),
        mask: new Uint8Array(3),
      }),
    ).toThrow(RangeError);
  });
});

describe("WsFrameDecoder.idle", () => {
  it("半帧、未收完的分片时不空闲；收全后空闲", () => {
    const decoder = new WsFrameDecoder({ masked: "forbidden" });
    expect(decoder.idle).toBe(true);
    const first = encodeWsFrame({
      opcode: OPCODE.text,
      payload: new TextEncoder().encode("ab"),
      fin: false,
    });
    decoder.feed(first.subarray(0, 2));
    expect(decoder.idle).toBe(false);
    decoder.feed(first.subarray(2));
    expect(decoder.idle).toBe(false);
    const events = decoder.feed(
      encodeWsFrame({
        opcode: OPCODE.continuation,
        payload: new TextEncoder().encode("c"),
      }),
    );
    expect(events).toHaveLength(1);
    expect(decoder.idle).toBe(true);
  });
});
