// pnpm relay:personal：从源码起一个个人中转开发实例（中继设计 §10）。
//
//   中继是 Go 二进制：设了 ARMADRA_RELAY_BIN 就用它，否则 `go build` 到 go/bin/armadra-relay
//   （增量构建，没改动时一两秒）。
//   https://127.0.0.1:8102，自签 TLS（启动日志打印 CA 指纹），数据目录 .data/personal/
//   首次运行：账号 dev，口令随机生成写进 .data/personal/dev.env（0600），由 serve 自动初始化
//   页面：ARMADRA_WEB_DIST 或 ../Armadra/apps/web/dist 有构建产物时托管在 /app 与 /j
//
// 不依赖 PostgreSQL / Redis。额外参数原样传给 `personal serve`。
import { spawn, spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const dataDir = join(root, ".data", "personal");
const envFile = join(dataDir, "dev.env");
const passwordFile = join(dataDir, "dev.password");
const ACCOUNT = "dev";

mkdirSync(dataDir, { recursive: true, mode: 0o700 });
const initialized = ["relay.db", "state.json"].some((name) =>
  existsSync(join(dataDir, name)),
);
if (!initialized && !existsSync(passwordFile)) {
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

const relayBin = resolveRelayBin();

const args = [
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

const child = spawn(relayBin, args, {
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

/** 中继二进制：ARMADRA_RELAY_BIN，否则从 go/ 构建到 go/bin/armadra-relay。 */
function resolveRelayBin() {
  const given = process.env.ARMADRA_RELAY_BIN?.trim();
  if (given) {
    const bin = resolve(root, given);
    if (!existsSync(bin)) {
      console.error(`ARMADRA_RELAY_BIN 指向的文件不存在：${bin}`);
      process.exit(1);
    }
    return bin;
  }
  const bin = join(root, "go", "bin", "armadra-relay");
  // 与镜像一样，/health 与 version 报根 package.json 的版本。
  const { version } = JSON.parse(
    readFileSync(join(root, "package.json"), "utf8"),
  );
  const built = spawnSync(
    "go",
    [
      "build",
      "-ldflags",
      `-X main.version=${version}`,
      "-o",
      bin,
      "./cmd/armadra-relay",
    ],
    { cwd: join(root, "go"), stdio: "inherit" },
  );
  if (built.error || built.status !== 0) {
    console.error(
      `构建中继失败（要 Go 工具链，见 mise.toml）：${built.error?.message ?? `退出码 ${built.status}`}`,
    );
    process.exit(1);
  }
  return bin;
}
