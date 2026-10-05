// pnpm relay:personal：从源码起一个个人中转开发实例（中继设计 §10）。
//
//   https://127.0.0.1:8102，自签 TLS（启动日志打印 CA 指纹），数据目录 .data/personal/
//   首次运行：账号 dev，口令随机生成写进 .data/personal/dev.env（0600），由 serve 自动初始化
//   页面：ARMADRA_WEB_DIST 或 ../Armadra/apps/web/dist 有构建产物时托管在 /app 与 /j
//
// 不依赖 PostgreSQL / Redis。额外参数原样传给 `personal serve`。
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const dataDir = join(root, ".data", "personal");
const envFile = join(dataDir, "dev.env");
const passwordFile = join(dataDir, "dev.password");
const ACCOUNT = "dev";

mkdirSync(dataDir, { recursive: true, mode: 0o700 });
if (!existsSync(join(dataDir, "state.json")) && !existsSync(passwordFile)) {
  const password = randomBytes(18).toString("base64url");
  writeFileSync(passwordFile, password, { mode: 0o600 });
  writeFileSync(
    envFile,
    `PERSONAL_RELAY_ACCOUNT=${ACCOUNT}\nPERSONAL_RELAY_PASSWORD=${password}\n`,
    { mode: 0o600 },
  );
  chmodSync(passwordFile, 0o600);
  chmodSync(envFile, 0o600);
  console.log(`首次运行：账号 ${ACCOUNT}，口令写在 ${envFile}`);
}

const webCandidates = [
  process.env.ARMADRA_WEB_DIST,
  join(root, "..", "Armadra", "apps", "web", "dist"),
].filter(Boolean);
const webRoot = webCandidates
  .map((dir) => resolve(root, dir))
  .find((dir) => existsSync(join(dir, "index.html")));

const args = [
  "apps/relay/src/cli.ts",
  "personal",
  "serve",
  "--data-dir",
  dataDir,
  "--host",
  "127.0.0.1",
  "--port",
  "8102",
  "--tls",
  "self-signed",
  ...(webRoot ? ["--web-root", webRoot] : []),
  ...process.argv.slice(2),
];

const child = spawn(process.execPath, args, {
  cwd: root,
  stdio: "inherit",
  env: {
    ...process.env,
    RELAY_PERSONAL_ACCOUNT: ACCOUNT,
    RELAY_PERSONAL_PASSWORD_FILE: passwordFile,
  },
});
for (const signal of ["SIGINT", "SIGTERM"])
  process.on(signal, () => child.kill(signal));
child.on("exit", (code, signal) => process.exit(code ?? (signal ? 1 : 0)));
