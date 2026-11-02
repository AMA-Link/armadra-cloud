/**
 * 撤销判断的失败方向（#192）：重判次数耗尽、控制对象异常都按「已撤销」处理（fail-closed），
 * 但异常不写缓存，下一次判定要能恢复。
 */
import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";

import type { SourceDO } from "../src/source-do.ts";

type Internals = {
  controlStub: () => { isRevoked: (jti: string) => Promise<boolean> };
  isRevoked: (jti: string) => Promise<boolean>;
  edgeUpgrade: (request: Request, edge: unknown) => Promise<Response>;
  invalidate: () => void;
  policy: {
    decideUpgrade: (edge: unknown) => Promise<unknown>;
    releaseUpgrade: (subject: string) => void;
  };
};

let seq = 0;
const stubOf = () =>
  env.SOURCE.get(env.SOURCE.idFromName(`src_revocation_unit_${++seq}`));

const asInternals = (instance: SourceDO) => instance as unknown as Internals;

describe("撤销判断的失败方向", () => {
  it("控制对象抛一次异常：这次按已撤销处理，但不写缓存，下一次恢复正常", async () => {
    await runInDurableObject(stubOf(), async (instance: SourceDO) => {
      const inner = asInternals(instance);
      let calls = 0;
      inner.controlStub = () => ({
        isRevoked: async () => {
          if (++calls === 1) throw new Error("控制对象暂时不可用");
          return false;
        },
      });
      expect(await inner.isRevoked("jti-transient")).toBe(true);
      expect(await inner.isRevoked("jti-transient")).toBe(false);
      expect(calls).toBe(2);
    });
  });

  it("真实的已撤销结果照常缓存，不再问控制对象", async () => {
    await runInDurableObject(stubOf(), async (instance: SourceDO) => {
      const inner = asInternals(instance);
      let calls = 0;
      inner.controlStub = () => ({
        isRevoked: async () => {
          calls++;
          return true;
        },
      });
      expect(await inner.isRevoked("jti-revoked")).toBe(true);
      expect(await inner.isRevoked("jti-revoked")).toBe(true);
      expect(calls).toBe(1);
    });
  });

  it("每次查询期间都有撤销作废缓存：次数耗尽后按已撤销处理，且不写缓存", async () => {
    await runInDurableObject(stubOf(), async (instance: SourceDO) => {
      const inner = asInternals(instance);
      let calls = 0;
      let churn = true;
      inner.controlStub = () => ({
        isRevoked: async () => {
          calls++;
          if (churn) inner.invalidate();
          return false;
        },
      });
      expect(await inner.isRevoked("jti-churn")).toBe(true);
      expect(calls).toBe(3);
      // 没写缓存：撤销风暴过去后，下一次判定能拿到真实结果。
      churn = false;
      expect(await inner.isRevoked("jti-churn")).toBe(false);
      expect(calls).toBe(4);
    });
  });
});

describe("升级判定的重判次数耗尽", () => {
  it("连续 3 次判定都跨过撤销作废：拒绝本次升级（4401 对应的 relay_token_invalid），并放掉每次占的名额", async () => {
    await runInDurableObject(stubOf(), async (instance: SourceDO) => {
      const inner = asInternals(instance);
      let decided = 0;
      const released: string[] = [];
      inner.policy.decideUpgrade = async () => {
        decided++;
        inner.invalidate();
        return {
          kind: "accept",
          target: { sourceId: "src_churn" },
          tunnel: {},
          subject: `sub-${decided}`,
          remoteIp: "203.0.113.7",
          subprotocols: [],
        };
      };
      inner.policy.releaseUpgrade = (subject) => {
        released.push(subject);
      };
      const response = await inner.edgeUpgrade(
        new Request("https://relay.armadra.test/", {
          headers: { upgrade: "websocket" },
        }),
        { method: "GET", url: "/", headers: {}, clientIp: "203.0.113.7" },
      );
      expect(response.status).toBe(401);
      expect(((await response.json()) as { code: string }).code).toBe(
        "relay_token_invalid",
      );
      expect(decided).toBe(3);
      expect(released).toEqual(["sub-1", "sub-2", "sub-3"]);
    });
  });
});
