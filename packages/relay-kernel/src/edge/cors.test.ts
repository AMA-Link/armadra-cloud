import { describe, expect, it } from "vitest";

import { NATIVE_APP_ORIGINS, originAllowed, errorCorsHeaders } from "./cors.ts";

describe("错误答复的 CORS", () => {
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

  it("错误答复：名单内回显 + Vary + 暴露 retry-after；名单外、无 Origin、* 都不加", () => {
    expect(
      errorCorsHeaders("https://app.example", ["https://app.example"]),
    ).toEqual({
      "access-control-allow-origin": "https://app.example",
      "access-control-expose-headers": "retry-after",
      vary: "origin",
    });
    expect(errorCorsHeaders("capacitor://localhost", [])).toMatchObject({
      "access-control-allow-origin": "capacitor://localhost",
    });
    expect(
      errorCorsHeaders("https://evil.example", ["https://app.example"]),
    ).toBeUndefined();
    expect(errorCorsHeaders(null, ["https://app.example"])).toBeUndefined();
    expect(errorCorsHeaders("*", ["*"])).toBeUndefined();
  });
});
