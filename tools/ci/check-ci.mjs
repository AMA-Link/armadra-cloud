/**
 * 并行跑静态检查（CI 的 check 作业与根 `pnpm check` 共用）。
 *
 *   node tools/ci/check-ci.mjs [--no-repo-test]
 *
 * 1. 先跑 `libs:build`（其余检查读协议包的构建产物）。
 * 2. 并行跑 `format:check`、`lint`、`typecheck`、`repo:check`、`contract:check`、
 *    `protocol:check`、`ci:workflows`、`notices:check`（缺省再加 `repo:test`；
 *    `pnpm check` 传 `--no-repo-test` 保持原有语义）。
 */
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { STATICS } from "./verify.mjs";

const root = fileURLToPath(new URL("../../", import.meta.url));
const logDir = join(root, "node_modules/.cache/check-ci");
const isWindows = process.platform === "win32";

/** 按命令行标志选出在 `libs:build` 之后并行执行的步骤。 */
export function selectStaticSteps(argv = []) {
  const noRepoTest = argv.includes("--no-repo-test");
  return noRepoTest ? STATICS.filter((id) => id !== "repo:test") : [...STATICS];
}

function logFile(id) {
  return join(logDir, `${id.replace(/[^\w.-]/g, "_")}.log`);
}

function runPnpm(args) {
  return new Promise((resolve) => {
    const chunks = [];
    const child = spawn("pnpm", args, {
      cwd: root,
      shell: isWindows,
      env: { ...process.env, FORCE_COLOR: "0" },
    });
    child.stdout.on("data", (chunk) => chunks.push(chunk));
    child.stderr.on("data", (chunk) => chunks.push(chunk));
    let settled = false;
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      resolve({ ok, output: Buffer.concat(chunks) });
    };
    child.on("error", (error) => {
      chunks.push(Buffer.from(`${error.message}\n`));
      finish(false);
    });
    child.on("close", (code) => finish(code === 0));
  });
}

async function timed(id, work) {
  const started = Date.now();
  const { ok, output } = await work();
  const log = logFile(id);
  writeFileSync(log, output);
  const seconds = (Date.now() - started) / 1000;
  process.stdout.write(
    `${ok ? "  ok  " : "  FAIL"} ${id} ${seconds.toFixed(1)}s\n`,
  );
  return { id, ok, seconds, output: output.toString("utf8"), log };
}

function report(results, started) {
  const failed = results.filter((result) => !result.ok);
  for (const result of failed) {
    process.stderr.write(
      `\n── ${result.id} 失败（日志：${result.log}）──\n${result.output.trimEnd()}\n`,
    );
  }
  const total = ((Date.now() - started) / 1000).toFixed(1);
  if (failed.length > 0) {
    console.error(
      `\n${failed.length} 步失败：${failed.map((r) => r.id).join("、")}（用时 ${total}s）`,
    );
    return 1;
  }
  console.log(`检查通过（${results.length} 步，用时 ${total}s）`);
  return 0;
}

export async function main(argv) {
  const steps = selectStaticSteps(argv);
  mkdirSync(logDir, { recursive: true });
  const started = Date.now();
  const results = [];

  results.push(await timed("libs:build", () => runPnpm(["libs:build"])));
  if (!results[0].ok) return report(results, started);

  const parallel = steps.map((id) => timed(id, () => runPnpm([id])));
  results.push(...(await Promise.all(parallel)));
  return report(results, started);
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = await main(process.argv.slice(2));
}
