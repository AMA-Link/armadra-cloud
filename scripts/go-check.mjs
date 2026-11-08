// Go 侧的检查（pnpm go:check），本地与 CI 同一份：
//
//   1. gofmt -l 必须为空；
//   2. go vet、staticcheck（固定版本，经 go run 调用，不进 go.mod）与
//      go test -race（超时见 TEST_TIMEOUT）三步并行，输出各自收齐后再打印；
//   3. go generate ./... 之后生成物与已提交的一致（git diff 为空、没有未跟踪的新文件）。
//
//   node scripts/go-check.mjs [--only <步骤,步骤>] [--changed [--base <ref>]]
//
// 步骤名：fmt、vet、staticcheck、test、generate。
//
// 缺省是全量（CI 用这个）：./... 全部包，go test 带 -count=1 不读测试缓存。
// `--changed` 是本机的快路径：只查相对 `--base`（缺省 origin/main）的合并基点受影响的包——
// 改过的包加上直接或间接 import 它们（含测试 import）的包，未提交与未跟踪的文件也算改动；
// go.mod、测试会读的协议包夹具与 schema 等变了，或者拿不到基点，就全量。go test 保留缓存，
// 没变的包直接取上次的结果。生成物新鲜度改成比对 go generate 前后的文件，不看 git。
import { spawn, spawnSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join, posix, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const goDir = `${root}go`;
/** 生成物所在的目录：新鲜度只看这里，别的未提交文件不算。 */
const GENERATED = "go/internal/protocol/gen/";

/** staticcheck 的精确版本；换版本时只改这里。 */
export const STATICCHECK = "honnef.co/go/tools/cmd/staticcheck@v0.8.2";

/**
 * go test 的单包超时（每个测试二进制各算一次）。显式写出来，不靠默认的 10 分钟：
 * 最重的包 internal/cloud/server 是真 PostgreSQL + Redis 的集成测试，-race 下并行跑完约几分钟；
 * 20 分钟是它的数倍余量，挂住时在 CI 作业的 30 分钟上限之前就带着 goroutine 堆栈失败，
 * 又不至于在负载高的机器上因抖动误报。该包真变慢到逼近这个值，要查原因，不是再加大它（#174）。
 */
export const TEST_TIMEOUT = "20m";

/**
 * 这些文件一变，Go 全量：模块依赖、所有包共用的冻结夹具，以及测试按路径读的仓库文件
 * （协议包的 schema、夹具、身份向量与格式向量，deploy/README.md 的参数表），还有本脚本。
 */
export const GO_FULL_TRIGGERS = [
  /^go\/go\.(mod|sum)$/,
  /^go\/testdata\//,
  /^packages\/platform-protocol\/(schema|fixtures|test)\//,
  /^packages\/platform-protocol\/src\/identity-vectors\//,
  /^deploy\/README\.md$/,
  /^scripts\/go-check\.mjs$/,
];

/**
 * 根据改动的文件挑要查的 Go 包。
 *
 * `changed` 是相对仓库根的文件列表，null 表示拿不到基点；`packages` 是 `go list` 的结果
 * `[{ importPath, dir, deps, testImports }]`（`dir` 相对仓库根、用 `/`，`deps` 是传递依赖，
 * `testImports` 是测试文件的直接 import）。返回 `{ full, packages }`：`full` 为 true 时查
 * ./...；否则 `packages` 是受影响包的 import path（排好序，可能为空）。
 */
export function planGo(changed, packages) {
  const everything = { full: true, packages: [] };
  if (changed === null) return everything;
  if (changed.some((file) => GO_FULL_TRIGGERS.some((re) => re.test(file))))
    return everything;

  const byDir = new Map(packages.map((pkg) => [pkg.dir, pkg.importPath]));
  const touched = new Set();
  for (const file of changed) {
    if (!file.startsWith("go/")) continue;
    let dir = posix.dirname(file);
    // .go 文件必须正好落在一个包里；落不进去（删了整个包、新包还认不出）就全量。
    if (file.endsWith(".go")) {
      if (!byDir.has(dir)) return everything;
    } else {
      // 其余文件（go:embed 的迁移、testdata）归最近的上层包；go/ 根下的 Dockerfile 等不归任何包。
      while (!byDir.has(dir) && dir.includes("/")) dir = posix.dirname(dir);
      if (!byDir.has(dir)) continue;
    }
    touched.add(byDir.get(dir));
  }
  if (touched.size === 0) return { full: false, packages: [] };

  const depsOf = new Map(packages.map((pkg) => [pkg.importPath, pkg.deps]));
  const affected = packages.filter((pkg) => {
    if (touched.has(pkg.importPath)) return true;
    if (pkg.deps.some((dep) => touched.has(dep))) return true;
    return pkg.testImports.some(
      (imp) =>
        touched.has(imp) || (depsOf.get(imp) ?? []).some((d) => touched.has(d)),
    );
  });
  return {
    full: false,
    packages: affected.map((pkg) => pkg.importPath).sort(),
  };
}

function git(args) {
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8" });
  return result.status === 0 ? result.stdout.trim() : null;
}

/** 相对 `ref` 的合并基点以来改过的文件（含未提交、未跟踪）。拿不到基点时 files 为 null。 */
export function changedFiles(ref) {
  const base = git(["merge-base", ref, "HEAD"]);
  if (!base) return { base: null, files: null };
  const diff = git(["diff", "--name-only", base]);
  const untracked = git(["ls-files", "--others", "--exclude-standard"]);
  if (diff === null || untracked === null) return { base, files: null };
  const files = [...diff.split("\n"), ...untracked.split("\n")].filter(Boolean);
  return { base, files: [...new Set(files)] };
}

/** `go list` 出模块里所有包的目录与依赖，供 planGo 用。 */
export function listPackages() {
  const format =
    '{{.ImportPath}}\t{{.Dir}}\t{{join .Deps " "}}\t{{join .TestImports " "}} {{join .XTestImports " "}}';
  const result = spawnSync("go", ["list", "-f", format, "./..."], {
    cwd: goDir,
    encoding: "utf8",
  });
  if (result.status !== 0) return null;
  const words = (text) => text.split(" ").filter(Boolean);
  return result.stdout
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      const [importPath, dir, deps = "", tests = ""] = line.split("\t");
      return {
        importPath,
        dir: relative(root, dir).split(sep).join("/"),
        deps: words(deps),
        testImports: words(tests),
      };
    });
}

/** 跑一条命令，收齐输出；返回 { status, stdout, stderr }。 */
function exec(command, args, cwd = goDir) {
  return new Promise((resolve) => {
    const out = [];
    const err = [];
    const child = spawn(command, args, { cwd });
    child.stdout.on("data", (chunk) => out.push(chunk));
    child.stderr.on("data", (chunk) => err.push(chunk));
    child.on("error", (error) =>
      resolve({ status: 1, stdout: "", stderr: `${error.message}\n` }),
    );
    child.on("close", (code) =>
      resolve({
        status: code,
        stdout: Buffer.concat(out).toString("utf8"),
        stderr: Buffer.concat(err).toString("utf8"),
      }),
    );
  });
}

/** gen/ 下每个文件的内容，比对 go generate 前后用。 */
function snapshot(dir) {
  const full = join(root, dir);
  const files = new Map();
  if (!existsSync(full)) return files;
  for (const entry of readdirSync(full, {
    recursive: true,
    withFileTypes: true,
  }))
    if (entry.isFile()) {
      const path = join(entry.parentPath, entry.name);
      files.set(relative(root, path).split(sep).join("/"), readFileSync(path));
    }
  return files;
}

/** 两次快照的差别：改了、多了、少了的文件。 */
export function snapshotDiff(before, after) {
  const problems = [];
  for (const [file, data] of after) {
    const old = before.get(file);
    if (!old) problems.push(`生成物多出新文件：${file}`);
    else if (!old.equals(data))
      problems.push(`生成物与 go generate 的结果不同：${file}`);
  }
  for (const file of before.keys())
    if (!after.has(file)) problems.push(`go generate 不再产出：${file}`);
  return problems;
}

/**
 * 列出各步骤。每步是 `{ name, group, run() }`，`run` 答 `{ output, problems }`；
 * 同一 `group` 的步骤可以并行。`scope` 是 `{ full, packages }`（planGo 的结果），
 * `local` 为 true 时 go test 保留缓存、生成物新鲜度按快照比对。
 * vet、staticcheck、test 另带 `args`（给 go 的完整参数；没有受影响的包时为 null，跳过）。
 */
export function goSteps({
  scope = { full: true, packages: [] },
  local = false,
} = {}) {
  const targets = scope.full ? ["./..."] : scope.packages;
  const command = (name, flags) => {
    const args = targets.length === 0 ? null : [...flags, ...targets];
    return {
      name,
      group: 2,
      args,
      async run() {
        if (!args)
          return { output: "没有受影响的 Go 包，跳过\n", problems: [] };
        const result = await exec("go", args);
        return {
          output: result.stdout + result.stderr,
          problems: result.status === 0 ? [] : [`${name} 失败`],
        };
      },
    };
  };
  return [
    {
      name: "fmt",
      group: 1,
      async run() {
        const result = await exec("gofmt", ["-l", "."]);
        const files = result.stdout.split("\n").filter(Boolean);
        return {
          output: result.stderr,
          problems: [
            ...files.map((file) => `gofmt 要改：go/${file}`),
            ...(result.status === 0 ? [] : ["gofmt 失败"]),
          ],
        };
      },
    },
    command("vet", ["vet"]),
    command("staticcheck", ["run", STATICCHECK]),
    command("test", [
      "test",
      "-race",
      ...(local ? [] : ["-count=1"]),
      `-timeout=${TEST_TIMEOUT}`,
    ]),
    {
      // 放在最后：go generate 会重写 gen/，不和编译这些文件的步骤同时跑。
      name: "generate",
      group: 3,
      async run() {
        const before = local ? snapshot(GENERATED) : null;
        const generated = await exec("go", ["generate", "./..."]);
        let output = generated.stdout + generated.stderr;
        if (generated.status !== 0)
          return { output, problems: ["go generate 失败"] };
        if (local)
          return {
            output,
            problems: snapshotDiff(before, snapshot(GENERATED)),
          };
        const problems = [];
        const diff = await exec(
          "git",
          ["diff", "--exit-code", "--", "go/"],
          root,
        );
        if (diff.status !== 0) {
          output += diff.stdout + diff.stderr;
          problems.push("生成物与已提交的不一致：重新运行 go generate 并提交");
        }
        const others = await exec(
          "git",
          ["ls-files", "--others", "--exclude-standard", "--", GENERATED],
          root,
        );
        for (const file of others.stdout.split("\n").filter(Boolean))
          problems.push(`生成物有未跟踪的新文件：${file}`);
        return { output, problems };
      },
    },
  ];
}

/** 解析命令行，决定查哪些包；`--changed` 时先 git diff 再 go list。 */
export function resolveScope(argv) {
  if (!argv.includes("--changed"))
    return { local: false, scope: { full: true, packages: [] }, note: "全量" };
  const at = argv.indexOf("--base");
  const ref = at === -1 ? "origin/main" : argv[at + 1];
  const { base, files } = changedFiles(ref);
  const packages = files === null ? null : listPackages();
  const scope =
    packages === null ? { full: true, packages: [] } : planGo(files, packages);
  const note = scope.full
    ? base
      ? `全量（相对 ${ref} 的改动波及所有包）`
      : `全量（拿不到 ${ref} 的合并基点）`
    : `受影响的包 ${scope.packages.length} 个（相对 ${ref} 的合并基点 ${base.slice(0, 9)}）`;
  return { local: true, scope, note };
}

export async function main(argv) {
  const at = argv.indexOf("--only");
  const only = at === -1 ? null : new Set(argv[at + 1]?.split(",") ?? []);
  const { local, scope, note } = resolveScope(argv);
  console.log(`Go 检查：${note}`);
  const steps = goSteps({ scope, local }).filter(
    (one) => !only || only.has(one.name),
  );
  const problems = [];
  const groups = [...new Set(steps.map((one) => one.group))];
  for (const group of groups) {
    const results = await Promise.all(
      steps
        .filter((one) => one.group === group)
        .map(async (one) => {
          const started = Date.now();
          const result = await one.run();
          return {
            ...result,
            name: one.name,
            seconds: (Date.now() - started) / 1000,
          };
        }),
    );
    for (const result of results) {
      console.log(`==> ${result.name}（${result.seconds.toFixed(1)}s）`);
      if (result.output) process.stdout.write(result.output);
      for (const problem of result.problems) console.error(`✗ ${problem}`);
      problems.push(...result.problems);
    }
    // 前面的步骤不过，后面的结果没有意义。
    if (problems.length > 0) break;
  }
  if (problems.length > 0) {
    console.error(`\nGo 检查失败：${problems.length} 处问题`);
    return 1;
  }
  console.log("Go 检查通过");
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = await main(process.argv.slice(2));
}
