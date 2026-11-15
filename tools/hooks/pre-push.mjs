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
/** 未跟踪时会被类型检查、lint、测试或 Go 编译读到的源码。 */
const SOURCE = /\.(tsx?|mjs|js|go)$/;

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

/**
 * 去掉 GIT_* 的环境。git 给钩子设的 GIT_DIR、GIT_INDEX_FILE 等会被子进程继承：
 * 测试在临时目录里起的 git 会改到本仓库。
 */
export function cleanEnv(env = process.env) {
  return Object.fromEntries(
    Object.entries(env).filter(([name]) => !name.startsWith("GIT_")),
  );
}

function gitRaw(args) {
  const result = spawnSync("git", args, {
    cwd: ROOT,
    encoding: "utf8",
    env: cleanEnv(),
  });
  return result.status === 0 ? result.stdout : null;
}

function git(args) {
  return gitRaw(args)?.trim() ?? null;
}

/** `git status --porcelain -z` 的条目：`{ code, path }`；改名只取新路径。 */
export function parseStatus(text) {
  const entries = [];
  const parts = text.split("\0");
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index];
    if (part.length < 4) continue;
    const code = part.slice(0, 2);
    entries.push({ code, path: part.slice(3) });
    // 改名、复制的下一段是原路径。
    if (code[0] === "R" || code[0] === "C") index += 1;
  }
  return entries;
}

/**
 * 工作区里推送范围之外的东西：未跟踪的源码（`untrackedCode`，默认拦）、其它未跟踪
 * 文件、范围外与范围内的未提交修改。检查跑在工作区上，这些都会让结果与 CI 不同。
 */
export function workingTreeReport(entries, changed, sourcePattern = SOURCE) {
  const report = {
    untrackedCode: [],
    untrackedOther: [],
    outside: [],
    inside: [],
  };
  for (const { code, path } of entries) {
    if (code === "??")
      (sourcePattern.test(path)
        ? report.untrackedCode
        : report.untrackedOther
      ).push(path);
    else if (code !== "!!")
      (changed.has(path) ? report.inside : report.outside).push(path);
  }
  return report;
}

function defaultBranch(remote) {
  const head = git(["symbolic-ref", "--quiet", `refs/remotes/${remote}/HEAD`]);
  if (head) return head.replace(/^refs\/remotes\//, "");
  for (const candidate of [`${remote}/main`, "origin/main"])
    if (git(["rev-parse", "--verify", "--quiet", candidate])) return candidate;
  return null;
}

/**
 * 基点：远端旧值与本地新值的合并基点、缺省分支与本地新值的合并基点，取更靠近本地
 * 新值的那个。新分支只有后者；变基后强推时前者落在很早的地方，后者才对。
 */
function baseOf(update, remote) {
  const branch = defaultBranch(remote);
  const candidates = [
    !ZERO.test(update.remoteSha) &&
    git(["cat-file", "-e", `${update.remoteSha}^{commit}`]) !== null
      ? git(["merge-base", update.remoteSha, update.localSha])
      : null,
    branch ? git(["merge-base", branch, update.localSha]) : null,
  ].filter(Boolean);
  if (candidates.length < 2) return candidates[0] ?? null;
  const [fromRemote, fromBranch] = candidates;
  const ancestor =
    spawnSync("git", ["merge-base", "--is-ancestor", fromRemote, fromBranch], {
      cwd: ROOT,
      env: cleanEnv(),
    }).status === 0;
  return ancestor ? fromBranch : fromRemote;
}

/** 跑一个子进程，收齐输出：`{ ok, output }`。 */
function exec(command, args, cwd = ".") {
  return new Promise((resolve) => {
    const chunks = [];
    const child = spawn(command, args, {
      cwd: join(ROOT, cwd),
      shell: isWindows,
      env: { ...cleanEnv(), FORCE_COLOR: "0" },
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

/** `go/` 下与 HEAD 不同的文件与未跟踪的文件（go generate 之后看）。 */
export function generatedDrift(diff, untracked) {
  return [...diff.split("\n"), ...untracked.split("\n")].filter(Boolean);
}

/**
 * go generate 之后，`go/` 必须与 HEAD 一致：推上去的是提交，不是工作区。只比生成前后
 * 会漏掉「上次钩子已经在工作区重新生成、没提交就重推」。
 */
const generateStep = (step) =>
  timed(`go:${step.name}`, async () => {
    const { output, problems } = await step.run();
    const drift = generatedDrift(
      gitRaw(["diff", "--name-only", "HEAD", "--", "go/"]) ?? "",
      gitRaw(["ls-files", "--others", "--exclude-standard", "--", "go/"]) ?? "",
    );
    const lines = [...problems];
    if (drift.length > 0)
      lines.push(
        `go generate 之后与 HEAD 不一致：${drift.join("、")}。gen/ 已在工作区更新，提交即可；其余是未提交的 Go 改动，先提交或还原`,
      );
    return {
      ok: lines.length === 0,
      output: output + lines.map((line) => `✗ ${line}\n`).join(""),
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
  // 子进程一律不带 GIT_*（见 cleanEnv）；go-check.mjs 的子进程也读 process.env。
  for (const name of Object.keys(process.env))
    if (name.startsWith("GIT_")) delete process.env[name];
  const skip = parseSkip(process.env.ARMADRA_SKIP_PREPUSH);
  if (skip.all) return 0;
  const remote = argv[0] && !argv[0].includes("/") ? argv[0] : "origin";
  const refs = refsToCheck(parsePushLines(input));
  if (refs.length === 0) return 0;

  const head = git(["rev-parse", "HEAD"]);
  const elsewhere = refs.find((ref) => ref.localSha !== head);
  if (elsewhere) {
    // 检查只能跑在工作区上（go generate 还会改写它）；推的不是当前检出就不碰。
    console.log(
      `pre-push：未检查：推送的提交不是当前检出（${elsewhere.localRef} ${elsewhere.localSha.slice(0, 9)}，HEAD ${head?.slice(0, 9) ?? "无"}）`,
    );
    return 0;
  }
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

  // 拿不到基点：塞一个 package.json，格式、lint、单测与 Go 都走全量。
  const plan = planChecks(
    unknownBase ? ["package.json", ...changed] : changed,
    {
      base: unknownBase ? null : base,
      exists: (file) => existsSync(join(ROOT, file)),
    },
  );
  console.log(
    `pre-push：${unknownBase ? "拿不到基点，按全量" : `相对 ${base.slice(0, 9)} 改了 ${changed.length} 个文件`}${plan.docsOnly ? "（只有文档）" : ""}`,
  );
  const tree = workingTreeReport(
    parseStatus(
      gitRaw(["status", "--porcelain", "-z", "--untracked-files=all"]) ?? "",
    ),
    files,
  );
  const list = (paths) =>
    `${paths.slice(0, 8).join("、")}${paths.length > 8 ? ` 等 ${paths.length} 个` : ""}`;
  if (tree.inside.length > 0)
    console.log(
      `  注意：推送范围内的文件还有未提交的修改：${list(tree.inside)}`,
    );
  if (!plan.docsOnly && tree.outside.length > 0)
    console.log(
      `  注意：范围外有未提交的修改，检查会看到它们：${list(tree.outside)}`,
    );
  if (!plan.docsOnly && tree.untrackedOther.length > 0)
    console.log(`  注意：有未跟踪的文件：${list(tree.untrackedOther)}`);

  const keep = (id) => {
    if (!skip.steps.has(id)) return true;
    console.log(`  skip ${id}`);
    return false;
  };
  const results = [];
  // 未跟踪的源码会被编译、lint、测试读到，CI 上却没有：先拦下。
  if (!plan.docsOnly && tree.untrackedCode.length > 0 && keep("untracked")) {
    const log = join(LOG_DIR, "untracked.log");
    writeFileSync(
      log,
      `未跟踪的源码（提交、删除或加进 .gitignore）：\n${tree.untrackedCode.join("\n")}\n`,
    );
    console.log("  FAIL untracked");
    results.push({ id: "untracked", ok: false, log });
  }
  if (plan.libs && keep(plan.libs.id) && results.every((result) => result.ok))
    results.push(await pnpmStep(plan.libs));
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
      for (const step of generate) results.push(await generateStep(step));
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
