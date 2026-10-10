/**
 * THIRD_PARTY_NOTICES.md：随镜像与 npm 包分发的第三方声明。
 *
 *   node tools/notices.mjs            重新生成根目录的 THIRD_PARTY_NOTICES.md
 *   node tools/notices.mjs --check    与仓库里的那份逐字节比较，不同就失败
 *
 * 来源是 `pnpm licenses list -r --prod --json`：工作区所有包的生产依赖（pnpm 12 起
 * 根目录不再缺省递归，要显式 `-r`），再补上它漏列的已解析 peer 与可选依赖。每个包的
 * 许可证正文从安装目录里的 LICENSE / NOTICE 等文件原样读出。输出只取决于锁文件：
 * 按名字与版本排序，不写绝对路径与时间，换行统一为 LF。
 */
import { spawnSync } from "node:child_process";
import {
  existsSync,
  readFileSync,
  readdirSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
export const NOTICES_FILE = "THIRD_PARTY_NOTICES.md";
const NOTICE_NAME = /^(licen[cs]e|copying|notice)([-._].*)?$/i;

/** `pnpm licenses list --json` 的输出 → `[{ name, version, license, homepage, path }]`。 */
export function packagesFrom(report) {
  const rows = [];
  for (const [license, entries] of Object.entries(report)) {
    for (const entry of entries) {
      entry.versions.forEach((version, index) => {
        rows.push({
          name: entry.name,
          version,
          license: entry.license ?? license,
          homepage: entry.homepage ?? "",
          path: entry.paths?.[index] ?? entry.paths?.[0] ?? "",
        });
      });
    }
  }
  return rows
    .filter((row) => !row.name.startsWith("@armadra/"))
    .sort((a, b) =>
      a.name === b.name
        ? a.version.localeCompare(b.version)
        : a.name.localeCompare(b.name),
    );
}

function noticeTexts(directory) {
  if (!directory || !existsSync(directory)) return [];
  return readdirSync(directory)
    .filter((name) => NOTICE_NAME.test(name))
    .sort()
    .map((name) =>
      readFileSync(join(directory, name), "utf8")
        .replace(/\r\n?/g, "\n")
        .trimEnd(),
    );
}

export function render(packages, readTexts = noticeTexts) {
  const lines = [
    "# Third-party notices",
    "",
    "armadra-cloud 的生产依赖及其许可证全文。本文件由 `node tools/notices.mjs` 生成，不要手改。",
    "",
    "| 包 | 版本 | 许可证 |",
    "| --- | --- | --- |",
    ...packages.map((p) => `| ${p.name} | ${p.version} | ${p.license} |`),
    "",
  ];
  for (const p of packages) {
    lines.push(`## ${p.name}@${p.version}`, "");
    lines.push(`License: ${p.license}`);
    if (p.homepage) lines.push(`Homepage: ${p.homepage}`);
    lines.push("");
    const texts = readTexts(p.path);
    if (texts.length === 0) lines.push("（包内没有附带许可证文件）", "");
    for (const text of texts) lines.push("```text", text, "```", "");
  }
  return `${lines.join("\n").trimEnd()}\n`;
}

function generate() {
  const result = spawnSync(
    "pnpm",
    ["licenses", "list", "-r", "--prod", "--json"],
    {
      cwd: root,
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
    },
  );
  if (result.status !== 0) {
    throw new Error(`pnpm licenses list failed:\n${result.stderr}`);
  }
  const text = result.stdout.trim();
  return render(
    packagesFrom(withResolvedPeers(text === "" ? {} : JSON.parse(text))),
  );
}

/**
 * pnpm 12 的 `licenses list` 不列已解析的 peer 与可选依赖（例如 `debug` 的
 * `supports-color`），它们照样装进 node_modules、随应用发出去。这里从已列出的
 * 每个包出发，按 pnpm 的目录布局（依赖是同一个 `node_modules` 下的兄弟目录）
 * 补上它实际装到的 peer 与可选依赖，以及它们自己的依赖，按许可证并回原来的分组。
 */
export function withResolvedPeers(listing) {
  const seen = new Set();
  for (const entries of Object.values(listing))
    for (const entry of entries)
      entry.versions.forEach((version) => seen.add(`${entry.name}@${version}`));
  const queue = Object.values(listing)
    .flat()
    .flatMap((entry) => entry.paths ?? []);
  while (queue.length > 0) {
    const path = queue.shift();
    const manifest = readManifest(path);
    if (manifest === null) continue;
    // 只有 peerDependenciesMeta 没有 peerDependencies 的写法也算（debug 就是）。
    const names = [
      manifest.dependencies,
      manifest.optionalDependencies,
      manifest.peerDependencies,
      manifest.peerDependenciesMeta,
    ].flatMap((field) => Object.keys(field ?? {}));
    const siblings = path.slice(
      0,
      path.lastIndexOf("/node_modules/") + "/node_modules".length,
    );
    for (const name of names) {
      const candidate = join(siblings, name);
      if (!existsSync(candidate)) continue;
      const real = realpathSync(candidate);
      const pkg = readManifest(real);
      if (pkg === null || pkg.name.startsWith("@armadra/")) continue;
      const key = `${pkg.name}@${pkg.version}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const license = typeof pkg.license === "string" ? pkg.license : "Unknown";
      (listing[license] ??= []).push({
        name: pkg.name,
        versions: [pkg.version],
        paths: [real],
        license,
        author: typeof pkg.author === "string" ? pkg.author : pkg.author?.name,
        homepage: pkg.homepage,
      });
      queue.push(real);
    }
  }
  return listing;
}

function readManifest(directory) {
  try {
    return JSON.parse(readFileSync(join(directory, "package.json"), "utf8"));
  } catch {
    return null;
  }
}

function main(argv) {
  const target = join(root, NOTICES_FILE);
  const output = generate();
  if (argv.includes("--check")) {
    const current = existsSync(target) ? readFileSync(target, "utf8") : "";
    if (current !== output) {
      console.error(
        `${NOTICES_FILE} 与依赖不一致；运行 pnpm notices 重新生成并提交。`,
      );
      return 1;
    }
    console.log(`${NOTICES_FILE} 与依赖一致`);
    return 0;
  }
  writeFileSync(target, output);
  console.log(`已写入 ${NOTICES_FILE}`);
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main(process.argv.slice(2)));
}
