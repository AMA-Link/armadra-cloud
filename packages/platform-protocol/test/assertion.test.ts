import { createPublicKey, verify } from "node:crypto";
import { describe, expect, it } from "vitest";

import {
  checkAudience,
  checkTimes,
  ReplayCache,
} from "../src/assertion/check.ts";
import {
  accessClaimsSchema,
  assertionClaimsSchema,
  parseScopeClaims,
  relayTokenClaimsSchema,
  scopeClaims,
  sourceJwsClaimsSchema,
  TOKEN_TYPES,
  tunnelTokenClaimsSchema,
} from "../src/assertion/claims.ts";
import {
  isIdentityPath,
  isReadOnlyAllowed,
} from "../src/assertion/read-only.ts";
import {
  buildSigningInput,
  joinCompactJws,
  JwsError,
  splitCompactJws,
} from "../src/assertion/jws.ts";
import {
  FIXED_NOW_MS,
  readTextFixture,
  TEST_KEYS,
} from "../src/fixtures/index.ts";
import { ASSERTION } from "../src/identity-vectors/constants.ts";

const issuerKey = createPublicKey({
  key: TEST_KEYS.issuer.publicJwk,
  format: "jwk",
});
const sourceKey = createPublicKey({
  key: TEST_KEYS.source.publicJwk,
  format: "jwk",
});
const SOURCE_ID = TEST_KEYS.source.sourceId;

function jwsError(fn: () => unknown): JwsError {
  try {
    fn();
  } catch (error) {
    if (error instanceof JwsError) return error;
    throw error;
  }
  throw new Error("没有抛 JwsError");
}

function verified(name: string, key = issuerKey) {
  const jws = splitCompactJws(readTextFixture(`assertion/${name}.jwt`));
  expect(verify(null, jws.signingInput, key, jws.signature)).toBe(true);
  return jws;
}

const options = {
  nowMs: FIXED_NOW_MS,
  expectedAud: SOURCE_ID,
  expectedIss: TEST_KEYS.issuer.issuer,
};

describe("compact JWS", () => {
  it("拆开再拼回逐字相同", () => {
    const token = readTextFixture("assertion/valid.jwt");
    const [encodedHeader, encodedPayload] = token.split(".") as [
      string,
      string,
    ];
    const jws = splitCompactJws(token);
    expect(joinCompactJws(encodedHeader, encodedPayload, jws.signature)).toBe(
      token,
    );
  });

  it("buildSigningInput 与拆出来的签名输入相同", () => {
    const jws = splitCompactJws(readTextFixture("assertion/valid.jwt"));
    expect(buildSigningInput(jws.header, jws.payload).signingInput).toEqual(
      jws.signingInput,
    );
  });

  it("格式错 → jws_malformed", () => {
    for (const bad of [
      "",
      "a.b",
      "a.b.c.d",
      "a..c",
      "!!!.e30.AA",
      "e30.e30.AAAA",
    ]) {
      expect(jwsError(() => splitCompactJws(bad)).code, bad).toBe(
        "jws_malformed",
      );
    }
    const noKid = buildSigningInput({ alg: "EdDSA" }, {});
    expect(
      jwsError(() =>
        splitCompactJws(
          joinCompactJws(
            noKid.encodedHeader,
            noKid.encodedPayload,
            new Uint8Array(64),
          ),
        ),
      ).code,
    ).toBe("jws_malformed");
  });

  it("wrong-alg.jwt → jws_alg_unsupported", () => {
    expect(
      jwsError(() =>
        splitCompactJws(readTextFixture("assertion/wrong-alg.jwt")),
      ).code,
    ).toBe("jws_alg_unsupported");
  });
});

describe("夹具里的令牌", () => {
  it("valid.jwt：验签、声明、时间与受众都过", () => {
    const jws = verified("valid");
    expect(jws.header).toEqual({
      alg: "EdDSA",
      kid: TEST_KEYS.issuer.kid,
      typ: TOKEN_TYPES.assertion,
    });
    const claims = assertionClaimsSchema.parse(jws.payload);
    expect(claims.iat * 1000).toBe(FIXED_NOW_MS);
    expect(claims.exp - claims.iat).toBe(ASSERTION.ttlMs / 1000);
    expect(checkTimes(claims, options)).toBeNull();
    expect(checkAudience(claims, options)).toBeNull();
  });

  it("expired.jwt → expired", () => {
    const claims = assertionClaimsSchema.parse(verified("expired").payload);
    expect(checkTimes(claims, options)).toEqual({ code: "expired" });
  });

  it("wrong-aud.jwt → audience_mismatch；错 iss → issuer_mismatch", () => {
    const claims = assertionClaimsSchema.parse(verified("wrong-aud").payload);
    expect(checkAudience(claims, options)).toEqual({
      code: "audience_mismatch",
    });
    const valid = assertionClaimsSchema.parse(verified("valid").payload);
    expect(
      checkAudience(valid, { ...options, expectedIss: "https://other.test" }),
    ).toEqual({ code: "issuer_mismatch" });
  });

  it("guest.jwt 与 with-link.jwt 带 link 声明", () => {
    const guest = assertionClaimsSchema.parse(verified("guest").payload);
    expect(guest.sub).toMatch(/^guest:/);
    expect(guest.link?.invitationId).toBe("inv_01");
    expect(guest.email).toBeUndefined();
    const withLink = assertionClaimsSchema.parse(verified("with-link").payload);
    expect(withLink.org).toEqual({
      orgId: "org_01JAAAAAAAAAAAAAAAAAAAAAAA",
      role: "member",
    });
  });

  it("访问令牌、中继令牌、隧道令牌、源 JWS 各自的 schema", () => {
    expect(accessClaimsSchema.parse(verified("access-token").payload).aud).toBe(
      "cloud",
    );
    expect(
      relayTokenClaimsSchema.parse(verified("relay-token").payload).src,
    ).toBe(SOURCE_ID);
    const tunnel = tunnelTokenClaimsSchema.parse(
      verified("tunnel-token").payload,
    );
    expect(tunnel.cnf.jwk.x).toBe(TEST_KEYS.source.publicJwk.x);
    const source = verified("source-jws", sourceKey);
    expect(source.header.kid).toBe(SOURCE_ID);
    expect(
      sourceJwsClaimsSchema.parse(source.payload).exp -
        sourceJwsClaimsSchema.parse(source.payload).iat,
    ).toBe(60);
  });

  it("令牌的 schema 互不通用（aud 不同）", () => {
    expect(
      relayTokenClaimsSchema.safeParse(verified("valid").payload).success,
    ).toBe(false);
    expect(
      accessClaimsSchema.safeParse(verified("relay-token").payload).success,
    ).toBe(false);
  });

  it("重放：同一个 jti 第二次被 ReplayCache 认出", () => {
    const claims = assertionClaimsSchema.parse(verified("valid").payload);
    const cache = new ReplayCache(ASSERTION.replayWindowMs);
    expect(cache.seen(claims.jti, claims.exp * 1000, FIXED_NOW_MS)).toBe(false);
    expect(cache.seen(claims.jti, claims.exp * 1000, FIXED_NOW_MS + 1)).toBe(
      true,
    );
  });
});

describe("checkTimes 的 skew 边界", () => {
  const iat = FIXED_NOW_MS / 1000;
  const claims = { iat, exp: iat + 300 };
  const at = (nowMs: number, skewMs?: number) =>
    checkTimes(claims, { nowMs, skewMs, expectedAud: "x" });

  it("exp + skew 之前 1 毫秒仍有效，正好到点即过期", () => {
    expect(at(claims.exp * 1000 + 300_000 - 1)).toBeNull();
    expect(at(claims.exp * 1000 + 300_000)).toEqual({ code: "expired" });
    expect(at(claims.exp * 1000, 0)).toEqual({ code: "expired" });
    expect(at(claims.exp * 1000 - 1, 0)).toBeNull();
  });

  it("iat 在未来超过 skew → not_yet_valid；正好 skew 内仍有效", () => {
    expect(at(iat * 1000 - 300_000)).toBeNull();
    expect(at(iat * 1000 - 300_001)).toEqual({ code: "not_yet_valid" });
  });

  it("nbf 同理", () => {
    const withNbf = { ...claims, nbf: iat + 600 };
    expect(
      checkTimes(withNbf, { nowMs: FIXED_NOW_MS, expectedAud: "x" }),
    ).toEqual({ code: "not_yet_valid" });
    expect(
      checkTimes(withNbf, { nowMs: FIXED_NOW_MS + 300_000, expectedAud: "x" }),
    ).toBeNull();
  });
});

describe("ReplayCache", () => {
  it("记到 max(exp, now + ttl)，之后同一 jti 可以再用", () => {
    const cache = new ReplayCache(1_000);
    expect(cache.seen("a", 500, 0)).toBe(false);
    expect(cache.seen("a", 500, 999)).toBe(true);
    expect(cache.seen("a", 500, 1_000)).toBe(false);
    expect(cache.seen("b", 5_000, 0)).toBe(false);
    expect(cache.seen("b", 5_000, 4_999)).toBe(true);
  });

  it("满了先清过期的，再淘汰最早记下的", () => {
    const cache = new ReplayCache(1_000, 3);
    cache.seen("old", 0, 0);
    cache.seen("x", 0, 500);
    cache.seen("y", 0, 600);
    // old 在 1000 过期：容量满时先被清掉，x、y 留下
    expect(cache.seen("z", 0, 1_200)).toBe(false);
    expect(cache.size).toBe(3);
    expect(cache.seen("x", 0, 1_300)).toBe(true);
    // 都没过期时淘汰最早的 x
    expect(cache.seen("w", 0, 1_300)).toBe(false);
    expect(cache.size).toBe(3);
    expect(cache.seen("x", 0, 1_301)).toBe(false);
  });

  it("参数必须为正", () => {
    expect(() => new ReplayCache(0)).toThrow(RangeError);
    expect(() => new ReplayCache(1, 0)).toThrow(RangeError);
  });
});

describe("协议 1.1：断言范围与只读令牌", () => {
  it("scoped-session.jwt：scp 是工作空间 + 会话 + 只读", () => {
    const claims = assertionClaimsSchema.parse(
      verified("scoped-session").payload,
    );
    expect(claims.scp).toEqual(["ws:ws_01", "sess:sess_01", "ro"]);
    expect(parseScopeClaims(claims.scp)).toEqual({
      workspaceIds: ["ws_01"],
      sessionIds: ["sess_01"],
      readOnly: true,
    });
    expect(claims.role).toBeUndefined();
  });

  it("team-grant.jwt：org、team、role 与多个工作空间", () => {
    const claims = assertionClaimsSchema.parse(verified("team-grant").payload);
    expect(claims.team).toEqual({
      teamId: "team_01JAAAAAAAAAAAAAAAAAAAAAAA",
      role: "member",
    });
    expect(claims.role).toBe("editor");
    expect(parseScopeClaims(claims.scp).workspaceIds).toEqual([
      "ws_01",
      "ws_02",
    ]);
  });

  it("旧断言没有 scp / role / team：解析出整台、可写", () => {
    const claims = assertionClaimsSchema.parse(verified("valid").payload);
    expect(claims.scp).toBeUndefined();
    expect(parseScopeClaims(claims.scp)).toEqual({
      workspaceIds: [],
      sessionIds: [],
      readOnly: false,
    });
  });

  it("relay-token-ro.jwt 与 tunnel-token-org.jwt；旧令牌没有 ro / org", () => {
    const ro = relayTokenClaimsSchema.parse(verified("relay-token-ro").payload);
    expect(ro.ro).toBe(true);
    expect(ro.org).toBe("org_01JAAAAAAAAAAAAAAAAAAAAAAA");
    const old = relayTokenClaimsSchema.parse(verified("relay-token").payload);
    expect(old.ro).toBeUndefined();
    expect(old.org).toBeUndefined();
    expect(
      tunnelTokenClaimsSchema.parse(verified("tunnel-token-org").payload).org,
    ).toBe("org_01JAAAAAAAAAAAAAAAAAAAAAAA");
  });

  it("协议 1.3：relay-token-dev.jwt 带 dev；老令牌没有；dev 不能是空串", () => {
    const withDev = relayTokenClaimsSchema.parse(
      verified("relay-token-dev").payload,
    );
    expect(withDev.dev).toBe("dev_01JAAAAAAAAAAAAAAAAAAAAAAA");
    const old = relayTokenClaimsSchema.parse(verified("relay-token").payload);
    expect(old.dev).toBeUndefined();
    expect(relayTokenClaimsSchema.safeParse({ ...old, dev: "" }).success).toBe(
      false,
    );
  });

  it("scp 的项只认 ws: / sess: / ro；role 只认四档", () => {
    const base = assertionClaimsSchema.parse(verified("valid").payload);
    for (const bad of [["admin"], ["ws:"], ["ws:a b"], ["sess:x/y"]])
      expect(
        assertionClaimsSchema.safeParse({ ...base, scp: bad }).success,
        bad.join(),
      ).toBe(false);
    expect(
      assertionClaimsSchema.safeParse({ ...base, role: "owner" }).success,
    ).toBe(false);
  });

  it("scopeClaims：顺序固定；会话范围总是带 ro；整台答空", () => {
    expect(scopeClaims(undefined)).toEqual([]);
    expect(scopeClaims({})).toEqual([]);
    expect(scopeClaims({ readOnly: false })).toEqual([]);
    expect(scopeClaims({ workspaceId: "w" })).toEqual(["ws:w"]);
    expect(scopeClaims({ readOnly: true })).toEqual(["ro"]);
    expect(scopeClaims({ workspaceId: "w", sessionId: "s" })).toEqual([
      "ws:w",
      "sess:s",
      "ro",
    ]);
  });

  it("isReadOnlyAllowed：安全方法与规范的身份域路径", () => {
    for (const method of ["GET", "HEAD", "OPTIONS", "get"])
      expect(isReadOnlyAllowed(method, "/api/workspaces")).toBe(true);
    for (const method of ["POST", "PUT", "PATCH", "DELETE"])
      expect(isReadOnlyAllowed(method, "/api/workspaces")).toBe(false);
    for (const path of [
      "/api/identity/cloud/login",
      "/api/identity/ws-ticket?x=1",
      "/api/rpc/identity/session/refresh",
    ])
      expect(isReadOnlyAllowed("POST", path), path).toBe(true);
    for (const path of [
      "/api/identity",
      "/api/identityx/login",
      "/api/identity/../terminals",
      "/api/identity/./x",
      "/api/identity//x",
      "/api/identity/%2e%2e/terminals",
      "/api/identity\\..\\terminals",
      "/x/api/identity/login",
    ])
      expect(isIdentityPath(path), path).toBe(false);
  });
});
