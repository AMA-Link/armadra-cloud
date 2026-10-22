/**
 * `/v1` 每条 procedure 的 input / output 样例（`fixtures/cloud-api/<key>.json`）。
 * 输出是 union 的给 `outputs` 列多个分支。样例只用测试值。
 */
const SOURCE_ID = "0123456789abcdef0123456789abcdef";
const TEAM_ID = "team_01JAAAAAAAAAAAAAAAAAAAAAAA";
const GROUP_ID = "grp_01JAAAAAAAAAAAAAAAAAAAAAAA";
const GRANT_ID = "gnt_01JAAAAAAAAAAAAAAAAAAAAAAA";
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
  orgs: [],
  teams: [
    { teamId: TEAM_ID, slug: "dev-team", name: "Dev Team", role: "owner" },
  ],
  mfaEnabled: false,
};
const team = {
  teamId: TEAM_ID,
  slug: "dev-team",
  name: "Dev Team",
  avatarUrl: null,
  role: "owner",
  plan: "free",
  createdAtMs: NOW,
  deletedAtMs: null,
  purgeAtMs: null,
};
const teamSettings = {
  membersCanRegisterHosts: false,
  defaultHostRole: "viewer",
  allowGuests: true,
  allowAnonymousLinks: false,
  deviceEndorsers: "owner_admin",
  memberSelfEndorse: true,
  requireMfa: false,
  sessionMaxAgeMs: null,
  allowedEmailDomains: [],
};
const teamInvitation = {
  invitationId: "tinv_01JAAAAAAAAAAAAAAAAAAAAAAA",
  kind: "team",
  email: "alice@armadra.test",
  state: "pending",
  teamId: TEAM_ID,
  teamRole: "member",
  invitedBy: ACCOUNT_ID,
  createdAtMs: NOW,
  expiresAtMs: NOW + 7 * 86_400_000,
  decidedAtMs: null,
};
const hostInvitation = {
  invitationId: "hinv_01JAAAAAAAAAAAAAAAAAAAAAAA",
  kind: "host",
  email: "bob@armadra.test",
  state: "pending",
  sourceId: SOURCE_ID,
  hostRole: "viewer",
  workspaceIds: ["ws_01"],
  invitedBy: ACCOUNT_ID,
  createdAtMs: NOW,
  expiresAtMs: NOW + 7 * 86_400_000,
  decidedAtMs: null,
};
const group = {
  groupId: GROUP_ID,
  teamId: TEAM_ID,
  name: "Platform",
  memberCount: 2,
  createdAtMs: NOW,
};
const groupMember = {
  accountId: ACCOUNT_ID,
  email: "dev@armadra.test",
  displayName: "Dev",
  isLead: true,
  addedAtMs: NOW,
};
const grant = {
  grantId: GRANT_ID,
  sourceId: SOURCE_ID,
  subject: { kind: "group", groupId: GROUP_ID },
  role: "editor",
  workspaceIds: ["ws_01", "ws_02"],
  expiresAtMs: NOW + 30 * 86_400_000,
  grantedBy: ACCOUNT_ID,
  createdAtMs: NOW,
  revokedAtMs: null,
};
const accessRequest = {
  requestId: "req_01JAAAAAAAAAAAAAAAAAAAAAAA",
  sourceId: SOURCE_ID,
  sourceName: "Workstation",
  requester: {
    accountId: ACCOUNT_ID,
    email: "dev@armadra.test",
    displayName: "Dev",
  },
  role: "editor",
  workspaceIds: [],
  reason: "Need to review the build",
  status: "pending",
  decidedBy: null,
  decidedAtMs: null,
  createdAtMs: NOW,
};
const auditEvent = {
  eventId: "aud_01JAAAAAAAAAAAAAAAAAAAAAAA",
  teamId: TEAM_ID,
  actor: { kind: "account", id: ACCOUNT_ID },
  action: "grant.create",
  target: { kind: "grant", id: GRANT_ID },
  jti: null,
  ipPrefix: "203.0.113.0/24",
  atMs: NOW,
};
const securityEvent = {
  eventId: "sec_01JAAAAAAAAAAAAAAAAAAAAAAA",
  action: "login",
  deviceName: "Dev laptop",
  ipPrefix: "203.0.113.0/24",
  atMs: NOW,
};
const apiToken = {
  tokenId: "tok_01JAAAAAAAAAAAAAAAAAAAAAAA",
  name: "CI",
  teamId: TEAM_ID,
  scopes: ["directory:read"],
  prefix: "armat_ab",
  expiresAtMs: NOW + 90 * 86_400_000,
  lastUsedAtMs: null,
  createdAtMs: NOW,
  revokedAtMs: null,
};
const transfer = {
  transferId: "xfer_01JAAAAAAAAAAAAAAAAAAAAAAA",
  sourceId: SOURCE_ID,
  from: { kind: "personal" },
  to: { kind: "team", teamId: TEAM_ID },
  status: "pending",
  confirmedBy: { from: true, to: false },
  createdAtMs: NOW,
  expiresAtMs: NOW + 7 * 86_400_000,
};
const adminAccount = {
  accountId: ACCOUNT_ID,
  email: "dev@armadra.test",
  displayName: "Dev",
  emailVerified: true,
  status: "active",
  plan: "free",
  hostCount: 1,
  teamCount: 1,
  createdAtMs: NOW,
  lastSeenAtMs: NOW,
};
const adminTeam = {
  teamId: TEAM_ID,
  slug: "dev-team",
  name: "Dev Team",
  status: "active",
  plan: "free",
  memberCount: 3,
  hostCount: 2,
  createdAtMs: NOW,
  deletedAtMs: null,
};
const adminNode = {
  node: "relay-1",
  region: "ap-east",
  online: true,
  tunnels: 12,
  draining: false,
  version: "0.10.0",
  lastSeenAtMs: NOW,
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
  scope: { workspaceId: "ws_01", readOnly: true },
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
    outputs: [
      {
        mode: "saas",
        issuer: ISSUER,
        protocol: { major: 1, minor: 0 },
        capabilities: ["auth.password", "auth.device-code", "me.stream"],
        relay: { addressing: "subdomain" },
        webApp: "https://app.armadra.test",
      },
      // 0.3.1 起可选的服务端名称（个人中转 `--name`）。
      {
        mode: "personal",
        name: "Home relay",
        issuer: ISSUER,
        protocol: { major: 1, minor: 1 },
        capabilities: ["auth.password", "me.stream"],
        relay: { addressing: "path" },
        webApp: null,
      },
      // 0.3.5 起：要求挑战（Workers 个人中转开了 Turnstile）。
      {
        mode: "personal",
        issuer: RELAY_ORIGIN,
        protocol: { major: 1, minor: 4 },
        capabilities: ["auth.password", "me.stream", "challenge.turnstile"],
        relay: { addressing: "path" },
        webApp: `${RELAY_ORIGIN}/app/`,
        challenge: {
          provider: "turnstile",
          siteKey: "test-site-key",
          scope: ["auth.login"],
        },
      },
    ],
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
  "auth.changePassword": {
    input: {
      password: "correct horse battery staple",
      newPassword: "another long passphrase here",
    },
    output: { session },
  },
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
  "me.traffic": {
    input: {
      fromMs: NOW - 86_400_000,
      toMs: NOW,
      sourceId: SOURCE_ID,
      granularity: "hour",
    },
    output: {
      buckets: [
        {
          bucketStartMs: NOW - 3_600_000,
          node: "relay-1",
          sourceId: SOURCE_ID,
          subject: "acct:owner",
          deviceId: "",
          kind: "ws",
          connections: 3,
          bytesToSource: 18_432,
          bytesToClient: 2_621_440,
        },
      ],
    },
  },
  "me.streamTicket": {
    input: {},
    output: { ticket: "test-ticket", expiresAtMs: NOW + 30_000 },
  },
  "sources.registrationToken": {
    input: { space: { kind: "team", teamId: TEAM_ID } },
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
    input: { teamId: TEAM_ID },
    output: { sources: [sourceSummary] },
  },
  "sources.get": { input: { sourceId: SOURCE_ID }, output: sourceDetail },
  "sources.update": {
    input: {
      sourceId: SOURCE_ID,
      name: "Workstation 2",
      tags: ["build", "gpu"],
      settings: { allowGuests: false },
    },
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
      scope: { workspaceId: "ws_01", readOnly: true },
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
      scope: { workspaceId: "ws_01", readOnly: true },
      expiresAtMs: link.expiresAtMs,
      exhausted: false,
      requiresAccount: true,
    },
  },
  "links.accept": {
    input: {
      linkId: link.linkId,
      secret: "test-link-secret",
      device,
      challenge: { provider: "turnstile", token: "test-challenge-token" },
    },
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
  "teams.create": {
    input: { slug: "dev-team", name: "Dev Team" },
    output: team,
  },
  "teams.list": { input: {}, output: { teams: [team] } },
  "teams.get": { input: { teamId: TEAM_ID }, output: team },
  "teams.update": {
    input: { teamId: TEAM_ID, name: "Dev Team 2", avatarUrl: null },
    output: { ...team, name: "Dev Team 2" },
  },
  "teams.delete": {
    input: { teamId: TEAM_ID },
    output: { purgeAtMs: NOW + 30 * 86_400_000 },
  },
  "teams.restore": { input: { teamId: TEAM_ID }, output: team },
  "teams.transferOwnership": {
    input: { teamId: TEAM_ID, accountId: ACCOUNT_ID },
    output: {},
  },
  "teams.members": {
    input: { teamId: TEAM_ID },
    output: { members: [member] },
  },
  "teams.memberRole": {
    input: { teamId: TEAM_ID, accountId: ACCOUNT_ID, role: "admin" },
    output: member,
  },
  "teams.memberRemove": {
    input: { teamId: TEAM_ID, accountId: ACCOUNT_ID },
    output: {},
  },
  "teams.leave": { input: { teamId: TEAM_ID }, output: {} },
  "teams.settings": { input: { teamId: TEAM_ID }, output: teamSettings },
  "teams.settingsUpdate": {
    input: { teamId: TEAM_ID, allowGuests: false },
    output: { ...teamSettings, allowGuests: false },
  },
  "teams.usage": {
    input: { teamId: TEAM_ID, period: "2026-10" },
    output: {
      teamId: TEAM_ID,
      period: "2026-10",
      usage: [
        { metric: "hosts", value: 2, limit: 3 },
        { metric: "relay.bytes", value: 1_048_576, limit: 5_368_709_120 },
      ],
    },
  },
  "invitations.create": {
    input: {
      kind: "team",
      teamId: TEAM_ID,
      email: "alice@armadra.test",
      role: "member",
    },
    output: teamInvitation,
  },
  "invitations.list": {
    input: { box: "sent", teamId: TEAM_ID },
    output: { invitations: [teamInvitation, hostInvitation] },
  },
  "invitations.revoke": {
    input: { invitationId: teamInvitation.invitationId },
    output: {},
  },
  "invitations.get": {
    input: {
      invitationId: teamInvitation.invitationId,
      token: "test-invite-token",
    },
    output: {
      kind: "team",
      state: "pending",
      teamName: "Dev Team",
      teamRole: "member",
      inviterName: "Dev",
      emailHint: "a***@armadra.test",
      expiresAtMs: teamInvitation.expiresAtMs,
    },
  },
  "invitations.accept": {
    input: {
      invitationId: teamInvitation.invitationId,
      token: "test-invite-token",
    },
    output: { teamId: TEAM_ID },
  },
  "invitations.decline": {
    input: {
      invitationId: teamInvitation.invitationId,
      token: "test-invite-token",
    },
    output: {},
  },
  "groups.list": { input: { teamId: TEAM_ID }, output: { groups: [group] } },
  "groups.create": {
    input: { teamId: TEAM_ID, name: "Platform" },
    output: group,
  },
  "groups.update": {
    input: { teamId: TEAM_ID, groupId: GROUP_ID, name: "Platform 2" },
    output: { ...group, name: "Platform 2" },
  },
  "groups.delete": {
    input: { teamId: TEAM_ID, groupId: GROUP_ID },
    output: {},
  },
  "groups.members": {
    input: { teamId: TEAM_ID, groupId: GROUP_ID },
    output: { members: [groupMember] },
  },
  "groups.memberAdd": {
    input: { teamId: TEAM_ID, groupId: GROUP_ID, accountId: ACCOUNT_ID },
    output: groupMember,
  },
  "groups.memberRemove": {
    input: { teamId: TEAM_ID, groupId: GROUP_ID, accountId: ACCOUNT_ID },
    output: {},
  },
  "groups.setLead": {
    input: {
      teamId: TEAM_ID,
      groupId: GROUP_ID,
      accountId: ACCOUNT_ID,
      isLead: true,
    },
    output: groupMember,
  },
  "grants.list": {
    input: { sourceId: SOURCE_ID },
    output: { grants: [grant] },
  },
  "grants.create": {
    input: {
      sourceId: SOURCE_ID,
      subject: { kind: "group", groupId: GROUP_ID },
      role: "editor",
      workspaceIds: ["ws_01", "ws_02"],
      expiresAtMs: grant.expiresAtMs,
    },
    output: grant,
  },
  "grants.update": {
    input: { grantId: GRANT_ID, role: "viewer", expiresAtMs: null },
    output: { ...grant, role: "viewer", expiresAtMs: null },
  },
  "grants.revoke": { input: { grantId: GRANT_ID }, output: {} },
  "grants.effective": {
    input: { sourceId: SOURCE_ID, accountId: ACCOUNT_ID },
    output: {
      sourceId: SOURCE_ID,
      accountId: ACCOUNT_ID,
      role: "editor",
      workspaceIds: ["ws_01", "ws_02"],
      expiresAtMs: grant.expiresAtMs,
      from: [
        {
          via: "group",
          grantId: GRANT_ID,
          role: "editor",
          workspaceIds: ["ws_01", "ws_02"],
          expiresAtMs: grant.expiresAtMs,
        },
        {
          via: "team",
          grantId: "gnt_01JBBBBBBBBBBBBBBBBBBBBBBB",
          role: "viewer",
          workspaceIds: ["ws_01"],
          expiresAtMs: null,
        },
      ],
    },
  },
  "accessRequests.create": {
    input: {
      sourceId: SOURCE_ID,
      role: "editor",
      reason: "Need to review the build",
    },
    output: accessRequest,
  },
  "accessRequests.list": {
    input: { box: "inbox", teamId: TEAM_ID, status: "pending" },
    output: { requests: [accessRequest] },
  },
  "accessRequests.approve": {
    input: { requestId: accessRequest.requestId, role: "viewer" },
    output: {
      request: {
        ...accessRequest,
        status: "approved",
        decidedBy: ACCOUNT_ID,
        decidedAtMs: NOW,
      },
      grant: { ...grant, subject: { kind: "member", accountId: ACCOUNT_ID } },
    },
  },
  "accessRequests.deny": {
    input: { requestId: accessRequest.requestId, reason: "Not needed" },
    output: {
      ...accessRequest,
      status: "denied",
      decidedBy: ACCOUNT_ID,
      decidedAtMs: NOW,
    },
  },
  "audit.list": {
    input: { teamId: TEAM_ID, fromMs: NOW - 86_400_000, toMs: NOW, limit: 100 },
    output: { events: [auditEvent], nextCursor: null },
  },
  "audit.export": {
    input: {
      teamId: TEAM_ID,
      fromMs: NOW - 86_400_000,
      toMs: NOW,
      format: "csv",
    },
    output: {
      format: "csv",
      rows: 1,
      content:
        "eventId,teamId,actorKind,actorId,action,targetKind,targetId,jti,ipPrefix,atMs\n",
    },
  },
  "billing.plan": {
    input: { teamId: TEAM_ID },
    output: {
      teamId: TEAM_ID,
      plan: "team",
      status: "trialing",
      seats: 5,
      periodEndsAtMs: NOW + 30 * 86_400_000,
      trialEndsAtMs: NOW + 14 * 86_400_000,
      cancelAtPeriodEnd: false,
    },
  },
  "billing.usage": {
    input: { teamId: TEAM_ID, period: "2026-10" },
    output: {
      teamId: TEAM_ID,
      period: "2026-10",
      usage: [{ metric: "relay.bytes", value: 1024, limit: null }],
    },
  },
  "billing.checkout": {
    input: {
      teamId: TEAM_ID,
      plan: "team",
      returnUrl: "https://app.armadra.test/teams/dev-team",
    },
    output: {
      url: "https://pay.armadra.test/checkout/cs_test",
      expiresAtMs: NOW + 3_600_000,
    },
  },
  "billing.portal": {
    input: {
      teamId: TEAM_ID,
      returnUrl: "https://app.armadra.test/teams/dev-team",
    },
    output: { url: "https://pay.armadra.test/portal/bps_test" },
  },
  "billing.webhook": {
    input: { provider: "stripe", id: "evt_test", type: "invoice.paid" },
    output: { received: true },
  },
  "apiTokens.create": {
    input: {
      name: "CI",
      teamId: TEAM_ID,
      scopes: ["directory:read"],
      expiresAtMs: apiToken.expiresAtMs,
    },
    output: { token: "armat_test-token", apiToken },
  },
  "apiTokens.list": {
    input: { teamId: TEAM_ID },
    output: { apiTokens: [apiToken] },
  },
  "apiTokens.revoke": { input: { tokenId: apiToken.tokenId }, output: {} },
  "admin.accounts": {
    input: { q: "dev", status: "active", limit: 100 },
    output: { accounts: [adminAccount], nextCursor: null },
  },
  "admin.account": { input: { accountId: ACCOUNT_ID }, output: adminAccount },
  "admin.accountStatus": {
    input: {
      accountId: ACCOUNT_ID,
      status: "disabled",
      reason: "abuse report",
    },
    output: { ...adminAccount, status: "disabled" },
  },
  "admin.teams": {
    input: { limit: 100 },
    output: { teams: [adminTeam], nextCursor: null },
  },
  "admin.team": { input: { teamId: TEAM_ID }, output: adminTeam },
  "admin.teamStatus": {
    input: { teamId: TEAM_ID, status: "suspended", reason: "abuse report" },
    output: { ...adminTeam, status: "suspended" },
  },
  "admin.relayNodes": { input: {}, output: { nodes: [adminNode] } },
  "admin.relayNodeDrain": {
    input: { node: "relay-1", drain: true },
    output: { ...adminNode, draining: true },
  },
  "admin.incident": {
    input: {},
    output: {
      pauseAssertions: false,
      pauseRegistrations: false,
      updatedAtMs: null,
    },
  },
  "admin.incidentSet": {
    input: { pauseRegistrations: true, reason: "spam wave" },
    output: {
      pauseAssertions: false,
      pauseRegistrations: true,
      updatedAtMs: NOW,
    },
  },
  "auth.emailVerify": {
    input: { token: "test-email-token" },
    output: { email: "dev@armadra.test", emailVerified: true },
  },
  "auth.emailVerifyResend": { input: {}, output: {} },
  "auth.passwordResetStart": {
    input: { email: "dev@armadra.test" },
    output: {},
  },
  "auth.passwordResetFinish": {
    input: {
      token: "test-email-token",
      newPassword: "another long passphrase here",
    },
    output: {},
  },
  "auth.oauthStart": {
    input: {
      provider: "github",
      redirectUri: "https://app.armadra.test/oauth/github",
    },
    output: {
      authorizationUrl: "https://github.test/login/oauth/authorize?state=st_01",
      state: "st_01",
      expiresAtMs: NOW + 600_000,
    },
  },
  "auth.oauthFinish": {
    input: {
      provider: "github",
      state: "st_01",
      code: "test-oauth-code",
      device,
    },
    outputs: [
      { session },
      { mfa: { challengeId: "mfa_01", methods: ["totp", "recovery"] } },
    ],
  },
  "auth.oauthUnlink": { input: { provider: "github" }, output: {} },
  "me.emailChange": {
    input: {
      newEmail: "dev2@armadra.test",
      password: "correct horse battery staple",
    },
    output: {},
  },
  "me.spaces": {
    input: {},
    output: {
      spaces: [
        { kind: "personal", plan: "free" },
        {
          kind: "team",
          teamId: TEAM_ID,
          slug: "dev-team",
          name: "Dev Team",
          role: "owner",
          plan: "free",
        },
      ],
    },
  },
  "me.securityLog": {
    input: { fromMs: NOW - 86_400_000, toMs: NOW, limit: 100 },
    output: { events: [securityEvent], nextCursor: null },
  },
  "me.delete": {
    input: { password: "correct horse battery staple" },
    output: { purgeAtMs: NOW + 7 * 86_400_000 },
  },
  "me.deleteCancel": { input: {}, output: {} },
  "me.export": {
    input: {},
    output: {
      exportedAtMs: NOW,
      account: me,
      devices: [
        {
          deviceId: "dev_01JAAAAAAAAAAAAAAAAAAAAAAA",
          platform: "desktop",
          name: "Dev laptop",
          lastSeenAtMs: NOW,
          current: true,
        },
      ],
      teams: me.teams,
      grants: [grant],
      securityLog: [securityEvent],
    },
  },
  "sources.transferStart": {
    input: { sourceId: SOURCE_ID, to: { kind: "team", teamId: TEAM_ID } },
    output: transfer,
  },
  "sources.transferList": {
    input: { sourceId: SOURCE_ID },
    output: { transfers: [transfer] },
  },
  "sources.transferAccept": {
    input: { transferId: transfer.transferId },
    output: {
      ...transfer,
      status: "completed",
      confirmedBy: { from: true, to: true },
    },
  },
  "sources.transferCancel": {
    input: { transferId: transfer.transferId },
    output: { ...transfer, status: "canceled" },
  },
  "relay.nodePresence": {
    input: {
      node: "cf",
      ttlMs: 120_000,
      events: [
        {
          type: "sourceOnline",
          sourceId: SOURCE_ID,
          tunnelId: "fedcba9876543210fedcba9876543210",
          coreVersion: "0.2.0",
          atMs: NOW,
        },
        {
          type: "sourceOffline",
          sourceId: SOURCE_ID,
          tunnelId: "0123456789abcdef0123456789abcdef",
          atMs: NOW - 1_000,
        },
      ],
    },
    output: { accepted: 2 },
  },
};
