import { ConfigError } from "@armadra/cloud-shared/config";
import { expect, it } from "vitest";

import { loadSaasConfig } from "./config.ts";

const base = {
  RELAY_DOMAIN: "src.localhost",
  RELAY_CLOUD_ISSUER: "http://127.0.0.1:8100",
  RELAY_REDIS_URL: "redis://127.0.0.1:6390",
};

it("缺省监听 0.0.0.0:8443，节点名取主机名", () => {
  const config = loadSaasConfig(base);
  expect(config.listen).toEqual({ host: "0.0.0.0", port: 8443 });
  expect(config.nodeId).toMatch(/^[A-Za-z0-9._-]{1,64}$/);
  expect(config.allowInsecureTunnel).toBe(false);
});

it("RELAY_ALLOW_INSECURE_TUNNEL=1 打开明文隧道", () => {
  expect(
    loadSaasConfig({ ...base, RELAY_ALLOW_INSECURE_TUNNEL: "1" })
      .allowInsecureTunnel,
  ).toBe(true);
});

it("saas 缺 issuer / Redis / 域名时拒绝", () => {
  expect(() => loadSaasConfig({})).toThrow(ConfigError);
});
