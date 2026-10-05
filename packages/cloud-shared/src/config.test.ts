import { describe, expect, it } from "vitest";
import { z } from "zod";

import {
  ConfigError,
  flagSchema,
  listenSchema,
  parseConfig,
  parseListen,
} from "./config.ts";

describe("parseListen", () => {
  it("解析 IPv4、主机名与 IPv6", () => {
    expect(parseListen("0.0.0.0:8100")).toEqual({
      host: "0.0.0.0",
      port: 8100,
    });
    expect(parseListen("localhost:0")).toEqual({ host: "localhost", port: 0 });
    expect(parseListen("[::1]:8443")).toEqual({ host: "::1", port: 8443 });
  });

  it("拒绝缺端口与越界端口", () => {
    expect(() => parseListen("8100")).toThrow();
    expect(() => parseListen("host:70000")).toThrow();
  });
});

describe("parseConfig", () => {
  const schema = z.object({
    LISTEN: listenSchema.default({ host: "127.0.0.1", port: 8100 }),
    DATABASE_URL: z.url(),
    FLAG: flagSchema,
  });

  it("空字符串等于没设，缺省值生效", () => {
    const config = parseConfig(schema, {
      LISTEN: "",
      DATABASE_URL: "postgres://u:p@127.0.0.1:5440/db",
    });
    expect(config.LISTEN).toEqual({ host: "127.0.0.1", port: 8100 });
    expect(config.FLAG).toBe(false);
  });

  it("错误只列变量名，不回显取值", () => {
    try {
      parseConfig(schema, {
        DATABASE_URL: "not a url secret-value",
        LISTEN: "bad",
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ConfigError);
      const message = (error as ConfigError).message;
      expect(message).toContain("DATABASE_URL");
      expect(message).toContain("LISTEN");
      expect(message).not.toContain("secret-value");
    }
  });
});
