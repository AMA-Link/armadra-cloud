import { describe, expect, it } from "vitest";

import {
  contentTypeFor,
  hashedAsset,
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
