import { describe, expect, it } from "vitest";

import { isLoopback, parsePersonalServe, UsageError } from "./args.ts";

describe("parsePersonalServe", () => {
  it("缺省：端口 8443、--tls self-signed；回环 host 只绑回环", () => {
    const options = parsePersonalServe(
      ["--data-dir", "d", "--host", "127.0.0.1"],
      {},
      "/work",
    );
    expect(options).toMatchObject({
      dataDir: "/work/d",
      host: "127.0.0.1",
      port: 8443,
      tls: "self-signed",
      listen: { host: "127.0.0.1", port: 8443 },
      logLevel: "info",
    });
  });

  it("公网 host 绑 0.0.0.0；RELAY_LISTEN 与 --listen 覆盖", () => {
    expect(
      parsePersonalServe(["--data-dir", "d", "--host", "203.0.113.5"], {})
        .listen,
    ).toEqual({
      host: "0.0.0.0",
      port: 8443,
    });
    expect(
      parsePersonalServe(["--data-dir", "d", "--host", "h", "--port", "9000"], {
        RELAY_LISTEN: "0.0.0.0:8443",
      }).listen,
    ).toEqual({ host: "0.0.0.0", port: 8443 });
    expect(
      parsePersonalServe(
        ["--data-dir", "d", "--host", "h", "--listen", "[::1]:1"],
        {},
      ).listen,
    ).toEqual({
      host: "::1",
      port: 1,
    });
  });

  it("环境变量兜底 data-dir 与 host", () => {
    const options = parsePersonalServe([], {
      RELAY_DATA_DIR: "/data",
      RELAY_HOST: "relay.example",
      RELAY_TLS: "plain",
    });
    expect(options).toMatchObject({
      dataDir: "/data",
      host: "relay.example",
      tls: "plain",
    });
  });

  it.each([
    [[], "--data-dir"],
    [["--data-dir", "d"], "--host"],
    [["--data-dir", "d", "--host", "h", "--port", "0"], "--port"],
    [["--data-dir", "d", "--host", "h", "--tls", "none"], "--tls"],
    [["--data-dir", "d", "--host", "h", "--bogus", "1"], "--bogus"],
    [["--data-dir"], "--data-dir"],
    [["stray"], "stray"],
  ])("%j 报 UsageError", (argv, mention) => {
    expect(() => parsePersonalServe(argv, {})).toThrow(UsageError);
    expect(() => parsePersonalServe(argv, {})).toThrow(mention);
  });
});

it("isLoopback", () => {
  expect(["127.0.0.1", "127.1.2.3", "::1", "localhost"].every(isLoopback)).toBe(
    true,
  );
  expect(["0.0.0.0", "203.0.113.5", "relay.example"].some(isLoopback)).toBe(
    false,
  );
});
