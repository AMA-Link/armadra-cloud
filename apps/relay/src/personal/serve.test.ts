import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, expect, it } from "vitest";

import { UsageError } from "./args.ts";
import { startPersonalRelay } from "./serve.ts";

const root = mkdtempSync(join(tmpdir(), "relay-personal-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

const base = {
  host: "127.0.0.1",
  port: 8443,
  listen: { host: "127.0.0.1", port: 0 },
  logLevel: "error" as const,
};

it("建数据目录（0700），/health 与 /ready 200", async () => {
  const dataDir = join(root, "a", "data");
  const relay = await startPersonalRelay({ ...base, dataDir, tls: "plain" });
  try {
    expect(existsSync(dataDir)).toBe(true);
    if (process.platform !== "win32")
      expect(statSync(dataDir).mode & 0o777).toBe(0o700);
    const health = await fetch(`${relay.url}/health`);
    expect(health.status).toBe(200);
    expect(await health.json()).toEqual({
      ok: true,
      mode: "personal",
      node: "127.0.0.1",
      version: "0.1.0",
      tunnels: 0,
      streams: 0,
    });
    const ready = await fetch(`${relay.url}/ready`);
    expect(ready.status).toBe(200);
  } finally {
    await relay.close();
  }
});

it("尚未实现的 TLS 模式拒绝启动，不静默退回明文", async () => {
  await expect(
    startPersonalRelay({
      ...base,
      dataDir: join(root, "b"),
      tls: "self-signed",
    }),
  ).rejects.toThrow(UsageError);
  expect(existsSync(join(root, "b"))).toBe(false);
});
