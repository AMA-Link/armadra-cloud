#!/usr/bin/env node
/**
 * armadra-relay <模式> <子命令>（平台设计 §17.8，D20）。参数解析不用库。
 *
 * 两种模式各自按需 import：personal 不加载 Redis 客户端，saas 不加载 personal 的状态。
 * 退出码：0 成功；64 用法 / 配置错误（EX_USAGE）；1 其它失败。
 */
import { CONFIG_EXIT_CODE, ConfigError } from "@armadra/cloud-shared/config";

import { RELAY_VERSION } from "./version.ts";

export const USAGE = `用法: armadra-relay <模式> <子命令> [选项]

  personal init|serve|status|passwd|rotate-key|links   单人中转，不带数据库
  saas serve                                           多租户中继（配置走 RELAY_* 环境变量）
  health                                               探活本机 /health（镜像的 HEALTHCHECK 用）
  version                                              打印版本

personal --help 列出 personal 的子命令与选项。`;

export const PERSONAL_USAGE = `用法: armadra-relay personal <子命令> [选项]

  init         建账号与签名钥：--data-dir --account [--password-file F] --host [--port] [--tls]
  serve        起个人中转
  status       打印账号、源、链接与 CA 指纹：--data-dir
  passwd       改口令并让账号的全部设备重新登录：--data-dir [--password-file F]
  rotate-key   启用新签名钥，旧钥一小时后退役：--data-dir
  links        list | revoke <linkId>：--data-dir

serve:
  --data-dir DIR           数据目录（或 RELAY_DATA_DIR）
  --host HOST              对外的 IP 或域名（或 RELAY_HOST）
  --port PORT              端口，默认 8443
  --public-origin URL      对外来源（或 RELAY_PUBLIC_ORIGIN），默认 https://<host>:<port>
  --tls MODE               self-signed | acme | file | plain，默认 self-signed；plain 只允许回环监听
  --tls-cert F --tls-key F 证书文件（--tls file；或 RELAY_TLS_CERT / RELAY_TLS_KEY），变化时热加载
  --acme-email E           ACME 账号邮箱（--tls acme）
  --acme-directory URL     ACME 目录，默认 Let's Encrypt
  --acme-ca-bundle F       信任 ACME 目录服务器的 PEM（测试 CA）
  --acme-http-listen H:P   http-01 挑战监听，默认 0.0.0.0:80
  --web-root DIR           Armadra apps/web 的构建产物（或 RELAY_WEB_ROOT），托管在 /app 与 /j
  --account NAME           首次启动自动初始化的账号（或 RELAY_PERSONAL_ACCOUNT）
  --password-file F        首次启动自动初始化的口令文件（或 RELAY_PERSONAL_PASSWORD_FILE）
  --listen HOST:PORT       监听地址（或 RELAY_LISTEN），默认回环 --host 用 127.0.0.1，否则 0.0.0.0
  --log-level LEVEL        debug | info | warn | error，默认 info
  --metrics-listen H:P     指标监听（或 RELAY_METRICS_LISTEN），默认不开；RELAY_METRICS_TOKEN 设访问令牌
  --limit-anonymous N      无令牌或令牌无效的请求按 IP 每分钟上限（或 RELAY_LIMIT_ANONYMOUS），默认 60
  --limit-preflight N      预检按 IP 每分钟上限（或 RELAY_LIMIT_PREFLIGHT），默认 300
  --limit-token N          有效令牌每分钟上限（或 RELAY_LIMIT_TOKEN），默认 600
  --limit-account N        同一账号全部令牌每分钟上限（或 RELAY_LIMIT_ACCOUNT），默认 1800`;

function onSignal(stop: () => Promise<void>): void {
  const handler = () => {
    void stop().then(() => process.exit(0));
  };
  process.once("SIGTERM", handler);
  process.once("SIGINT", handler);
}

const print = (line: string) => console.log(line);

async function personal(
  command: string | undefined,
  rest: string[],
): Promise<number> {
  if (
    command === undefined ||
    command === "--help" ||
    command === "-h" ||
    rest.includes("--help")
  ) {
    console.log(PERSONAL_USAGE);
    return command === undefined ? CONFIG_EXIT_CODE : 0;
  }
  const args = await import("./personal/args.ts");
  const { StateError } = await import("./personal/state.ts");
  const { TlsError } = await import("./personal/tls.ts");
  try {
    switch (command) {
      case "serve": {
        const { startPersonalRelay } = await import("./personal/serve.ts");
        const relay = await startPersonalRelay(
          args.parsePersonalServe(rest, process.env),
        );
        onSignal(relay.close);
        return -1;
      }
      case "init": {
        const { runInit } = await import("./personal/commands.ts");
        await runInit(args.parsePersonalInit(rest, process.env), print);
        return 0;
      }
      case "status": {
        const { runStatus } = await import("./personal/commands.ts");
        runStatus(args.parseDataDirOnly(rest, process.env).dataDir, print);
        return 0;
      }
      case "passwd": {
        const { runPasswd } = await import("./personal/commands.ts");
        const options = args.parsePasswd(rest, process.env);
        await runPasswd(options.dataDir, options.passwordFile, print);
        return 0;
      }
      case "rotate-key": {
        const { runRotateKey } = await import("./personal/commands.ts");
        runRotateKey(args.parseDataDirOnly(rest, process.env).dataDir, print);
        return 0;
      }
      case "links": {
        const commands = await import("./personal/commands.ts");
        const options = args.parseLinks(rest, process.env);
        if (options.action === "list")
          commands.runLinksList(options.dataDir, print);
        else commands.runLinksRevoke(options.dataDir, options.linkId, print);
        return 0;
      }
      default:
        console.error(`未知子命令：personal ${command}\n\n${PERSONAL_USAGE}`);
        return CONFIG_EXIT_CODE;
    }
  } catch (error) {
    if (error instanceof args.UsageError) {
      console.error(error.message);
      return CONFIG_EXIT_CODE;
    }
    if (error instanceof StateError) {
      console.error(error.message);
      return error.code === "not_initialized" || error.code === "exists"
        ? CONFIG_EXIT_CODE
        : 1;
    }
    if (error instanceof TlsError) {
      console.error(error.message);
      return 1;
    }
    throw error;
  }
}

async function saas(
  command: string | undefined,
  rest: string[],
): Promise<number> {
  if (command === "--help" || rest.includes("--help")) {
    console.log(USAGE);
    return 0;
  }
  if (command !== "serve" || rest.length > 0) {
    console.error(
      `未知命令：saas ${[command, ...rest].filter(Boolean).join(" ")}\n\n${USAGE}`,
    );
    return CONFIG_EXIT_CODE;
  }
  const { loadSaasConfig } = await import("./config.ts");
  const { startSaasRelay } = await import("./saas/main.ts");
  const relay = await startSaasRelay(loadSaasConfig(process.env));
  onSignal(relay.close);
  return -1;
}

/**
 * 镜像的 HEALTHCHECK：打本机监听端口的 `/health`，先 HTTPS（自签证书，不验）再明文；
 * 200 就是活着。端口取 `RELAY_LISTEN`，缺省 8443。
 */
async function health(): Promise<number> {
  const port = Number(
    /:(\d+)$/.exec(process.env.RELAY_LISTEN ?? "")?.[1] ?? 8443,
  );
  const probe = async (secure: boolean) => {
    const { get } = await import(secure ? "node:https" : "node:http");
    return new Promise<number>((resolve) => {
      const req = get(
        {
          host: "127.0.0.1",
          port,
          path: "/health",
          timeout: 3000,
          ...(secure ? { rejectUnauthorized: false } : {}),
        },
        (res: import("node:http").IncomingMessage) => {
          res.resume();
          resolve(res.statusCode ?? 0);
        },
      );
      req.on("timeout", () => req.destroy());
      req.on("error", () => resolve(0));
    });
  };
  const status = (await probe(true)) || (await probe(false));
  return status === 200 ? 0 : 1;
}

async function main(argv: string[]): Promise<number> {
  const [mode, command, ...rest] = argv;
  switch (mode) {
    case undefined:
      console.log(USAGE);
      return CONFIG_EXIT_CODE;
    case "--help":
    case "-h":
    case "help":
      console.log(USAGE);
      return 0;
    case "version":
      console.log(RELAY_VERSION);
      return 0;
    case "health":
      return health();
    case "personal":
      return personal(command, rest);
    case "saas":
      return saas(command, rest);
    default:
      console.error(`未知命令：${argv.join(" ")}\n\n${USAGE}`);
      return CONFIG_EXIT_CODE;
  }
}

main(process.argv.slice(2)).then(
  (code) => {
    if (code >= 0) process.exit(code);
  },
  (error: unknown) => {
    if (error instanceof ConfigError) {
      console.error(error.message);
      process.exit(CONFIG_EXIT_CODE);
    }
    console.error((error as Error).message);
    process.exit(1);
  },
);
