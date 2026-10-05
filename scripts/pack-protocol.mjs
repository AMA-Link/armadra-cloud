// pnpm protocol:pack：构建协议包并 `pnpm pack` 成 tgz（默认放 .data/pack/），再核对
// tarball 里有 ESM、类型声明与黄金夹具。只打包、不发布。
//
//   node scripts/pack-protocol.mjs [--out <目录>]
import { execFileSync } from "node:child_process";
import { mkdirSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const flag = process.argv.indexOf("--out");
const out = resolve(
  root,
  flag === -1 ? ".data/pack" : (process.argv[flag + 1] ?? ".data/pack"),
);
mkdirSync(out, { recursive: true });

const run = (args, cwd = root) =>
  execFileSync("pnpm", args, {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
  });

run(["--filter", "@armadra/platform-protocol", "build"]);
run(
  ["pack", "--pack-destination", out],
  join(root, "packages/platform-protocol"),
);
const tarball = readdirSync(out)
  .filter((name) => /^armadra-platform-protocol-.*\.tgz$/.test(name))
  .sort()
  .at(-1);
if (!tarball) throw new Error(`没有在 ${out} 找到 tgz`);
const path = join(out, tarball);
const entries = execFileSync("tar", ["-tzf", path], { encoding: "utf8" })
  .split("\n")
  .filter(Boolean);

const REQUIRED = [
  "package/package.json",
  "package/LICENSE",
  "package/dist/index.js",
  "package/dist/index.d.ts",
  "package/dist/tunnel/index.js",
  "package/dist/assertion/index.js",
  "package/dist/cloud-api/index.js",
  "package/dist/core-api/index.js",
  "package/dist/errors/index.js",
  "package/dist/identity-vectors/index.js",
  "package/dist/identity-vectors/vectors.json",
  "package/dist/fixtures/index.js",
  "package/fixtures/tunnel/sequence-http-roundtrip.bin",
  "package/fixtures/tunnel/handshake.json",
  "package/fixtures/assertion/keys.json",
  "package/fixtures/assertion/valid.jwt",
  "package/fixtures/cloud-api/auth.login.json",
  "package/fixtures/errors.json",
];
const missing = REQUIRED.filter((entry) => !entries.includes(entry));
const stray = entries.filter((entry) =>
  /\.test\.|\/src\/|\/test\//.test(entry),
);
if (missing.length > 0 || stray.length > 0) {
  console.error(
    `tarball 不对：\n${[...missing.map((m) => `缺 ${m}`), ...stray.map((s) => `多 ${s}`)].join("\n")}`,
  );
  process.exit(1);
}
console.log(`${path}（${entries.length} 个文件）`);
