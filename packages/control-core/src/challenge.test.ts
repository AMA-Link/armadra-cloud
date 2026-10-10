/**
 * 人机挑战（cloud-api §16）：闸本身（假提供方）、siteverify 的请求体与答复解读、登录与接受链接的
 * 次序（限流先计、挑战先于口令、挑战失败不计入登录失败）、`platform.info` 的声明。
 */
import { describe, expect, it } from "vitest";

import { createAccounts } from "./accounts.ts";
import {
  createChallengeGate,
  parseChallengeScope,
  readSiteverify,
  siteverifyBody,
  type ChallengeVerdict,
  type ChallengeVerifier,
} from "./challenge.ts";
import { ControlError } from "./errors.ts";
import { hashPassword } from "./passwords.ts";
import { createPersonalProcedures } from "./procedures.ts";
import { memoryHarness } from "./testing/memory-ports.ts";

const PASSWORD = "plover-quartz-lantern-41";
const DEVICE = { platform: "desktop" as const, name: "test" };
const turnstile = (token: string) => ({
  provider: "turnstile" as const,
  token,
});

const codeOf = async (run: () => unknown) => {
  try {
    await run();
  } catch (error) {
    return error instanceof ControlError ? error.code : String(error);
  }
  return "ok";
};

/** 假提供方：`good-*` 通过一次（再来答重复），`down` 抛网络错，其余答令牌无效。 */
function fakeVerifier() {
  const calls: { token: string; remoteIp?: string; idempotencyKey: string }[] =
    [];
  const used = new Set<string>();
  const verifier: ChallengeVerifier = {
    async verify(input) {
      calls.push(input);
      if (input.token === "down") throw new Error("ECONNREFUSED");
      if (input.token === "flaky" && calls.length === 1)
        return { ok: false, reasons: ["network"], retryable: true };
      if (input.token === "flaky") return { ok: true, reasons: [] };
      if (input.token.startsWith("good")) {
        if (used.has(input.token))
          return { ok: false, reasons: ["timeout-or-duplicate"] };
        used.add(input.token);
        return { ok: true, reasons: [] };
      }
      if (input.token === "expired")
        return { ok: false, reasons: ["timeout-or-duplicate"] };
      return { ok: false, reasons: ["invalid-input-response"] };
    },
  };
  return { verifier, calls };
}

describe("挑战闸", () => {
  const setup = (scope?: Parameters<typeof parseChallengeScope>[0]) => {
    const fake = fakeVerifier();
    const clock = { now: 1_000_000 };
    const rejected: string[][] = [];
    const gate = createChallengeGate({
      siteKey: " 0x4AAAA-site ",
      ...(scope ? { scope: parseChallengeScope(scope) } : {}),
      verifier: fake.verifier,
      now: () => clock.now,
      onRejected: (_procedure, reasons) => rejected.push(reasons),
    });
    return { gate, clock, rejected, ...fake };
  };

  it("报的声明：provider、去掉空白的 siteKey、缺省只要求登录", () => {
    expect(setup().gate.info()).toEqual({
      provider: "turnstile",
      siteKey: "0x4AAAA-site",
      scope: ["auth.login"],
    });
    expect(setup("auth.login, links.accept").gate.info().scope).toEqual([
      "auth.login",
      "links.accept",
    ]);
  });

  it("不在 scope 里的 procedure 不要求、不问提供方", async () => {
    const { gate, calls } = setup();
    await gate.check("links.accept", undefined, "198.51.100.1");
    expect(calls).toEqual([]);
  });

  it("缺令牌 challenge_required；成功一次；同一令牌重放本地就拒、不再问提供方", async () => {
    const { gate, calls, rejected } = setup();
    expect(await codeOf(() => gate.check("auth.login", undefined, "ip"))).toBe(
      "challenge_required",
    );
    await gate.check("auth.login", turnstile("good-1"), "198.51.100.1");
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({
      token: "good-1",
      remoteIp: "198.51.100.1",
    });
    expect(calls[0]?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
    expect(
      await codeOf(() => gate.check("auth.login", turnstile("good-1"), "ip")),
    ).toBe("challenge_invalid");
    expect(calls).toHaveLength(1);
    expect(rejected).toEqual([["replayed"]]);
  });

  it("本地表过了 300 秒就忘，重放交给提供方拒（timeout-or-duplicate）", async () => {
    const { gate, clock, calls, rejected } = setup();
    await gate.check("auth.login", turnstile("good-2"), "ip");
    clock.now += 300_001;
    expect(
      await codeOf(() => gate.check("auth.login", turnstile("good-2"), "ip")),
    ).toBe("challenge_invalid");
    expect(calls).toHaveLength(2);
    expect(rejected).toEqual([["timeout-or-duplicate"]]);
  });

  it("过期、无效、网络错一律 challenge_invalid；网络错用同一个幂等键重试一次", async () => {
    const { gate, calls, rejected } = setup();
    for (const token of ["expired", "garbage", "down"])
      expect(
        await codeOf(() => gate.check("auth.login", turnstile(token), "ip")),
      ).toBe("challenge_invalid");
    expect(rejected).toEqual([
      ["timeout-or-duplicate"],
      ["invalid-input-response"],
      ["network"],
    ]);
    const down = calls.filter((call) => call.token === "down");
    expect(down).toHaveLength(2);
    expect(down[0]?.idempotencyKey).toBe(down[1]?.idempotencyKey);
  });

  it("提供方第一次没给结论、第二次通过：放行", async () => {
    const { gate, calls } = setup();
    await gate.check("auth.login", turnstile("flaky"), "");
    expect(calls).toHaveLength(2);
    expect(calls[0]?.remoteIp).toBeUndefined();
  });

  it("配置不对就抛：空 siteKey、空或不认识的 scope", () => {
    const { verifier } = fakeVerifier();
    expect(() =>
      createChallengeGate({ siteKey: " ", verifier, now: Date.now }),
    ).toThrow();
    expect(() => parseChallengeScope(" , ")).toThrow();
    expect(() => parseChallengeScope("auth.login,me.get")).toThrow(
      "挑战范围不认识：me.get",
    );
    expect(parseChallengeScope("links.accept,links.accept")).toEqual([
      "links.accept",
    ]);
  });
});

describe("siteverify", () => {
  it("请求体：secret、response、remoteip（unknown 不带）、idempotency_key", () => {
    const body = siteverifyBody({
      secret: "s",
      token: "t",
      remoteIp: "203.0.113.9",
      idempotencyKey: "k",
    });
    expect(Object.fromEntries(body)).toEqual({
      secret: "s",
      response: "t",
      remoteip: "203.0.113.9",
      idempotency_key: "k",
    });
    expect(
      siteverifyBody({
        secret: "s",
        token: "t",
        remoteIp: "unknown",
        idempotencyKey: "k",
      }).has("remoteip"),
    ).toBe(false);
  });

  it("答复解读", () => {
    const cases: [number, unknown, ChallengeVerdict][] = [
      [200, { success: true, hostname: "relay" }, { ok: true, reasons: [] }],
      [
        200,
        { success: false, "error-codes": ["timeout-or-duplicate"] },
        { ok: false, reasons: ["timeout-or-duplicate"], retryable: false },
      ],
      [
        200,
        { success: false, "error-codes": ["internal-error"] },
        { ok: false, reasons: ["internal-error"], retryable: true },
      ],
      [
        200,
        { success: false },
        { ok: false, reasons: ["rejected"], retryable: false },
      ],
      [503, null, { ok: false, reasons: ["network"], retryable: true }],
      [
        200,
        "<html>",
        { ok: false, reasons: ["bad-response"], retryable: true },
      ],
      [
        400,
        { nope: 1 },
        { ok: false, reasons: ["bad-response"], retryable: false },
      ],
    ];
    for (const [status, body, verdict] of cases)
      expect(readSiteverify(status, body)).toEqual(verdict);
  });
});

describe("登录与接受链接的次序", () => {
  async function harness(scope = "auth.login") {
    const fake = fakeVerifier();
    const limited: string[] = [];
    const h = memoryHarness({
      limit: (kind) => {
        limited.push(kind);
        return { allowed: true };
      },
    });
    h.ports.addAccount(h.context.tenantId, {
      accountId: "u-1",
      name: "alice",
      password: await hashPassword(PASSWORD),
      createdAtMs: 0,
      updatedAtMs: 0,
      failures: 0,
      lockedUntilMs: 0,
      lastFailureAtMs: 0,
    });
    h.context.challenge = createChallengeGate({
      siteKey: "site",
      scope: parseChallengeScope(scope),
      verifier: fake.verifier,
      now: h.context.now,
    });
    return { h, limited, ...fake };
  }

  it("限流先计；挑战先于口令；挑战失败不计入登录失败次数", async () => {
    const { h, limited, calls } = await harness();
    const accounts = createAccounts(h.context);
    for (let attempt = 0; attempt < 6; attempt += 1)
      expect(
        await codeOf(() =>
          accounts.login(
            { account: "alice", password: "wrong", device: DEVICE },
            "ip",
          ),
        ),
      ).toBe("challenge_required");
    expect(limited).toHaveLength(6);
    expect(calls).toEqual([]);
    expect(
      (await h.ports.accounts.get(h.context.tenantId, "u-1"))?.failures,
    ).toBe(0);
    expect(
      await codeOf(() =>
        accounts.login(
          {
            account: "alice",
            password: PASSWORD,
            device: DEVICE,
            challenge: turnstile("garbage"),
          },
          "ip",
        ),
      ),
    ).toBe("challenge_invalid");
    const session = await accounts.login(
      {
        account: "alice",
        password: PASSWORD,
        device: DEVICE,
        challenge: turnstile("good-login"),
      },
      "ip",
    );
    expect(session.accessToken).toBeTruthy();
  });

  it("platform.info 报能力 challenge.turnstile 与声明；links.accept 只在 scope 里才要求", async () => {
    const { h } = await harness("auth.login,links.accept");
    const { handlers } = createPersonalProcedures(h.context, {
      stream: {} as never,
      webApp: () => null,
    });
    const info = (await handlers["platform.info"]({
      input: {},
      ip: "ip",
      authorization: undefined,
      caller: () => Promise.reject(new Error("anonymous")),
    })) as { capabilities: string[]; challenge?: unknown };
    expect(info.capabilities).toContain("challenge.turnstile");
    expect(info.challenge).toEqual({
      provider: "turnstile",
      siteKey: "site",
      scope: ["auth.login", "links.accept"],
    });
    expect(
      await codeOf(() =>
        handlers["links.accept"]({
          input: { linkId: "0123456789abcdef", secret: "x" },
          ip: "ip",
          authorization: undefined,
          caller: () => Promise.reject(new Error("anonymous")),
        }),
      ),
    ).toBe("challenge_required");
  });

  it("没配挑战：platform.info 不报，带不带 challenge 行为都不变", async () => {
    const h = memoryHarness();
    const { handlers } = createPersonalProcedures(h.context, {
      stream: {} as never,
      webApp: () => null,
    });
    const info = (await handlers["platform.info"]({
      input: {},
      ip: "ip",
      authorization: undefined,
      caller: () => Promise.reject(new Error("anonymous")),
    })) as { capabilities: string[]; challenge?: unknown };
    expect(info.capabilities).not.toContain("challenge.turnstile");
    expect(info.challenge).toBeUndefined();
  });
});
