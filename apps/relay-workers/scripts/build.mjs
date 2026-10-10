// 页面产物：把 Armadra `apps/web` 的构建产物拷进 assets/（Workers Static Assets 发布它）。
//
// 用法: node scripts/build.mjs [--web <目录> | --image <镜像>]
//
//   --web DIR      产物目录（含 index.html），如 Armadra 仓的 apps/web/dist
//   --image IMAGE  从服务器壳镜像里取 /app/web（与 Node 中继镜像同一份产物；docker create + cp，
//                  不运行容器），如 ghcr.io/owlbay/armadra-server:<deploy/compat.json 的 web>
//   都不给         清空 assets/（只留 .assetsignore）：中继答自有的错误页与落地页
//
// 只在本地改文件，不连 Cloudflare。
import { execFileSync } from "node:child_process";
import { cpSync, existsSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const USAGE = `用法: node scripts/build.mjs [--web <目录> | --image <镜像>]

  --web DIR      Armadra apps/web 的构建产物目录（含 index.html）
  --image IMAGE  从服务器壳镜像里取 /app/web`;

const assets = fileURLToPath(new URL("../assets/", import.meta.url));
const KEEP = ".assetsignore";

function fail(message) {
  console.error(`${message}\n${USAGE}`);
  process.exit(2);
}

const args = process.argv.slice(2);
if (args.includes("--help") || args.includes("-h")) {
  console.log(USAGE);
  process.exit(0);
}
const options = new Map();
for (let index = 0; index < args.length; index += 2) {
  const [flag, value] = [args[index], args[index + 1]];
  if (flag !== "--web" && flag !== "--image") fail(`未知选项：${flag}`);
  if (value === undefined || value.startsWith("--")) fail(`${flag} 缺少取值`);
  options.set(flag, value);
}
if (options.size > 1) fail("--web 与 --image 只能给一个");

for (const name of readdirSync(assets))
  if (name !== KEEP)
    rmSync(join(assets, name), { recursive: true, force: true });

function copyFrom(dir) {
  if (!existsSync(join(dir, "index.html")))
    fail(`产物目录里没有 index.html：${dir}`);
  cpSync(dir, assets, { recursive: true });
  console.log(`页面产物已拷进 assets/：${dir}`);
}

if (options.has("--web")) copyFrom(options.get("--web"));
else if (options.has("--image")) {
  const image = options.get("--image");
  const work = mkdtempSync(join(tmpdir(), "armadra-relay-web-"));
  const id = execFileSync("docker", ["create", image], {
    encoding: "utf8",
  }).trim();
  try {
    execFileSync("docker", ["cp", `${id}:/app/web`, join(work, "web")]);
    copyFrom(join(work, "web"));
  } finally {
    execFileSync("docker", ["rm", id]);
    rmSync(work, { recursive: true, force: true });
  }
} else console.log("没有给页面产物：assets/ 已清空，中继答自有页面");
