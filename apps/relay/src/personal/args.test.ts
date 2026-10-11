import { describe, expect, it } from "vitest";

import {
  defaultIssuer,
  isLoopback,
  parseBackup,
  parseLinks,
  parseMigrate,
  parsePersonalDoctor,
  parsePersonalInit,
  parsePersonalServe,
  parseDuration,
  parseTraffic,
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

describe("parsePersonalServe 服务端名称", () => {
  const base = ["--data-dir", "d", "--host", "127.0.0.1"];
  it("缺省不给（serve 再取状态文件与 --host）", () => {
    expect(parsePersonalServe(base, {}, "/w").name).toBeUndefined();
  });
  it("命令行优先于环境变量，去首尾空白", () => {
    expect(
      parsePersonalServe([...base, "--name", "  家里  "], {
        RELAY_NAME: "公司",
      }).name,
    ).toBe("家里");
    expect(parsePersonalServe(base, { RELAY_NAME: "公司" }).name).toBe("公司");
    expect(parsePersonalServe(base, { RELAY_NAME: "" }).name).toBeUndefined();
  });
  it("空白或超过 128 个字符就拒绝", () => {
    for (const value of ["   ", "x".repeat(129)])
      expect(() =>
        parsePersonalServe([...base, "--name", value], {}, "/w"),
      ).toThrow(UsageError);
  });
});

describe("存储参数", () => {
  const serve = ["--data-dir", "d", "--host", "relay.example"];
  it("--store：缺省不给（按数据目录定）；命令行优先于 RELAY_STORE；只认 sqlite | file", () => {
    expect(parsePersonalServe(serve, {}).store).toBeUndefined();
    expect(parsePersonalServe(serve, { RELAY_STORE: "file" }).store).toBe(
      "file",
    );
    expect(
      parsePersonalServe([...serve, "--store", "sqlite"], {
        RELAY_STORE: "file",
      }).store,
    ).toBe("sqlite");
    expect(() => parsePersonalServe([...serve, "--store", "pg"], {})).toThrow(
      "--store 只能是 file | sqlite",
    );
    expect(
      parsePersonalInit([...serve, "--account", "me", "--store", "file"], {})
        .store,
    ).toBe("file");
  });

  it("migrate --to 必填且只认 sqlite | file；backup 要 --out", () => {
    expect(
      parseMigrate(["--data-dir", "d", "--to", "sqlite"], {}, "/w"),
    ).toEqual({ dataDir: "/w/d", to: "sqlite" });
    expect(() => parseMigrate(["--data-dir", "d"], {})).toThrow("--to");
    expect(() => parseMigrate(["--data-dir", "d", "--to", "pg"], {})).toThrow(
      "--to 只能是",
    );
    expect(parseBackup(["--data-dir", "d", "--out", "b.db"], {}, "/w")).toEqual(
      { dataDir: "/w/d", out: "/w/b.db" },
    );
    expect(() => parseBackup(["--data-dir", "d"], {})).toThrow("--out");
  });

  it("doctor 收 serve 的同名参数与环境变量；明文公网不在这里拒绝", () => {
    expect(
      parsePersonalDoctor(["--data-dir", "d", "--tls", "plain"], {}, "/w"),
    ).toEqual({
      dataDir: "/w/d",
      listen: { host: "0.0.0.0", port: 8443 },
      tls: "plain",
    });
    expect(
      parsePersonalDoctor(["--data-dir", "d"], {
        RELAY_HOST: "127.0.0.1",
        RELAY_TRUSTED_PROXY: "127.0.0.1/32",
        RELAY_PERSONAL_PASSWORD_FILE: "/run/pw",
      }),
    ).toMatchObject({
      listen: { host: "127.0.0.1", port: 8443 },
      tls: "self-signed",
      trustedProxy: "127.0.0.1/32",
      passwordFile: "/run/pw",
    });
  });
});

describe("流量统计参数", () => {
  const base = ["--data-dir", "d", "--host", "127.0.0.1"];
  it("serve 保留天数：缺省 90；命令行优先于 RELAY_TRAFFIC_RETENTION_DAYS；0 = 不记录", () => {
    expect(parsePersonalServe(base, {}, "/w").trafficRetentionDays).toBe(90);
    expect(
      parsePersonalServe(base, { RELAY_TRAFFIC_RETENTION_DAYS: "30" }, "/w")
        .trafficRetentionDays,
    ).toBe(30);
    expect(
      parsePersonalServe(
        [...base, "--traffic-retention-days", "0"],
        { RELAY_TRAFFIC_RETENTION_DAYS: "30" },
        "/w",
      ).trafficRetentionDays,
    ).toBe(0);
    expect(
      parsePersonalServe(base, { RELAY_TRAFFIC_RETENTION_DAYS: "" }, "/w")
        .trafficRetentionDays,
    ).toBe(90);
    for (const bad of ["-1", "1.5", "abc", "3651"])
      expect(() =>
        parsePersonalServe(
          [...base, "--traffic-retention-days", bad],
          {},
          "/w",
        ),
      ).toThrow(UsageError);
  });

  it("traffic：--since 缺省 7 天，认 d / h / m；--source 要 32 位 hex", () => {
    expect(parseTraffic(["--data-dir", "d"], {}, "/w")).toEqual({
      dataDir: "/w/d",
      sinceMs: 7 * 86_400_000,
    });
    expect(
      parseTraffic(
        ["--data-dir", "d", "--since", "24h", "--source", "a".repeat(32)],
        {},
        "/w",
      ),
    ).toEqual({
      dataDir: "/w/d",
      sinceMs: 86_400_000,
      sourceId: "a".repeat(32),
    });
    expect(parseDuration("30m")).toBe(1_800_000);
    for (const bad of ["0d", "7", "7w", "-1d"])
      expect(parseDuration(bad)).toBeNull();
    expect(() =>
      parseTraffic(["--data-dir", "d", "--since", "week"], {}, "/w"),
    ).toThrow(UsageError);
    expect(() =>
      parseTraffic(["--data-dir", "d", "--source", "nope"], {}, "/w"),
    ).toThrow(UsageError);
  });
});
