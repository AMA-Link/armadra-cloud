/**
 * 源（中继设计 §6.4，平台设计 §17.3 / §17.5）：注册令牌、注册、源 JWS 换隧道令牌、源目录、
 * 撤销、访问断言 + 中继令牌。
 *
 * 撤销靠 `jti` 的形状即时生效、重启后仍然有效（{@link isRevokedJti}）：
 *
 * - 隧道令牌 `t.<sourceId>.<随机>`：源被撤销或不在登记里 → 撤销；
 * - 中继令牌 `r.<deviceId>.<sourceId>.<随机>`：设备被撤销 / 过期、源被撤销，或访客的链接被撤销
 *   → 撤销；
 * - 访问令牌 `a.<deviceId>.<随机>`：同设备（校验在 sessions.ts）。
 */
import { createPublicKey, verify } from "node:crypto";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import {
  AUDIENCES,
  checkTimes,
  ReplayCache,
  sourceJwsClaimsSchema,
  splitCompactJws,
  TOKEN_TYPES,
  type Ed25519PublicJwk,
} from "@armadra/platform-protocol/assertion";
import type {
  DeviceInput,
  SourceDetail,
  SourceSummary,
} from "@armadra/platform-protocol/cloud-api";
import {
  ASSERTION,
  REGISTRATION_TOKEN,
  RELAY_TOKEN,
  SOURCE_JWS,
  TUNNEL_TOKEN,
} from "@armadra/platform-protocol/identity-vectors";
import { LIMITS } from "@armadra/platform-protocol/tunnel";

import {
  guestLinkId,
  ownerSubject,
  PersonalError,
  type PersonalContext,
} from "./context.ts";
import { randomToken, secretHash, sameHex } from "./secrets.ts";
import { isLiveDevice, type Caller } from "./sessions.ts";
import type { DeviceRecord, SourceRecord, State } from "./state.ts";

/** 同时有效的注册令牌上限。 */
const MAX_REGISTRATION_TOKENS = 20;

export function relayBaseUrl(issuer: string, sourceId: string): string {
  return `${issuer}/s/${sourceId}`;
}

/** 隧道节点：issuer 的 `https` → `wss`（`http` → `ws`）+ `/t/v1`。 */
export function tunnelUrl(issuer: string): string {
  return `${issuer.replace(/^http/, "ws")}/t/v1`;
}

/** 页面来源：issuer 本身（托管页面在同一来源的 `/app/`）。 */
export function trustedOriginsFor(state: State): string[] {
  return [...new Set([...state.settings.trustedOrigins, state.issuer])];
}

/** 按 `jti` 判断令牌是否已撤销（见文件头）。 */
export function isRevokedJti(
  state: State,
  jti: string,
  nowMs: number,
): boolean {
  const [kind, id, sourceId] = jti.split(".");
  if (kind === "t") {
    const source = state.sources.find((row) => row.sourceId === id);
    return !source || source.revokedAtMs !== null;
  }
  if (kind === "r" || kind === "a") {
    const device = state.devices.find((row) => row.deviceId === id);
    if (!device || !isLiveDevice(device, nowMs)) return true;
    if (kind === "r") {
      const source = state.sources.find((row) => row.sourceId === sourceId);
      if (!source || source.revokedAtMs !== null) return true;
    }
    const linkId = guestLinkId(device.subject);
    if (linkId === null) return false;
    const link = state.links.find((row) => row.linkId === linkId);
    return !link || link.revokedAtMs !== null;
  }
  return false;
}

function liveSource(state: State, sourceId: string): SourceRecord | undefined {
  return state.sources.find(
    (row) => row.sourceId === sourceId && row.revokedAtMs === null,
  );
}

/** 调用方能看到的源：owner → 全部未撤销的；访客 → 链接指向的那一个。 */
function visibleSources(state: State, caller: Caller): SourceRecord[] {
  if (caller.role === "owner")
    return state.sources.filter((row) => row.revokedAtMs === null);
  const link = state.links.find(
    (row) => row.linkId === caller.linkId && row.revokedAtMs === null,
  );
  if (!link) return [];
  const source = liveSource(state, link.sourceId);
  return source ? [source] : [];
}

export function requireOwner(caller: Caller): void {
  if (caller.role !== "owner") throw new PersonalError("forbidden");
}

export function createSources(context: PersonalContext) {
  const replay = new ReplayCache(2 * SOURCE_JWS.ttlMs + ASSERTION.skewMs);

  async function summary(
    state: State,
    source: SourceRecord,
    caller: Caller,
  ): Promise<SourceSummary> {
    const route = await context.control.routes.get(source.sourceId);
    return {
      sourceId: source.sourceId,
      name: source.name,
      kind: source.kind,
      online: route !== null,
      lastSeenAtMs: route ? context.now() : source.lastSeenAtMs,
      owner: caller.role === "owner",
      via: caller.role === "owner" ? "owner" : "link",
      relayOrigin: state.issuer,
      coreVersion: route?.coreVersion ?? source.coreVersion,
    };
  }

  /** 签断言 + 中继令牌（owner 或访客，已核过可见性）。 */
  async function grant(
    source: SourceRecord,
    device: DeviceRecord,
    extra: {
      name?: string;
      link?: { linkId: string; invitationId: string };
      device?: Pick<DeviceInput, "platform" | "name">;
    },
  ) {
    const issuer = context.signer.issuer;
    const assertion = context.signer.sign(
      TOKEN_TYPES.assertion,
      {
        sub: device.subject,
        aud: source.sourceId,
        ...(extra.name ? { name: extra.name } : {}),
        ...(extra.link ? { link: extra.link } : {}),
        device: {
          deviceId: device.deviceId,
          platform: extra.device?.platform ?? device.platform,
          name: extra.device?.name ?? device.name,
        },
      },
      { ttlMs: ASSERTION.ttlMs },
    );
    const relay = context.signer.sign(
      TOKEN_TYPES.relay,
      { sub: device.subject, aud: AUDIENCES.relay, src: source.sourceId },
      {
        ttlMs: RELAY_TOKEN.ttlMs,
        jtiPrefix: `r.${device.deviceId}.${source.sourceId}`,
      },
    );
    const route = await context.control.routes.get(source.sourceId);
    return {
      assertion: assertion.token,
      assertionExpiresAtMs: assertion.expiresAtMs,
      relayToken: relay.token,
      relayTokenExpiresAtMs: relay.expiresAtMs,
      relayOrigin: issuer,
      relayBaseUrl: relayBaseUrl(issuer, source.sourceId),
      online: route !== null,
    };
  }

  return {
    grant,

    registrationToken(caller: Caller) {
      requireOwner(caller);
      const nowMs = context.now();
      const token = randomToken(32);
      const expiresAtMs = nowMs + REGISTRATION_TOKEN.ttlMs;
      context.store.update((draft) => {
        draft.registrationTokens = draft.registrationTokens.filter(
          (row) => row.expiresAtMs > nowMs,
        );
        if (draft.registrationTokens.length >= MAX_REGISTRATION_TOKENS)
          throw new PersonalError("limit_reached");
        draft.registrationTokens.push({
          tokenHash: secretHash("registration", token),
          expiresAtMs,
        });
      });
      return {
        registrationToken: token,
        expiresAtMs,
        issuer: context.signer.issuer,
      };
    },

    async register(
      input: {
        registrationToken: string;
        sourceId: string;
        publicKey: Ed25519PublicJwk;
        name: string;
        kind: SourceRecord["kind"];
        coreVersion: string;
        capabilities: string[];
        protocol: { major: number; minor: number };
      },
      ip: string,
    ) {
      const limited = await context.control.limits.hit(
        "login.ip",
        `register:${ip}`,
      );
      if (!limited.allowed)
        throw new PersonalError("rate_limited", {
          retryAfterMs: limited.retryAfterMs,
        });
      const nowMs = context.now();
      const hash = secretHash("registration", input.registrationToken);
      context.store.update((draft) => {
        const index = draft.registrationTokens.findIndex(
          (row) => row.expiresAtMs > nowMs && sameHex(row.tokenHash, hash),
        );
        if (index < 0) throw new PersonalError("registration_token_invalid");
        // 一次性：先消耗，后面就算拒绝也不能再用。
        draft.registrationTokens.splice(index, 1);
      });
      if (input.protocol.major !== PROTOCOL_VERSION.major)
        throw new PersonalError("protocol_unsupported");
      const publicJwk = {
        kty: "OKP" as const,
        crv: "Ed25519" as const,
        x: input.publicKey.x,
      };
      const state = context.store.update((draft) => {
        const existing = draft.sources.find(
          (row) => row.sourceId === input.sourceId,
        );
        if (
          existing &&
          existing.revokedAtMs === null &&
          existing.publicJwk.x !== publicJwk.x
        )
          throw new PersonalError("source_taken");
        const record: SourceRecord = {
          sourceId: input.sourceId,
          name: input.name,
          kind: input.kind,
          publicJwk,
          coreVersion: input.coreVersion,
          capabilities: input.capabilities,
          registeredAtMs:
            existing && existing.revokedAtMs === null
              ? existing.registeredAtMs
              : nowMs,
          lastSeenAtMs: existing?.lastSeenAtMs ?? null,
          revokedAtMs: null,
          trustedOrigins: existing?.trustedOrigins ?? [],
        };
        draft.sources = draft.sources.filter(
          (row) => row.sourceId !== input.sourceId,
        );
        draft.sources.push(record);
        return draft;
      });
      return {
        issuer: state.issuer,
        jwksUrl: `${state.issuer}/.well-known/jwks.json`,
        relayOrigins: [state.issuer],
        trustedOrigins: trustedOriginsFor(state),
        sourceId: input.sourceId,
        ownerAccountId: ownerSubject(state.account.name),
      };
    },

    /** `Authorization: Source <jws>` → 已登记的源；失败 `unauthenticated` / `source_revoked`。 */
    authenticateSource(header: string | undefined): SourceRecord {
      const match = /^Source\s+(\S+)$/.exec(header ?? "");
      if (!match?.[1]) throw new PersonalError("unauthenticated");
      let parts: ReturnType<typeof splitCompactJws>;
      try {
        parts = splitCompactJws(match[1]);
      } catch {
        throw new PersonalError("unauthenticated");
      }
      if (
        parts.header.typ !== undefined &&
        parts.header.typ !== TOKEN_TYPES.source
      )
        throw new PersonalError("unauthenticated");
      const state = context.store.current;
      const source = state.sources.find(
        (row) => row.sourceId === parts.header.kid,
      );
      if (!source) throw new PersonalError("unauthenticated");
      let valid = false;
      try {
        valid = verify(
          null,
          parts.signingInput,
          createPublicKey({ key: source.publicJwk, format: "jwk" }),
          parts.signature,
        );
      } catch {
        valid = false;
      }
      if (!valid) throw new PersonalError("unauthenticated");
      if (source.revokedAtMs !== null)
        throw new PersonalError("source_revoked");
      const claims = sourceJwsClaimsSchema.safeParse(parts.payload);
      if (!claims.success) throw new PersonalError("unauthenticated");
      const nowMs = context.now();
      const { iss, aud, iat, exp, jti } = claims.data;
      if (iss !== source.sourceId || aud !== state.issuer)
        throw new PersonalError("unauthenticated");
      if (checkTimes({ iat, exp }, { nowMs, expectedAud: aud }))
        throw new PersonalError("unauthenticated");
      if ((exp - iat) * 1000 > 2 * SOURCE_JWS.ttlMs)
        throw new PersonalError("unauthenticated");
      if (replay.seen(`${source.sourceId}:${jti}`, exp * 1000, nowMs))
        throw new PersonalError("unauthenticated");
      return source;
    },

    /** `GET /v1/sources/me/relay`：签隧道令牌（`cnf.jwk` = 登记的公钥）。 */
    relayFor(source: SourceRecord) {
      const issuer = context.signer.issuer;
      const tunnel = context.signer.sign(
        TOKEN_TYPES.tunnel,
        {
          sub: source.sourceId,
          aud: AUDIENCES.tunnel,
          cnf: {
            jwk: {
              kty: source.publicJwk.kty,
              crv: source.publicJwk.crv,
              x: source.publicJwk.x,
            },
          },
        },
        { ttlMs: TUNNEL_TOKEN.ttlMs, jtiPrefix: `t.${source.sourceId}` },
      );
      return {
        tunnelToken: tunnel.token,
        expiresAtMs: tunnel.expiresAtMs,
        nodes: [{ url: tunnelUrl(issuer), weight: 1 }],
        limits: {
          maxStreams: LIMITS.maxStreams,
          streamWindow: LIMITS.streamWindow,
          tunnelWindow: LIMITS.tunnelWindow,
          maxFrameBytes: LIMITS.maxFrameBytes,
        },
      };
    },

    async list(caller: Caller): Promise<SourceSummary[]> {
      const state = context.store.current;
      return Promise.all(
        visibleSources(state, caller).map((source) =>
          summary(state, source, caller),
        ),
      );
    },

    async get(caller: Caller, sourceId: string): Promise<SourceDetail> {
      const state = context.store.current;
      const source = visibleSources(state, caller).find(
        (row) => row.sourceId === sourceId,
      );
      if (!source) throw new PersonalError("not_found");
      return {
        ...(await summary(state, source, caller)),
        trustedOrigins: [...source.trustedOrigins],
        capabilities: [...source.capabilities],
        registeredAtMs: source.registeredAtMs,
      };
    },

    /** 撤销：标记、关隧道、断开经中继的客户端 WS、通知 me/stream。 */
    revoke(caller: Caller, sourceId: string): void {
      requireOwner(caller);
      const nowMs = context.now();
      context.store.update((draft) => {
        const source = draft.sources.find(
          (row) => row.sourceId === sourceId && row.revokedAtMs === null,
        );
        if (!source) throw new PersonalError("not_found");
        source.revokedAtMs = nowMs;
      });
      context.control.command({ type: "closeTunnel", sourceId });
      context.disconnect({ sourceId });
      context.emit({ type: "sourceRevoked", sourceId });
    },

    async assertion(
      caller: Caller,
      sourceId: string,
      device: Pick<DeviceInput, "platform" | "name"> | undefined,
    ) {
      const state = context.store.current;
      const any = state.sources.find((row) => row.sourceId === sourceId);
      if (any && any.revokedAtMs !== null)
        throw new PersonalError("source_revoked");
      const source = visibleSources(state, caller).find(
        (row) => row.sourceId === sourceId,
      );
      if (!source) throw new PersonalError("source_access_denied");
      const record = state.devices.find(
        (row) => row.deviceId === caller.deviceId,
      );
      if (!record) throw new PersonalError("unauthenticated");
      const link =
        caller.role === "guest"
          ? state.links.find((row) => row.linkId === caller.linkId)
          : undefined;
      return grant(source, record, {
        name:
          caller.role === "owner"
            ? state.account.name
            : link?.label || undefined,
        ...(link
          ? { link: { linkId: link.linkId, invitationId: link.invitationId } }
          : {}),
        ...(device ? { device } : {}),
      });
    },
  };
}

export type Sources = ReturnType<typeof createSources>;
