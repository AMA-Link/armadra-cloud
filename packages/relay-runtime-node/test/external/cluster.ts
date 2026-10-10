/** 两个节点的控制面合同：路由互见、在线事件与节点命令广播、撤销同步、限流共用一个桶。 */
import type {
  NodeCommand,
  PresenceEvent,
} from "@armadra/relay-kernel/control/types";
import { describe, expect, it, vi } from "vitest";

import type { ClusterControlPlane } from "../../src/control/types.ts";

export interface Pair {
  a: ClusterControlPlane & { settle(): Promise<void> };
  b: ClusterControlPlane & { settle(): Promise<void> };
}

export const LIMITED = {
  "login.ip": { type: "rate", max: 5, windowMs: 60_000 },
  "edge.concurrent.acct": { type: "concurrent", max: 1 },
} as const;

const SOURCE = "a1".repeat(16);
const route = (node: string, tunnelId: string) => ({
  node,
  tunnelId,
  sinceMs: 1,
  coreVersion: "0.3.0",
});

export function clusterContract(
  label: string,
  pair: () => Promise<Pair>,
): void {
  describe(`${label}：两个节点`, () => {
    it("路由互见；touch 续期；delete 只删同一条隧道的", async () => {
      const { a, b } = await pair();
      await a.routes.set(SOURCE, route("node-a", "t1"), 30_000);
      expect(await b.routes.get(SOURCE)).toEqual(route("node-a", "t1"));
      await b.routes.delete(SOURCE, "t-other");
      await b.settle();
      expect(await a.routes.get(SOURCE)).toEqual(route("node-a", "t1"));
      await b.routes.set(SOURCE, route("node-b", "t2"), 30_000);
      await a.routes.delete(SOURCE, "t1");
      await a.settle();
      expect(await a.routes.get(SOURCE)).toEqual(route("node-b", "t2"));
      await b.routes.touch(SOURCE, 30_000);
      await b.routes.delete(SOURCE, "t2");
      await b.settle();
      expect(await a.routes.get(SOURCE)).toBeNull();
      await a.routes.set(SOURCE, route("node-a", "t3"), 1);
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(await b.routes.get(SOURCE)).toBeNull();
    });

    it("在线事件与节点命令：本节点收一次，另一节点经广播收到", async () => {
      const { a, b } = await pair();
      const seenA: PresenceEvent[] = [];
      const seenB: PresenceEvent[] = [];
      a.presence.subscribe((event) => seenA.push(event));
      b.presence.subscribe((event) => seenB.push(event));
      const event: PresenceEvent = {
        type: "sourceOnline",
        sourceId: SOURCE,
        node: "node-a",
        tunnelId: "t1",
        coreVersion: "0.3.0",
        atMs: 5,
      };
      await a.presence.publish(event);
      await vi.waitFor(() => expect(seenB).toEqual([event]));
      expect(seenA).toEqual([event]);

      const commandsA: NodeCommand[] = [];
      const commandsB: NodeCommand[] = [];
      a.nodeCommands.subscribe((command) => commandsA.push(command));
      b.nodeCommands.subscribe((command) => commandsB.push(command));
      b.command({ type: "closeTunnel", sourceId: SOURCE });
      await vi.waitFor(() =>
        expect(commandsA).toEqual([{ type: "closeTunnel", sourceId: SOURCE }]),
      );
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(commandsB).toEqual([{ type: "closeTunnel", sourceId: SOURCE }]);
      expect(commandsA).toHaveLength(1);
    });

    it("撤销：一个节点记下，另一个节点经广播立即拒绝；过期的不记", async () => {
      const { a, b } = await pair();
      expect(await b.tokens.isRevoked("jti-1")).toBe(false);
      await a.tokens.revoke("jti-1", Date.now() + 60_000);
      expect(await a.tokens.isRevoked("jti-1")).toBe(true);
      await vi.waitFor(async () =>
        expect(await b.tokens.isRevoked("jti-1")).toBe(true),
      );
      await a.tokens.revoke("jti-old", Date.now() - 1);
      expect(await b.tokens.isRevoked("jti-old")).toBe(false);
    });

    it("速率限流两个节点共用一个桶；并发限流按节点", async () => {
      const { a, b } = await pair();
      const hits = [];
      for (const node of [a, b, a, b, a])
        hits.push(await node.limits.hit("login.ip", "198.51.100.7"));
      expect(hits.every((hit) => hit.allowed)).toBe(true);
      const denied = await b.limits.hit("login.ip", "198.51.100.7");
      expect(denied.allowed).toBe(false);
      expect(denied.retryAfterMs).toBeGreaterThan(0);
      expect(denied.retryAfterMs).toBeLessThanOrEqual(60_000);
      expect((await a.limits.hit("login.ip", "198.51.100.8")).allowed).toBe(
        true,
      );

      expect((await a.limits.hit("edge.concurrent.acct", "u")).allowed).toBe(
        true,
      );
      expect((await a.limits.hit("edge.concurrent.acct", "u")).allowed).toBe(
        false,
      );
      expect((await b.limits.hit("edge.concurrent.acct", "u")).allowed).toBe(
        true,
      );
      await a.limits.release?.("edge.concurrent.acct", "u");
      expect((await a.limits.hit("edge.concurrent.acct", "u")).allowed).toBe(
        true,
      );
    });
  });
}
