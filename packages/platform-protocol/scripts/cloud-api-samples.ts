/**
 * `/v1` 每条 procedure 的 input / output 样例（`fixtures/cloud-api/<key>.json`）。
 * 输出是 union 的给 `outputs` 列多个分支。样例只用测试值。
 */
const SOURCE_ID = "0123456789abcdef0123456789abcdef";
const ORG_ID = "org_01JAAAAAAAAAAAAAAAAAAAAAAA";
const ACCOUNT_ID = "acct_01JAAAAAAAAAAAAAAAAAAAAAAA";
const NOW = 1_760_000_000_000;
const ISSUER = "https://cloud.armadra.test";
const RELAY_ORIGIN = "https://relay.armadra.test";

const device = { platform: "desktop", name: "Dev laptop" };
const session = {
  accessToken: "test-access-token",
  refreshToken: "test-refresh-token",
  accessExpiresAtMs: NOW + 15 * 60_000,
  expiresAtMs: NOW + 30 * 24 * 60 * 60_000,
  account: {
    accountId: ACCOUNT_ID,
    email: "dev@armadra.test",
    displayName: "Dev",
    emailVerified: true,
  },
  device: { deviceId: "dev_01JAAAAAAAAAAAAAAAAAAAAAAA" },
};
const sourceSummary = {
  sourceId: SOURCE_ID,
  name: "Workstation",
  kind: "desktop",
  online: true,
  lastSeenAtMs: NOW,
  owner: true,
  via: "owner",
  relayOrigin: RELAY_ORIGIN,
  coreVersion: "0.2.0",
};
const sourceDetail = {
  ...sourceSummary,
  trustedOrigins: ["https://app.armadra.test"],
  capabilities: ["identity.native-session.v1"],
  registeredAtMs: NOW - 86_400_000,
};
const me = {
  accountId: ACCOUNT_ID,
  email: "dev@armadra.test",
  displayName: "Dev",
  emailVerified: true,
  orgs: [{ orgId: ORG_ID, slug: "dev-org", name: "Dev Org", role: "owner" }],
  mfaEnabled: false,
};
const org = {
  orgId: ORG_ID,
  slug: "dev-org",
  name: "Dev Org",
  role: "owner",
  createdAtMs: NOW,
};
const member = {
  accountId: ACCOUNT_ID,
  email: "dev@armadra.test",
  displayName: "Dev",
  role: "admin",
  joinedAtMs: NOW,
};
const link = {
  linkId: "lnk_01JAAAAAAAAAAAAAAAAAAAAAAA",
  kind: "source_invite",
  label: "For Alice",
  role: "editor",
  sourceId: SOURCE_ID,
  url: `${ISSUER}/j/lnk_01JAAAAAAAAAAAAAAAAAAAAAAA`,
  uses: 1,
  maxUses: 5,
  expiresAtMs: NOW + 7 * 86_400_000,
  createdAtMs: NOW,
  revokedAtMs: null,
};
const webauthnOptions = {
  challenge: "dGVzdC1jaGFsbGVuZ2U",
  rp: { id: "armadra.test", name: "Armadra" },
};
const ED25519_X = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo";

export interface Sample {
  input: unknown;
  output?: unknown;
  outputs?: unknown[];
}

export const CLOUD_API_SAMPLES: Record<string, Sample> = {
  "platform.info": {
    input: {},
    output: {
      mode: "saas",
      issuer: ISSUER,
      protocol: { major: 1, minor: 0 },
      capabilities: ["auth.password", "auth.device-code", "me.stream"],
      relay: { addressing: "subdomain" },
      webApp: "https://app.armadra.test",
    },
  },
  "platform.jwks": {
    input: {},
    output: {
      keys: [
        {
          kty: "OKP",
          crv: "Ed25519",
          x: ED25519_X,
          kid: "k1",
          alg: "EdDSA",
          use: "sig",
        },
      ],
    },
  },
  "platform.health": {
    input: {},
    output: { ok: true, version: "0.1.0", mode: "saas" },
  },
  "auth.register": {
    input: {
      email: "dev@armadra.test",
      password: "correct horse battery staple",
      displayName: "Dev",
      device,
    },
    output: { session },
  },
  "auth.login": {
    input: {
      account: "dev@armadra.test",
      password: "correct horse battery staple",
      device,
    },
    outputs: [
      { session },
      { mfa: { challengeId: "mfa_01", methods: ["totp", "recovery"] } },
    ],
  },
  "auth.mfaVerify": {
    input: { challengeId: "mfa_01", code: "123456" },
    output: { session },
  },
  "auth.refresh": {
    input: { refreshToken: "test-refresh-token" },
    output: { session },
  },
  "auth.logout": { input: {}, output: {} },
  "auth.deviceStart": {
    input: { device: { platform: "server", name: "armadra-server" } },
    output: {
      deviceCode: "test-device-code",
      userCode: "BCDF-GHJK",
      verificationUrl: `${ISSUER}/device`,
      expiresAtMs: NOW + 600_000,
      intervalMs: 5_000,
    },
  },
  "auth.devicePoll": {
    input: { deviceCode: "test-device-code" },
    outputs: [
      { status: "pending" },
      { status: "approved", session },
      { status: "denied" },
      { status: "expired" },
    ],
  },
  "auth.deviceApprove": {
    input: { userCode: "BCDF-GHJK", approve: true },
    output: {},
  },
  "auth.passkeyRegisterOptions": {
    input: { label: "YubiKey" },
    output: { challengeId: "pk_01", options: webauthnOptions },
  },
  "auth.passkeyRegister": {
    input: {
      challengeId: "pk_01",
      response: { id: "cred", type: "public-key" },
      label: "YubiKey",
    },
    output: {
      credentialId: "cred",
      label: "YubiKey",
      aaguid: "00000000-0000-0000-0000-000000000000",
      transports: ["usb"],
      createdAtMs: NOW,
    },
  },
  "auth.passkeyLoginOptions": {
    input: {},
    output: { challengeId: "pk_02", options: webauthnOptions },
  },
  "auth.passkeyLogin": {
    input: {
      challengeId: "pk_02",
      response: { id: "cred", type: "public-key" },
      device,
    },
    output: { session },
  },
  "auth.totpEnroll": {
    input: {},
    output: {
      secret: "JBSWY3DPEHPK3PXP",
      otpauthUri: "otpauth://totp/Armadra:dev?secret=JBSWY3DPEHPK3PXP",
    },
  },
  "auth.totpConfirm": {
    input: { code: "123456" },
    output: { recoveryCodes: ["abcde-fghij", "klmno-pqrst"] },
  },
  "auth.totpDisable": { input: { code: "123456" }, output: { disabled: true } },
  "auth.recoveryCodesRotate": {
    input: { code: "123456" },
    output: { recoveryCodes: ["uvwxy-zabcd"] },
  },
  "me.get": { input: {}, output: me },
  "me.update": {
    input: { displayName: "Dev 2" },
    output: { ...me, displayName: "Dev 2" },
  },
  "me.sources": { input: {}, output: { sources: [sourceSummary] } },
  "me.devices": {
    input: {},
    output: {
      devices: [
        {
          deviceId: "dev_01JAAAAAAAAAAAAAAAAAAAAAAA",
          platform: "desktop",
          name: "Dev laptop",
          lastSeenAtMs: NOW,
          current: true,
        },
      ],
    },
  },
  "me.deviceRevoke": {
    input: { deviceId: "dev_01JBBBBBBBBBBBBBBBBBBBBBBB" },
    output: {},
  },
  "me.streamTicket": {
    input: {},
    output: { ticket: "test-ticket", expiresAtMs: NOW + 30_000 },
  },
  "sources.registrationToken": {
    input: {},
    output: {
      registrationToken: "test-registration-token",
      expiresAtMs: NOW + 600_000,
      issuer: ISSUER,
    },
  },
  "sources.register": {
    input: {
      registrationToken: "test-registration-token",
      sourceId: SOURCE_ID,
      publicKey: { kty: "OKP", crv: "Ed25519", x: ED25519_X },
      name: "Workstation",
      kind: "desktop",
      coreVersion: "0.2.0",
      capabilities: ["identity.native-session.v1"],
      protocol: { major: 1, minor: 0 },
    },
    output: {
      issuer: ISSUER,
      jwksUrl: `${ISSUER}/.well-known/jwks.json`,
      relayOrigins: [RELAY_ORIGIN],
      trustedOrigins: ["https://app.armadra.test"],
      sourceId: SOURCE_ID,
      ownerAccountId: ACCOUNT_ID,
    },
  },
  "sources.heartbeatHint": {
    input: {},
    output: {
      tunnelToken: "test-tunnel-token",
      expiresAtMs: NOW + 3_600_000,
      nodes: [
        {
          url: "wss://relay-1.armadra.test/t/v1",
          region: "ap-east",
          weight: 1,
        },
      ],
      limits: {
        maxStreams: 256,
        streamWindow: 262_144,
        tunnelWindow: 8_388_608,
        maxFrameBytes: 1_048_576,
      },
    },
  },
  "sources.list": {
    input: { orgId: ORG_ID },
    output: { sources: [sourceSummary] },
  },
  "sources.get": { input: { sourceId: SOURCE_ID }, output: sourceDetail },
  "sources.update": {
    input: { sourceId: SOURCE_ID, name: "Workstation 2" },
    output: { ...sourceDetail, name: "Workstation 2" },
  },
  "sources.revoke": { input: { sourceId: SOURCE_ID }, output: {} },
  "sources.assertion": {
    input: { sourceId: SOURCE_ID, device },
    output: {
      assertion: "test-assertion",
      assertionExpiresAtMs: NOW + 300_000,
      relayToken: "test-relay-token",
      relayTokenExpiresAtMs: NOW + 3_600_000,
      relayOrigin: RELAY_ORIGIN,
      relayBaseUrl: `https://${SOURCE_ID}.src.relay.armadra.test`,
      online: true,
    },
  },
  "sources.accessList": {
    input: { sourceId: SOURCE_ID },
    output: {
      access: [
        {
          accountId: ACCOUNT_ID,
          email: "dev@armadra.test",
          via: "link",
          linkId: link.linkId,
          grantedAtMs: NOW,
        },
      ],
    },
  },
  "sources.accessRevoke": {
    input: { sourceId: SOURCE_ID, accountId: ACCOUNT_ID },
    output: {},
  },
  "links.create": {
    input: {
      kind: "source_invite",
      sourceId: SOURCE_ID,
      invitationId: "inv_01",
      label: "For Alice",
      role: "editor",
      expiresAtMs: NOW + 7 * 86_400_000,
      maxUses: 5,
    },
    output: {
      linkId: link.linkId,
      url: link.url,
      expiresAtMs: link.expiresAtMs,
    },
  },
  "links.get": {
    input: { linkId: link.linkId },
    output: {
      kind: "source_invite",
      label: "For Alice",
      role: "editor",
      sourceName: "Workstation",
      expiresAtMs: link.expiresAtMs,
      exhausted: false,
      requiresAccount: true,
    },
  },
  "links.accept": {
    input: { linkId: link.linkId, secret: "test-link-secret", device },
    output: {
      sourceId: SOURCE_ID,
      relayOrigin: RELAY_ORIGIN,
      relayBaseUrl: `https://${SOURCE_ID}.src.relay.armadra.test`,
      assertion: "test-assertion",
      relayToken: "test-relay-token",
    },
  },
  "links.list": { input: { sourceId: SOURCE_ID }, output: { links: [link] } },
  "links.update": {
    input: { linkId: link.linkId, label: "For Bob" },
    output: { ...link, label: "For Bob" },
  },
  "links.revoke": { input: { linkId: link.linkId }, output: {} },
  "orgs.create": { input: { slug: "dev-org", name: "Dev Org" }, output: org },
  "orgs.get": { input: { orgId: ORG_ID }, output: org },
  "orgs.update": {
    input: { orgId: ORG_ID, name: "Dev Org 2" },
    output: { ...org, name: "Dev Org 2" },
  },
  "orgs.delete": { input: { orgId: ORG_ID }, output: {} },
  "orgs.members": { input: { orgId: ORG_ID }, output: { members: [member] } },
  "orgs.memberRole": {
    input: { orgId: ORG_ID, accountId: ACCOUNT_ID, role: "admin" },
    output: member,
  },
  "orgs.memberRemove": {
    input: { orgId: ORG_ID, accountId: ACCOUNT_ID },
    output: {},
  },
  "orgs.sources": {
    input: { orgId: ORG_ID },
    output: { sources: [{ ...sourceSummary, via: "org", owner: false }] },
  },
  "orgs.sourceAttach": {
    input: { orgId: ORG_ID, sourceId: SOURCE_ID },
    output: {},
  },
  "orgs.sourceDetach": {
    input: { orgId: ORG_ID, sourceId: SOURCE_ID },
    output: {},
  },
  "billing.plan": {
    input: { orgId: ORG_ID },
    output: {
      orgId: ORG_ID,
      plan: "team",
      status: "active",
      seats: 5,
      periodEndsAtMs: NOW + 30 * 86_400_000,
    },
  },
  "billing.usage": {
    input: { orgId: ORG_ID, period: "2025-10" },
    output: {
      orgId: ORG_ID,
      period: "2025-10",
      usage: [{ metric: "relay.bytes", value: 1024, limit: null }],
    },
  },
};
