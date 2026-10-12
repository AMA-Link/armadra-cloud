import { describe, expect, it } from "vitest";

import { hostAddressing, pathAddressing } from "./addressing.ts";

const ID = "0123456789abcdef0123456789abcdef";

describe("addressing", () => {
  it("personal：/s/<id> 前缀剥掉后转发，query 保留", () => {
    const a = pathAddressing();
    const at = (url: string) => a.resolve({ url, host: "relay" });
    expect(at(`/s/${ID}/api/x?y=1`)).toEqual({
      sourceId: ID,
      path: "/api/x?y=1",
    });
    expect(at(`/s/${ID}`)).toEqual({ sourceId: ID, path: "/" });
    expect(at(`/s/${ID}?t=1`)).toEqual({ sourceId: ID, path: "/?t=1" });
    expect(at(`/s/${ID}x/api`)).toBeNull();
    expect(at(`/s/${ID.toUpperCase()}/api`)).toBeNull();
    expect(at("/s/abc/api")).toBeNull();
    expect(at("/health")).toBeNull();
  });

  it("saas：<id>.src.<域>，路径原样", () => {
    const a = hostAddressing("relay.armadra.test");
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relay.armadra.test:443` }),
    ).toEqual({ sourceId: ID, path: "/api/x" });
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relay.armadra.test` }),
    ).not.toBeNull();
    expect(
      a.resolve({ url: "/api/x", host: `${ID}.src.relayXarmadra.test` }),
    ).toBeNull();
    expect(a.resolve({ url: "/api/x", host: "relay.armadra.test" })).toBeNull();
    expect(a.resolve({ url: "/api/x", host: undefined })).toBeNull();
  });
});
