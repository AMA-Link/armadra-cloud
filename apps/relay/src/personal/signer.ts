/**
 * 签名钥与令牌签发（中继设计 §6.3，协议包 §4.2）。Ed25519，`kid` 递增（k1、k2…）。
 *
 * - JWKS 答所有未退役（`retiredAtMs` 为空或在未来）的公钥；
 * - 签发用最新的那把未退役钥；
 * - `rotate-key` 追加新钥，旧钥 `retiredAtMs = now + 1h`（最长令牌寿命），期间旧令牌仍验得过。
 *
 * 时间声明是整数秒。私钥只在状态文件里（0600），不进日志与响应。
 */
import {
  createPrivateKey,
  generateKeyPairSync,
  sign,
  type KeyObject,
} from "node:crypto";

import {
  buildSigningInput,
  CLAIMS_VERSION,
  joinCompactJws,
  type Ed25519PublicJwk,
  type JwkSet,
  type TokenType,
} from "@armadra/platform-protocol/assertion";
import {
  RELAY_TOKEN,
  TUNNEL_TOKEN,
} from "@armadra/platform-protocol/identity-vectors";

import { randomHex } from "./secrets.ts";
import type { SigningKeyRecord, State } from "./state.ts";

/** 旧钥退役的宽限：最长令牌寿命（中继令牌、隧道令牌都是 1 小时）。 */
export const KEY_RETIRE_GRACE_MS = Math.max(
  RELAY_TOKEN.ttlMs,
  TUNNEL_TOKEN.ttlMs,
);

export function generateSigningKey(
  kid: string,
  nowMs: number,
): SigningKeyRecord {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const jwk = publicKey.export({ format: "jwk" }) as { x: string };
  return {
    kid,
    privateKeyPkcs8: (
      privateKey.export({ type: "pkcs8", format: "der" }) as Buffer
    ).toString("base64"),
    publicJwk: {
      kty: "OKP",
      crv: "Ed25519",
      x: jwk.x,
      kid,
      alg: "EdDSA",
      use: "sig",
    },
    activeFromMs: nowMs,
    retiredAtMs: null,
  };
}

/** 下一把钥的 kid：现有最大编号 + 1。 */
export function nextKid(keys: readonly SigningKeyRecord[]): string {
  let max = 0;
  for (const key of keys) {
    const match = /^k(\d+)$/.exec(key.kid);
    if (match) max = Math.max(max, Number(match[1]));
  }
  return `k${max + 1}`;
}

/** 在状态副本上轮换：追加新钥，其余未退役的钥定在 `now + 宽限` 退役。答新 kid。 */
export function rotateSigningKey(draft: State, nowMs: number): string {
  const kid = nextKid(draft.signingKeys);
  for (const key of draft.signingKeys) {
    if (
      key.retiredAtMs === null ||
      key.retiredAtMs > nowMs + KEY_RETIRE_GRACE_MS
    )
      key.retiredAtMs = nowMs + KEY_RETIRE_GRACE_MS;
  }
  // 早已过了退役时间的钥不必再留在文件里。
  draft.signingKeys = draft.signingKeys.filter(
    (key) => key.retiredAtMs === null || key.retiredAtMs > nowMs,
  );
  draft.signingKeys.push(generateSigningKey(kid, nowMs));
  return kid;
}

function live(key: SigningKeyRecord, nowMs: number): boolean {
  return key.retiredAtMs === null || key.retiredAtMs > nowMs;
}

export interface SignedToken {
  token: string;
  jti: string;
  expiresAtMs: number;
}

export class Signer {
  private readonly keys = new Map<string, KeyObject>();
  private readonly state: () => State;
  private readonly now: () => number;

  constructor(state: () => State, now: () => number = Date.now) {
    this.state = state;
    this.now = now;
  }

  get issuer(): string {
    return this.state().issuer;
  }

  jwks(): JwkSet {
    const at = this.now();
    return {
      keys: this.state()
        .signingKeys.filter((key) => live(key, at))
        .map((key) => ({ ...key.publicJwk }) as Ed25519PublicJwk),
    };
  }

  /** 签一个令牌：`iss`、`iat`、`exp`、`jti`（给了前缀就是 `<前缀>.<随机>`）由这里补。 */
  sign(
    typ: TokenType,
    claims: Record<string, unknown>,
    options: { ttlMs: number; jtiPrefix?: string; ver?: boolean },
  ): SignedToken {
    const key = this.current();
    const nowMs = this.now();
    const iat = Math.floor(nowMs / 1000);
    const exp = Math.floor((nowMs + options.ttlMs) / 1000);
    const jti = options.jtiPrefix
      ? `${options.jtiPrefix}.${randomHex(12)}`
      : randomHex(16);
    const payload = {
      iss: this.issuer,
      ...claims,
      iat,
      exp,
      jti,
      ...(options.ver === false ? {} : { ver: CLAIMS_VERSION }),
    };
    const { signingInput, encodedHeader, encodedPayload } = buildSigningInput(
      { alg: "EdDSA", kid: key.kid, typ },
      payload,
    );
    const signature = sign(null, signingInput, this.privateKey(key));
    return {
      token: joinCompactJws(
        encodedHeader,
        encodedPayload,
        new Uint8Array(signature),
      ),
      jti,
      expiresAtMs: exp * 1000,
    };
  }

  private current(): SigningKeyRecord {
    const at = this.now();
    const candidates = this.state().signingKeys.filter(
      (key) => key.retiredAtMs === null && key.activeFromMs <= at + 1000,
    );
    const pool =
      candidates.length > 0
        ? candidates
        : this.state().signingKeys.filter((key) => live(key, at));
    const newest = pool.reduce<SigningKeyRecord | undefined>(
      (best, key) =>
        best === undefined || key.activeFromMs >= best.activeFromMs
          ? key
          : best,
      undefined,
    );
    if (!newest) throw new Error("没有可用的签名钥");
    return newest;
  }

  private privateKey(record: SigningKeyRecord): KeyObject {
    let key = this.keys.get(record.kid);
    if (!key) {
      key = createPrivateKey({
        key: Buffer.from(record.privateKeyPkcs8, "base64"),
        format: "der",
        type: "pkcs8",
      });
      this.keys.set(record.kid, key);
    }
    return key;
  }
}
