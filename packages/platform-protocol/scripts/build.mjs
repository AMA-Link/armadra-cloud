// 构建：清空 dist，再用 tsc 产出 ESM 与类型声明（不打包、不压缩，保留子路径结构）。
import { spawnSync } from "node:child_process";
import { rmSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL("../", import.meta.url));
rmSync(new URL("../dist", import.meta.url), { recursive: true, force: true });
const tsc = createRequire(import.meta.url).resolve("typescript/bin/tsc");
const result = spawnSync(process.execPath, [tsc, "-p", "tsconfig.build.json"], {
  cwd: here,
  stdio: "inherit",
});
process.exit(result.status ?? 1);
