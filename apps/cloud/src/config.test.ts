import { ConfigError } from "@armadra/cloud-shared/config";
import { describe, expect, it } from "vitest";

import { loadMigrateConfig, loadServeConfig } from "./config.ts";

const base = {
  ARMADRA_CLOUD_ISSUER: "http://127.0.0.1:8100",
  ARMADRA_CLOUD_APP_ORIGIN: "http://127.0.0.1:8100",
  ARMADRA_CLOUD_DATABASE_URL:
    "postgres://armadra:pw@127.0.0.1:5440/armadra_cloud",
  ARMADRA_CLOUD_REDIS_URL: "redis://127.0.0.1:6390",
};

describe("loadServeConfig", () => {
  it("缺省监听 0.0.0.0:8100", () => {
    const config = loadServeConfig(base);
    expect(config.listen).toEqual({ host: "0.0.0.0", port: 8100 });
    expect(config.logLevel).toBe("info");
    expect(config.relayDomain).toBeNull();
  });

  it("缺必填项时列出全部变量名", () => {
    expect(() => loadServeConfig({})).toThrow(ConfigError);
    try {
      loadServeConfig({});
    } catch (error) {
      const issues = (error as ConfigError).issues.join("\n");
      for (const key of Object.keys(base)) expect(issues).toContain(key);
    }
  });

  it("master key 必须是 32 字节 hex", () => {
    expect(() =>
      loadServeConfig({ ...base, ARMADRA_CLOUD_MASTER_KEY: "abc" }),
    ).toThrow(ConfigError);
    expect(() =>
      loadServeConfig({ ...base, ARMADRA_CLOUD_MASTER_KEY: "a".repeat(64) }),
    ).not.toThrow();
  });
});

it("migrate 只要数据库地址，迁移目录缺省指向 control-store-postgres 的 migrations", () => {
  const config = loadMigrateConfig({
    ARMADRA_CLOUD_DATABASE_URL: base.ARMADRA_CLOUD_DATABASE_URL,
  });
  expect(config.migrationsDir.replaceAll("\\", "/")).toMatch(
    /control-store-postgres\/migrations\/$/,
  );
});
