/**
 * 会话（中继设计 §6.2）：访问令牌（EdDSA JWT，15 分钟，`aud: "cloud"`，`sid` = deviceId）
 * 与刷新令牌（30 天，`<deviceId>.<随机>`，状态文件只存哈希，每次刷新旋转）。
 *
 * - 旧刷新令牌再来 = 重放 → 撤销整台设备，答 `session_revoked`。
 * - 访问令牌校验除了签名与声明，还要求 `sid` 指向的设备未撤销、未过期——登出 / 改口令 /
 *   撤销链接即时生效，不等 15 分钟。
 * - `lastSeenAtMs` 只在内存里累计，每 5 分钟批量写一次。
 */
import { createPublicKey, type KeyObject } from "node:crypto";

import {
  accessClaimsSchema,
  AUDIENCES,
  TOKEN_TYPES,
  type DevicePlatform,
} from "@armadra/platform-protocol/assertion";
import type {
  DeviceInput,
  Session,
} from "@armadra/platform-protocol/cloud-api";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";

import { verifyJwt } from "../auth/jwt.ts";
import {
  guestLinkId,
  ownerSubject,
  PersonalError,
  type PersonalContext,
} from "./context.ts";
import { randomHex, randomToken, refreshHash, sameHex } from "./secrets.ts";
import type { DeviceRecord, State } from "./state.ts";

export const LAST_SEEN_FLUSH_MS = 5 * 60_000;

/** 已认证的调用方。 */
export interface Caller {
  subject: string;
  deviceId: string;
  /** owner：账号本人；guest：经链接进来的访客。 */
  role: "owner" | "guest";
  /** 访客的链接。 */
  linkId: string | null;
}

export function isLiveDevice(device: DeviceRecord, nowMs: number): boolean {
  return device.revokedAtMs === null && device.expiresAtMs > nowMs;
}

/** 在状态副本上建一台设备，答它与刷新令牌明文（只返回这一次）。 */
export function createDevice(
  draft: State,
  input: {
    subject: string;
    device: Pick<DeviceInput, "platform" | "name">;
    nowMs: number;
    ttlMs?: number;
  },
): { device: DeviceRecord; refreshToken: string } {
  const deviceId = randomHex(16);
  const refreshToken = `${deviceId}.${randomToken(32)}`;
  const device: DeviceRecord = {
    deviceId,
    subject: input.subject,
    platform: input.device.platform as DevicePlatform,
    name: input.device.name,
    refreshHash: refreshHash(refreshToken),
    previousRefreshHash: null,
    rotation: 1,
    createdAtMs: input.nowMs,
    expiresAtMs: input.nowMs + (input.ttlMs ?? SESSION.refreshTtlMs),
    revokedAtMs: null,
    lastSeenAtMs: input.nowMs,
  };
  // 顺手清掉早已失效的设备行，状态文件不无限长。
  draft.devices = draft.devices.filter(
    (row) =>
      isLiveDevice(row, input.nowMs) ||
      Math.max(row.revokedAtMs ?? 0, row.expiresAtMs) >
        input.nowMs - SESSION.refreshTtlMs,
  );
  draft.devices.push(device);
  return { device, refreshToken };
}

function displayNameOf(context: PersonalContext, subject: string): string {
  const state = context.store.current;
  if (subject === ownerSubject(state.account.name)) return state.account.name;
  const linkId = guestLinkId(subject);
  const link = state.links.find((row) => row.linkId === linkId);
  return link?.label || "guest";
}

/** 给一台设备签访问令牌，拼成线上的 `Session`。 */
export function sessionFor(
  context: PersonalContext,
  device: DeviceRecord,
  refreshToken: string,
): Session {
  const access = context.signer.sign(
    TOKEN_TYPES.access,
    { sub: device.subject, aud: AUDIENCES.access, sid: device.deviceId },
    { ttlMs: SESSION.accessTtlMs, jtiPrefix: `a.${device.deviceId}` },
  );
  return {
    accessToken: access.token,
    refreshToken,
    accessExpiresAtMs: access.expiresAtMs,
    expiresAtMs: device.expiresAtMs,
    account: {
      accountId: device.subject,
      email: null,
      displayName: displayNameOf(context, device.subject),
      emailVerified: false,
    },
    device: { deviceId: device.deviceId },
  };
}

export function refresh(
  context: PersonalContext,
  refreshToken: string,
): Session {
  const deviceId = refreshToken.split(".")[0] ?? "";
  if (!/^[0-9a-f]{32}$/.test(deviceId))
    throw new PersonalError("session_revoked");
  const hash = refreshHash(refreshToken);
  const nowMs = context.now();
  type Outcome =
    | { error: "session_expired" | "session_revoked" }
    | { device: DeviceRecord; refreshToken: string };
  const outcome = context.store.update((draft): Outcome => {
    const device = draft.devices.find((row) => row.deviceId === deviceId);
    if (!device || device.revokedAtMs !== null)
      return { error: "session_revoked" };
    if (device.expiresAtMs <= nowMs) return { error: "session_expired" };
    if (sameHex(hash, device.refreshHash)) {
      const next = `${device.deviceId}.${randomToken(32)}`;
      device.previousRefreshHash = device.refreshHash;
      device.refreshHash = refreshHash(next);
      device.rotation += 1;
      device.lastSeenAtMs = nowMs;
      return { device: { ...device }, refreshToken: next };
    }
    if (
      device.previousRefreshHash !== null &&
      sameHex(hash, device.previousRefreshHash)
    ) {
      // 用过的令牌又来了：要么被偷，要么被重放。整台设备作废。
      device.revokedAtMs = nowMs;
      return { error: "session_revoked" };
    }
    return { error: "session_revoked" };
  });
  if ("error" in outcome) throw new PersonalError(outcome.error);
  return sessionFor(context, outcome.device, outcome.refreshToken);
}

export function revokeDevice(
  context: PersonalContext,
  deviceId: string,
): boolean {
  const nowMs = context.now();
  const revoked = context.store.update((draft) => {
    const device = draft.devices.find((row) => row.deviceId === deviceId);
    if (!device || device.revokedAtMs !== null) return null;
    device.revokedAtMs = nowMs;
    return device.subject;
  });
  return revoked !== null;
}

function keyLookup(context: PersonalContext) {
  const cache = new Map<string, KeyObject>();
  return async (kid: string): Promise<KeyObject | null> => {
    const jwk = context.signer.jwks().keys.find((key) => key.kid === kid);
    if (!jwk) return null;
    let key = cache.get(`${kid}:${jwk.x}`);
    if (!key) {
      key = createPublicKey({
        key: { kty: jwk.kty, crv: jwk.crv, x: jwk.x },
        format: "jwk",
      });
      cache.set(`${kid}:${jwk.x}`, key);
    }
    return key;
  };
}

export interface Authenticator {
  /** `Authorization: Bearer <访问令牌>` → 调用方；缺或无效 → `unauthenticated`。 */
  authenticate(header: string | undefined): Promise<Caller>;
  /** 记一次「最近使用」（内存，批量落盘）。 */
  touch(deviceId: string): void;
  /** 把累计的 `lastSeenAtMs` 写进状态文件。 */
  flush(): void;
}

export function createAuthenticator(context: PersonalContext): Authenticator {
  const key = keyLookup(context);
  const seen = new Map<string, number>();
  return {
    async authenticate(header) {
      const match = /^Bearer\s+(\S+)$/i.exec(header ?? "");
      if (!match?.[1]) throw new PersonalError("unauthenticated");
      const nowMs = context.now();
      const result = await verifyJwt(match[1], {
        typ: TOKEN_TYPES.access,
        schema: accessClaimsSchema,
        expectedAud: AUDIENCES.access,
        expectedIss: context.signer.issuer,
        nowMs,
        key,
        isRevoked: async () => false,
      });
      if (!result.ok) throw new PersonalError("unauthenticated");
      const claims = result.claims;
      const state = context.store.current;
      const device = state.devices.find((row) => row.deviceId === claims.sid);
      if (
        !device ||
        device.subject !== claims.sub ||
        !isLiveDevice(device, nowMs)
      )
        throw new PersonalError("unauthenticated");
      const owner = claims.sub === ownerSubject(state.account.name);
      const linkId = owner ? null : guestLinkId(claims.sub);
      if (!owner && linkId === null) throw new PersonalError("unauthenticated");
      seen.set(device.deviceId, nowMs);
      return {
        subject: claims.sub,
        deviceId: device.deviceId,
        role: owner ? "owner" : "guest",
        linkId,
      };
    },
    touch(deviceId) {
      seen.set(deviceId, context.now());
    },
    flush() {
      if (seen.size === 0) return;
      const batch = new Map(seen);
      seen.clear();
      context.store.update((draft) => {
        for (const device of draft.devices) {
          const at = batch.get(device.deviceId);
          if (at !== undefined && (device.lastSeenAtMs ?? 0) < at)
            device.lastSeenAtMs = at;
        }
      });
    },
  };
}
