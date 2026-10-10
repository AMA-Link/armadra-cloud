/**
 * Node 中继的挑战（cloud-api §16）：`--challenge` 参数、对假 siteverify 服务器的校验（成功 / 过期 /
 * 重放 / 网络错 / 5xx），以及开了挑战的 `personal serve` 走 HTTP 的登录。
 */
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "@armadra/cloud-shared/log";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { PASSWORD } from "../../test/personal.ts";
import { parsePersonalServe, UsageError } from "./args.ts";
import { createTurnstileVerifier, readTurnstileSecret } from "./challenge.ts";
import { startPersonalRelay } from "./serve.ts";

const SECRET = "test-turnstile-secret";
const ASK = { action: "armadra-login", hostnames: ["relay.test"] };

/** 假 siteverify：记下每个请求体；`ok-*` 通过一次、再来答重复，`slow` 不答，`boom` 答 500。 */
let server: Server;
let endpoint = "";
const bodies: Record<string, string>[] = [];
const used = new Set<string>();

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const body = Object.fromEntries(
        new URLSearchParams(Buffer.concat(chunks).toString("utf8")),
      );
      bodies.push(body);
      const token = body.response ?? "";
      if (token === "slow") return; // 不答，等客户端超时
      const send = (status: number, value: unknown) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify(value));
      };
      if (token === "boom") return send(500, { success: false });
      if (body.secret !== SECRET)
        return send(200, {
          success: false,
          "error-codes": ["invalid-input-secret"],
        });
      if (token.startsWith("ok-") && !used.has(token)) {
        used.add(token);
        return send(200, {
          success: true,
          action: "armadra-login",
          hostname: "relay.test",
        });
      }
      return send(200, {
        success: false,
        "error-codes": [
          token.startsWith("ok-")
            ? "timeout-or-duplicate"
            : token === "expired"
              ? "timeout-or-duplicate"
              : "invalid-input-response",
        ],
      });
    });
  });
  await new Promise<void>((resolve) =>
    server.listen(0, "127.0.0.1", () => resolve()),
  );
  endpoint = `http://127.0.0.1:${(server.address() as AddressInfo).port}/siteverify`;
});

afterAll(async () => {
  server.closeAllConnections();
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

describe("--challenge 参数", () => {
  const base = ["--data-dir", "d", "--host", "relay.test"];

  it("缺省不开；turnstile 要 siteKey 与 secret 文件；scope 缺省只有登录", () => {
    expect(parsePersonalServe(base, {}, "/w").challenge).toBeUndefined();
    expect(
      parsePersonalServe(
        [
          ...base,
          "--challenge",
          "turnstile",
          "--turnstile-site-key",
          "0x4AAA",
          "--turnstile-secret-file",
          "secret",
        ],
        {},
        "/w",
      ).challenge,
    ).toEqual({
      provider: "turnstile",
      siteKey: "0x4AAA",
      secretFile: "/w/secret",
      scope: ["auth.login"],
      hostnames: [],
    });
    const given = (env: Record<string, string>) =>
      parsePersonalServe(
        base,
        {
          RELAY_CHALLENGE: "turnstile",
          RELAY_TURNSTILE_SITE_KEY: "k",
          RELAY_TURNSTILE_SECRET_FILE: "s",
          ...env,
        },
        "/w",
      ).challenge?.hostnames;
    expect(given({ RELAY_TURNSTILE_HOSTNAMES: "Relay.Test" })).toEqual([
      "relay.test",
    ]);
    // 公网 https 地址下不得含本机地址；没给公网地址（本地开发）可以
    expect(given({ RELAY_TURNSTILE_HOSTNAMES: "localhost" })).toEqual([
      "localhost",
    ]);
    expect(() =>
      given({
        RELAY_TURNSTILE_HOSTNAMES: "relay.test,127.0.0.1",
        RELAY_PUBLIC_ORIGIN: "https://relay.test",
      }),
    ).toThrow(UsageError);
    expect(
      parsePersonalServe(
        base,
        {
          RELAY_CHALLENGE: "turnstile",
          RELAY_TURNSTILE_SITE_KEY: "k",
          RELAY_TURNSTILE_SECRET_FILE: "/run/secrets/turnstile",
          RELAY_CHALLENGE_SCOPE: "auth.login,links.accept",
        },
        "/w",
      ).challenge?.scope,
    ).toEqual(["auth.login", "links.accept"]);
    // compose 把没设的变量传成空串：当作没给。
    expect(
      parsePersonalServe(
        base,
        { RELAY_CHALLENGE: "", RELAY_TURNSTILE_SITE_KEY: "" },
        "/w",
      ).challenge,
    ).toBeUndefined();
  });

  it("配错就拒绝", () => {
    for (const extra of [
      ["--challenge", "recaptcha"],
      ["--challenge", "turnstile"],
      ["--challenge", "turnstile", "--turnstile-site-key", "k"],
      ["--turnstile-site-key", "k"],
      [
        "--challenge",
        "turnstile",
        "--turnstile-site-key",
        "k",
        "--turnstile-secret-file",
        "s",
        "--challenge-scope",
        "me.get",
      ],
    ])
      expect(() => parsePersonalServe([...base, ...extra], {}, "/w")).toThrow(
        UsageError,
      );
  });
});

describe("对假 siteverify 校验", () => {
  const verifier = () =>
    createTurnstileVerifier({ secret: SECRET, endpoint, timeoutMs: 300 });

  it("成功：请求体带 secret、response、remoteip 与幂等键", async () => {
    const verdict = await verifier().verify({
      token: "ok-1",
      remoteIp: "203.0.113.7",
      idempotencyKey: "key-1",
      ...ASK,
    });
    expect(verdict).toEqual({ ok: true, reasons: [] });
    expect(bodies.at(-1)).toEqual({
      secret: SECRET,
      response: "ok-1",
      remoteip: "203.0.113.7",
      idempotency_key: "key-1",
    });
  });

  it("重放与过期答 timeout-or-duplicate；无效令牌、错 secret 不可重试", async () => {
    expect(
      await verifier().verify({ token: "ok-1", idempotencyKey: "k", ...ASK }),
    ).toEqual({
      ok: false,
      reasons: ["timeout-or-duplicate"],
      retryable: false,
    });
    expect(
      await verifier().verify({
        token: "expired",
        idempotencyKey: "k",
        ...ASK,
      }),
    ).toMatchObject({ ok: false, reasons: ["timeout-or-duplicate"] });
    expect(
      await createTurnstileVerifier({ secret: "wrong", endpoint }).verify({
        token: "ok-2",
        idempotencyKey: "k",
        ...ASK,
      }),
    ).toEqual({
      ok: false,
      reasons: ["invalid-input-secret"],
      retryable: false,
    });
  });

  it("超时、连不上、5xx：network，可重试", async () => {
    expect(
      await verifier().verify({ token: "slow", idempotencyKey: "k", ...ASK }),
    ).toEqual({ ok: false, reasons: ["network"], retryable: true });
    expect(
      await createTurnstileVerifier({
        secret: SECRET,
        endpoint: "http://127.0.0.1:1/siteverify",
      }).verify({ token: "ok-3", idempotencyKey: "k", ...ASK }),
    ).toEqual({ ok: false, reasons: ["network"], retryable: true });
    expect(
      await verifier().verify({ token: "boom", idempotencyKey: "k", ...ASK }),
    ).toEqual({ ok: false, reasons: ["network"], retryable: true });
  });
});

describe("开了挑战的 personal serve", () => {
  const root = mkdtempSync(join(tmpdir(), "relay-challenge-"));
  afterAll(() => rmSync(root, { recursive: true, force: true }));

  it("secret 文件为空拒绝启动；platform.info 报挑战；登录缺 / 坏令牌 400，好令牌 200，日志不含令牌与 secret", async () => {
    const passwordFile = join(root, "pw");
    writeFileSync(passwordFile, PASSWORD);
    const secretFile = join(root, "turnstile");
    writeFileSync(secretFile, "  \n");
    expect(() => readTurnstileSecret(secretFile)).toThrow();
    writeFileSync(secretFile, `${SECRET}\n`);
    expect(readTurnstileSecret(secretFile)).toBe(SECRET);

    const lines: string[] = [];
    const relay = await startPersonalRelay(
      {
        dataDir: join(root, "data"),
        host: "127.0.0.1",
        port: 8443,
        listen: { host: "127.0.0.1", port: 0 },
        tls: "plain",
        logLevel: "info",
        account: "me",
        passwordFile,
        challenge: {
          provider: "turnstile",
          siteKey: "0x4AAA-site",
          secretFile,
          scope: ["auth.login"],
          hostnames: ["relay.test"],
        },
      },
      {
        log: createLogger({
          name: "relay",
          level: "info",
          write: (line) => lines.push(line),
        }),
        challengeVerifier: createTurnstileVerifier({
          secret: readTurnstileSecret(secretFile),
          endpoint,
        }),
      },
    );
    try {
      const info = (await (
        await fetch(`${relay.url}/.well-known/armadra-platform`)
      ).json()) as { capabilities: string[]; challenge?: unknown };
      expect(info.capabilities).toContain("challenge.turnstile");
      expect(info.challenge).toEqual({
        provider: "turnstile",
        siteKey: "0x4AAA-site",
        scope: ["auth.login"],
      });
      const login = (challenge?: unknown) =>
        fetch(`${relay.url}/v1/auth/login`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            account: "me",
            password: PASSWORD,
            device: { platform: "desktop", name: "t" },
            ...(challenge ? { challenge } : {}),
          }),
        });
      const missing = await login();
      expect(missing.status).toBe(400);
      expect(await missing.json()).toMatchObject({
        code: "challenge_required",
      });
      const bad = await login({ provider: "turnstile", token: "nope-token" });
      expect(bad.status).toBe(400);
      expect(await bad.json()).toMatchObject({ code: "challenge_invalid" });
      const good = await login({ provider: "turnstile", token: "ok-serve" });
      expect(good.status).toBe(200);
      expect(bodies.at(-1)).toMatchObject({
        response: "ok-serve",
        remoteip: "127.0.0.1",
      });
      const replay = await login({ provider: "turnstile", token: "ok-serve" });
      expect(replay.status).toBe(400);
      await replay.arrayBuffer();
    } finally {
      await relay.close();
    }
    const log = lines.join("\n");
    expect(log).toContain("已开启人机挑战");
    expect(log).toContain("人机挑战没通过");
    expect(log).not.toContain(SECRET);
    expect(log).not.toContain("nope-token");
    expect(log).not.toContain("ok-serve");
  });
});
