// pnpm dev：读 .env.dev，并行起 cloud(8100，`node --watch` 改源码自动重启) 与 relay saas(8101，
// Go 二进制：ARMADRA_RELAY_BIN 或 go build 到 go/bin/armadra-relay，改 Go 代码要重启 pnpm dev)；
// 日志加前缀；任一退出就都退出。先跑 pnpm dev:up。
import { spawn } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { resolveRelayBin } from "./relay-bin.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const ENV_FILE = ".env.dev";

if (!existsSync(new URL(`../${ENV_FILE}`, import.meta.url))) {
  console.error(`缺少 ${ENV_FILE}：先运行 pnpm dev:up`);
  process.exit(1);
}

/** .env.dev（KEY=VALUE 行，不带引号）→ 对象；Go 中继不认 node 的 --env-file，自己读。 */
function readEnvFile(file) {
  const env = {};
  for (const line of readFileSync(file, "utf8").split("\n")) {
    const match = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(line.trim());
    if (match) env[match[1]] = match[2];
  }
  return env;
}

const processes = [
  {
    name: "cloud",
    command: process.execPath,
    args: [
      `--env-file=${ENV_FILE}`,
      "--watch",
      "--disable-warning=ExperimentalWarning",
      "apps/cloud/src/cli.ts",
      "serve",
    ],
    env: process.env,
  },
  {
    name: "relay",
    command: resolveRelayBin(root),
    args: ["saas", "serve"],
    env: {
      ...process.env,
      ...readEnvFile(new URL(`../${ENV_FILE}`, import.meta.url)),
    },
  },
];

const children = [];
let stopping = false;

function prefix(name, stream, target) {
  let pending = "";
  stream.setEncoding("utf8");
  stream.on("data", (chunk) => {
    pending += chunk;
    const lines = pending.split("\n");
    pending = lines.pop() ?? "";
    for (const line of lines) target.write(`[${name}] ${line}\n`);
  });
}

function stopAll(code) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill("SIGTERM");
  setTimeout(() => process.exit(code), 500).unref();
}

for (const { name, command, args, env } of processes) {
  const child = spawn(command, args, {
    cwd: root,
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  prefix(name, child.stdout, process.stdout);
  prefix(name, child.stderr, process.stderr);
  child.on("exit", (code, signal) => {
    if (!stopping) {
      console.error(`[${name}] 已退出（${signal ?? code}），停止全部`);
      stopAll(code ?? 1);
    }
  });
  children.push(child);
}

process.on("SIGINT", () => stopAll(0));
process.on("SIGTERM", () => stopAll(0));
