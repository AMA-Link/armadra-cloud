#!/usr/bin/env node
/**
 * armadra-relay <模式> <子命令>（平台设计 §17.8，D20）。参数解析不用库。
 *
 * 两种模式各自按需 import：personal 不加载 Redis 客户端，saas 不加载 personal 的状态。
 * 现在可运行的是 `personal serve`（健康检查，`--tls plain`）与 `saas serve`（预留）；
 * personal 的其余子命令在 R2 落地。
 */
import { CONFIG_EXIT_CODE, ConfigError } from "@armadra/cloud-shared/config";

import { RELAY_VERSION } from "./version.ts";

export const USAGE = `用法: armadra-relay <模式> <子命令> [选项]

  personal init|serve|status|passwd|links   单人中转，不带数据库
  saas serve                                多租户中继（预留，配置走 RELAY_* 环境变量）
  version                                   打印版本

personal --help 列出 personal 的子命令与选项。`;

export const PERSONAL_USAGE = `用法: armadra-relay personal <子命令> [选项]

  serve    起个人中转
  init     建账号与签名密钥（尚未实现）
  status   打印状态与 CA 指纹（尚未实现）
  passwd   改口令（尚未实现）
  links    list | revoke <linkId>（尚未实现）

serve:
  --data-dir DIR         数据目录（或 RELAY_DATA_DIR）
  --host HOST            对外的 IP 或域名（或 RELAY_HOST）
  --port PORT            端口，默认 8443
  --tls MODE             self-signed | acme | file | plain，默认 self-signed；现在只支持 plain
  --listen HOST:PORT     监听地址（或 RELAY_LISTEN），默认回环 --host 用 127.0.0.1，否则 0.0.0.0
  --log-level LEVEL      debug | info | warn | error，默认 info
  --metrics-listen H:P   指标监听（或 RELAY_METRICS_LISTEN），默认不开；RELAY_METRICS_TOKEN 设访问令牌`;

/** 尚未实现的子命令以 69 退出（EX_UNAVAILABLE），用法错误以 64 退出（EX_USAGE）。 */
const NOT_IMPLEMENTED_EXIT_CODE = 69;

function onSignal(stop: () => Promise<void>): void {
  const handler = () => {
    void stop().then(() => process.exit(0));
  };
  process.once("SIGTERM", handler);
  process.once("SIGINT", handler);
}

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
  if (command !== "serve") {
    if (!["init", "status", "passwd", "links"].includes(command)) {
      console.error(`未知子命令：personal ${command}\n\n${PERSONAL_USAGE}`);
      return CONFIG_EXIT_CODE;
    }
    console.error(`personal ${command} 尚未实现`);
    return NOT_IMPLEMENTED_EXIT_CODE;
  }
  const { parsePersonalServe, UsageError } = await import("./personal/args.ts");
  const { startPersonalRelay } = await import("./personal/serve.ts");
  try {
    const relay = await startPersonalRelay(
      parsePersonalServe(rest, process.env),
    );
    onSignal(relay.close);
    return -1;
  } catch (error) {
    if (error instanceof UsageError) {
      console.error(error.message);
      return /尚未实现/.test(error.message)
        ? NOT_IMPLEMENTED_EXIT_CODE
        : CONFIG_EXIT_CODE;
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
