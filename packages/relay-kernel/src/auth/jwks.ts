/**
 * JWKS 缓存（中继设计 §1 `auth/jwks.ts`）：内存缓存 10 分钟；`kid` 不认识时刷新一次
 * （同一时间只有一次在飞，两次刷新至少隔 `minRefreshMs`，防止拿乱写的 `kid` 打爆来源）；
 * 刷新失败就继续用旧缓存。来源是 `ControlPlane.jwks()`：personal 是自己的签名钥，
 * saas 是 cloud 的 `/.well-known/jwks.json`（R3 在控制面里再加一层 Redis 共享缓存）。
 */
import { createPublicKey, type KeyObject } from "node:crypto";

import type { JwkSet } from "../control/types.ts";

export interface JwksCacheOptions {
  ttlMs?: number;
  minRefreshMs?: number;
  now?: () => number;
}

export const JWKS_TTL_MS = 600_000;

export class JwksCache {
  private keys = new Map<string, KeyObject>();
  private loadedAtMs = Number.NEGATIVE_INFINITY;
  private attemptedAtMs = Number.NEGATIVE_INFINITY;
  private inflight: Promise<void> | null = null;
  private readonly ttlMs: number;
  private readonly minRefreshMs: number;
  private readonly now: () => number;
  private readonly load: () => Promise<JwkSet>;

  constructor(load: () => Promise<JwkSet>, options: JwksCacheOptions = {}) {
    this.load = load;
    this.ttlMs = options.ttlMs ?? JWKS_TTL_MS;
    this.minRefreshMs = options.minRefreshMs ?? 10_000;
    this.now = options.now ?? Date.now;
  }

  /** 按 `kid` 取公钥；找不到（刷新后仍没有）答 null。 */
  async key(kid: string): Promise<KeyObject | null> {
    const at = this.now();
    if (at - this.loadedAtMs >= this.ttlMs) await this.refresh();
    const known = this.keys.get(kid);
    if (known) return known;
    if (this.now() - this.attemptedAtMs >= this.minRefreshMs)
      await this.refresh();
    return this.keys.get(kid) ?? null;
  }

  private refresh(): Promise<void> {
    if (this.inflight) return this.inflight;
    this.attemptedAtMs = this.now();
    this.inflight = this.load()
      .then((set) => {
        const next = new Map<string, KeyObject>();
        for (const jwk of set.keys) {
          if (!jwk.kid) continue;
          try {
            next.set(
              jwk.kid,
              createPublicKey({
                key: { kty: jwk.kty, crv: jwk.crv, x: jwk.x },
                format: "jwk",
              }),
            );
          } catch {
            // 坏的 JWK 跳过，不影响其它钥。
          }
        }
        this.keys = next;
        this.loadedAtMs = this.now();
      })
      .catch(() => {
        // 来源不可达：继续用旧缓存（中继设计 §9「刷新失败用缓存」）。
      })
      .finally(() => {
        this.inflight = null;
      });
    return this.inflight;
  }
}
