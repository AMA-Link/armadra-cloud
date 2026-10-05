import { createPublicKey, verify } from "node:crypto";
import { describe, expect, it } from "vitest";

import { readJsonFixture, TEST_KEYS } from "../src/fixtures/index.ts";
import { fromBase64Url } from "../src/internal/bytes.ts";
import {
  authSchema,
  challengeSchema,
  challengeSigningInput,
  helloSchema,
  readySchema,
  rejectSchema,
  tunnelClientMessageSchema,
  tunnelServerMessageSchema,
} from "../src/tunnel/handshake.ts";

interface HandshakeFixture {
  signingInput: string;
  hello: Record<string, unknown>;
  challenge: Record<string, unknown>;
  auth: Record<string, unknown>;
  ready: Record<string, unknown>;
  reject: Record<string, unknown>;
}

const fixture = readJsonFixture<HandshakeFixture>("tunnel/handshake.json");

describe("四条消息的 schema", () => {
  it("夹具全部通过", () => {
    expect(helloSchema.parse(fixture.hello)).toEqual(fixture.hello);
    expect(challengeSchema.parse(fixture.challenge)).toEqual(fixture.challenge);
    expect(authSchema.parse(fixture.auth)).toEqual(fixture.auth);
    expect(readySchema.parse(fixture.ready)).toEqual(fixture.ready);
    expect(rejectSchema.parse(fixture.reject)).toEqual(fixture.reject);
  });

  it("按 type 分到客户端 / 服务端两个联合", () => {
    expect(tunnelClientMessageSchema.parse(fixture.hello).type).toBe("hello");
    expect(tunnelClientMessageSchema.parse(fixture.auth).type).toBe("auth");
    expect(tunnelServerMessageSchema.parse(fixture.ready).type).toBe("ready");
    expect(tunnelServerMessageSchema.safeParse(fixture.hello).success).toBe(
      false,
    );
  });

  it.each([
    "protocol",
    "sourceId",
    "coreVersion",
    "capabilities",
    "tunnelToken",
    "nonce",
  ])("hello 缺 %s 被拒", (field) => {
    const { [field]: _, ...rest } = fixture.hello;
    expect(helloSchema.safeParse(rest).success).toBe(false);
  });

  it("challenge 缺 nonce2、auth 缺 sig、ready 缺 limits 都被拒", () => {
    const { nonce2: _n, ...challenge } = fixture.challenge;
    const { sig: _s, ...auth } = fixture.auth;
    const { limits: _l, ...ready } = fixture.ready;
    expect(challengeSchema.safeParse(challenge).success).toBe(false);
    expect(authSchema.safeParse(auth).success).toBe(false);
    expect(readySchema.safeParse(ready).success).toBe(false);
  });

  it("sourceId 必须是 32 位小写 hex，nonce 必须是 16 字节 base64url", () => {
    expect(
      helloSchema.safeParse({ ...fixture.hello, sourceId: "ABC" }).success,
    ).toBe(false);
    expect(
      helloSchema.safeParse({ ...fixture.hello, nonce: "short" }).success,
    ).toBe(false);
  });

  it("reject 只收登记的码", () => {
    expect(
      rejectSchema.safeParse({ ...fixture.reject, code: "replaced" }).success,
    ).toBe(true);
    expect(
      rejectSchema.safeParse({ ...fixture.reject, code: "nope" }).success,
    ).toBe(false);
  });

  it("更高 minor 的未知字段被忽略", () => {
    expect(helloSchema.parse({ ...fixture.hello, future: true })).toEqual(
      fixture.hello,
    );
  });
});

describe("签名输入", () => {
  const hello = helloSchema.parse(fixture.hello);
  const challenge = challengeSchema.parse(fixture.challenge);
  const input = challengeSigningInput({
    sourceId: hello.sourceId,
    nonce: hello.nonce,
    nonce2: challenge.nonce2,
    relayNode: challenge.relayNode,
  });

  it("与夹具逐字相同：sourceId \\n nonce \\n nonce2 \\n relayNode", () => {
    expect(new TextDecoder().decode(input)).toBe(fixture.signingInput);
    expect(fixture.signingInput.split("\n")).toEqual([
      hello.sourceId,
      hello.nonce,
      challenge.nonce2,
      challenge.relayNode,
    ]);
  });

  it("auth.sig 能用源公钥验过，改一个字节就不过", () => {
    const key = createPublicKey({
      key: TEST_KEYS.source.publicJwk,
      format: "jwk",
    });
    const sig = fromBase64Url(authSchema.parse(fixture.auth).sig);
    expect(verify(null, input, key, sig)).toBe(true);
    const tampered = input.slice();
    tampered[0] = (tampered[0] ?? 0) ^ 1;
    expect(verify(null, tampered, key, sig)).toBe(false);
  });
});
