/** 部署配置的冒烟：按仓库里的 wrangler.jsonc 起本地 workerd，入口、绑定与生产 issuer 都对。 */
import { SELF } from "cloudflare:test";
import { expect, it } from "vitest";

const ORIGIN = "https://relay.armadra.com";

it("/health、平台信息与隧道入口按生产配置答", async () => {
  const health = await SELF.fetch(`${ORIGIN}/health`);
  expect(await health.json()).toMatchObject({
    ok: true,
    mode: "personal",
    node: "cf",
  });
  const info = (await (
    await SELF.fetch(`${ORIGIN}/.well-known/armadra-platform`, {
      headers: { "cf-connecting-ip": "203.0.113.7" },
    })
  ).json()) as Record<string, unknown>;
  expect(info).toMatchObject({
    mode: "personal",
    issuer: ORIGIN,
    protocol: { major: 1, minor: 3 },
    name: "relay.armadra.com",
  });
  // 没有页面产物（仓库里只有 .assetsignore）：不报 webApp，/app/ 答错误页。
  expect(info.webApp).toBeNull();
  expect((await SELF.fetch(`${ORIGIN}/app/`)).status).toBe(503);
  expect((await SELF.fetch(`${ORIGIN}/t/v1?src=bad`)).status).toBe(404);
  expect((await SELF.fetch(`${ORIGIN}/metrics`)).status).toBe(404);
});
