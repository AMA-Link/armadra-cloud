import {
  cloudContract,
  listProcedures,
} from "@armadra/platform-protocol/cloud-api";
import { describe, expect, it } from "vitest";

import { createCloudRouter } from "./router.ts";

const SOURCE_ID = "0123456789abcdef0123456789abcdef";
const ok = () => ({});

describe("createCloudRouter", () => {
  it("给了处理器的是启用的，其余是预留的，合起来就是整份契约（按声明顺序）", () => {
    const router = createCloudRouter({ "sources.get": ok, "auth.login": ok });
    const contract = listProcedures(cloudContract).map((entry) => entry.key);
    expect(router.enabled.map((route) => route.key)).toEqual(
      contract.filter((key) => key === "sources.get" || key === "auth.login"),
    );
    expect(router.reserved.map((route) => route.key)).toEqual(
      contract.filter((key) => key !== "sources.get" && key !== "auth.login"),
    );
  });

  it("exclude 的 procedure 两边都不在", () => {
    const router = createCloudRouter({}, { exclude: ["platform.health"] });
    expect(router.reserved.map((route) => route.key)).not.toContain(
      "platform.health",
    );
    expect(router.match("GET", "/health")).toBeNull();
  });

  it("静态段优先；参数按段解码", () => {
    const router = createCloudRouter({
      "sources.heartbeatHint": ok,
      "sources.get": ok,
    });
    expect(router.match("GET", "/v1/sources/me/relay")?.route.key).toBe(
      "sources.heartbeatHint",
    );
    const hit = router.match("GET", `/v1/sources/${SOURCE_ID}`);
    expect(hit?.route.key).toBe("sources.get");
    expect(hit?.params).toEqual({ sourceId: SOURCE_ID });
    expect(router.match("GET", "/v1/sources/a%2Fb")?.params).toEqual({
      sourceId: "a/b",
    });
  });

  it("先查启用表再查预留表；只命中预留的答 handler null；都不中答 null", () => {
    const router = createCloudRouter({ "sources.heartbeatHint": ok });
    const enabled = router.match("GET", "/v1/sources/me/relay");
    expect(enabled?.route.key).toBe("sources.heartbeatHint");
    expect(enabled?.handler).toBe(ok);
    const reserved = router.match("GET", `/v1/sources/${SOURCE_ID}`);
    expect(reserved).toMatchObject({
      route: { key: "sources.get" },
      params: { sourceId: SOURCE_ID },
      handler: null,
    });
    expect(router.match("POST", "/v1/auth/login")?.handler).toBeNull();
    expect(router.match("GET", "/v1/nope")).toBeNull();
    expect(router.match("PUT", "/v1/me")).toBeNull();
  });

  it("处理器的名字拼错直接拒绝（否则会悄悄变成预留）", () => {
    expect(() => createCloudRouter({ "sources.gett": ok })).toThrow(
      "sources.gett",
    );
  });
});
