// 镜像用的单文件：src/cli.ts → out/main.js（ESM、node22、依赖全部打进来）。
import { build } from "esbuild";
import { rmSync } from "node:fs";

rmSync(new URL("../out", import.meta.url), { recursive: true, force: true });
await build({
  entryPoints: [new URL("../src/cli.ts", import.meta.url).pathname],
  outfile: new URL("../out/main.js", import.meta.url).pathname,
  bundle: true,
  platform: "node",
  format: "esm",
  target: "node22",
  legalComments: "none",
  // 打进来的 CommonJS 依赖会调用 require；ESM 单文件里补一个。
  banner: {
    js: "import { createRequire as __armadraCreateRequire } from 'node:module'; const require = __armadraCreateRequire(import.meta.url);",
  },
  logLevel: "info",
});
