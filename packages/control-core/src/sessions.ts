/**
 * 会话（中继设计 §6.2）：访问令牌（EdDSA JWT，15 分钟，`aud: "cloud"`，`sid` = deviceId）
 * 与刷新令牌（30 天，`<deviceId>.<随机>`，存储只存哈希，每次刷新旋转）。
 *
 * - 旧刷新令牌再来 = 重放 → 撤销整台设备，答 `session_revoked`。
 * - 访问令牌校验除了签名与声明，还要求 `sid` 指向的设备未撤销、未过期——登出 / 改口令 /
 *   撤销链接即时生效，不等 15 分钟。
 * - `lastSeenAtMs` 只在内存里累计，每 5 分钟批量写一次。
 */
import { createPublicKey, type KeyObject } from "node:crypto";

import { verifyJwt } from "@armadra/cloud-shared/jwt";
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

import { ControlError } from "./errors.ts";
import type { ControlContext, DeviceRecord } from "./ports.ts";
import { randomHex, randomToken, refreshHash, sameHex } from "./secrets.ts";
import { guestLinkId } from "./subjects.ts";

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

/** 早已失效、可以从存储里清掉的设备行（失效超过一个刷新令牌寿命）。 */
export function isStaleDevice(device: DeviceRecord, nowMs: number): boolean {
  return (
    !isLiveDevice(device, nowMs) &&
    Math.max(device.revokedAtMs ?? 0, device.expiresAtMs) <=
      nowMs - SESSION.refreshTtlMs
  );
}

/** 建一台设备，答它与刷新令牌明文（只返回这一次）。 */
export async function createDevice(
  context: Pick<ControlContext, "tenantId" | "ports">,
  input: {
    subject: string;
    device: Pick<DeviceInput, "platform" | "name">;
    nowMs: number;
    ttlMs?: number;
  },
): Promise<{ device: DeviceRecord; refreshToken: string }> {
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
  await context.ports.devices.insert(
    context.tenantId,
    { ...device },
    input.nowMs,
  );
  return { device, refreshToken };
}

/** 线上显示名：账号本人是账号名，访客是链接备注（没有就是 `guest`）。 */
export async function displayNameOf(
  context: Pick<ControlContext, "tenantId" | "ports">,
  subject: string,
): Promise<string> {
  const account = await context.ports.accounts.get(context.tenantId, subject);
  if (account) return account.name;
  const linkId = guestLinkId(subject);
  const link = linkId
    ? await context.ports.links.get(context.tenantId, linkId)
    : null;
  return link?.label || "guest";
}

/** 给一台设备签访问令牌，拼成线上的 `Session`。 */
export async function sessionFor(
  context: ControlContext,
  device: DeviceRecord,
  refreshToken: string,
): Promise<Session> {
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
      displayName: await displayNameOf(context, device.subject),
      emailVerified: false,
    },
    device: { deviceId: device.deviceId },
  };
}

export async function refresh(
  context: ControlContext,
  refreshToken: string,
): Promise<Session> {
  const deviceId = refreshToken.split(".")[0] ?? "";
  if (!/^[0-9a-f]{32}$/.test(deviceId))
    throw new ControlError("session_revoked");
  const hash = refreshHash(refreshToken);
  const nowMs = context.now();
  type Outcome =
    | { error: "session_expired" | "session_revoked" }
    | { device: DeviceRecord; refreshToken: string };
  const outcome = await context.ports.devices.update(
    context.tenantId,
    deviceId,
    (device): Outcome => {
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
    },
  );
  if ("error" in outcome) throw new ControlError(outcome.error);
  return sessionFor(context, outcome.device, outcome.refreshToken);
}

export async function revokeDevice(
  context: Pick<ControlContext, "tenantId" | "ports" | "now">,
  deviceId: string,
): Promise<boolean> {
  const nowMs = context.now();
  const revoked = await context.ports.devices.update(
    context.tenantId,
    deviceId,
    (device) => {
      if (!device || device.revokedAtMs !== null) return null;
      device.revokedAtMs = nowMs;
      return device.subject;
    },
  );
  return revoked !== null;
}

function keyLookup(context: ControlContext) {
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
  /** 把累计的 `lastSeenAtMs` 写进存储。 */
  flush(): Promise<void>;
}

export function createAuthenticator(context: ControlContext): Authenticator {
  const key = keyLookup(context);
  const seen = new Map<string, number>();
  return {
    async authenticate(header) {
      const match = /^Bearer\s+(\S+)$/i.exec(header ?? "");
      if (!match?.[1]) throw new ControlError("unauthenticated");
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
      if (!result.ok) throw new ControlError("unauthenticated");
      const claims = result.claims;
      const device = await context.ports.devices.get(
        context.tenantId,
        claims.sid,
      );
      if (
        !device ||
        device.subject !== claims.sub ||
        !isLiveDevice(device, nowMs)
      )
        throw new ControlError("unauthenticated");
      const account = await context.ports.accounts.get(
        context.tenantId,
        claims.sub,
      );
      const owner = account !== null;
      const linkId = owner ? null : guestLinkId(claims.sub);
      if (!owner && linkId === null) throw new ControlError("unauthenticated");
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
    async flush() {
      if (seen.size === 0) return;
      const batch = new Map(seen);
      seen.clear();
      await context.ports.devices.touch(context.tenantId, batch);
    },
  };
}
