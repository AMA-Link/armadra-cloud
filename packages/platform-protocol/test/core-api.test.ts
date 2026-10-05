import { describe, expect, it } from "vitest";

import {
  cloudLoginInputSchema,
  cloudLoginOutputSchema,
  cloudRegisterInputSchema,
  cloudRegisterOutputSchema,
  cloudStatusOutputSchema,
  cloudTunnelEventSchema,
  clientSourceSchema,
  remoteAddInputSchema,
  remoteAddOutputSchema,
  sourcesSessionOutputSchema,
  tunnelStatusSchema,
} from "../src/core-api/index.ts";
import { TEST_KEYS } from "../src/fixtures/index.ts";

const tunnel = {
  state: "ready",
  node: "relay-1",
  since: 1,
  streams: 2,
  lastError: null,
};

describe("§31 云登录与登记", () => {
  it("login 的输入输出", () => {
    expect(cloudLoginInputSchema.parse({ assertion: "a.b.c" })).toEqual({
      assertion: "a.b.c",
    });
    const output = cloudLoginOutputSchema.parse({
      session: {
        hostId: "h",
        device: { deviceId: "d" },
        native: { accessToken: "a", refreshToken: "r" },
      },
      principal: { principalId: "p", kind: "member", displayName: "Guest" },
      created: true,
    });
    // 原生会话与 core §3 同形：缺省字段补齐
    expect(output.session.scopes).toEqual([]);
    expect(output.session.device.role).toBe("owner");
  });

  it("register 要 URL 形式的 issuer", () => {
    expect(
      cloudRegisterInputSchema.safeParse({
        issuer: "not a url",
        registrationToken: "t",
      }).success,
    ).toBe(false);
    expect(
      cloudRegisterOutputSchema.parse({
        issuer: "https://cloud.armadra.test",
        sourceId: TEST_KEYS.source.sourceId,
        relayOrigins: [],
        trustedOrigins: [],
        tunnel,
      }).tunnel.state,
    ).toBe("ready");
  });

  it("status 带源公钥与每个登记的隧道状态", () => {
    const status = cloudStatusOutputSchema.parse({
      registrations: [
        {
          issuer: "https://cloud.armadra.test",
          mode: "personal",
          jwksFetchedAtMs: null,
          trustedOrigins: [],
          relayOrigins: [],
          registeredAtMs: 1,
          tunnel,
        },
      ],
      sourcePublicKey: {
        kty: "OKP",
        crv: "Ed25519",
        x: TEST_KEYS.source.publicJwk.x,
      },
      sourceId: TEST_KEYS.source.sourceId,
    });
    expect(status.registrations).toHaveLength(1);
  });
});

describe("§32 隧道状态", () => {
  it("状态只取登记的六个值", () => {
    expect(
      tunnelStatusSchema.safeParse({ ...tunnel, state: "flying" }).success,
    ).toBe(false);
    expect(
      cloudTunnelEventSchema.parse({ issuer: "https://x", state: "backoff" })
        .state,
    ).toBe("backoff");
  });
});

describe("§33 客户端源表", () => {
  it("ClientSource 只报 hasCredentials，不带凭据", () => {
    const row = clientSourceSchema.parse({
      sourceId: TEST_KEYS.source.sourceId,
      kind: "relayed",
      label: "",
      baseUrl: "",
      relayOrigin: "https://relay.armadra.test",
      fingerprint: "",
      cloudIssuer: "https://cloud.armadra.test",
      principalHint: "",
      addedAtMs: 0,
      lastOkAtMs: 0,
      orderIndex: 0,
      hasCredentials: true,
      refreshToken: "must-be-dropped",
    });
    expect(row).not.toHaveProperty("refreshToken");
  });

  it("远程服务按 kind 区分；saas 走设备码", () => {
    expect(
      remoteAddInputSchema.parse({ kind: "saas", issuer: "https://c" }).kind,
    ).toBe("saas");
    expect(
      remoteAddInputSchema.safeParse({ kind: "personal", issuer: "https://r" })
        .success,
    ).toBe(false);
    const output = remoteAddOutputSchema.parse({
      remote: {
        serviceId: "s",
        kind: "saas",
        issuer: "https://c",
        label: "",
        accountHint: "",
        fingerprint: "",
        addedAtMs: 0,
        lastOkAtMs: 0,
        registered: false,
        hasCredentials: false,
      },
      next: {
        deviceCode: {
          userCode: "BCDF-GHJK",
          verificationUrl: "https://c/device",
          expiresAtMs: 1,
        },
      },
    });
    expect(output.next).not.toBe("ready");
  });

  it("session 的 via 只能是 direct / relayed", () => {
    const base = {
      accessToken: "a",
      accessExpiresAtMs: 1,
      httpBase: "https://h",
      wsBase: "wss://h",
    };
    expect(
      sourcesSessionOutputSchema.safeParse({ ...base, via: "relayed" }).success,
    ).toBe(true);
    expect(
      sourcesSessionOutputSchema.safeParse({ ...base, via: "carrier-pigeon" })
        .success,
    ).toBe(false);
  });
});
