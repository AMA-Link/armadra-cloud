/**
 * 本机一条命令跑完 CI 的 check 与 go 两个作业的检查（#219）。
 *
 *   pnpm verify [--full] [--base <ref>] [--no-test]
 *
 * 1. `libs:build`（其余步骤都读协议包的产物；增量编译，没改动时一秒内）。
 * 2. 并行：prettier、ESLint、typecheck、仓库规则、契约、协议夹具、workflow、许可证、
 *    repo:test，加上 Go 的 gofmt、go vet、staticcheck、go test -race 与协议包的 vitest。
 *    prettier 与 ESLint 带缓存、tsc 是增量的，第二次起只看改过的文件。
 * 3. 最后 go generate，比对生成物（它会重写 gen/，不和编译同时跑）。
 *
 * 测试缺省只跑相对 `--base`（缺省 origin/main）的合并基点受影响的部分，未提交与未跟踪的
 * 文件也算改动：Go 只查改过的包与直接或间接 import 它们的包，go test 保留缓存（规则见
 * scripts/go-check.mjs 的 planGo）；协议包交给 `vitest run --changed <基点>` 按模块图挑，
 * 测试按路径读的夹具、schema 与配置变了就整包跑。根配置、锁文件变了，或拿不到基点，就全量。
 * `--full` 强制全量（Go 也带 -count=1，与 CI 相同）；`--no-test` 只跑静态检查。
 *
 * 没有 REDIS_URL、MAILPIT_*、ARMADRA_CLOUD_TEST_DATABASE_URL 时，相关集成测试跳过
 * （`pnpm dev:up` 起这些服务；CI 的 go 作业带服务容器，全部都跑）。
 *
 * 结尾打印每一步的耗时；任何一步失败，退出码非零，并打印那一步输出的末尾。完整输出
 * 在 node_modules/.cache/verify/<步骤>.log。CI 仍然全量跑，这里只是本机的快路径。
 */
import { spawn } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  changedFiles,
  goSteps,
  listPackages,
  planGo,
} from "../../scripts/go-check.mjs";

const root = fileURLToPath(new URL("../../", import.meta.url));
const logDir = join(root, "node_modules/.cache/verify");

/** 这些文件一变，任何测试结果都可能变：全量。 */
const GLOBAL_TRIGGERS = [
  /^package\.json$/,
  /^pnpm-lock\.yaml$/,
  /^pnpm-workspace\.yaml$/,
  /^tsconfig\.base\.json$/,
  /^\.npmrc$/,
];

const PROTOCOL = "packages/platform-protocol/";

/** 静态检查：都是根 package.json 里的脚本，互不依赖。 */
export const STATICS = [
  "format:check",
  "lint",
  "typecheck",
  "repo:check",
  "contract:check",
  "protocol:check",
  "ci:workflows",
  "notices:check",
  "repo:test",
];

/**
 * 协议包的 vitest 怎么跑。测试会按路径读夹具、schema 与 test/ 下的 JSON，这些变化
 * vitest 的模块图看不见：包里 src/、test/ 的 .ts 以外的文件一变就整包跑。
 * 返回在包目录里给 pnpm 的参数；不用跑时返回 null。
 */
export function planVitest(changed, { full = false, base = null } = {}) {
  const run = ["exec", "vitest", "run"];
  if (full || changed === null || base === null) return run;
  if (changed.some((file) => GLOBAL_TRIGGERS.some((re) => re.test(file))))
    return run;
  const mine = changed.filter((file) => file.startsWith(PROTOCOL));
  if (mine.length === 0) return null;
  const graphOnly = mine.every((file) =>
    /^packages\/platform-protocol\/(src|test)\/.*\.ts$/.test(file),
  );
  return graphOnly ? [...run, "--changed", base, "--passWithNoTests"] : run;
}

/**
 * 挑 Go 的范围：`--full` 或根配置变了就全量，否则交给 planGo。
 * `packages` 为 null（go list 失败）时全量，让 go vet 报出原因。
 */
export function planGoScope(changed, packages, { full = false } = {}) {
  if (full || changed === null || packages === null)
    return { full: true, packages: [] };
  if (changed.some((file) => GLOBAL_TRIGGERS.some((re) => re.test(file))))
    return { full: true, packages: [] };
  return planGo(changed, packages);
}

const isWindows = process.platform === "win32";

function logFile(id) {
  // Windows 的文件名里不能有冒号。
  return join(logDir, `${id.replace(/[^\w.-]/g, "_")}.log`);
}

/** 跑一步并计时：`work` 答 { ok, output }。返回 { id, ok, seconds, log }。 */
async function timed(id, work) {
  const started = Date.now();
  const { ok, output } = await work();
  const log = logFile(id);
  writeFileSync(log, output);
  const seconds = (Date.now() - started) / 1000;
  process.stdout.write(
    `${ok ? "  ok  " : "  FAIL"} ${id} ${seconds.toFixed(1)}s\n`,
  );
  return { id, ok, seconds, log };
}

/** 跑一条 pnpm 命令，收齐输出。 */
function pnpm(args, cwd = ".") {
  return new Promise((resolve) => {
    const chunks = [];
    const child = spawn("pnpm", args, {
      cwd: join(root, cwd),
      shell: isWindows,
      env: { ...process.env, FORCE_COLOR: "0" },
    });
    child.stdout.on("data", (chunk) => chunks.push(chunk));
    child.stderr.on("data", (chunk) => chunks.push(chunk));
    let done = false;
    const finish = (ok) => {
      if (done) return;
      done = true;
      resolve({ ok, output: Buffer.concat(chunks) });
    };
    child.on("error", (error) => {
      chunks.push(Buffer.from(`${error.message}\n`));
      finish(false);
    });
    child.on("close", (code) => finish(code === 0));
  });
}

/** 把 go-check 的一步包成 verify 的一步。 */
function goStep(step) {
  return timed(`go:${step.name}`, async () => {
    const { output, problems } = await step.run();
    const lines = problems.map((problem) => `✗ ${problem}\n`).join("");
    return { ok: problems.length === 0, output: output + lines };
  });
}

function tail(file, lines = 40) {
  return readFileSync(file, "utf8").split("\n").slice(-lines).join("\n");
}

const INTEGRATION_ENV = [
  "REDIS_URL",
  "MAILPIT_SMTP_ADDR",
  "ARMADRA_CLOUD_TEST_DATABASE_URL",
];

async function main(argv) {
  const full = argv.includes("--full");
  const noTest = argv.includes("--no-test");
  const baseIndex = argv.indexOf("--base");
  const ref = baseIndex >= 0 ? argv[baseIndex + 1] : "origin/main";
  mkdirSync(logDir, { recursive: true });
  const started = Date.now();
  const results = [];

  console.log("libs");
  results.push(await timed("libs:build", () => pnpm(["libs:build"])));
  if (!results[0].ok) return report(results, started);

  const { base, files } = changedFiles(ref);
  const everything = full || files === null;
  console.log(
    everything
      ? `范围：全量${full ? "" : `（拿不到 ${ref} 的合并基点）`}`
      : `范围：相对 ${ref} 的合并基点 ${base.slice(0, 9)}，改动 ${files.length} 个文件`,
  );
  const scope = planGoScope(files, everything ? null : listPackages(), {
    full,
  });
  // --no-test 只去掉 go test，vet 与 staticcheck 是静态检查，照跑。
  const steps = goSteps({ scope, local: !full }).filter(
    (step) => !noTest || step.name !== "test",
  );
  console.log(
    scope.full ? "Go：全量" : `Go：受影响的包 ${scope.packages.length} 个`,
  );
  const missing = INTEGRATION_ENV.filter((name) => !process.env[name]);
  if (!noTest && missing.length > 0)
    console.log(`没有 ${missing.join("、")}：相关集成测试跳过（pnpm dev:up）`);
  const vitest = noTest ? null : planVitest(files, { full, base });

  console.log("检查与测试（并行）");
  const parallel = [
    ...STATICS.map((id) => timed(id, () => pnpm([id]))),
    ...steps.filter((step) => step.name !== "generate").map(goStep),
  ];
  if (vitest)
    parallel.push(timed("test:protocol", () => pnpm(vitest, PROTOCOL)));
  else if (!noTest) console.log("  协议包没有受影响的测试");
  results.push(...(await Promise.all(parallel)));

  for (const step of steps.filter((step) => step.name === "generate"))
    results.push(await goStep(step));
  return report(results, started);
}

function report(results, started) {
  const failed = results.filter((result) => !result.ok);
  for (const result of failed) {
    console.log(
      `\n── ${result.id} 失败（完整输出：${result.log}）──\n${tail(result.log)}`,
    );
  }
  const width = Math.max(...results.map((result) => result.id.length));
  console.log("\n耗时");
  for (const result of [...results].sort((a, b) => b.seconds - a.seconds))
    console.log(
      `  ${result.id.padEnd(width)}  ${result.seconds.toFixed(1).padStart(6)}s${result.ok ? "" : "  失败"}`,
    );
  const total = ((Date.now() - started) / 1000).toFixed(1);
  console.log(
    failed.length === 0
      ? `\n全部通过，用时 ${total}s`
      : `\n${failed.length} 步失败：${failed.map((r) => r.id).join("、")}；用时 ${total}s`,
  );
  return failed.length === 0 ? 0 : 1;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = await main(process.argv.slice(2));
}
