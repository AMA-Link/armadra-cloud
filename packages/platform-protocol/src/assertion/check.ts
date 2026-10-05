/**
 * 声明校验辅助（协议包设计 §4.3）：时间、受众、签发者与 `jti` 重放。签名在调用方。
 */
export interface CheckOptions {
  nowMs: number;
  /** 时钟偏差容忍，缺省 5 分钟。 */
  skewMs?: number;
  expectedAud: string;
  expectedIss?: string;
}

export type CheckFailure = {
  code: "expired" | "not_yet_valid" | "audience_mismatch" | "issuer_mismatch";
};

export const DEFAULT_SKEW_MS = 300_000;

/**
 * `exp` / `iat` / `nbf` 是秒。过期：`nowMs - skew >= exp*1000`；
 * 未生效：`iat*1000 > nowMs + skew` 或 `nbf*1000 > nowMs + skew`。
 */
export function checkTimes(
  claims: { exp: number; iat: number; nbf?: number },
  o: CheckOptions,
): CheckFailure | null {
  const skew = o.skewMs ?? DEFAULT_SKEW_MS;
  if (o.nowMs - skew >= claims.exp * 1000) return { code: "expired" };
  if (claims.iat * 1000 > o.nowMs + skew) return { code: "not_yet_valid" };
  if (claims.nbf !== undefined && claims.nbf * 1000 > o.nowMs + skew)
    return { code: "not_yet_valid" };
  return null;
}

/** `aud` 必须逐字相等；给了 `expectedIss` 时 `iss` 也必须相等。 */
export function checkAudience(
  claims: { aud: string; iss?: string },
  o: CheckOptions,
): CheckFailure | null {
  if (claims.aud !== o.expectedAud) return { code: "audience_mismatch" };
  if (o.expectedIss !== undefined && claims.iss !== o.expectedIss)
    return { code: "issuer_mismatch" };
  return null;
}

/**
 * 纯内存的 `jti` 重放表（core 与 personal 中继用；saas 用 Redis 的同一接口适配）。
 * 一个 `jti` 记到 `max(expMs, nowMs + ttlMs)`；超过容量时先清过期的，再淘汰最早记下的。
 */
export class ReplayCache {
  private readonly entries = new Map<string, number>();
  private readonly ttlMs: number;
  private readonly max: number;

  constructor(ttlMs: number, max = 10_000) {
    if (!(ttlMs > 0) || !(max > 0))
      throw new RangeError("ttlMs 与 max 必须为正");
    this.ttlMs = ttlMs;
    this.max = max;
  }

  get size(): number {
    return this.entries.size;
  }

  /** 见过且未过期 → true（重放）；否则记下并返回 false。 */
  seen(jti: string, expMs: number, nowMs: number): boolean {
    const until = this.entries.get(jti);
    if (until !== undefined && until > nowMs) return true;
    if (until !== undefined) this.entries.delete(jti);
    if (this.entries.size >= this.max) this.evict(nowMs);
    this.entries.set(jti, Math.max(expMs, nowMs + this.ttlMs));
    return false;
  }

  private evict(nowMs: number): void {
    for (const [key, until] of this.entries) {
      if (until <= nowMs) this.entries.delete(key);
    }
    while (this.entries.size >= this.max) {
      const oldest = this.entries.keys().next();
      if (oldest.done) break;
      this.entries.delete(oldest.value);
    }
  }
}
