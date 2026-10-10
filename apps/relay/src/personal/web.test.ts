import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { listen, type RunningServer } from "@armadra/cloud-shared/http";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  createWebPages,
  PAGE_CSP,
  PAGE_MESSAGES,
  pickLocale,
  usableWebRoot,
} from "./web.ts";

const root = mkdtempSync(join(tmpdir(), "relay-web-"));
const webRoot = join(root, "dist");
const LINK = "0123456789abcdef";
let withRoot: RunningServer;
let withoutRoot: RunningServer;
let withChallenge: RunningServer;

beforeAll(async () => {
  mkdirSync(join(webRoot, "assets"), { recursive: true });
  writeFileSync(
    join(webRoot, "index.html"),
    "<!doctype html><title>armadra</title>",
  );
  writeFileSync(join(webRoot, "assets", "index-B9KJZmSX.js"), "console.log(1)");
  writeFileSync(join(webRoot, "icon.png"), "png");
  writeFileSync(join(root, "secret.txt"), "outside");
  symlinkSync(join(root, "secret.txt"), join(webRoot, "assets", "leak.txt"));
  const serve =
    (pages: ReturnType<typeof createWebPages>) =>
    (req: IncomingMessage, res: ServerResponse) => {
      if (!pages.handle(req, res)) {
        res.writeHead(404);
        res.end();
      }
    };
  withRoot = await listen(
    { host: "127.0.0.1", port: 0 },
    serve(
      createWebPages({ webRoot: usableWebRoot(webRoot), caPem: () => "PEM" }),
    ),
  );
  withChallenge = await listen(
    { host: "127.0.0.1", port: 0 },
    serve(
      createWebPages({
        webRoot: usableWebRoot(webRoot),
        caPem: () => undefined,
        challenge: () => ({ embedOrigins: ["https://relay.test"] }),
      }),
    ),
  );
  withoutRoot = await listen(
    { host: "127.0.0.1", port: 0 },
    serve(
      createWebPages({
        webRoot: usableWebRoot(join(root, "none")),
        caPem: () => undefined,
      }),
    ),
  );
});
afterAll(async () => {
  await withRoot.close();
  await withoutRoot.close();
  await withChallenge.close();
  rmSync(root, { recursive: true, force: true });
});

describe("有页面产物", () => {
  it("/ 与 /app 308 到 /app/；/app/*、/j/<id> 答 index.html（CSP、不缓存）", async () => {
    const home = await fetch(`${withRoot.url}/`, { redirect: "manual" });
    expect(home.status).toBe(308);
    expect(home.headers.get("location")).toBe("/app/");
    for (const path of ["/app/", "/app/board/42", `/j/${LINK}`]) {
      const response = await fetch(`${withRoot.url}${path}`);
      expect(response.status, path).toBe(200);
      expect(await response.text()).toContain("<title>armadra</title>");
      expect(response.headers.get("content-security-policy")).toBe(PAGE_CSP);
      expect(response.headers.get("cache-control")).toBe("no-store");
      expect(response.headers.get("referrer-policy")).toBe("no-referrer");
    }
    expect(PAGE_CSP).toContain("connect-src 'self'");
  });

  it("根下的真实文件直接答：带哈希的资源永久缓存", async () => {
    const asset = await fetch(`${withRoot.url}/assets/index-B9KJZmSX.js`);
    expect(asset.status).toBe(200);
    expect(asset.headers.get("content-type")).toContain("text/javascript");
    expect(asset.headers.get("cache-control")).toContain("immutable");
    expect(
      (await fetch(`${withRoot.url}/app/icon.png`)).headers.get("content-type"),
    ).toBe("image/png");
  });

  it("越界与符号链接都拿不到根外的文件", async () => {
    for (const path of [
      "/app/..%2f..%2fsecret.txt",
      "/app/assets/leak.txt",
      "/assets/leak.txt",
    ]) {
      const response = await fetch(`${withRoot.url}${path}`);
      expect(await response.text(), path).not.toContain("outside");
    }
  });

  it("/ca.crt 发 CA", async () => {
    const response = await fetch(`${withRoot.url}/ca.crt`);
    expect(await response.text()).toBe("PEM");
    expect(response.headers.get("content-type")).toBe("application/x-pem-file");
  });
});

describe("没有页面产物", () => {
  it("/app/* 答错误页（503），文案按 Accept-Language，带语言切换，脚本与样式只靠 nonce", async () => {
    const zh = await fetch(`${withoutRoot.url}/app/`);
    expect(zh.status).toBe(503);
    const html = await zh.text();
    expect(html).toContain('lang="zh-CN"');
    expect(html).toContain("页面不可用");
    expect(html).toContain('id="relay-locale"');
    const csp = zh.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("default-src 'none'");
    expect(csp).toMatch(/script-src 'nonce-[A-Za-z0-9+/=]+'/);
    const en = await fetch(`${withoutRoot.url}/app/x`, {
      headers: { "accept-language": "en-US,en;q=0.9" },
    });
    expect(await en.text()).toContain("Page unavailable");
  });

  it("/j/<id> 答最小落地页：一个「在 Armadra 中打开」深链，片段只在浏览器里拼", async () => {
    const response = await fetch(`${withoutRoot.url}/j/${LINK}`);
    expect(response.status).toBe(200);
    const html = await response.text();
    expect(html).toContain(`data-link="${LINK}"`);
    expect(html).toContain("在 Armadra 中打开");
    expect(html).toContain("armadra://join?");
    expect(html).toContain("location.hash");
    expect((await fetch(`${withoutRoot.url}/ca.crt`)).status).toBe(404);
    expect((await fetch(`${withoutRoot.url}/assets/x.js`)).status).toBe(404);
  });

  it("文案中英同步，按 Accept-Language 选初始语言", () => {
    expect(Object.keys(PAGE_MESSAGES.en).sort()).toEqual(
      Object.keys(PAGE_MESSAGES["zh-CN"]).sort(),
    );
    expect(pickLocale("en-GB,en")).toBe("en");
    expect(pickLocale("zh-CN,zh;q=0.9")).toBe("zh-CN");
    expect(pickLocale(undefined)).toBe("zh-CN");
  });
});

describe("开了 Turnstile", () => {
  it("/app/* 的 CSP 放行 Turnstile；只有 /app/challenge 可被受信来源内嵌", async () => {
    const challenge = await fetch(`${withChallenge.url}/app/challenge`);
    const csp = challenge.headers.get("content-security-policy") ?? "";
    expect(csp).toContain(
      "script-src 'self' https://challenges.cloudflare.com",
    );
    expect(csp).toContain(
      "frame-ancestors 'self' https://localhost capacitor://localhost http://localhost http://localhost:* https://relay.test",
    );
    await challenge.arrayBuffer();
    const other = await fetch(`${withChallenge.url}/app/board/1`);
    expect(other.headers.get("content-security-policy")).toContain(
      "frame-ancestors 'none'",
    );
    await other.arrayBuffer();
  });

  it("没开：/app/challenge 的头与其他页相同", async () => {
    const response = await fetch(`${withRoot.url}/app/challenge`);
    expect(response.headers.get("content-security-policy")).toBe(PAGE_CSP);
    await response.arrayBuffer();
  });
});
