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
