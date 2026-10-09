import { describe, expect, it } from "vitest";

import { createClientIp, normalizeIp } from "./client-ip.ts";

function req(peer: string, headers: Record<string, string> = {}) {
  return { headers, socket: { remoteAddress: peer } } as never;
}

describe("clientIp", () => {
  it("未配置：永远用对端地址，转发头一律忽略", () => {
    const clientIp = createClientIp();
    expect(
      clientIp(
        req("127.0.0.1", {
          "x-forwarded-for": "1.2.3.4",
          "x-real-ip": "5.6.7.8",
        }),
      ),
    ).toBe("127.0.0.1");
    expect(createClientIp({ proxies: [] })(req("::ffff:10.0.0.1"))).toBe(
      "10.0.0.1",
    );
  });

  it("对端不在受信列表：伪造的 XFF / X-Real-IP 被忽略", () => {
    const clientIp = createClientIp({ proxies: ["127.0.0.1/32"] });
    expect(
      clientIp(
        req("203.0.113.9", {
          "x-forwarded-for": "1.1.1.1",
          "x-real-ip": "2.2.2.2",
        }),
      ),
    ).toBe("203.0.113.9");
  });

  it("受信对端：取 XFF 里自右向左第一个不受信的地址", () => {
    const clientIp = createClientIp({
      proxies: ["127.0.0.1/32", "10.0.0.0/8"],
    });
    expect(
      clientIp(req("127.0.0.1", { "x-forwarded-for": "198.51.100.7" })),
    ).toBe("198.51.100.7");
    // 客户端自己塞的左侧伪造项不会被取到：右边第一个不受信的是 198.51.100.7
    expect(
      clientIp(
        req("127.0.0.1", {
          "x-forwarded-for": "9.9.9.9, 198.51.100.7, 10.1.1.1, 10.2.2.2",
        }),
      ),
    ).toBe("198.51.100.7");
    // 全部受信：取最左
    expect(
      clientIp(req("127.0.0.1", { "x-forwarded-for": "10.0.0.5, 10.0.0.6" })),
    ).toBe("10.0.0.5");
  });

  it("多个 XFF 头行合并按顺序处理", () => {
    const clientIp = createClientIp({ proxies: ["127.0.0.1"] });
    expect(
      clientIp({
        headers: { "x-forwarded-for": ["8.8.8.8", "198.51.100.7"] },
        socket: { remoteAddress: "127.0.0.1" },
      } as never),
    ).toBe("198.51.100.7");
  });

  it("IPv6：CIDR、映射地址、方括号", () => {
    const clientIp = createClientIp({
      proxies: ["::1/128", "fd00::/8", "127.0.0.1"],
    });
    expect(clientIp(req("::1", { "x-forwarded-for": "2001:db8::7" }))).toBe(
      "2001:db8::7",
    );
    expect(
      clientIp(req("::1", { "x-forwarded-for": "2001:DB8::7, fd00::2" })),
    ).toBe("2001:db8::7");
    expect(
      clientIp(req("::ffff:127.0.0.1", { "x-forwarded-for": "[2001:db8::8]" })),
    ).toBe("2001:db8::8");
    expect(
      clientIp(req("2001:db8::99", { "x-forwarded-for": "1.1.1.1" })),
    ).toBe("2001:db8::99");
    expect(
      createClientIp({ proxies: ["127.0.0.1"] })(
        req("::ffff:127.0.0.1", { "x-forwarded-for": "4.4.4.4" }),
      ),
    ).toBe("4.4.4.4");
  });

  it("X-Real-IP 作备选，prefer 可调", () => {
    const xffOnly = { "x-forwarded-for": "1.1.1.1" };
    const both = { "x-forwarded-for": "1.1.1.1", "x-real-ip": "2.2.2.2" };
    const first = createClientIp({ proxies: ["127.0.0.1"] });
    const real = createClientIp({ proxies: ["127.0.0.1"], prefer: "real-ip" });
    expect(first(req("127.0.0.1", both))).toBe("1.1.1.1");
    expect(real(req("127.0.0.1", both))).toBe("2.2.2.2");
    expect(first(req("127.0.0.1", { "x-real-ip": "2.2.2.2" }))).toBe("2.2.2.2");
    expect(real(req("127.0.0.1", xffOnly))).toBe("1.1.1.1");
  });

  it("头不合法：回退到对端", () => {
    const clientIp = createClientIp({ proxies: ["127.0.0.1"] });
    expect(
      clientIp(req("127.0.0.1", { "x-forwarded-for": "1.1.1.1, evil" })),
    ).toBe("127.0.0.1");
    expect(
      clientIp(req("127.0.0.1", { "x-real-ip": "1.1.1.1, 2.2.2.2" })),
    ).toBe("127.0.0.1");
    expect(clientIp(req("127.0.0.1"))).toBe("127.0.0.1");
  });

  it("配置不合法直接抛", () => {
    for (const bad of ["nope", "10.0.0.0/33", "::1/129", "1.2.3.4/x"])
      expect(() => createClientIp({ proxies: [bad] }), bad).toThrow();
  });

  it("normalizeIp", () => {
    expect(normalizeIp(" ::FFFF:1.2.3.4 ")).toBe("1.2.3.4");
    expect(normalizeIp("fe80::1%en0")).toBe("fe80::1");
    expect(normalizeIp("1.2.3")).toBeNull();
  });
});
