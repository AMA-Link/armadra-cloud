/**
 * 源（中继设计 §6.4，平台设计 §17.3 / §17.5）：注册令牌、注册、源 JWS 换隧道令牌、源目录、
 * 撤销、访问断言 + 中继令牌。撤销判断见 `revocation.ts`。
 */
import { createPublicKey, verify } from "node:crypto";

import { PROTOCOL_VERSION } from "@armadra/platform-protocol";
import {
  AUDIENCES,
  checkTimes,
  ReplayCache,
  SCOPE_READ_ONLY,
  scopeClaims,
  sourceJwsClaimsSchema,
  splitCompactJws,
  TOKEN_TYPES,
  type Ed25519PublicJwk,
} from "@armadra/platform-protocol/assertion";
import type {
  DeviceInput,
  LinkScope,
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

import { ControlError } from "./errors.ts";
import type {
  ControlContext,
  DeviceRecord,
  SourceRecord,
  TenantSettings,
} from "./ports.ts";
import { randomToken, secretHash } from "./secrets.ts";
import type { Caller } from "./sessions.ts";

/** 同时有效的注册令牌上限。 */
const MAX_REGISTRATION_TOKENS = 20;

export function relayBaseUrl(issuer: string, sourceId: string): string {
  return `${issuer}/s/${sourceId}`;
}

/** 隧道节点：issuer 的 `https` → `wss`（`http` → `ws`）+ `/t/v1`。 */
export function tunnelUrl(issuer: string): string {
  return `${issuer.replace(/^http/, "ws")}/t/v1`;
}

/** 页面来源：设置里的可信来源 + issuer 本身（托管页面在同一来源的 `/app/`）。 */
export function trustedOriginsOf(
  settings: Pick<TenantSettings, "issuer" | "trustedOrigins">,
): string[] {
  return [...new Set([...settings.trustedOrigins, settings.issuer])];
}

export function requireOwner(caller: Caller): void {
  if (caller.role !== "owner") throw new ControlError("forbidden");
}

export function createSources(context: ControlContext) {
  const { tenantId, ports } = context;
  const replay = new ReplayCache(2 * SOURCE_JWS.ttlMs + ASSERTION.skewMs);

  async function liveSource(sourceId: string): Promise<SourceRecord | null> {
    const source = await ports.sources.get(tenantId, sourceId);
    return source && source.revokedAtMs === null ? source : null;
  }

  /** 调用方能看到的源：owner → 全部未撤销的；访客 → 链接指向的那一个。 */
  async function visibleSources(caller: Caller): Promise<SourceRecord[]> {
    if (caller.role === "owner")
      return (await ports.sources.list(tenantId)).filter(
        (row) => row.revokedAtMs === null,
      );
    const link = caller.linkId
      ? await ports.links.get(tenantId, caller.linkId)
      : null;
    if (!link || link.revokedAtMs !== null) return [];
    const source = await liveSource(link.sourceId);
    return source ? [source] : [];
  }

  async function summary(
    issuer: string,
    source: SourceRecord,
    caller: Caller,
  ): Promise<SourceSummary> {
    const route = await context.relay.route(source.sourceId);
    return {
      sourceId: source.sourceId,
      name: source.name,
      kind: source.kind,
      online: route !== null,
      lastSeenAtMs: route ? context.now() : source.lastSeenAtMs,
      owner: caller.role === "owner",
      via: caller.role === "owner" ? "owner" : "link",
      relayOrigin: issuer,
      coreVersion: route?.coreVersion ?? source.coreVersion,
    };
  }

  /**
   * 签断言 + 中继令牌（owner 或访客，已核过可见性）。`scope`（链接的分享范围）→ 断言 `scp`；
   * 只读 → 中继令牌 `ro: true`。没有范围时两者都不出现（与 0.2.x 签出的字节同形）。
   */
  async function grant(
    source: SourceRecord,
    device: DeviceRecord,
    extra: {
      name?: string;
      link?: { linkId: string; invitationId: string };
      device?: Pick<DeviceInput, "platform" | "name">;
      scope?: LinkScope;
    },
  ) {
    const issuer = context.signer.issuer;
    const scp = scopeClaims(extra.scope);
    const readOnly = scp.includes(SCOPE_READ_ONLY);
    const assertion = context.signer.sign(
      TOKEN_TYPES.assertion,
      {
        sub: device.subject,
        aud: source.sourceId,
        ...(extra.name ? { name: extra.name } : {}),
        ...(extra.link ? { link: extra.link } : {}),
        ...(scp.length > 0 ? { scp } : {}),
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
      {
        sub: device.subject,
        aud: AUDIENCES.relay,
        src: source.sourceId,
        ...(readOnly ? { ro: true } : {}),
      },
      {
        ttlMs: RELAY_TOKEN.ttlMs,
        jtiPrefix: `r.${device.deviceId}.${source.sourceId}`,
      },
    );
    const route = await context.relay.route(source.sourceId);
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

    async registrationToken(caller: Caller) {
      requireOwner(caller);
      const nowMs = context.now();
      const token = randomToken(32);
      const expiresAtMs = nowMs + REGISTRATION_TOKEN.ttlMs;
      const added = await ports.registrations.add(
        tenantId,
        {
          tokenHash: secretHash("registration", token),
          expiresAtMs,
          accountId: caller.subject,
        },
        { nowMs, max: MAX_REGISTRATION_TOKENS },
      );
      if (!added) throw new ControlError("limit_reached");
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
      const limited = await context.limits.hit("login.ip", `register:${ip}`);
      if (!limited.allowed)
        throw new ControlError("rate_limited", {
          retryAfterMs: limited.retryAfterMs,
        });
      const nowMs = context.now();
      const hash = secretHash("registration", input.registrationToken);
      // 一次性：先消耗，后面就算拒绝也不能再用。
      const consumed = await ports.registrations.consume(tenantId, hash, nowMs);
      if (!consumed) throw new ControlError("registration_token_invalid");
      if (input.protocol.major !== PROTOCOL_VERSION.major)
        throw new ControlError("protocol_unsupported");
      const publicJwk = {
        kty: "OKP" as const,
        crv: "Ed25519" as const,
        x: input.publicKey.x,
      };
      await ports.sources.put(tenantId, input.sourceId, (existing) => {
        if (
          existing &&
          existing.revokedAtMs === null &&
          existing.publicJwk.x !== publicJwk.x
        )
          throw new ControlError("source_taken");
        return {
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
      });
      const settings = await ports.tenants.get(tenantId);
      return {
        issuer: settings.issuer,
        jwksUrl: `${settings.issuer}/.well-known/jwks.json`,
        relayOrigins: [settings.issuer],
        trustedOrigins: trustedOriginsOf(settings),
        sourceId: input.sourceId,
        ownerAccountId: consumed.accountId,
      };
    },

    /** `Authorization: Source <jws>` → 已登记的源；失败 `unauthenticated` / `source_revoked`。 */
    async authenticateSource(
      header: string | undefined,
    ): Promise<SourceRecord> {
      const match = /^Source\s+(\S+)$/.exec(header ?? "");
      if (!match?.[1]) throw new ControlError("unauthenticated");
      let parts: ReturnType<typeof splitCompactJws>;
      try {
        parts = splitCompactJws(match[1]);
      } catch {
        throw new ControlError("unauthenticated");
      }
      if (
        parts.header.typ !== undefined &&
        parts.header.typ !== TOKEN_TYPES.source
      )
        throw new ControlError("unauthenticated");
      const source = await ports.sources.get(tenantId, parts.header.kid);
      if (!source) throw new ControlError("unauthenticated");
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
      if (!valid) throw new ControlError("unauthenticated");
      if (source.revokedAtMs !== null) throw new ControlError("source_revoked");
      const claims = sourceJwsClaimsSchema.safeParse(parts.payload);
      if (!claims.success) throw new ControlError("unauthenticated");
      const nowMs = context.now();
      const { iss, aud, iat, exp, jti } = claims.data;
      if (iss !== source.sourceId || aud !== context.signer.issuer)
        throw new ControlError("unauthenticated");
      if (checkTimes({ iat, exp }, { nowMs, expectedAud: aud }))
        throw new ControlError("unauthenticated");
      if ((exp - iat) * 1000 > 2 * SOURCE_JWS.ttlMs)
        throw new ControlError("unauthenticated");
      if (replay.seen(`${source.sourceId}:${jti}`, exp * 1000, nowMs))
        throw new ControlError("unauthenticated");
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
        nodes: [
          {
            url: context.tunnelUrl?.(source.sourceId) ?? tunnelUrl(issuer),
            weight: 1,
          },
        ],
        limits: {
          maxStreams: LIMITS.maxStreams,
          streamWindow: LIMITS.streamWindow,
          tunnelWindow: LIMITS.tunnelWindow,
          maxFrameBytes: LIMITS.maxFrameBytes,
        },
      };
    },

    async list(caller: Caller): Promise<SourceSummary[]> {
      const issuer = (await ports.tenants.get(tenantId)).issuer;
      return Promise.all(
        (await visibleSources(caller)).map((source) =>
          summary(issuer, source, caller),
        ),
      );
    },

    async get(caller: Caller, sourceId: string): Promise<SourceDetail> {
      const issuer = (await ports.tenants.get(tenantId)).issuer;
      const source = (await visibleSources(caller)).find(
        (row) => row.sourceId === sourceId,
      );
      if (!source) throw new ControlError("not_found");
      return {
        ...(await summary(issuer, source, caller)),
        trustedOrigins: [...source.trustedOrigins],
        capabilities: [...source.capabilities],
        registeredAtMs: source.registeredAtMs,
      };
    },

    /** 撤销：标记、关隧道、断开经中继的客户端 WS、通知 me/stream。 */
    async revoke(caller: Caller, sourceId: string): Promise<void> {
      requireOwner(caller);
      const nowMs = context.now();
      await ports.sources.update(tenantId, sourceId, (source) => {
        if (!source || source.revokedAtMs !== null)
          throw new ControlError("not_found");
        source.revokedAtMs = nowMs;
      });
      context.relay.closeTunnel(sourceId);
      context.relay.disconnect({ sourceId });
      context.emit({ type: "sourceRevoked", sourceId });
    },

    async assertion(
      caller: Caller,
      sourceId: string,
      device: Pick<DeviceInput, "platform" | "name"> | undefined,
    ) {
      const any = await ports.sources.get(tenantId, sourceId);
      if (any && any.revokedAtMs !== null)
        throw new ControlError("source_revoked");
      const source = (await visibleSources(caller)).find(
        (row) => row.sourceId === sourceId,
      );
      if (!source) throw new ControlError("source_access_denied");
      const record = await ports.devices.get(tenantId, caller.deviceId);
      if (!record) throw new ControlError("unauthenticated");
      const link =
        caller.role === "guest" && caller.linkId
          ? await ports.links.get(tenantId, caller.linkId)
          : null;
      const owner =
        caller.role === "owner"
          ? await ports.accounts.get(tenantId, caller.subject)
          : null;
      return grant(source, record, {
        name: caller.role === "owner" ? owner?.name : link?.label || undefined,
        ...(link
          ? { link: { linkId: link.linkId, invitationId: link.invitationId } }
          : {}),
        ...(link?.scope ? { scope: link.scope } : {}),
        ...(device ? { device } : {}),
      });
    },
  };
}

export type Sources = ReturnType<typeof createSources>;
