#!/usr/bin/env node
/**
 * armadra-cloud <命令>
 *
 *   serve     起控制面（缺省）
 *   migrate   执行数据库迁移
 *   version   打印版本
 *
 * 配置全走环境变量（控制面设计 §9）；配置错误以 64 退出。
 */
import { CONFIG_EXIT_CODE, ConfigError } from "@armadra/cloud-shared/config";

import { loadMigrateConfig, loadServeConfig } from "./config.ts";
import { startCloud } from "./main.ts";
import { migrate } from "./migrate.ts";
import { CLOUD_VERSION } from "./version.ts";

export const USAGE = `用法：armadra-cloud <命令>

命令：
  serve     起控制面（缺省）
  migrate   执行数据库迁移
  version   打印版本

配置见 deploy/README.md（ARMADRA_CLOUD_* 环境变量）。`;

async function main(argv: string[]): Promise<number> {
  const [command = "serve", ...rest] = argv;
  if (
    command === "--help" ||
    command === "-h" ||
    command === "help" ||
    rest.includes("--help")
  ) {
    console.log(USAGE);
    return 0;
  }
  switch (command) {
    case "version":
      console.log(CLOUD_VERSION);
      return 0;
    case "migrate": {
      const result = await migrate(loadMigrateConfig(process.env));
      console.log(`迁移完成：新执行 ${result.applied} 个，数据库已是最新`);
      return 0;
    }
    case "serve": {
      const cloud = await startCloud(loadServeConfig(process.env));
      const stop = () => {
        void cloud.close().then(() => process.exit(0));
      };
      process.once("SIGTERM", stop);
      process.once("SIGINT", stop);
      return -1;
    }
    default:
      console.error(`未知命令：${command}\n\n${USAGE}`);
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
