// pnpm dev：读 .env.dev，并行起 cloud(8100) 与 relay saas(8101)，`node --watch`
// 改源码自动重启；日志加前缀；任一退出就都退出。先跑 pnpm dev:up。
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const ENV_FILE = ".env.dev";

if (!existsSync(new URL(`../${ENV_FILE}`, import.meta.url))) {
  console.error(`缺少 ${ENV_FILE}：先运行 pnpm dev:up`);
  process.exit(1);
}

const processes = [
  { name: "cloud", args: ["apps/cloud/src/cli.ts", "serve"] },
  { name: "relay", args: ["apps/relay/src/cli.ts", "saas", "serve"] },
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

for (const { name, args } of processes) {
  const child = spawn(
    process.execPath,
    [
      `--env-file=${ENV_FILE}`,
      "--watch",
      "--disable-warning=ExperimentalWarning",
      ...args,
    ],
    { cwd: root, stdio: ["ignore", "pipe", "pipe"] },
  );
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
