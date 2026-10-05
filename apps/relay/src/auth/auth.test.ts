import { generateKeyPairSync } from "node:crypto";

import {
  FIXED_NOW_MS,
  readJsonFixture,
  readTextFixture,
  TEST_KEYS,
} from "@armadra/platform-protocol/fixtures";
import { describe, expect, it, vi } from "vitest";

import {
  ISSUER,
  relayToken,
  signJwt,
  SOURCE_ID,
  testControlPlane,
  tunnelToken,
} from "../../test/tokens.ts";
import { createAuthContext } from "./context.ts";
import { JwksCache } from "./jwks.ts";
import {
  relayTokenFromHeaders,
  splitRelaySubprotocol,
  verifyRelayToken,
} from "./relay-token.ts";
import { verifyTunnelToken } from "./tunnel-token.ts";

const OTHER_SOURCE = "fedcba9876543210fedcba9876543210";

function context(nowMs = FIXED_NOW_MS) {
  const control = testControlPlane({ now: () => nowMs });
  return { control, auth: createAuthContext(control, () => nowMs) };
}

describe("中继令牌", () => {
  it("黄金夹具 relay-token.jwt 通过", async () => {
    const { auth } = context();
    const token = readTextFixture("assertion/relay-token.jwt");
    const result = await verifyRelayToken(auth, token, SOURCE_ID);
    expect(result).toMatchObject({ ok: true, claims: { src: SOURCE_ID } });
  });

  it("src 不符 → relay_source_mismatch", async () => {
    const { auth } = context();
    const token = readTextFixture("assertion/relay-token.jwt");
    expect(await verifyRelayToken(auth, token, OTHER_SOURCE)).toMatchObject({
      ok: false,
      error: "relay_source_mismatch",
    });
  });

  it.each([
    ["缺令牌", undefined, "missing"],
    ["乱码", "abc", "malformed"],
    ["断言冒充中继令牌（typ 不对）", "assertion/valid.jwt", "wrong_type"],
    ["隧道令牌冒充中继令牌", "assertion/tunnel-token.jwt", "wrong_type"],
    ["头声称 HS256", "assertion/wrong-alg.jwt", "malformed"],
  ])("%s → relay_token_invalid", async (_name, input, failure) => {
    const { auth } = context();
    const token = input?.endsWith(".jwt") ? readTextFixture(input) : input;
    expect(await verifyRelayToken(auth, token, SOURCE_ID)).toMatchObject({
      ok: false,
      error: "relay_token_invalid",
      failure,
    });
  });

  it("过期、错 aud、错 iss、坏签名、未知 kid、撤销都拒绝", async () => {
    const { auth, control } = context();
    const cases: [string, string][] = [
      ["expired", relayToken({}, FIXED_NOW_MS - 3 * 3600_000)],
      // aud 在 schema 里是字面量，错 aud 在声明校验那一步就被拒。
      ["bad_claims", relayToken({ aud: "relay-tunnel" }, FIXED_NOW_MS)],
      ["issuer_mismatch", relayToken({ iss: "https://evil" }, FIXED_NOW_MS)],
      ["bad_claims", relayToken({ src: "nothex" }, FIXED_NOW_MS)],
      ["bad_signature", relayToken({}, FIXED_NOW_MS).replace(/.{4}$/, "AAAA")],
      [
        "unknown_key",
        signJwt(
          "armadra-relay",
          { iss: ISSUER },
          { kid: "nope", key: generateKeyPairSync("ed25519").privateKey },
        ),
      ],
    ];
    for (const [failure, token] of cases) {
      expect(
        await verifyRelayToken(auth, token, SOURCE_ID),
        failure,
      ).toMatchObject({ ok: false, error: "relay_token_invalid", failure });
    }
    await control.tokens.revoke("jti-relay-0001", FIXED_NOW_MS + 3600_000);
    expect(
      await verifyRelayToken(
        auth,
        readTextFixture("assertion/relay-token.jwt"),
        SOURCE_ID,
      ),
    ).toMatchObject({ ok: false, failure: "revoked" });
  });

  it("从头与子协议里取令牌，其余子协议留下", () => {
    expect(relayTokenFromHeaders({ "armadra-relay-token": "t" })).toBe("t");
    expect(relayTokenFromHeaders({})).toBeUndefined();
    expect(
      splitRelaySubprotocol(
        "armadra-ticket.abc, armadra-relay.x.y.z ,armadra-relay.second",
      ),
    ).toEqual({ token: "x.y.z", rest: ["armadra-ticket.abc"] });
    expect(splitRelaySubprotocol(undefined)).toEqual({
      token: undefined,
      rest: [],
    });
  });
});

describe("隧道令牌", () => {
  it("黄金夹具 tunnel-token.jwt 通过并取出 cnf 钥", async () => {
    const { auth } = context();
    const hello = readJsonFixture<{ hello: { tunnelToken: string } }>(
      "tunnel/handshake.json",
    ).hello;
    const result = await verifyTunnelToken(auth, hello.tunnelToken, SOURCE_ID);
    expect(result.ok).toBe(true);
    if (result.ok)
      expect(result.sourceKey.export({ format: "jwk" })).toMatchObject({
        x: TEST_KEYS.source.publicJwk.x,
      });
  });

  it("过期 → tunnel_token_expired；错 sub / 错 aud / 错 typ / 撤销 → tunnel_token_invalid", async () => {
    const { auth, control } = context();
    const cases: [string, string, string][] = [
      [
        "tunnel_token_expired",
        "expired",
        tunnelToken({}, FIXED_NOW_MS - 3 * 3600_000),
      ],
      [
        "tunnel_token_invalid",
        "subject_mismatch",
        tunnelToken({ sub: OTHER_SOURCE }, FIXED_NOW_MS),
      ],
      [
        "tunnel_token_invalid",
        "bad_claims",
        tunnelToken({ aud: "relay" }, FIXED_NOW_MS),
      ],
      [
        "tunnel_token_invalid",
        "wrong_type",
        readTextFixture("assertion/relay-token.jwt"),
      ],
    ];
    for (const [reject, failure, token] of cases)
      expect(
        await verifyTunnelToken(auth, token, SOURCE_ID),
        failure,
      ).toMatchObject({ ok: false, reject, failure });
    await control.tokens.revoke("jti-tunnel-0001", FIXED_NOW_MS + 3600_000);
    expect(
      await verifyTunnelToken(
        auth,
        readTextFixture("assertion/tunnel-token.jwt"),
        SOURCE_ID,
      ),
    ).toMatchObject({
      ok: false,
      reject: "tunnel_token_invalid",
      failure: "revoked",
    });
  });
});

describe("JWKS 缓存", () => {
  const set = { keys: [TEST_KEYS.issuer.publicJwk] };

  it("命中缓存不再拉；TTL 到了才重拉", async () => {
    let now = 0;
    const load = vi.fn(async () => set);
    const cache = new JwksCache(load, { ttlMs: 1000, now: () => now });
    expect(await cache.key(TEST_KEYS.issuer.kid)).not.toBeNull();
    expect(await cache.key(TEST_KEYS.issuer.kid)).not.toBeNull();
    expect(load).toHaveBeenCalledTimes(1);
    now = 1000;
    await cache.key(TEST_KEYS.issuer.kid);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("kid 未知刷新一次；短时间内不重复刷新", async () => {
    let now = 0;
    const load = vi.fn(async () => set);
    const cache = new JwksCache(load, { minRefreshMs: 500, now: () => now });
    await cache.key(TEST_KEYS.issuer.kid);
    now = 600;
    expect(await cache.key("new-kid")).toBeNull();
    expect(load).toHaveBeenCalledTimes(2);
    now = 700;
    expect(await cache.key("new-kid")).toBeNull();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("刷新失败继续用缓存", async () => {
    let now = 0;
    let fail = false;
    const cache = new JwksCache(
      async () => {
        if (fail) throw new Error("down");
        return set;
      },
      { ttlMs: 100, now: () => now },
    );
    await cache.key(TEST_KEYS.issuer.kid);
    fail = true;
    now = 1000;
    expect(await cache.key(TEST_KEYS.issuer.kid)).not.toBeNull();
  });
});
