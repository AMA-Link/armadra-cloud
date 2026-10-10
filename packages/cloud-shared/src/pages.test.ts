import { describe, expect, it } from "vitest";

import {
  contentTypeFor,
  hashedAsset,
  PAGE_CSP,
  pageCsp,
  PAGE_MESSAGES,
  pickLocale,
  renderPage,
} from "./pages.ts";

describe("中继自有页面", () => {
  it("中英文案键一致", () => {
    expect(Object.keys(PAGE_MESSAGES.en).sort()).toEqual(
      Object.keys(PAGE_MESSAGES["zh-CN"]).sort(),
    );
  });

  it("按扩展名给类型，带哈希的文件名可永久缓存", () => {
    expect(contentTypeFor("/assets/index-D3fK9x2a.js")).toBe(
      "text/javascript; charset=utf-8",
    );
    expect(contentTypeFor("dir.v1/README")).toBe("application/octet-stream");
    expect(contentTypeFor("ICON.PNG")).toBe("image/png");
    expect(hashedAsset("index-D3fK9x2a.js")).toBe(true);
    expect(hashedAsset("index.html")).toBe(false);
  });

  it("页面用一次性 nonce，落地页带链接 id，语言按 Accept-Language", () => {
    expect(pickLocale("en-US,en;q=0.9")).toBe("en");
    expect(pickLocale(undefined)).toBe("zh-CN");
    const join = renderPage("en", "join", "0123456789abcdef");
    const nonce = /'nonce-([^']+)'/.exec(join.csp)?.[1];
    expect(nonce).toBeTruthy();
    expect(join.html).toContain(`nonce="${nonce}"`);
    expect(join.html).toContain('data-link="0123456789abcdef"');
    expect(join.html).toContain("Open in Armadra");
    const other = renderPage("zh-CN", "unavailable");
    expect(other.csp).not.toBe(join.csp);
    expect(other.html).toContain("页面不可用");
  });
});

describe("pageCsp", () => {
  const directive = (csp: string, name: string) =>
    csp.split("; ").find((item) => item.startsWith(`${name} `));

  it("没配 Turnstile：与 PAGE_CSP 完全相同", () => {
    expect(pageCsp("/app/challenge")).toBe(PAGE_CSP);
    expect(pageCsp("/app/")).toBe(PAGE_CSP);
    expect(PAGE_CSP).not.toContain("challenges.cloudflare.com");
    expect(directive(PAGE_CSP, "frame-ancestors")).toBe(
      "frame-ancestors 'none'",
    );
  });

  it("配了：放行 Turnstile 的 script / frame / connect；只有 /app/challenge 可被受信来源内嵌", () => {
    const csp = pageCsp("/app/challenge", {
      embedOrigins: ["https://relay.test", "https://x.test;script-src *", "*"],
    });
    expect(directive(csp, "script-src")).toBe(
      "script-src 'self' https://challenges.cloudflare.com",
    );
    expect(directive(csp, "connect-src")).toContain(
      "https://challenges.cloudflare.com",
    );
    expect(directive(csp, "frame-src")).toContain(
      "https://challenges.cloudflare.com",
    );
    expect(directive(csp, "frame-ancestors")).toBe(
      "frame-ancestors 'self' https://localhost capacitor://localhost http://localhost http://localhost:* http://127.0.0.1:* https://relay.test",
    );
    expect(csp).not.toContain("*;");
    for (const path of ["/app/", "/app/board/1", "/app/challengex"])
      expect(
        directive(pageCsp(path, { embedOrigins: [] }), "frame-ancestors"),
      ).toBe("frame-ancestors 'none'");
  });

  it("桌面壳（页面来源 http://127.0.0.1:<随机端口>）能内嵌挑战页；只放行回环主机的端口，不通配域名", () => {
    const ancestors = (
      directive(
        pageCsp("/app/challenge", { embedOrigins: [] }),
        "frame-ancestors",
      ) ?? ""
    )
      .split(" ")
      .slice(1);
    expect(ancestors).toContain("http://127.0.0.1:*");
    for (const source of ancestors)
      expect(source).toMatch(
        /^('self'|(https?|capacitor):\/\/(localhost|127\.0\.0\.1)(:\*)?)$/,
      );
  });
});
