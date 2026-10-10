/**
 * Workers 中继的挑战（cloud-api §16）与闸：配置（录了 secret 才要求）、对假 siteverify（换掉 `fetch`）
 * 的校验、Rate Limiting 绑定缺失或出错时放行。
 */
import { describe, expect, it } from "vitest";

import { TURNSTILE_SITEVERIFY_URL } from "@armadra/control-core/challenge";

import { createWorkersTurnstileVerifier } from "../src/challenge.ts";
import { ConfigError, readConfig, type RelayEnv } from "../src/env.ts";
import { passGate } from "../src/gates.ts";

const base = { RELAY_ISSUER: "https://relay.armadra.test" } as RelayEnv;

describe("挑战配置", () => {
  it("没录 secret 不要求（站点密钥单独配了也不要求）", () => {
    expect(readConfig(base).challenge).toBeNull();
    expect(
      readConfig({ ...base, RELAY_TURNSTILE_SITE_KEY: "0x4AAA" }).challenge,
    ).toBeNull();
    expect(
      readConfig({ ...base, RELAY_TURNSTILE_SECRET: "  " }).challenge,
    ).toBeNull();
  });

  it("录了 secret：要站点密钥；scope 缺省只有登录，可加 links.accept", () => {
    expect(
      readConfig({
        ...base,
        RELAY_TURNSTILE_SECRET: "s",
        RELAY_TURNSTILE_SITE_KEY: " 0x4AAA ",
      }).challenge,
    ).toEqual({ siteKey: "0x4AAA", scope: ["auth.login"] });
    expect(
      readConfig({
        ...base,
        RELAY_TURNSTILE_SECRET: "s",
        RELAY_TURNSTILE_SITE_KEY: "k",
        RELAY_CHALLENGE_SCOPE: "auth.login,links.accept",
      }).challenge?.scope,
    ).toEqual(["auth.login", "links.accept"]);
    for (const bad of [
      { RELAY_TURNSTILE_SECRET: "s" },
      {
        RELAY_TURNSTILE_SECRET: "s",
        RELAY_TURNSTILE_SITE_KEY: "k",
        RELAY_CHALLENGE_SCOPE: "me.get",
      },
      {
        RELAY_TURNSTILE_SECRET: "s",
        RELAY_TURNSTILE_SITE_KEY: "k".repeat(257),
      },
    ])
      expect(() => readConfig({ ...base, ...bad })).toThrow(ConfigError);
  });
});

describe("对假 siteverify 校验", () => {
  type Seen = { url: string; body: Record<string, string> };
  const fake = (answer: (body: Record<string, string>) => Response) => {
    const seen: Seen[] = [];
    const fetcher = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const body = Object.fromEntries(init?.body as URLSearchParams);
      seen.push({ url: String(input), body });
      return answer(body);
    }) as typeof fetch;
    return { seen, fetcher };
  };
  const json = (status: number, value: unknown) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "content-type": "application/json" },
    });

  it("成功：打到 Turnstile 的 siteverify，体里有 secret、response、remoteip、幂等键", async () => {
    const { seen, fetcher } = fake(() => json(200, { success: true }));
    const verifier = createWorkersTurnstileVerifier({
      secret: "sekret",
      fetch: fetcher,
    });
    expect(
      await verifier.verify({
        token: "tok",
        remoteIp: "203.0.113.4",
        idempotencyKey: "k",
      }),
    ).toEqual({ ok: true, reasons: [] });
    expect(seen).toEqual([
      {
        url: TURNSTILE_SITEVERIFY_URL,
        body: {
          secret: "sekret",
          response: "tok",
          remoteip: "203.0.113.4",
          idempotency_key: "k",
        },
      },
    ]);
  });

  it("过期 / 重放、5xx、网络错、超时", async () => {
    const duplicate = createWorkersTurnstileVerifier({
      secret: "s",
      fetch: fake(() =>
        json(200, { success: false, "error-codes": ["timeout-or-duplicate"] }),
      ).fetcher,
    });
    expect(await duplicate.verify({ token: "t", idempotencyKey: "k" })).toEqual(
      { ok: false, reasons: ["timeout-or-duplicate"], retryable: false },
    );
    const broken = createWorkersTurnstileVerifier({
      secret: "s",
      fetch: fake(() => new Response("oops", { status: 502 })).fetcher,
    });
    expect(await broken.verify({ token: "t", idempotencyKey: "k" })).toEqual({
      ok: false,
      reasons: ["network"],
      retryable: true,
    });
    const offline = createWorkersTurnstileVerifier({
      secret: "s",
      fetch: (async () => {
        throw new TypeError("network");
      }) as typeof fetch,
    });
    expect(await offline.verify({ token: "t", idempotencyKey: "k" })).toEqual({
      ok: false,
      reasons: ["network"],
      retryable: true,
    });
    const hanging = createWorkersTurnstileVerifier({
      secret: "s",
      timeoutMs: 50,
      fetch: ((_input: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) =>
          init?.signal?.addEventListener("abort", () =>
            reject(new DOMException("timeout", "TimeoutError")),
          ),
        )) as typeof fetch,
    });
    expect(await hanging.verify({ token: "t", idempotencyKey: "k" })).toEqual({
      ok: false,
      reasons: ["network"],
      retryable: true,
    });
  });
});

describe("闸的放行规则", () => {
  it("没配绑定、绑定出错都放行（对象里还有内存桶）；绑定说不行就拦", async () => {
    expect(await passGate(base, "LIMIT_LOGIN", "ip")).toBe(true);
    expect(await passGate(base, null, "ip")).toBe(true);
    const throwing = {
      ...base,
      LIMIT_LOGIN: {
        limit: () => Promise.reject(new Error("binding down")),
      } as unknown as RateLimit,
    };
    expect(await passGate(throwing, "LIMIT_LOGIN", "ip")).toBe(true);
    const keys: string[] = [];
    const denying = {
      ...base,
      LIMIT_LINK_ACCEPT: {
        limit: ({ key }: { key: string }) => {
          keys.push(key);
          return Promise.resolve({ success: false });
        },
      } as unknown as RateLimit,
    };
    expect(await passGate(denying, "LIMIT_LINK_ACCEPT", "198.51.100.3")).toBe(
      false,
    );
    expect(keys).toEqual(["198.51.100.3"]);
  });
});
