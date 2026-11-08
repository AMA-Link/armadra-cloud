// 构建：用 tsc 产出 ESM 与类型声明（不打包、不压缩，保留子路径结构）。
//
// 增量编译：构建信息记在 node_modules/.cache/tsc/（不进 dist，也就不进 npm 包），
// 源码没变时 tsc 只核对一遍就退出。dist 不在、或者少了任何一个产物，先清掉构建信息
// 整体重编；源文件删了，对应的产物随后删掉。`--clean` 强制清空 dist 重编。
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL("../", import.meta.url));
const src = join(here, "src");
const dist = join(here, "dist");
const buildInfo = join(here, "node_modules/.cache/tsc/build.tsbuildinfo");

function files(dir) {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => relative(dir, join(entry.parentPath, entry.name)));
}

/** 产物对应的源文件：x.js、x.d.ts 来自 x.ts，其余（JSON）原样复制。 */
function sourceOf(output) {
  if (output.endsWith(".d.ts")) return output.slice(0, -".d.ts".length) + ".ts";
  if (output.endsWith(".js")) return output.slice(0, -".js".length) + ".ts";
  return output;
}

/** 源文件应有的产物：测试不编译；x.ts 出 x.js 与 x.d.ts；JSON 原样复制。 */
function outputsOf(source) {
  if (source.endsWith(".test.ts")) return [];
  if (source.endsWith(".ts")) {
    const stem = source.slice(0, -".ts".length);
    return [`${stem}.js`, `${stem}.d.ts`];
  }
  return source.endsWith(".json") ? [source] : [];
}

function main(argv) {
  if (argv.includes("--clean")) rmSync(dist, { recursive: true, force: true });
  const built = new Set(files(dist));
  const sources = files(src);
  const complete =
    built.size > 0 &&
    sources.every((source) =>
      outputsOf(source).every((output) => built.has(output)),
    );
  if (!complete) rmSync(buildInfo, { force: true });

  const tsc = createRequire(import.meta.url).resolve("typescript/bin/tsc");
  const result = spawnSync(
    process.execPath,
    [
      tsc,
      "-p",
      "tsconfig.build.json",
      "--incremental",
      "--tsBuildInfoFile",
      buildInfo,
    ],
    { cwd: here, stdio: "inherit" },
  );
  if (result.status !== 0) return result.status ?? 1;

  const present = new Set(sources);
  for (const output of files(dist))
    if (!present.has(sourceOf(output))) rmSync(join(dist, output));
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main(process.argv.slice(2)));
}
