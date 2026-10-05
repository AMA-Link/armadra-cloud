#!/usr/bin/env node
/**
 * armadra-relay <saas|personal> <子命令>（平台设计 §17.8，D20）。参数解析不用库。
 * C0-1 只有 `saas serve` 可运行；`personal` 的子命令在 R2 落地。
 */
import { CONFIG_EXIT_CODE, ConfigError } from "@armadra/cloud-shared/config";

import { loadSaasConfig } from "./config.ts";
import { startSaasRelay } from "./main.ts";
import { RELAY_VERSION } from "./version.ts";

export const USAGE = `用法：armadra-relay <模式> <子命令> [选项]

模式：
  saas serve                     多租户中继（配置走 RELAY_* 环境变量）
  personal init|serve|status|passwd|links
                                 单人中转（不带数据库）
  version                        打印版本

选项：
  --help                         打印本帮助`;

export const PERSONAL_USAGE = `用法：armadra-relay personal <子命令> [选项]

子命令：
  init     --data-dir <目录> --account <名> [--password-file <文件>] [--host <主机>] [--port 8443]
  serve    --data-dir <目录> --host <公网 IP 或域名> [--port 8443] [--tls self-signed|acme|file|plain]
  status   --data-dir <目录>
  passwd   --data-dir <目录> [--password-file <文件>]
  links    list|revoke <linkId> --data-dir <目录>`;

/** 不支持的组合以 64 退出（EX_USAGE），尚未实现的以 69 退出（EX_UNAVAILABLE）。 */
const NOT_IMPLEMENTED_EXIT_CODE = 69;

async function main(argv: string[]): Promise<number> {
  const [mode, command, ...rest] = argv;
  const wantsHelp = argv.includes("--help") || argv.includes("-h");
  if (
    mode === undefined ||
    mode === "help" ||
    (wantsHelp && mode !== "personal")
  ) {
    console.log(USAGE);
    return mode === undefined ? CONFIG_EXIT_CODE : 0;
  }
  if (mode === "version") {
    console.log(RELAY_VERSION);
    return 0;
  }
  if (mode === "personal") {
    if (wantsHelp || command === undefined) {
      console.log(PERSONAL_USAGE);
      return wantsHelp ? 0 : CONFIG_EXIT_CODE;
    }
    console.error(`personal ${command} 尚未实现`);
    return NOT_IMPLEMENTED_EXIT_CODE;
  }
  if (mode === "saas" && command === "serve" && rest.length === 0) {
    const relay = await startSaasRelay(loadSaasConfig(process.env));
    const stop = () => {
      void relay.close().then(() => process.exit(0));
    };
    process.once("SIGTERM", stop);
    process.once("SIGINT", stop);
    return -1;
  }
  console.error(`未知命令：${argv.join(" ")}\n\n${USAGE}`);
  return CONFIG_EXIT_CODE;
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
