import { sign } from "node:crypto";

import {
  FIXED_NOW_MS,
  readJsonFixture,
} from "@armadra/platform-protocol/fixtures";
import {
  challengeSigningInput,
  CLOSE,
  type Auth,
  type Challenge,
  type Hello,
} from "@armadra/platform-protocol/tunnel";
import { describe, expect, it } from "vitest";

import {
  sourceKey,
  SOURCE_ID,
  testControlPlane,
  tunnelToken,
} from "../../test/tokens.ts";
import { createAuthContext } from "../auth/context.ts";
import { Handshake, rejectStep, type HandshakeDeps } from "./handshake.ts";

const fixture = readJsonFixture<{
  hello: Hello;
  challenge: Challenge;
  auth: Auth;
  reject: unknown;
}>("tunnel/handshake.json");

function machine(overrides: Partial<HandshakeDeps> = {}) {
  const control = testControlPlane({ now: () => FIXED_NOW_MS });
  const handshake = new Handshake({
    auth: createAuthContext(control, () => FIXED_NOW_MS),
    limits: control.limits,
    relayNode: fixture.challenge.relayNode,
    nonce: () => fixture.challenge.nonce2,
    ...overrides,
  });
  return { handshake, control };
}

const text = (value: unknown) => JSON.stringify(value);

describe("握手（黄金夹具 handshake.json）", () => {
  it("hello → challenge → auth → ready", async () => {
    const { handshake } = machine();
    const challenge = await handshake.onText(text(fixture.hello));
    expect(challenge).toEqual({ kind: "send", message: fixture.challenge });
    const ready = await handshake.onText(text(fixture.auth));
    expect(ready).toMatchObject({
      kind: "ready",
      hello: { sourceId: SOURCE_ID, coreVersion: "0.2.0" },
      claims: { sub: SOURCE_ID, jti: "jti-tunnel-0001" },
    });
    expect(handshake.state).toBe("done");
  });

  it("reject 消息与夹具相同", () => {
    expect(rejectStep("tunnel_token_expired")).toMatchObject({
      message: fixture.reject,
      closeCode: CLOSE.tunnelAuthFailed,
    });
  });

  it("坏 major → reject protocol_unsupported，4409", async () => {
    const { handshake } = machine();
    const step = await handshake.onText(
      text({ ...fixture.hello, protocol: { major: 2, minor: 0 } }),
    );
    expect(step).toMatchObject({
      kind: "reject",
      message: { type: "reject", code: "protocol_unsupported" },
      closeCode: CLOSE.protocolUnsupported,
    });
  });

  it.each([
    [
      "过期",
      { tunnelToken: tunnelToken({}, FIXED_NOW_MS - 3 * 3600_000) },
      "tunnel_token_expired",
    ],
    [
      "错 aud",
      { tunnelToken: tunnelToken({ aud: "relay" }, FIXED_NOW_MS) },
      "tunnel_token_invalid",
    ],
    [
      "错 sub（令牌是别的源的）",
      { sourceId: "fedcba9876543210fedcba9876543210" },
      "tunnel_token_invalid",
    ],
  ])("令牌%s → reject，4490", async (_name, patch, reject) => {
    const { handshake } = machine();
    const step = await handshake.onText(text({ ...fixture.hello, ...patch }));
    expect(step).toMatchObject({
      kind: "reject",
      message: { code: reject },
      closeCode: CLOSE.tunnelAuthFailed,
    });
  });

  it("令牌已撤销 → tunnel_token_invalid", async () => {
    const { handshake, control } = machine();
    await control.tokens.revoke("jti-tunnel-0001", FIXED_NOW_MS + 3600_000);
    expect(await handshake.onText(text(fixture.hello))).toMatchObject({
      kind: "reject",
      message: { code: "tunnel_token_invalid" },
    });
  });

  it("签名错 → reject signature_invalid，4490", async () => {
    const { handshake } = machine({ nonce: () => "AAAAAAAAAAAAAAAAAAAAAA" });
    await handshake.onText(text(fixture.hello));
    expect(await handshake.onText(text(fixture.auth))).toMatchObject({
      kind: "reject",
      message: { code: "signature_invalid" },
      closeCode: CLOSE.tunnelAuthFailed,
    });
  });

  it("用源私钥对新 nonce2 签名同样通过", async () => {
    const { handshake } = machine({ nonce: () => "AAAAAAAAAAAAAAAAAAAAAA" });
    await handshake.onText(text(fixture.hello));
    const sig = sign(
      null,
      challengeSigningInput({
        sourceId: SOURCE_ID,
        nonce: fixture.hello.nonce,
        nonce2: "AAAAAAAAAAAAAAAAAAAAAA",
        relayNode: fixture.challenge.relayNode,
      }),
      sourceKey,
    ).toString("base64url");
    expect((await handshake.onText(text({ type: "auth", sig }))).kind).toBe(
      "ready",
    );
  });

  it("按 sourceId 限流 → reject rate_limited，4429", async () => {
    const { handshake } = machine({
      limits: { hit: async () => ({ allowed: false, retryAfterMs: 1 }) },
    });
    expect(await handshake.onText(text(fixture.hello))).toMatchObject({
      kind: "reject",
      message: { code: "rate_limited" },
      closeCode: CLOSE.limitReached,
    });
  });

  it.each([
    ["不是 JSON", "{"],
    ["不符 schema", text({ type: "hello" })],
    ["先发 auth", text(fixture.auth)],
  ])("%s → 直接关 4400", async (_name, message) => {
    const { handshake } = machine();
    expect(await handshake.onText(message)).toMatchObject({
      kind: "fail",
      closeCode: CLOSE.badFrame,
    });
  });

  it("challenge 之后发的不是 auth → 4400", async () => {
    const { handshake } = machine();
    await handshake.onText(text(fixture.hello));
    expect(await handshake.onText(text(fixture.hello))).toMatchObject({
      kind: "fail",
      closeCode: CLOSE.badFrame,
    });
  });
});
