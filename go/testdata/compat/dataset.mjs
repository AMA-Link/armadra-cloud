// 兼容夹具用的固定数据集：一份 state.json（schema v1）形状的状态，SQLite 库、PostgreSQL 转储与
// state.json 夹具都从它来。全部是测试值，不含真实口令或密钥；密钥由固定种子派生，
// 重新生成时除 applied_at_ms 外逐字节一致。
import { createPrivateKey, createPublicKey } from "node:crypto";

import { hashPassword } from "../../../packages/control-core/src/passwords.ts";
import {
  refreshHash,
  secretHash,
} from "../../../packages/control-core/src/secrets.ts";

/** 与 go/internal/testutil 的 FixedNow 相同。 */
export const NOW = 1_760_000_000_000;
export const PASSWORD = "plover-quartz-lantern-41";
export const IDS = {
  source: "0123456789abcdef0123456789abcdef",
  revokedSource: "fedcba9876543210fedcba9876543210",
  ownerDevice: "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1",
  revokedDevice: "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2",
  guestDevice: "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3",
  link: "1111111111111111",
  revokedLink: "2222222222222222",
};
export const SECRETS = {
  ownerRefreshToken: `${IDS.ownerDevice}.fixture-owner-refresh-token`,
  registrationToken: "fixture-registration-token",
  linkSecret: "fixture-link-secret",
};

const PKCS8_ED25519_PREFIX = Buffer.from(
  "302e020100300506032b657004220420",
  "hex",
);

function signingKey(kid, seedByte, activeFromMs, retiredAtMs) {
  const seed = Buffer.alloc(32, seedByte);
  const der = Buffer.concat([PKCS8_ED25519_PREFIX, seed]);
  const publicJwk = createPublicKey(
    createPrivateKey({ key: der, format: "der", type: "pkcs8" }),
  ).export({ format: "jwk" });
  return {
    kid,
    privateKeyPkcs8: der.toString("base64"),
    publicJwk: {
      kty: "OKP",
      crv: "Ed25519",
      x: publicJwk.x,
      kid,
      alg: "EdDSA",
      use: "sig",
    },
    activeFromMs,
    retiredAtMs,
  };
}

function sourcePublicJwk(seedByte) {
  const der = Buffer.concat([PKCS8_ED25519_PREFIX, Buffer.alloc(32, seedByte)]);
  const jwk = createPublicKey(
    createPrivateKey({ key: der, format: "der", type: "pkcs8" }),
  ).export({ format: "jwk" });
  return { kty: "OKP", crv: "Ed25519", x: jwk.x };
}

/** state.json（schema v1）的内容。 */
export async function buildState() {
  const password = await hashPassword(PASSWORD, Buffer.alloc(16, 7));
  return {
    version: 1,
    revision: 7,
    issuer: "https://relay.armadra.test",
    account: {
      name: "owner",
      password,
      createdAtMs: NOW - 86_400_000,
      updatedAtMs: NOW - 86_400_000,
      failures: 2,
      lockedUntilMs: 0,
      lastFailureAtMs: NOW - 3_600_000,
    },
    signingKeys: [
      signingKey("k1", 1, NOW - 7_200_000, NOW + 1_800_000),
      signingKey("k2", 2, NOW - 3_600_000, null),
    ],
    sources: [
      {
        sourceId: IDS.source,
        name: "workstation",
        kind: "desktop",
        publicJwk: sourcePublicJwk(11),
        coreVersion: "0.9.2",
        capabilities: ["terminal", "e2e"],
        registeredAtMs: NOW - 80_000_000,
        lastSeenAtMs: NOW - 5_000,
        revokedAtMs: null,
        trustedOrigins: ["https://app.armadra.test"],
      },
      {
        sourceId: IDS.revokedSource,
        name: "old laptop",
        kind: "server",
        publicJwk: sourcePublicJwk(12),
        coreVersion: "0.8.0",
        capabilities: [],
        registeredAtMs: NOW - 90_000_000,
        lastSeenAtMs: null,
        revokedAtMs: NOW - 1_000_000,
        trustedOrigins: [],
      },
    ],
    registrationTokens: [
      {
        tokenHash: secretHash("registration", SECRETS.registrationToken),
        expiresAtMs: NOW + 600_000,
      },
    ],
    devices: [
      {
        deviceId: IDS.ownerDevice,
        subject: "acct:owner",
        platform: "desktop",
        name: "laptop",
        refreshHash: refreshHash(SECRETS.ownerRefreshToken),
        previousRefreshHash: null,
        rotation: 3,
        createdAtMs: NOW - 86_000_000,
        expiresAtMs: NOW + 2_000_000_000,
        revokedAtMs: null,
        lastSeenAtMs: NOW - 60_000,
      },
      {
        deviceId: IDS.revokedDevice,
        subject: "acct:owner",
        platform: "browser",
        name: "old browser",
        refreshHash: refreshHash("fixture-revoked-refresh-token"),
        previousRefreshHash: refreshHash("fixture-revoked-previous-token"),
        rotation: 2,
        createdAtMs: NOW - 90_000_000,
        expiresAtMs: NOW + 2_000_000_000,
        revokedAtMs: NOW - 2_000_000,
        lastSeenAtMs: null,
      },
      {
        deviceId: IDS.guestDevice,
        subject: `guest:${IDS.link}:abcd1234`,
        platform: "browser",
        name: "guest",
        refreshHash: refreshHash("fixture-guest-refresh-token"),
        previousRefreshHash: null,
        rotation: 1,
        createdAtMs: NOW - 1_000_000,
        expiresAtMs: NOW + 2_000_000_000,
        revokedAtMs: null,
        lastSeenAtMs: null,
      },
    ],
    links: [
      {
        linkId: IDS.link,
        sourceId: IDS.source,
        invitationId: "inv-fixture",
        label: "friend",
        role: "viewer",
        secretHash: secretHash("link", SECRETS.linkSecret),
        maxUses: 3,
        uses: 1,
        createdAtMs: NOW - 1_000_000,
        expiresAtMs: NOW + 2_000_000_000,
        revokedAtMs: null,
        scope: { workspaceId: "ws_01", readOnly: true },
      },
      {
        linkId: IDS.revokedLink,
        sourceId: IDS.source,
        invitationId: "inv-revoked",
        label: "",
        role: "editor",
        secretHash: secretHash("link", "fixture-revoked-link-secret"),
        maxUses: null,
        uses: 0,
        createdAtMs: NOW - 2_000_000,
        expiresAtMs: NOW + 2_000_000_000,
        revokedAtMs: NOW - 500_000,
      },
    ],
    settings: {
      allowGuests: true,
      trustedOrigins: ["https://app.armadra.test"],
      rateLimits: { token: 600 },
      displayName: "Fixture relay",
      trustedProxy: { proxies: ["10.0.0.0/8"], prefer: "forwarded-for" },
    },
  };
}

/** 流量统计（不在 state.json 里，只进两个库）。 */
export const TRAFFIC = [
  {
    bucketStartMs: Math.floor(NOW / 3_600_000) * 3_600_000 - 3_600_000,
    node: "relay-1",
    sourceId: IDS.source,
    subject: "acct:owner",
    deviceId: IDS.ownerDevice,
    kind: "ws",
    connections: 3,
    bytesToSource: 18_432,
    bytesToClient: 2_621_440,
  },
  {
    bucketStartMs: Math.floor(NOW / 3_600_000) * 3_600_000,
    node: "relay-1",
    sourceId: IDS.source,
    subject: `guest:${IDS.link}:abcd1234`,
    deviceId: "",
    kind: "http",
    connections: 1,
    bytesToSource: 100,
    bytesToClient: 5_000_000_000,
  },
];
