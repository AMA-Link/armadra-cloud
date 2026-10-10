import {
  readFixture,
  readJsonFixture,
} from "@armadra/platform-protocol/fixtures";
import { describe, expect, it } from "vitest";

import { fixtureBytes, joinBytes, type FixtureBytes } from "../../test/wire.ts";
import {
  encodeChunk,
  encodeRequestHead,
  LAST_CHUNK,
  requestFraming,
  Http1RequestError,
} from "./request.ts";
import {
  Http1ParseError,
  ResponseParser,
  type ResponseEvent,
  type ResponseHead,
} from "./response.ts";

interface ResponseCase {
  name: string;
  file: string;
  method: string;
  expect?: {
    version: string;
    status: number;
    statusText: string;
    headers: [string, string][];
    body: FixtureBytes;
    upgrade: boolean;
    upgraded?: FixtureBytes;
    needsEof: boolean;
  };
  error?: string;
  errorAtEof?: boolean;
}

interface RequestCase {
  name: string;
  file: string;
  method: string;
  path: string;
  headers: [string, string][];
  framing: "none" | "length" | "chunked";
  chunks: string[];
}

const cases = readJsonFixture<{
  responses: ResponseCase[];
  requests: RequestCase[];
}>("http1/cases.json");

interface Parsed {
  head?: ResponseHead;
  upgrade?: boolean;
  body: Uint8Array;
  upgraded: Uint8Array;
  ended: boolean;
  endedBeforeEof: boolean;
}

/** 把字节按 `step` 切开喂给解析器（step 0 = 一次喂完），最后报 EOF。 */
function parse(bytes: Uint8Array, method: string, step: number): Parsed {
  const parser = new ResponseParser({ method });
  const events: ResponseEvent[] = [];
  const size = step === 0 ? bytes.length : step;
  for (let at = 0; at < bytes.length; at += size)
    events.push(...parser.feed(bytes.subarray(at, at + size)));
  const endedBeforeEof = events.some((event) => event.type === "end");
  events.push(...parser.finish());
  const result: Parsed = {
    body: new Uint8Array(0),
    upgraded: new Uint8Array(0),
    ended: false,
    endedBeforeEof,
  };
  const body: Uint8Array[] = [];
  const upgraded: Uint8Array[] = [];
  for (const event of events) {
    if (event.type === "head") {
      expect(result.head, "只有一个最终响应头").toBeUndefined();
      result.head = event.head;
      result.upgrade = event.upgrade;
    } else if (event.type === "body") body.push(event.bytes.slice());
    else if (event.type === "upgraded") upgraded.push(event.bytes.slice());
    else result.ended = true;
  }
  result.body = joinBytes(body);
  result.upgraded = joinBytes(upgraded);
  return result;
}

describe("http1/response：黄金夹具", () => {
  it("夹具齐全", () => {
    expect(cases.responses.length).toBeGreaterThan(20);
    expect(
      cases.responses.filter((entry) => entry.error).length,
    ).toBeGreaterThan(10);
  });

  for (const entry of cases.responses) {
    const bytes = readFixture(entry.file);
    if (entry.error) {
      it(`${entry.name}：不合法（${entry.error}）`, () => {
        for (const step of [0, 1, 7]) {
          const fresh = new ResponseParser({ method: entry.method });
          let reason: string | undefined;
          let atEof = false;
          try {
            const size = step === 0 ? bytes.length : step;
            for (let at = 0; at < bytes.length; at += size)
              fresh.feed(bytes.subarray(at, at + size));
            atEof = true;
            fresh.finish();
          } catch (error) {
            expect(error).toBeInstanceOf(Http1ParseError);
            reason = (error as Http1ParseError).reason;
          }
          expect(reason, `step ${step}`).toBe(entry.error);
          expect(atEof, `step ${step}`).toBe(entry.errorAtEof === true);
        }
      });
      continue;
    }
    const expected = entry.expect as NonNullable<ResponseCase["expect"]>;
    it(`${entry.name}：整段与逐字节解析结果相同`, () => {
      for (const step of [0, 1, 3, 1000]) {
        const parsed = parse(bytes, entry.method, step);
        expect(parsed.head, `step ${step}`).toEqual({
          version: expected.version,
          status: expected.status,
          statusText: expected.statusText,
          headers: expected.headers,
        });
        expect(parsed.upgrade).toBe(expected.upgrade);
        expect(parsed.body).toEqual(fixtureBytes(expected.body));
        expect(parsed.upgraded).toEqual(
          expected.upgraded
            ? fixtureBytes(expected.upgraded)
            : new Uint8Array(0),
        );
        if (expected.upgrade) expect(parsed.ended).toBe(false);
        else {
          expect(parsed.ended).toBe(true);
          expect(parsed.endedBeforeEof).toBe(!expected.needsEof);
        }
      }
    });
  }

  it("响应结束后多出来的字节丢弃；升级后的字节原样交出", () => {
    const parser = new ResponseParser({ method: "GET" });
    const done = parser.feed(
      new TextEncoder().encode(
        "HTTP/1.1 200 OK\r\ncontent-length: 1\r\n\r\nxHTTP/1.1 200 OK\r\n\r\n",
      ),
    );
    expect(done.map((event) => event.type)).toEqual(["head", "body", "end"]);
    expect(parser.done).toBe(true);
    expect(parser.feed(new Uint8Array([1, 2]))).toEqual([]);
    expect(parser.finish()).toEqual([]);
  });

  it("头部上限可调", () => {
    const parser = new ResponseParser({ method: "GET", maxHeadBytes: 16 });
    expect(() =>
      parser.feed(
        new TextEncoder().encode("HTTP/1.1 200 OK\r\nx-a: 12345\r\n\r\n"),
      ),
    ).toThrow("head_too_large");
  });
});

describe("http1/request：黄金夹具", () => {
  for (const entry of cases.requests) {
    it(`${entry.name}：头与体的字节与夹具相同`, () => {
      const encoder = new TextEncoder();
      const framing = requestFraming(entry.headers, entry.chunks.length > 0);
      expect(framing.kind).toBe(entry.framing);
      const parts = [
        encodeRequestHead(entry.method, entry.path, entry.headers, framing),
      ];
      for (const chunk of entry.chunks)
        parts.push(
          framing.kind === "chunked"
            ? encodeChunk(encoder.encode(chunk))
            : encoder.encode(chunk),
        );
      if (framing.kind === "chunked") parts.push(LAST_CHUNK);
      expect(Buffer.from(joinBytes(parts))).toEqual(
        Buffer.from(readFixture(entry.file)),
      );
    });
  }

  it("Content-Length 不合法或不一致：拒绝", () => {
    for (const value of ["-1", "abc", "1, 2", ""])
      expect(() => requestFraming([["content-length", value]], true)).toThrow(
        Http1RequestError,
      );
    expect(requestFraming([["Content-Length", "3, 3"]], true)).toEqual({
      kind: "length",
      length: 3,
    });
    expect(requestFraming([], false)).toEqual({ kind: "none" });
  });

  it("空块不写（空块会被当成结束）", () => {
    expect(encodeChunk(new Uint8Array(0))).toEqual(new Uint8Array(0));
  });
});
