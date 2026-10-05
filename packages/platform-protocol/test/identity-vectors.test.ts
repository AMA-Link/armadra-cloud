import { createHash, scryptSync } from "node:crypto";
import { describe, expect, it } from "vitest";

import {
  lockDurationMs,
  PASSWORD_POLICY,
  SCRYPT,
  THROTTLE,
  TOKEN_HASH_KINDS,
  TOKEN_HASH_PREFIX,
  vectors,
} from "../src/identity-vectors/index.ts";

describe("向量", () => {
  it.each(vectors.scrypt.map((vector) => [vector.password, vector] as const))(
    "scrypt(%s)",
    (_, vector) => {
      const hash = scryptSync(
        vector.password,
        Buffer.from(vector.salt, "hex"),
        SCRYPT.length,
        {
          N: SCRYPT.cost,
          r: SCRYPT.block,
          p: SCRYPT.parallel,
          maxmem: 64 * 1024 * 1024,
        },
      );
      expect(hash.toString("hex")).toBe(vector.hex);
      expect(Buffer.from(vector.salt, "hex").length).toBe(SCRYPT.saltBytes);
    },
  );

  it("令牌哈希：sha256(prefix + kind + \\0 + value)，每种用途都有", () => {
    expect(new Set(vectors.tokenHash.map((vector) => vector.kind))).toEqual(
      new Set(TOKEN_HASH_KINDS),
    );
    for (const vector of vectors.tokenHash) {
      const hex = createHash("sha256")
        .update(`${TOKEN_HASH_PREFIX}${vector.kind}\0${vector.value}`, "utf8")
        .digest("hex");
      expect(hex).toBe(vector.hex);
    }
  });
});

describe("常量", () => {
  it("口令策略与 core 一致", () => {
    expect(PASSWORD_POLICY.minLengthDefault).toBe(12);
    expect(PASSWORD_POLICY.minLengthFloor).toBeLessThanOrEqual(
      PASSWORD_POLICY.minLengthDefault,
    );
    expect(PASSWORD_POLICY.minLengthCeiling).toBeGreaterThanOrEqual(
      PASSWORD_POLICY.minLengthDefault,
    );
  });

  it("锁定时长：第 5 次起 1 分钟，翻倍到 15 分钟封顶", () => {
    expect(lockDurationMs(THROTTLE.lockThreshold - 1)).toBe(0);
    expect(lockDurationMs(5)).toBe(60_000);
    expect(lockDurationMs(6)).toBe(120_000);
    expect(lockDurationMs(9)).toBe(15 * 60_000);
    expect(lockDurationMs(100)).toBe(15 * 60_000);
  });
});
