import type { IncomingMessage } from "node:http";

import { describe, expect, it } from "vitest";

import {
  isPreflight,
  NATIVE_APP_ORIGINS,
  originAllowed,
  PREFLIGHT_HEADERS,
  preflightHeaders,
} from "./cors.ts";

const req = (method: string, headers: Record<string, string>) =>
  ({ method, headers }) as unknown as IncomingMessage;

describe("CORS 预检", () => {
  it("只有 OPTIONS + Origin + Access-Control-Request-Method 才算预检", () => {
    const origin = { origin: "https://localhost" };
    const asked = { ...origin, "access-control-request-method": "POST" };
    expect(isPreflight(req("OPTIONS", asked))).toBe(true);
    expect(isPreflight(req("OPTIONS", origin))).toBe(false);
    expect(
      isPreflight(req("OPTIONS", { "access-control-request-method": "POST" })),
    ).toBe(false);
    expect(isPreflight(req("POST", asked))).toBe(false);
    expect(isPreflight(req("GET", asked))).toBe(false);
  });

  it("名单：原生 App 两个来源总放行；其余按规范拼法与可信来源比", () => {
    for (const origin of NATIVE_APP_ORIGINS)
      expect(originAllowed(origin, [])).toBe(true);
    const trusted = ["https://Relay.Example:443", "http://127.0.0.1:5173"];
    expect(originAllowed("https://relay.example", trusted)).toBe(true);
    expect(originAllowed("http://127.0.0.1:5173", trusted)).toBe(true);
    expect(originAllowed("https://evil.example", trusted)).toBe(false);
    expect(originAllowed("https://relay.example:8443", trusted)).toBe(false);
    // 不是规范拼法的 Origin（浏览器不会这样发）一律不认。
    expect(originAllowed("https://relay.example/", trusted)).toBe(false);
    expect(originAllowed("https://RELAY.example", trusted)).toBe(false);
    expect(originAllowed("null", trusted)).toBe(false);
    expect(originAllowed("*", ["*"])).toBe(false);
    expect(originAllowed("capacitor://evil", [])).toBe(false);
    expect(originAllowed("http://localhost", [])).toBe(false);
  });

  it("答案按 Origin 回显，带 Vary: Origin，不用 * 也不发 allow-credentials；头含中继令牌", () => {
    const headers = preflightHeaders("https://localhost");
    expect(headers["access-control-allow-origin"]).toBe("https://localhost");
    expect(headers.vary).toBe("origin");
    expect(headers["access-control-allow-credentials"]).toBeUndefined();
    expect(headers["access-control-allow-methods"]).toBe(
      "GET,POST,PUT,PATCH,DELETE,OPTIONS",
    );
    expect(PREFLIGHT_HEADERS.split(", ")).toEqual([
      "authorization",
      "content-type",
      "x-armadra-csrf",
      "armadra-relay-token",
    ]);
  });
});
