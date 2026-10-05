/**
 * personal 单测的装配：临时数据目录里的状态文件 + 签名钥 + 内存控制面 + 假时钟。
 * 口令派生是真的 scrypt（每次约几十毫秒），所以一个文件里只建一次账号。
 */
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { TOKEN_TYPES } from "@armadra/platform-protocol/assertion";
import { TEST_KEYS } from "@armadra/platform-protocol/fixtures";

import { createMemoryControlPlane } from "../src/control/memory.ts";
import type { EdgeConnectionFilter } from "../src/edge/server.ts";
import {
  createEventBus,
  type PersonalContext,
  type PersonalEvent,
} from "../src/personal/context.ts";
import { createState } from "../src/personal/init.ts";
import { Signer } from "../src/personal/signer.ts";
import { isRevokedJti } from "../src/personal/sources.ts";
import { signJwt, SOURCE_ID, sourceKey } from "./tokens.ts";

export const PASSWORD = "plover-quartz-lantern-41";
export const ISSUER = "https://relay.test:8443";

export interface Harness {
  dir: string;
  context: PersonalContext;
  clock: { now: number };
  events: PersonalEvent[];
  disconnects: EdgeConnectionFilter[];
  cleanup(): void;
}

export async function personalHarness(
  options: { account?: string; nowMs?: number } = {},
): Promise<Harness> {
  const dir = mkdtempSync(join(tmpdir(), "relay-personal-"));
  const clock = { now: options.nowMs ?? Date.UTC(2026, 9, 6, 12) };
  const now = () => clock.now;
  const store = await createState({
    dataDir: join(dir, "data"),
    account: options.account ?? "me",
    password: PASSWORD,
    issuer: ISSUER,
    nowMs: clock.now,
  });
  const signer = new Signer(() => store.current, now);
  const control = createMemoryControlPlane({
    issuer: ISSUER,
    jwks: () => signer.jwks(),
    revoked: (jti) => isRevokedJti(store.current, jti, now()),
    now,
  });
  const events: PersonalEvent[] = [];
  const disconnects: EdgeConnectionFilter[] = [];
  const bus = createEventBus();
  bus.subscribe((event) => events.push(event));
  return {
    dir,
    clock,
    events,
    disconnects,
    context: {
      store,
      signer,
      control,
      now,
      disconnect: (filter) => disconnects.push(filter),
      ...bus,
    },
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

export const DEVICE = { platform: "desktop" as const, name: "test-mac" };

// ------------------------------------------------------------ 源（core 那一侧）

export { SOURCE_ID, sourceKey };
export const SOURCE_PUBLIC_JWK = {
  kty: "OKP" as const,
  crv: "Ed25519" as const,
  x: TEST_KEYS.source.publicJwk.x,
};

/** core 对控制面的源 JWS：头 `kid = sourceId`，`iss = sourceId`、`aud = issuer`、60 秒。 */
export function sourceJws(
  issuer: string,
  nowMs: number,
  overrides: Record<string, unknown> = {},
  options: { sourceId?: string; key?: typeof sourceKey } = {},
): string {
  const sourceId = options.sourceId ?? SOURCE_ID;
  const iat = Math.floor(nowMs / 1000);
  return signJwt(
    TOKEN_TYPES.source,
    {
      iss: sourceId,
      aud: issuer,
      iat,
      exp: iat + 60,
      jti: randomBytes(12).toString("hex"),
      ...overrides,
    },
    { kid: sourceId, key: options.key ?? sourceKey },
  );
}

export function registerInput(registrationToken: string) {
  return {
    registrationToken,
    sourceId: SOURCE_ID,
    publicKey: SOURCE_PUBLIC_JWK,
    name: "我的 MacBook",
    kind: "desktop" as const,
    coreVersion: "0.2.0-test",
    capabilities: ["terminal"],
    protocol: { major: 1, minor: 0 },
  };
}
