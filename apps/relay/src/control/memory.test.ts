import { readFileSync } from "node:fs";
import { expect, it } from "vitest";

import { createMemoryControlPlane } from "./memory.ts";
import type { PresenceEvent } from "./types.ts";

function plane(
  limits?: Parameters<typeof createMemoryControlPlane>[0]["limits"],
) {
  const clock = { now: 1_000_000 };
  const control = createMemoryControlPlane({
    issuer: "https://relay.test",
    jwks: () => ({ keys: [] }),
    now: () => clock.now,
    limits,
  });
  return { control, clock };
}

const route = (tunnelId: string) => ({
  node: "n1",
  tunnelId,
  sinceMs: 0,
  coreVersion: "0.2.0",
});

it("路由按 TTL 过期，touch 续期", async () => {
  const { control, clock } = plane();
  await control.routes.set("s", route("t1"), 1000);
  expect(await control.routes.get("s")).toMatchObject({ tunnelId: "t1" });
  clock.now += 900;
  await control.routes.touch("s", 1000);
  clock.now += 900;
  expect(await control.routes.get("s")).not.toBeNull();
  clock.now += 101;
  expect(await control.routes.get("s")).toBeNull();
  expect(control.routeCount()).toBe(0);
});

it("路由删除是 CAS：只删 tunnelId 相同的那条", async () => {
  const { control } = plane();
  await control.routes.set("s", route("new"), 1000);
  await control.routes.delete("s", "old");
  expect(await control.routes.get("s")).toMatchObject({ tunnelId: "new" });
  await control.routes.delete("s", "new");
  expect(await control.routes.get("s")).toBeNull();
});

it("touch 不会复活已过期的路由", async () => {
  const { control, clock } = plane();
  await control.routes.set("s", route("t"), 10);
  clock.now += 11;
  await control.routes.touch("s", 1000);
  expect(await control.routes.get("s")).toBeNull();
});

it("速率限流是固定窗口，答 retryAfterMs", async () => {
  const { control, clock } = plane({
    "edge.connect.ip": { type: "rate", max: 2, windowMs: 1000 },
  });
  expect((await control.limits.hit("edge.connect.ip", "ip")).allowed).toBe(
    true,
  );
  clock.now += 100;
  expect((await control.limits.hit("edge.connect.ip", "ip")).allowed).toBe(
    true,
  );
  clock.now += 100;
  expect(await control.limits.hit("edge.connect.ip", "ip")).toEqual({
    allowed: false,
    retryAfterMs: 800,
  });
  expect((await control.limits.hit("edge.connect.ip", "other")).allowed).toBe(
    true,
  );
  clock.now += 800;
  expect((await control.limits.hit("edge.connect.ip", "ip")).allowed).toBe(
    true,
  );
});

it("并发限流占名额，release 归还", async () => {
  const { control } = plane({
    "edge.concurrent.acct": { type: "concurrent", max: 2 },
  });
  const hit = () => control.limits.hit("edge.concurrent.acct", "a");
  expect((await hit()).allowed).toBe(true);
  expect((await hit()).allowed).toBe(true);
  expect((await hit()).allowed).toBe(false);
  await control.limits.release?.("edge.concurrent.acct", "a");
  expect((await hit()).allowed).toBe(true);
});

it("缺省限流：隧道每源每分钟 10 次", async () => {
  const { control } = plane();
  for (let index = 0; index < 10; index += 1)
    expect((await control.limits.hit("tunnel.connect.src", "s")).allowed).toBe(
      true,
    );
  expect((await control.limits.hit("tunnel.connect.src", "s")).allowed).toBe(
    false,
  );
});

it("撤销记到 exp，过期后自动忘掉", async () => {
  const { control, clock } = plane();
  await control.tokens.revoke("j1", clock.now + 1000);
  await control.tokens.revoke("already-expired", clock.now - 1);
  expect(await control.tokens.isRevoked("j1")).toBe(true);
  expect(await control.tokens.isRevoked("already-expired")).toBe(false);
  clock.now += 1000;
  expect(await control.tokens.isRevoked("j1")).toBe(false);
});

it("在线事件发给所有订阅者，退订后不再收", async () => {
  const { control } = plane();
  const seen: PresenceEvent[] = [];
  const off = control.presence.subscribe((event) => seen.push(event));
  control.presence.subscribe(() => {
    throw new Error("坏订阅者");
  });
  const event: PresenceEvent = {
    type: "sourceOffline",
    sourceId: "s",
    node: "n1",
    tunnelId: "t",
    atMs: 1,
  };
  await control.presence.publish(event);
  off();
  await control.presence.publish(event);
  expect(seen).toEqual([event]);
});

it("memory.ts 不 import Redis / PostgreSQL（personal 模式无外部依赖）", () => {
  const text = readFileSync(new URL("./memory.ts", import.meta.url), "utf8");
  const imports = [...text.matchAll(/from\s+["']([^"']+)["']/g)].map(
    (match) => match[1],
  );
  expect(imports.every((path) => path?.startsWith("./"))).toBe(true);
});
