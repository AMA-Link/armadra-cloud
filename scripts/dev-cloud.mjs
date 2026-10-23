// 用 Go 控制面二进制执行子命令，环境取自 .env.dev：node scripts/dev-cloud.mjs migrate
// （dev:up 的迁移用；pnpm dev 的 serve 在 dev-run.mjs）。
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { cloudEnv, readEnvFile } from "./dev-env.mjs";
import { resolveCloudBin } from "./relay-bin.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const env = cloudEnv(readEnvFile(new URL("../.env.dev", import.meta.url)));
const run = spawnSync(resolveCloudBin(root), process.argv.slice(2), {
  cwd: root,
  env: { ...process.env, ...env },
  stdio: "inherit",
});
process.exit(run.status ?? 1);
