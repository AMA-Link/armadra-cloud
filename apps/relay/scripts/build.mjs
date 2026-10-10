// 镜像用的单文件：src/cli.ts → out/cli.js（ESM、node22、依赖全部打进来）。
// 运行时按 import.meta.url 读的数据文件（常见口令表、SQLite 迁移）同时拷到 out/。
import { build } from "esbuild";
import { copyFileSync, cpSync, rmSync } from "node:fs";

rmSync(new URL("../out", import.meta.url), { recursive: true, force: true });
await build({
  entryPoints: [new URL("../src/cli.ts", import.meta.url).pathname],
  outfile: new URL("../out/cli.js", import.meta.url).pathname,
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
copyFileSync(
  new URL("../src/personal/common-passwords.txt", import.meta.url),
  new URL("../out/common-passwords.txt", import.meta.url),
);
// SQLite 迁移（control-store-sqlite 先找单文件旁的 migrations/）。
cpSync(
  new URL("../../../packages/control-store-sql/migrations/", import.meta.url),
  new URL("../out/migrations/", import.meta.url),
  { recursive: true },
);
