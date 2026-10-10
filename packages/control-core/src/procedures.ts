/**
 * personal 实现的 procedure（平台设计 §17.3 的子集表，契约 §10）：服务 → 契约输出形状。
 * 交给 `createCloudRouter`；没列出的 procedure 由装配方答 `404 not_found`。
 */
import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import { PLATFORM_NAME_MAX } from "@armadra/platform-protocol/cloud-api";

import { createAccounts } from "./accounts.ts";
import { ControlError } from "./errors.ts";
import { createLinks } from "./links.ts";
import type { MeStreamCore } from "./me-stream.ts";
import type { ControlContext } from "./ports.ts";
import type { ProcedureHandler } from "./router.ts";
import {
  createAuthenticator,
  isLiveDevice,
  refresh,
  revokeDevice,
  type Authenticator,
} from "./sessions.ts";
import { createSources } from "./sources.ts";

/** personal 报的能力（契约 `PLATFORM_CAPABILITIES` 的子集）。 */
export const PERSONAL_CAPABILITIES = [
  "auth.password",
  "links.source-invite",
  "links.update",
  "links.scope",
  "me.stream",
] as const;

/** personal 实现的 procedure（平台设计 §17.3 的子集表）；其余答 404。 */
export const PERSONAL_PROCEDURES = [
  "platform.info",
  "platform.jwks",
  "auth.login",
  "auth.refresh",
  "auth.logout",
  "me.get",
  "me.sources",
  "me.devices",
  "me.deviceRevoke",
  "me.streamTicket",
  "sources.registrationToken",
  "sources.register",
  "sources.heartbeatHint",
  "sources.list",
  "sources.get",
  "sources.revoke",
  "sources.assertion",
  "links.create",
  "links.get",
  "links.accept",
  "links.list",
  "links.update",
  "links.revoke",
] as const;
type PersonalProcedure = (typeof PERSONAL_PROCEDURES)[number];

export interface PersonalProcedures {
  handlers: Record<PersonalProcedure, ProcedureHandler>;
  authenticator: Authenticator;
}

export function createPersonalProcedures(
  context: ControlContext,
  options: {
    stream: MeStreamCore;
    /** 托管页面的地址；没有页面产物时为 null。 */
    webApp: () => string | null;
    /** 服务端名称（`platform.info.name`，0.3.1 起）；不给或为空不报。 */
    name?: () => string | null;
  },
): PersonalProcedures {
  const { tenantId, ports } = context;
  const authenticator = createAuthenticator(context);
  const accounts = createAccounts(context);
  const sources = createSources(context);
  const links = createLinks(context, sources);

  const handlers: Record<PersonalProcedure, ProcedureHandler> = {
    "platform.info": () => {
      const name = options.name?.()?.trim().slice(0, PLATFORM_NAME_MAX) ?? "";
      return {
        mode: "personal",
        ...(name === "" ? {} : { name }),
        issuer: context.signer.issuer,
        protocol: { ...PROTOCOL_VERSION },
        capabilities: [...PERSONAL_CAPABILITIES],
        relay: { addressing: "path" },
        webApp: options.webApp(),
      };
    },
    "platform.jwks": () => context.signer.jwks(),
    "auth.login": ({ input, ip }) =>
      accounts
        .login(input as unknown as Parameters<typeof accounts.login>[0], ip)
        .then((session) => ({ session })),
    "auth.refresh": async ({ input }) => ({
      session: await refresh(context, input.refreshToken as string),
    }),
    "auth.logout": async ({ caller }) => {
      await revokeDevice(context, (await caller()).deviceId);
      return {};
    },
    "me.get": async ({ caller }) => {
      const who = await caller();
      const displayName =
        who.role === "owner"
          ? ((await ports.accounts.get(tenantId, who.subject))?.name ?? "")
          : ((who.linkId
              ? (await ports.links.get(tenantId, who.linkId))?.label
              : undefined) ?? "guest");
      return {
        accountId: who.subject,
        email: null,
        displayName,
        emailVerified: false,
        orgs: [],
        mfaEnabled: false,
      };
    },
    "me.sources": async ({ caller }) => ({
      sources: await sources.list(await caller()),
    }),
    "me.devices": async ({ caller }) => {
      const who = await caller();
      const nowMs = context.now();
      const devices = await ports.devices.listBySubject(tenantId, who.subject);
      return {
        devices: devices
          .filter((row) => isLiveDevice(row, nowMs))
          .map((row) => ({
            deviceId: row.deviceId,
            platform: row.platform,
            name: row.name,
            lastSeenAtMs: row.lastSeenAtMs,
            current: row.deviceId === who.deviceId,
          })),
      };
    },
    "me.deviceRevoke": async ({ input, caller }) => {
      const who = await caller();
      const device = await ports.devices.get(
        tenantId,
        input.deviceId as string,
      );
      if (
        !device ||
        device.subject !== who.subject ||
        !(await revokeDevice(context, device.deviceId))
      )
        throw new ControlError("not_found");
      return {};
    },
    "me.streamTicket": async ({ caller }) =>
      options.stream.ticket(await caller()),
    "sources.registrationToken": async ({ caller }) =>
      sources.registrationToken(await caller()),
    "sources.register": ({ input, ip }) =>
      sources.register(
        input as unknown as Parameters<typeof sources.register>[0],
        ip,
      ),
    "sources.heartbeatHint": async ({ authorization }) =>
      sources.relayFor(await sources.authenticateSource(authorization)),
    "sources.list": async ({ caller }) => ({
      sources: await sources.list(await caller()),
    }),
    "sources.get": async ({ input, caller }) =>
      sources.get(await caller(), input.sourceId as string),
    "sources.revoke": async ({ input, caller }) => {
      await sources.revoke(await caller(), input.sourceId as string);
      return {};
    },
    "sources.assertion": async ({ input, caller }) =>
      sources.assertion(
        await caller(),
        input.sourceId as string,
        input.device as Parameters<typeof sources.assertion>[2],
      ),
    "links.create": async ({ input, caller }) =>
      links.create(
        await caller(),
        input as unknown as Parameters<typeof links.create>[1],
      ),
    "links.get": ({ input }) => links.get(input.linkId as string),
    "links.accept": ({ input, ip }) =>
      links.accept(input as unknown as Parameters<typeof links.accept>[0], ip),
    "links.list": async ({ input, caller }) => ({
      links: await links.list(
        await caller(),
        input.sourceId as string | undefined,
      ),
    }),
    "links.update": async ({ input, caller }) =>
      links.update(await caller(), {
        linkId: input.linkId as string,
        label: input.label as string,
      }),
    "links.revoke": async ({ input, caller }) => {
      await links.revoke(await caller(), input.linkId as string);
      return {};
    },
  };

  return { handlers, authenticator };
}
