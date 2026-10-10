/**
 * personal 单测的装配：临时数据目录里的存储（`file` 或 `sqlite`，控制核端口）+ 签名钥 + 内存控制面
 * + 假时钟。口令派生是真的 scrypt（每次约几十毫秒），所以一个文件里每种存储只建一次账号。
 */
import { randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { ControlEvent } from "@armadra/control-core/events";
import type { ConnectionFilter } from "@armadra/control-core/ports";
import { TOKEN_TYPES } from "@armadra/platform-protocol/assertion";
import { TEST_KEYS } from "@armadra/platform-protocol/fixtures";
import { createMemoryControlPlane } from "@armadra/relay-kernel/control/memory";
import {
  signJwt,
  SOURCE_ID,
  sourceKey,
} from "@armadra/relay-kernel/testing/tokens";

import {
  createPersonalContext,
  stateRevocation,
  stateSigner,
  type PersonalContext,
} from "../src/personal/context.ts";
import { createState } from "../src/personal/init.ts";
import type { StoreKind } from "../src/personal/store-types.ts";

export const PASSWORD = "plover-quartz-lantern-41";
export const ISSUER = "https://relay.test:8443";

export interface Harness {
  dir: string;
  context: PersonalContext;
  clock: { now: number };
  events: ControlEvent[];
  disconnects: ConnectionFilter[];
  /** 存储落盘的全部字节（状态文件，或库连同 WAL）：查有没有不该落盘的明文。 */
  persisted(): string;
  cleanup(): void;
}

export async function personalHarness(
  options: { account?: string; nowMs?: number; store?: StoreKind } = {},
): Promise<Harness> {
  const dir = mkdtempSync(join(tmpdir(), "relay-personal-"));
  const clock = { now: options.nowMs ?? Date.UTC(2026, 9, 6, 12) };
  const now = () => clock.now;
  const store = await createState({
    dataDir: join(dir, "data"),
    account: options.account ?? "me",
    password: PASSWORD,
    issuer: ISSUER,
    store: options.store ?? "sqlite",
    nowMs: clock.now,
  });
  const signer = stateSigner(store, now);
  const control = createMemoryControlPlane({
    issuer: ISSUER,
    jwks: () => signer.jwks(),
    revoked: stateRevocation(store, now),
    now,
  });
  const events: ControlEvent[] = [];
  const disconnects: ConnectionFilter[] = [];
  const context = createPersonalContext({
    store,
    control,
    signer,
    now,
    disconnect: (filter) => disconnects.push(filter),
  });
  context.subscribe((event) => events.push(event));
  return {
    dir,
    clock,
    events,
    disconnects,
    context,
    persisted: () =>
      [store.path, `${store.path}-wal`]
        .filter((file) => existsSync(file))
        .map((file) => readFileSync(file).toString("latin1"))
        .join("\n"),
    cleanup: () => {
      store.close();
      rmSync(dir, { recursive: true, force: true });
    },
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
