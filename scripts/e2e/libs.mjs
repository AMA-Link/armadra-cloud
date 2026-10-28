// 端到端用到的工作区包产物（#143）：协议包经 dist 导出，检出换了提交而 dist 没重建时，
// 用例会拿旧常量（如没有 TOKEN_TYPES.node）误报失败。启动前查新鲜度，旧了或没有就先构建，
// 构建完仍不新鲜则报错退出。其余工作区包（relay-kernel、e2e-fake-core）直接导出源码，不用构建。
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

export const BUILT_LIBS = [
  {
    name: "@armadra/platform-protocol",
    dir: "packages/platform-protocol",
    inputs: ["src", "package.json", "tsconfig.build.json"],
    output: "dist",
  },
];

function walk(path, visit) {
  const stat = statSync(path);
  if (!stat.isDirectory()) {
    visit(path, stat);
    return;
  }
  for (const entry of readdirSync(path)) walk(join(path, entry), visit);
}

// 产物不新鲜的原因；新鲜时返回 null。比较「最新的输入」与「最旧的产物」的修改时间。
export function staleReason(root, lib) {
  const base = join(root, lib.dir);
  const output = join(base, lib.output);
  if (!existsSync(output)) return `没有 ${lib.dir}/${lib.output}`;
  let oldestOutput = Infinity;
  walk(output, (_, stat) => {
    oldestOutput = Math.min(oldestOutput, stat.mtimeMs);
  });
  if (oldestOutput === Infinity) return `${lib.dir}/${lib.output} 是空的`;
  let newest;
  for (const input of lib.inputs) {
    const path = join(base, input);
    if (!existsSync(path)) continue;
    walk(path, (file, stat) => {
      if (file.endsWith(".test.ts")) return;
      if (newest === undefined || stat.mtimeMs > newest.mtimeMs)
        newest = { file, mtimeMs: stat.mtimeMs };
    });
  }
  if (newest !== undefined && newest.mtimeMs > oldestOutput)
    return `源码比产物新：${relative(root, newest.file)}`;
  return null;
}

// 旧了就构建；构建失败或构建后仍不新鲜则抛错。build 可替换（测试用）。
export function ensureBuiltLibs({
  root,
  log = () => {},
  libs = BUILT_LIBS,
  build = (lib) =>
    spawnSync("pnpm", ["--filter", lib.name, "build"], {
      cwd: root,
      stdio: "inherit",
    }).status === 0,
}) {
  for (const lib of libs) {
    const reason = staleReason(root, lib);
    if (reason === null) continue;
    log(`构建 ${lib.name}（${reason}）`);
    if (!build(lib)) throw new Error(`构建 ${lib.name} 失败`);
    const after = staleReason(root, lib);
    if (after !== null) throw new Error(`${lib.name} 构建后仍不新鲜：${after}`);
  }
}
