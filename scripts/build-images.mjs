// 并行构建本地 Go 镜像（armadra-cloud:local 与 armadra-relay:local），启用 BuildKit 缓存挂载。
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));

const IMAGES = [
  { tag: "armadra-cloud:local", dockerfile: "go/Dockerfile.cloud" },
  { tag: "armadra-relay:local", dockerfile: "go/Dockerfile" },
];

function buildOne({ tag, dockerfile }) {
  const started = Date.now();
  return new Promise((resolve) => {
    const chunks = [];
    const child = spawn("docker", ["build", "-f", dockerfile, "-t", tag, "."], {
      cwd: root,
      env: { ...process.env, DOCKER_BUILDKIT: "1" },
    });
    child.stdout.on("data", (chunk) => chunks.push(chunk));
    child.stderr.on("data", (chunk) => chunks.push(chunk));
    let settled = false;
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      const seconds = ((Date.now() - started) / 1000).toFixed(1);
      console.log(`${ok ? "  ok  " : "  FAIL"} ${tag} ${seconds}s`);
      resolve({ tag, ok, output: Buffer.concat(chunks).toString("utf8") });
    };
    child.on("error", (error) => {
      chunks.push(Buffer.from(`${error.message}\n`));
      finish(false);
    });
    child.on("close", (code) => finish(code === 0));
  });
}

async function main() {
  const started = Date.now();
  const results = await Promise.all(IMAGES.map(buildOne));
  const failed = results.filter((r) => !r.ok);
  for (const r of failed) {
    process.stderr.write(`\n── ${r.tag} 构建失败 ──\n${r.output.trimEnd()}\n`);
  }
  if (failed.length > 0) return 1;
  console.log(
    `两个镜像构建完成（用时 ${((Date.now() - started) / 1000).toFixed(1)}s）`,
  );
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = await main();
}
