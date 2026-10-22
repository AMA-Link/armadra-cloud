// Go 侧的检查（pnpm go:check），本地与 CI 同一份：
//
//   1. gofmt -l 必须为空；
//   2. go vet ./...；
//   3. staticcheck ./...（固定版本，经 go run 调用，不进 go.mod）；
//   4. go test -race ./...；
//   5. go generate ./... 之后生成物与已提交的一致（git diff 为空、没有未跟踪的新文件）。
//
//   node scripts/go-check.mjs [--only <步骤,步骤>]   步骤名：fmt、vet、staticcheck、test、generate
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const goDir = `${root}go`;
/** 生成物所在的目录：新鲜度只看这里，别的未提交文件不算。 */
const GENERATED = "go/internal/protocol/gen/";

/** staticcheck 的精确版本；换版本时只改这里。 */
export const STATICCHECK = "honnef.co/go/tools/cmd/staticcheck@v0.8.2";

function run(command, args, cwd) {
  return spawnSync(command, args, { cwd, encoding: "utf8" });
}

function show(result) {
  if (result.stdout) process.stdout.write(result.stdout);
  if (result.stderr) process.stderr.write(result.stderr);
}

function step(name, command, args, cwd = goDir) {
  return {
    name,
    run() {
      const result = run(command, args, cwd);
      show(result);
      return result.status === 0 ? [] : [`${command} ${args.join(" ")} 失败`];
    },
  };
}

export const STEPS = [
  {
    name: "fmt",
    run() {
      const result = run("gofmt", ["-l", "."], goDir);
      show({ stderr: result.stderr });
      const files = result.stdout.split("\n").filter(Boolean);
      return files.map((file) => `gofmt 要改：go/${file}`);
    },
  },
  step("vet", "go", ["vet", "./..."]),
  step("staticcheck", "go", ["run", STATICCHECK, "./..."]),
  step("test", "go", ["test", "-race", "-count=1", "./..."]),
  {
    name: "generate",
    run() {
      const generated = run("go", ["generate", "./..."], goDir);
      show(generated);
      if (generated.status !== 0) return ["go generate 失败"];
      const problems = [];
      const diff = run("git", ["diff", "--exit-code", "--", "go/"], root);
      if (diff.status !== 0) {
        show(diff);
        problems.push("生成物与已提交的不一致：重新运行 go generate 并提交");
      }
      const others = run(
        "git",
        ["ls-files", "--others", "--exclude-standard", "--", GENERATED],
        root,
      );
      for (const file of others.stdout.split("\n").filter(Boolean))
        problems.push(`生成物有未跟踪的新文件：${file}`);
      return problems;
    },
  },
];

export function main(argv) {
  const at = argv.indexOf("--only");
  const only = at === -1 ? null : new Set(argv[at + 1]?.split(",") ?? []);
  const problems = [];
  for (const one of STEPS) {
    if (only && !only.has(one.name)) continue;
    console.log(`==> ${one.name}`);
    const found = one.run();
    for (const problem of found) console.error(`✗ ${problem}`);
    problems.push(...found);
    // 前面的步骤不过，后面的结果没有意义。
    if (found.length > 0) break;
  }
  if (problems.length > 0) {
    console.error(`\nGo 检查失败：${problems.length} 处问题`);
    return 1;
  }
  console.log("Go 检查通过");
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main(process.argv.slice(2)));
}
