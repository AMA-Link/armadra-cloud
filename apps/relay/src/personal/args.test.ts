import { describe, expect, it } from "vitest";

import {
  defaultIssuer,
  isLoopback,
  parseLinks,
  parsePersonalInit,
  parsePersonalServe,
  UsageError,
} from "./args.ts";

describe("parsePersonalServe 限流额度", () => {
  const base = ["--data-dir", "d", "--host", "127.0.0.1"];
  it("缺省不给；命令行优先于环境变量", () => {
    expect(parsePersonalServe(base, {}, "/w").rateLimits).toBeUndefined();
    expect(
      parsePersonalServe(
        [...base, "--limit-token", "900"],
        { RELAY_LIMIT_TOKEN: "100", RELAY_LIMIT_PREFLIGHT: "50" },
        "/w",
      ).rateLimits,
    ).toEqual({ token: 900, preflight: 50 });
  });
  it("不是正整数就拒绝", () => {
    for (const bad of ["0", "-1", "1.5", "abc"])
      expect(() =>
        parsePersonalServe([...base, "--limit-anonymous", bad], {}, "/w"),
      ).toThrow(UsageError);
  });
});

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

  it("环境变量兜底 data-dir、host、tls、web-root 与首次初始化的账号", () => {
    const options = parsePersonalServe(
      [],
      {
        RELAY_DATA_DIR: "/data",
        RELAY_HOST: "relay.example",
        RELAY_TLS: "self-signed",
        RELAY_WEB_ROOT: "/app/web",
        RELAY_PERSONAL_ACCOUNT: "me",
        RELAY_PERSONAL_PASSWORD_FILE: "/run/secrets/pw",
        RELAY_PUBLIC_ORIGIN: "https://relay.example/ignored-path",
      },
      "/work",
    );
    expect(options).toMatchObject({
      dataDir: "/data",
      host: "relay.example",
      tls: "self-signed",
      webRoot: "/app/web",
      account: "me",
      passwordFile: "/run/secrets/pw",
      publicOrigin: "https://relay.example",
    });
  });

  it("--tls plain 只允许监听回环地址", () => {
    expect(
      parsePersonalServe(
        ["--data-dir", "d", "--host", "127.0.0.1", "--tls", "plain"],
        {},
      ).tls,
    ).toBe("plain");
    expect(() =>
      parsePersonalServe(
        ["--data-dir", "d", "--host", "relay.example", "--tls", "plain"],
        {},
      ),
    ).toThrow("--tls plain 只允许监听回环地址");
    expect(() =>
      parsePersonalServe(
        [
          "--data-dir",
          "d",
          "--host",
          "127.0.0.1",
          "--tls",
          "plain",
          "--listen",
          "0.0.0.0:8443",
        ],
        {},
      ),
    ).toThrow(UsageError);
  });

  it("--tls file 要成对的证书文件；--tls acme 要邮箱", () => {
    expect(() =>
      parsePersonalServe(
        ["--data-dir", "d", "--host", "h", "--tls", "file", "--tls-cert", "c"],
        {},
      ),
    ).toThrow("--tls-key");
    expect(() =>
      parsePersonalServe(
        ["--data-dir", "d", "--host", "h", "--tls", "acme"],
        {},
      ),
    ).toThrow("--acme-email");
    expect(
      parsePersonalServe(
        [
          "--data-dir",
          "d",
          "--host",
          "relay.example",
          "--tls",
          "acme",
          "--acme-email",
          "ops@example.com",
        ],
        {},
        "/work",
      ).acme,
    ).toEqual({
      email: "ops@example.com",
      httpListen: { host: "0.0.0.0", port: 80 },
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

describe("parsePersonalInit", () => {
  it("要账号与 host；账号名只允许字母数字与 . _ -", () => {
    expect(
      parsePersonalInit(
        [
          "--data-dir",
          "d",
          "--account",
          "me",
          "--host",
          "h",
          "--password-file",
          "-",
        ],
        {},
        "/work",
      ),
    ).toMatchObject({
      dataDir: "/work/d",
      account: "me",
      host: "h",
      port: 8443,
      passwordFile: "-",
    });
    expect(() =>
      parsePersonalInit(["--data-dir", "d", "--host", "h"], {}),
    ).toThrow("--account");
    expect(() =>
      parsePersonalInit(
        ["--data-dir", "d", "--host", "h", "--account", "a b"],
        {},
      ),
    ).toThrow(UsageError);
  });
});

describe("parseLinks", () => {
  it("list | revoke <16 位 hex>", () => {
    expect(parseLinks(["list", "--data-dir", "d"], {}, "/w")).toEqual({
      action: "list",
      dataDir: "/w/d",
    });
    expect(
      parseLinks(["revoke", "0123456789abcdef", "--data-dir", "d"], {}, "/w"),
    ).toEqual({
      action: "revoke",
      linkId: "0123456789abcdef",
      dataDir: "/w/d",
    });
    expect(() => parseLinks(["revoke", "x", "--data-dir", "d"], {})).toThrow(
      UsageError,
    );
    expect(() => parseLinks(["drop"], {})).toThrow(UsageError);
  });
});

it("defaultIssuer：443 省略端口，IPv6 加方括号", () => {
  expect(defaultIssuer("relay.example", 443)).toBe("https://relay.example");
  expect(defaultIssuer("203.0.113.5", 8443)).toBe("https://203.0.113.5:8443");
  expect(defaultIssuer("2001:db8::1", 8443)).toBe("https://[2001:db8::1]:8443");
});

it("isLoopback", () => {
  expect(["127.0.0.1", "127.1.2.3", "::1", "localhost"].every(isLoopback)).toBe(
    true,
  );
  expect(["0.0.0.0", "203.0.113.5", "relay.example"].some(isLoopback)).toBe(
    false,
  );
});

describe("parsePersonalServe 受信代理", () => {
  const base = ["--data-dir", "d", "--host", "127.0.0.1"];
  it("缺省不给（不信任任何代理）", () => {
    expect(parsePersonalServe(base, {}, "/w").trustedProxy).toBeUndefined();
  });
  it("命令行优先于环境变量，逗号分隔", () => {
    expect(
      parsePersonalServe(
        [
          ...base,
          "--trusted-proxy",
          "127.0.0.1/32, ::1/128",
          "--trusted-proxy-prefer",
          "real-ip",
        ],
        { RELAY_TRUSTED_PROXY: "10.0.0.0/8" },
        "/w",
      ).trustedProxy,
    ).toEqual({ proxies: ["127.0.0.1/32", "::1/128"], prefer: "real-ip" });
    expect(
      parsePersonalServe(base, { RELAY_TRUSTED_PROXY: "10.0.0.0/8" }, "/w")
        .trustedProxy,
    ).toEqual({ proxies: ["10.0.0.0/8"] });
  });
  it("不合法就拒绝", () => {
    for (const extra of [
      ["--trusted-proxy", "nope"],
      ["--trusted-proxy", "10.0.0.0/40"],
      ["--trusted-proxy", "127.0.0.1", "--trusted-proxy-prefer", "x"],
      ["--trusted-proxy-prefer", "real-ip"],
    ])
      expect(() => parsePersonalServe([...base, ...extra], {}, "/w")).toThrow(
        UsageError,
      );
  });
});
