/**
 * 推送前本地把关（#243）。`.githooks/pre-push` 调用，`pnpm install` 时由
 * `tools/hooks/install.mjs` 把 `core.hooksPath` 指到 `.githooks`。
 *
 * 只查这次推送带上的改动，判定与 CI（.github/workflows/ci.yml 的 `changes` 作业）一致：
 *
 * - 改动按推送的 ref 范围算：已有的远端分支是「远端旧值..本地新值」，新分支是相对
 *   远端缺省分支的合并基点。删除分支、只推 tag 时什么都不做。
 * - 只改文档：改动文件的 prettier 与 `repo:check`（CI 的 docs 作业）。
 * - 有代码：`libs:build` 后并行跑 `pnpm check` 的各项（prettier 与 ESLint 只看改动文件，
 *   配置变了全量）、`repo:test`、协议包受影响的 vitest（`verify.mjs` 的 `planVitest`）；
 *   命中 CI 的 `go` 判定时加上 `scripts/go-check.mjs` 的 gofmt、vet、staticcheck、受影响
 *   包的 `go test -race`（`planGoScope` 挑包），最后比对 `go generate` 的生成物。
 * - terraform、镜像、协议包打包：装了 terraform 就 `fmt -check`，其余只提示本机命令。
 *
 * 失败就拦下推送（退出码 1）。临时跳过：`git push --no-verify`，或
 * `ARMADRA_SKIP_PREPUSH=1`（全部）/ `ARMADRA_SKIP_PREPUSH=lint,go:test`（指定步骤）。
 * 每步完整输出在 node_modules/.cache/pre-push/<步骤>.log。
 */
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { goSteps, listPackages } from "../../scripts/go-check.mjs";
import { planGoScope, planVitest } from "../ci/verify.mjs";

const ROOT = fileURLToPath(new URL("../../", import.meta.url));
const LOG_DIR = join(ROOT, "node_modules/.cache/pre-push");
const ZERO = /^0+$/;
const isWindows = process.platform === "win32";
const PROTOCOL = "packages/platform-protocol/";
const TERRAFORM = "deploy/cloudflare/terraform";

/** 命令行一次传多少个文件；再多就改跑全量，免得撞上命令行长度上限。 */
const MAX_FILE_ARGS = 200;

/** CI 的 `changes` 作业：返回 `{ code, go }`。 */
export function classify(files) {
  let code = files.length === 0;
  let go = files.length === 0;
  for (const file of files) {
    if (
      file.startsWith("go/") ||
      /^packages\/platform-protocol\/(schema|fixtures|src\/identity-vectors)\//.test(
        file,
      ) ||
      /^(scripts\/go-check\.mjs|package\.json|repo\.rules\.json)$/.test(file) ||
      file.startsWith(".github/workflows/")
    ) {
      code = true;
      go = true;
    } else if (
      file === "THIRD_PARTY_NOTICES.md" ||
      file.startsWith("docs/contracts/")
    )
      code = true;
    else if (!(
      file.endsWith(".md") ||
      file.startsWith("docs/") ||
      file.startsWith(".github/ISSUE_TEMPLATE/")
    ))
      code = true;
  }
  return { code, go };
}

const FORMAT_GLOBAL = [
  /^\.prettierrc/,
  /^\.prettierignore$/,
  /^package\.json$/,
  /^pnpm-lock\.yaml$/,
];
const LINT_GLOBAL = [
  /^eslint\.config\.js$/,
  /^package\.json$/,
  /^pnpm-lock\.yaml$/,
  /(^|\/)tsconfig[^/]*\.json$/,
];
const LINTABLE = /\.(c|m)?(j|t)sx?$/;

/** `pnpm check` 里除 libs:build、format、lint 以外的各项，加上 repo:test。 */
const WHOLE_REPO = [
  "typecheck",
  "repo:check",
  "contract:check",
  "protocol:check",
  "ci:workflows",
  "notices:check",
  "repo:test",
];

/**
 * 按改动的文件排出 pnpm 这一侧的步骤。返回 `{ docsOnly, go, libs, statics, vitest,
 * hints }`；步骤是 `{ id, args, cwd? }`（给 pnpm 的参数）。Go 的范围另由 `planGoScope` 定。
 */
export function planChecks(files, { base, exists = () => true } = {}) {
  const { code, go } = classify(files);
  const present = files.filter(exists);
  const format =
    files.some((file) => FORMAT_GLOBAL.some((re) => re.test(file))) ||
    present.length > MAX_FILE_ARGS
      ? { id: "format", args: ["format:check"] }
      : present.length > 0
        ? {
            id: "format",
            args: [
              "exec",
              "prettier",
              "--check",
              "--ignore-unknown",
              "--cache",
              "--cache-location",
              "node_modules/.cache/prettier/.prettier-cache",
              ...present,
            ],
          }
        : null;
  const repoCheck = { id: "repo:check", args: ["repo:check"] };
  if (!code)
    return {
      docsOnly: true,
      go: false,
      libs: null,
      statics: [format, repoCheck].filter(Boolean),
      vitest: null,
      hints: [],
    };

  const lintFiles = present.filter((file) => LINTABLE.test(file));
  const lint =
    files.some((file) => LINT_GLOBAL.some((re) => re.test(file))) ||
    lintFiles.length > MAX_FILE_ARGS
      ? { id: "lint", args: ["lint"] }
      : lintFiles.length > 0
        ? {
            id: "lint",
            args: [
              "exec",
              "eslint",
              "--cache",
              "--cache-location",
              "node_modules/.cache/eslint/",
              "--no-warn-ignored",
              ...lintFiles,
            ],
          }
        : null;
  const statics = [
    format,
    lint,
    ...WHOLE_REPO.map((id) => ({ id, args: [id] })),
  ].filter(Boolean);
  const vitestArgs = planVitest(files, { base });
  const hints = [];
  if (files.some((file) => /^go\/Dockerfile/.test(file)))
    hints.push(
      "改了 Dockerfile：CI 会构建两个镜像并冒烟，本机 pnpm images:build",
    );
  if (
    files.some(
      (file) =>
        file === `${PROTOCOL}package.json` ||
        file === "scripts/pack-protocol.mjs",
    )
  )
    hints.push(
      "改了协议包的打包：本机 pnpm protocol:pack --out <临时目录> 核对内容",
    );
  return {
    docsOnly: false,
    go,
    libs: { id: "libs:build", args: ["libs:build"] },
    statics,
    vitest: vitestArgs
      ? { id: "test:protocol", args: vitestArgs, cwd: PROTOCOL }
      : null,
    terraform: files.some((file) => file.startsWith(`${TERRAFORM}/`)),
    hints,
  };
}

/** pre-push 的标准输入：每行「本地 ref 本地 sha 远端 ref 远端 sha」。 */
export function parsePushLines(text) {
  return text
    .split("\n")
    .map((line) => line.trim().split(/\s+/))
    .filter((parts) => parts.length === 4)
    .map(([localRef, localSha, remoteRef, remoteSha]) => ({
      localRef,
      localSha,
      remoteRef,
      remoteSha,
    }));
}

/** 要查的 ref：删除（本地 sha 全 0）与非分支（tag 等）都不查。 */
export function refsToCheck(updates) {
  return updates.filter(
    (update) =>
      !ZERO.test(update.localSha) && update.remoteRef.startsWith("refs/heads/"),
  );
}

/** 解析 `ARMADRA_SKIP_PREPUSH`：`{ all }` 或要跳过的步骤名集合。 */
export function parseSkip(value) {
  const text = (value ?? "").trim();
  if (text === "") return { all: false, steps: new Set() };
  if (text === "1" || text === "true" || text === "all")
    return { all: true, steps: new Set() };
  return {
    all: false,
    steps: new Set(
      text
        .split(",")
        .map((part) => part.trim())
        .filter(Boolean),
    ),
  };
}

function git(args) {
  const result = spawnSync("git", args, { cwd: ROOT, encoding: "utf8" });
  return result.status === 0 ? result.stdout.trim() : null;
}

function defaultBranch(remote) {
  const head = git(["symbolic-ref", "--quiet", `refs/remotes/${remote}/HEAD`]);
  if (head) return head.replace(/^refs\/remotes\//, "");
  for (const candidate of [`${remote}/main`, "origin/main"])
    if (git(["rev-parse", "--verify", "--quiet", candidate])) return candidate;
  return null;
}

/** 已有的远端分支用远端旧值（强推时取合并基点），新分支用缺省分支的合并基点。 */
function baseOf(update, remote) {
  if (
    !ZERO.test(update.remoteSha) &&
    git(["cat-file", "-e", `${update.remoteSha}^{commit}`]) !== null
  )
    return git(["merge-base", update.remoteSha, update.localSha]);
  const branch = defaultBranch(remote);
  return branch ? git(["merge-base", branch, update.localSha]) : null;
}

/** 跑一个子进程，收齐输出：`{ ok, output }`。 */
function exec(command, args, cwd = ".") {
  return new Promise((resolve) => {
    const chunks = [];
    const child = spawn(command, args, {
      cwd: join(ROOT, cwd),
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

/** 跑一步并计时；`work` 答 `{ ok, output }`。 */
async function timed(id, work) {
  const started = Date.now();
  const { ok, output } = await work();
  const log = join(LOG_DIR, `${id.replace(/[^\w.-]/g, "_")}.log`);
  writeFileSync(log, output);
  console.log(
    `  ${ok ? "ok  " : "FAIL"} ${id} ${((Date.now() - started) / 1000).toFixed(1)}s`,
  );
  return { id, ok, log };
}

const pnpmStep = (step) =>
  timed(step.id, () => exec("pnpm", step.args, step.cwd));

const goStep = (step) =>
  timed(`go:${step.name}`, async () => {
    const { output, problems } = await step.run();
    return {
      ok: problems.length === 0,
      output: output + problems.map((problem) => `✗ ${problem}\n`).join(""),
    };
  });

function tail(file, lines = 30) {
  return readFileSync(file, "utf8")
    .trimEnd()
    .split("\n")
    .slice(-lines)
    .join("\n");
}

function hasCommand(command, args) {
  return spawnSync(command, args, { stdio: "ignore" }).status === 0;
}

async function main(argv, input) {
  // git 给钩子设的 GIT_DIR、GIT_INDEX_FILE 等会被子进程继承：测试在临时目录里起的 git
  // 会找错仓库（「must be run in a work tree」）。和 CI 一样干净，全部去掉。
  for (const name of Object.keys(process.env))
    if (name.startsWith("GIT_")) delete process.env[name];
  const skip = parseSkip(process.env.ARMADRA_SKIP_PREPUSH);
  if (skip.all) return 0;
  const remote = argv[0] && !argv[0].includes("/") ? argv[0] : "origin";
  const refs = refsToCheck(parsePushLines(input));
  if (refs.length === 0) return 0;

  const head = git(["rev-parse", "HEAD"]);
  const files = new Set();
  let base = null;
  let unknownBase = false;
  for (const ref of refs) {
    const refBase = baseOf(ref, remote);
    if (!refBase) {
      unknownBase = true;
      continue;
    }
    if (ref.localSha === head || base === null) base = refBase;
    const diff = git(["diff", "--name-only", refBase, ref.localSha]);
    for (const file of (diff ?? "").split("\n").filter(Boolean))
      files.add(file);
  }
  const changed = [...files].sort();
  if (changed.length === 0 && !unknownBase) return 0;
  if (!existsSync(join(ROOT, "node_modules"))) {
    console.error(
      "pre-push：没装依赖，先 pnpm install（或 git push --no-verify 跳过检查）",
    );
    return 1;
  }
  mkdirSync(LOG_DIR, { recursive: true });

  const plan = planChecks(unknownBase ? [] : changed, {
    base: unknownBase ? null : base,
    exists: (file) => existsSync(join(ROOT, file)),
  });
  console.log(
    `pre-push：${unknownBase ? "拿不到基点，按全量" : `相对 ${base.slice(0, 9)} 改了 ${changed.length} 个文件`}${plan.docsOnly ? "（只有文档）" : ""}`,
  );
  if (refs.some((ref) => ref.localSha !== head))
    console.log("  注意：推送的提交不是当前检出，检查跑在工作区上");
  const dirty = (git(["status", "--porcelain"]) ?? "")
    .split("\n")
    .map((line) => line.slice(3))
    .filter((file) => files.has(file));
  if (dirty.length > 0)
    console.log(
      `  注意：${dirty.length} 个改动文件还有未提交的修改，检查看的是工作区内容`,
    );

  const keep = (id) => {
    if (!skip.steps.has(id)) return true;
    console.log(`  skip ${id}`);
    return false;
  };
  const results = [];
  if (plan.libs && keep(plan.libs.id)) results.push(await pnpmStep(plan.libs));
  if (results.every((result) => result.ok)) {
    const parallel = plan.statics.filter((step) => keep(step.id)).map(pnpmStep);
    if (plan.vitest && keep(plan.vitest.id))
      parallel.push(pnpmStep(plan.vitest));
    let generate = [];
    if (plan.go) {
      const scope = planGoScope(
        unknownBase ? null : changed,
        unknownBase ? null : listPackages(),
      );
      const steps = goSteps({ scope, local: true }).filter((step) =>
        keep(`go:${step.name}`),
      );
      parallel.push(
        ...steps.filter((step) => step.name !== "generate").map(goStep),
      );
      generate = steps.filter((step) => step.name === "generate");
    }
    if (plan.terraform && keep("terraform")) {
      if (hasCommand("terraform", ["version"]))
        parallel.push(
          timed("terraform", () =>
            exec("terraform", [
              `-chdir=${TERRAFORM}`,
              "fmt",
              "-check",
              "-diff",
              "-recursive",
            ]),
          ),
        );
      else
        plan.hints.push(
          "改了 terraform：本机没有 terraform，CI 会跑 fmt -check 与 validate",
        );
    }
    results.push(...(await Promise.all(parallel)));
    // go generate 会重写 gen/，等编译它们的步骤都结束再跑。
    if (results.every((result) => result.ok))
      for (const step of generate) results.push(await goStep(step));
  }
  for (const hint of plan.hints ?? []) console.log(`提示：${hint}`);

  const failed = results.filter((result) => !result.ok);
  if (failed.length === 0) return 0;
  for (const result of failed)
    console.log(
      `\n── ${result.id} 失败（完整输出：${result.log}）──\n${tail(result.log)}`,
    );
  console.log(
    [
      "",
      `推送已拦下：${failed.map((result) => result.id).join("、")} 失败。修好、提交后再推。`,
      "  格式：pnpm format；Go 格式：gofmt -w go；生成物：pnpm --filter @armadra/platform-protocol fixtures / schema、cd go && go generate ./...",
      `  确需先推：git push --no-verify，或 ARMADRA_SKIP_PREPUSH=${failed[0].id} git push 只跳过这一步。`,
    ].join("\n"),
  );
  return 1;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = await main(
    process.argv.slice(2),
    readFileSync(process.stdin.fd, "utf8"),
  );
}
