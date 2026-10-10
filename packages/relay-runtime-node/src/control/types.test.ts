import { describe, expect, it } from "vitest";

import { limitId } from "./postgres.ts";
import { listeners, parseBroadcast } from "./types.ts";

describe("多节点控制面的广播", () => {
  it("只认三种形状，别的版本发的忽略", () => {
    const presence = {
      origin: "o",
      kind: "presence",
      event: {
        type: "sourceOffline",
        sourceId: "s",
        node: "n",
        tunnelId: "t",
        atMs: 1,
      },
    };
    expect(parseBroadcast(JSON.stringify(presence))).toEqual(presence);
    const command = {
      origin: "o",
      kind: "command",
      command: { type: "closeTunnel", sourceId: "s" },
    };
    expect(parseBroadcast(JSON.stringify(command))).toEqual(command);
    const revoked = { origin: "o", kind: "revoked", jti: "j", expMs: 5 };
    expect(parseBroadcast(JSON.stringify(revoked))).toEqual(revoked);
    for (const text of [
      "not json",
      "null",
      JSON.stringify({ kind: "revoked", jti: "j", expMs: 5 }),
      JSON.stringify({ origin: "o", kind: "revoked", jti: "j" }),
      JSON.stringify({ origin: "o", kind: "command", command: {} }),
      JSON.stringify({ origin: "o", kind: "other" }),
    ])
      expect(parseBroadcast(text)).toBeNull();
  });

  it("一个订阅者出错不影响其余", () => {
    const bus = listeners<number>();
    const seen: number[] = [];
    bus.add(() => {
      throw new Error("boom");
    });
    const off = bus.add((value) => seen.push(value));
    bus.emit(1);
    off();
    bus.emit(2);
    expect(seen).toEqual([1]);
  });

  it("限流桶的键不带明文 IP，种类分开", () => {
    const id = limitId("login.ip", "198.51.100.7");
    expect(id).toMatch(/^login\.ip:[0-9a-f]{64}$/);
    expect(id).not.toContain("198.51.100.7");
    expect(limitId("link.accept.ip", "198.51.100.7")).not.toBe(id);
  });
});
