import { createPrivateKey } from "node:crypto";
import { mkdtempSync, rmSync, statSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterAll, describe, expect, it } from "vitest";

import { startAcme, type AcmeIssuer } from "./acme.ts";
import { createCa, issueLeaf, TlsError } from "./tls.ts";

const root = mkdtempSync(join(tmpdir(), "relay-acme-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

const ca = createCa(new Date());
const caKey = ca.key;
const caCert = ca.parsed;

async function freePort(): Promise<number> {
  return new Promise((resolve) => {
    const server = createServer();
    server.listen(0, "127.0.0.1", () => {
      const port = (server.address() as { port: number }).port;
      server.close(() => resolve(port));
    });
  });
}

/** 假 ACME：先经挑战监听取回 key authorization（模拟 CA 的 http-01 验证），再签 `days` 天的证书。 */
function fakeIssuer(
  port: () => number,
  days: number,
  calls: string[],
): AcmeIssuer {
  return async (request) => {
    request.challenges.set("tok-1", "tok-1.thumbprint");
    const response = await fetch(
      `http://127.0.0.1:${port()}/.well-known/acme-challenge/tok-1`,
    );
    calls.push(`${response.status}:${await response.text()}`);
    request.challenges.delete("tok-1");
    const leaf = issueLeaf(
      { key: caKey, parsed: caCert },
      request.names,
      new Date(),
      days,
      createPrivateKey(request.certificateKey),
    );
    return leaf.cert;
  };
}

describe("ACME（http-01）", () => {
  it("首签经挑战监听答出 key authorization；文件 0600；非挑战请求 308 到对外来源", async () => {
    const port = await freePort();
    const calls: string[] = [];
    const manager = await startAcme({
      dataDir: join(root, "a"),
      email: "ops@relay.test",
      names: ["relay.test"],
      directoryUrl: "https://acme.invalid/directory",
      httpListen: { host: "127.0.0.1", port },
      redirectOrigin: "https://relay.test:8443",
      issuer: fakeIssuer(() => port, 60, calls),
    });
    try {
      expect(calls).toEqual(["200:tok-1.thumbprint"]);
      expect(manager.current().source).toBe("acme");
      expect(manager.current().names).toEqual(["relay.test"]);
      for (const name of ["account.key", "cert.pem", "key.pem"])
        expect(statSync(join(root, "a", "acme", name)).mode & 0o777).toBe(
          0o600,
        );
      const redirect = await fetch(`http://127.0.0.1:${port}/x?y=1`, {
        redirect: "manual",
      });
      expect(redirect.status).toBe(308);
      expect(redirect.headers.get("location")).toBe(
        "https://relay.test:8443/x?y=1",
      );
      expect(
        (
          await fetch(
            `http://127.0.0.1:${port}/.well-known/acme-challenge/none`,
          )
        ).status,
      ).toBe(404);
    } finally {
      await manager.close();
    }
  });

  it("剩 30 天以内续期并回调；续期失败继续用旧证书并计数", async () => {
    const port = await freePort();
    const calls: string[] = [];
    let fail = false;
    const base = fakeIssuer(() => port, 20, calls);
    const events: string[] = [];
    const manager = await startAcme({
      dataDir: join(root, "b"),
      email: "ops@relay.test",
      names: ["relay.test"],
      directoryUrl: "https://acme.invalid/directory",
      httpListen: { host: "127.0.0.1", port },
      redirectOrigin: "https://relay.test",
      issuer: async (request) => {
        if (fail) throw new Error("boom");
        return base(request);
      },
      retryMs: 50,
      onEvent: (event, failures) => events.push(`${event}:${failures}`),
    });
    try {
      const first = manager.current().cert;
      const changed: string[] = [];
      fail = true;
      const stop = manager.start(
        (material) => changed.push(material.cert),
        () => {},
      );
      await new Promise((resolve) => setTimeout(resolve, 180));
      expect(manager.current().cert).toBe(first);
      expect(events[0]).toBe("renew_failed:1");
      fail = false;
      await new Promise((resolve) => setTimeout(resolve, 200));
      stop();
      expect(changed.length).toBeGreaterThan(0);
      expect(events).toContain("renewed:0");
    } finally {
      await manager.close();
    }
  });

  it("签不出来就拒绝启动（不退回自签），挑战监听也关掉", async () => {
    const port = await freePort();
    await expect(
      startAcme({
        dataDir: join(root, "c"),
        email: "ops@relay.test",
        names: ["relay.test"],
        directoryUrl: "https://acme.invalid/directory",
        httpListen: { host: "127.0.0.1", port },
        redirectOrigin: "https://relay.test",
        issuer: async () => {
          throw new Error("unreachable");
        },
      }),
    ).rejects.toThrow(TlsError);
    await expect(fetch(`http://127.0.0.1:${port}/`)).rejects.toThrow();
  });
});
