// 仓库完整性校验：读取 repo.rules.json，对 git 跟踪的文件做静态检查。
// 规则的自然语言版本见 docs/design/repository-structure.md §3。
//
//   node tools/repo-check.mjs [--root <目录>] [--rules <文件>] [--only <规则,规则>]
//
// 只读文件系统与 `git ls-files`，不编译、不联网，秒级完成。
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { dirname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const RULES = [
  "docs-index",
  "links",
  "blacklist",
  "naming",
  "root-allowlist",
  "file-size",
  "migrations",
  "pinned-versions",
  "package-dependencies",
  "go-modules",
];

// ------------------------------------------------------------------ helpers

/** git 跟踪的文件，路径统一用 `/` 分隔，便于规则里直接写字面量。 */
export function trackedFiles(root) {
  const output = execFileSync("git", ["-C", root, "ls-files", "-z"], {
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
  return output.split("\0").filter(Boolean);
}

function readIfPresent(root, relativePath) {
  const absolute = join(root, relativePath);
  return existsSync(absolute) ? readFileSync(absolute, "utf8") : null;
}

function sha256(buffer) {
  return createHash("sha256").update(buffer).digest("hex");
}

/** `output/`、`*.db*` 这类简写模式：`/` 结尾匹配整个子树，`*` 不跨目录分隔符。 */
function patternToRegExp(pattern) {
  if (pattern.endsWith("/")) {
    const head = pattern.slice(0, -1).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return new RegExp(`(^|/)${head}/`);
  }
  const body = pattern
    .split("*")
    .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
    .join("[^/]*");
  return new RegExp(`(^|/)${body}$`);
}

/** Markdown 与内嵌 HTML 中指向仓库内文件的链接。 */
function localLinks(markdown) {
  const targets = [];
  const inline = /\[[^\]]*\]\(\s*<?([^)<>\s]+)>?(?:\s+"[^"]*")?\s*\)/g;
  const html = /\b(?:src|href)\s*=\s*"([^"]+)"/g;
  for (const source of [inline, html]) {
    let match;
    while ((match = source.exec(markdown)) !== null) targets.push(match[1]);
  }
  return targets.filter(
    (target) =>
      !/^[a-z][a-z0-9+.-]*:/i.test(target) &&
      !target.startsWith("#") &&
      !target.startsWith("//"),
  );
}

/** 索引里出现过的路径：链接目标与行内代码都算登记。 */
function registeredPaths(markdown) {
  const registered = new Set(localLinks(markdown).map(stripAnchor));
  const code = /`([^`\n]+)`/g;
  let match;
  while ((match = code.exec(markdown)) !== null) registered.add(match[1]);
  return registered;
}

function stripAnchor(target) {
  const withoutAnchor = target.split("#")[0].split("?")[0];
  return withoutAnchor.replace(/^\.\//, "");
}

function tomlPackageName(text) {
  const section = text
    .split(/^\[/m)
    .find((part) => part.startsWith("package]"));
  return section?.match(/^\s*name\s*=\s*"([^"]+)"/m)?.[1] ?? null;
}

function countLines(absolute) {
  const text = readFileSync(absolute, "utf8");
  if (text.length === 0) return 0;
  const lines = text.split("\n").length;
  return text.endsWith("\n") ? lines - 1 : lines;
}

// ------------------------------------------------------------------- checks

function checkDocsIndex(context, problems) {
  const rule = context.rules.docs;
  if (!rule) return;
  const index = readIfPresent(context.root, rule.index);
  if (index === null) {
    problems.push(`文档索引缺失：${rule.index}`);
    return;
  }
  const registered = registeredPaths(index);
  const directoryOnly = rule.directoryOnly ?? [];
  const pending = new Set(directoryOnly);
  for (const path of context.files) {
    if (!path.startsWith(`${rule.root}/`) || !path.endsWith(".md")) continue;
    const inDocs = path.slice(rule.root.length + 1);
    if (path === rule.index) continue;
    const directory = directoryOnly.find(
      (name) => inDocs === name || inDocs.startsWith(`${name}/`),
    );
    if (directory) {
      const covered = [...registered].some(
        (entry) => entry === directory || entry.startsWith(`${directory}/`),
      );
      if (covered) pending.delete(directory);
      continue;
    }
    if (!registered.has(inDocs)) {
      problems.push(`文档未在 ${rule.index} 登记：${path}`);
    }
  }
  for (const directory of pending) {
    if (
      context.files.some((path) =>
        path.startsWith(`${rule.root}/${directory}/`),
      )
    ) {
      problems.push(`目录未在 ${rule.index} 登记：${rule.root}/${directory}/`);
    }
  }
}

function checkLinks(context, problems) {
  const rule = context.rules.links;
  if (!rule) return;
  const matchers = (rule.include ?? []).map(patternToRegExp);
  const excluded = (rule.exclude ?? []).map(patternToRegExp);
  const skip = new Set(rule.ignoreTargets ?? []);
  for (const path of context.files) {
    if (!path.endsWith(".md")) continue;
    if (
      matchers.length > 0 &&
      !matchers.some((matcher) => matcher.test(path))
    ) {
      continue;
    }
    if (excluded.some((matcher) => matcher.test(path))) continue;
    const text = readFileSync(join(context.root, path), "utf8");
    for (const target of localLinks(text)) {
      const cleaned = stripAnchor(target);
      if (cleaned === "" || skip.has(cleaned)) continue;
      const resolved = normalize(join(dirname(path), cleaned));
      if (resolved.startsWith("..")) {
        problems.push(`链接越出仓库：${path} → ${target}`);
        continue;
      }
      if (!existsSync(join(context.root, resolved))) {
        problems.push(`链接无法解析：${path} → ${target}`);
      }
    }
  }
}

function checkBlacklist(context, problems) {
  const patterns = context.rules.blacklist ?? [];
  const matchers = patterns.map((pattern) => [
    pattern,
    patternToRegExp(pattern),
  ]);
  for (const path of context.files) {
    for (const [pattern, matcher] of matchers) {
      if (matcher.test(path))
        problems.push(`禁止跟踪的文件：${path}（${pattern}）`);
    }
  }
}

function checkNaming(context, problems) {
  for (const rule of context.rules.naming ?? []) {
    const base = join(context.root, rule.directory);
    if (!existsSync(base)) continue;
    // 待改名目录登记在这里，改名落地后必须删除条目，否则本检查报告过期豁免。
    const exempt = new Map(
      (rule.exemptions ?? []).map((item) => [item.directory, item]),
    );
    const usedExemptions = new Set();
    for (const entry of readdirSync(base, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      const manifest = join(base, entry.name, rule.manifest);
      if (!existsSync(manifest)) continue;
      const text = readFileSync(manifest, "utf8");
      const name = rule.manifest.endsWith(".json")
        ? JSON.parse(text).name
        : tomlPackageName(text);
      if (!name) {
        problems.push(
          `无法读取包名：${rule.directory}/${entry.name}/${rule.manifest}`,
        );
        continue;
      }
      const expected = name.startsWith(rule.prefix)
        ? name.slice(rule.prefix.length)
        : name;
      if (expected === entry.name) continue;
      const directory = `${rule.directory}/${entry.name}`;
      if (exempt.has(directory)) {
        usedExemptions.add(directory);
        continue;
      }
      problems.push(
        `目录名与包名不一致：${directory} 应为 ${rule.directory}/${expected}（${name}）`,
      );
    }
    for (const directory of exempt.keys()) {
      if (!usedExemptions.has(directory)) {
        problems.push(
          `改名豁免已过期，可从 repo.rules.json 移除：${directory}`,
        );
      }
    }
  }
}

function checkRootAllowlist(context, problems) {
  const allow = new Set(context.rules.root ?? []);
  if (allow.size === 0) return;
  const seen = new Set(context.files.map((path) => path.split("/")[0]));
  for (const entry of [...seen].sort()) {
    if (!allow.has(entry)) problems.push(`根目录不在白名单：${entry}`);
  }
}

function checkFileSize(context, problems) {
  const rule = context.rules.fileSize;
  if (!rule) return;
  const exemptions = new Map(
    (rule.exemptions ?? []).map((entry) => [entry.path, entry]),
  );
  const used = new Set();
  for (const path of context.files) {
    if (!(rule.roots ?? []).some((root) => path.startsWith(`${root}/`)))
      continue;
    if (!(rule.extensions ?? []).some((suffix) => path.endsWith(suffix)))
      continue;
    if (
      (rule.excludeSegments ?? []).some((segment) => path.includes(segment))
    ) {
      continue;
    }
    const absolute = join(context.root, path);
    if (!existsSync(absolute)) continue;
    const lines = countLines(absolute);
    const exemption = exemptions.get(path);
    if (lines <= rule.max) {
      if (exemption) {
        used.add(path);
        problems.push(
          `豁免已过期，可从 repo.rules.json 移除：${path}（${lines} 行）`,
        );
      }
      continue;
    }
    if (!exemption) {
      problems.push(`源码单文件超过 ${rule.max} 行：${path}（${lines} 行）`);
      continue;
    }
    used.add(path);
    if (lines > exemption.lines) {
      problems.push(
        `豁免文件继续增长：${path}（${lines} 行 > 登记的 ${exemption.lines} 行）`,
      );
    }
  }
  for (const path of exemptions.keys()) {
    if (!used.has(path)) problems.push(`豁免登记的文件不存在：${path}`);
  }
}

/** 迁移编号必须从 1 起连续。 */
function checkMigrationNumbers(label, numbers, problems) {
  [...numbers]
    .sort((left, right) => left - right)
    .forEach((value, index) => {
      if (value !== index + 1) {
        problems.push(`迁移编号不连续：${label} 第 ${index + 1} 个为 ${value}`);
      }
    });
}

function checkMigrations(context, problems) {
  const rule = context.rules.migrations;
  if (!rule) return;
  const lockText = readIfPresent(context.root, rule.lock);
  if (lockText === null) {
    problems.push(`迁移校验表缺失：${rule.lock}`);
    return;
  }
  const lock = JSON.parse(lockText);
  for (const source of rule.sources ?? []) {
    const recorded = lock[source.path] ?? {};
    const actual = new Map();
    const base = join(context.root, source.path);
    if (!existsSync(base)) {
      problems.push(`迁移目录缺失：${source.path}`);
      continue;
    }
    for (const name of readdirSync(base).sort()) {
      if (!name.endsWith(".sql")) continue;
      actual.set(name, sha256(readFileSync(join(base, name))));
    }
    checkMigrationNumbers(
      source.path,
      [...actual.keys()].map((name) => Number.parseInt(name.slice(0, 4), 10)),
      problems,
    );
    for (const [name, digest] of actual) {
      const expected = recorded[name];
      if (expected === undefined) {
        problems.push(
          `新迁移未登记到 ${rule.lock}：${source.path}/${name}（sha256 ${digest}）`,
        );
      } else if (expected !== digest) {
        problems.push(`已发布迁移被改动：${source.path}/${name}`);
      }
    }
    for (const name of Object.keys(recorded)) {
      if (!actual.has(name)) {
        problems.push(`已登记的迁移消失：${source.path}/${name}`);
      }
    }
  }
  checkMigrationCopies(context, rule, lock, problems);
}

/**
 * 迁移 SQL 的副本（Go 的 go:embed 目录）：SQL 以 `from` 目录里的为准，`to` 里每个文件必须与
 * migrations.lock 登记的字节一致，文件集合也要一样——新迁移只在 TS 包里加，`go generate` 拷过去。
 */
function checkMigrationCopies(context, rule, lock, problems) {
  for (const copy of rule.copies ?? []) {
    const recorded = lock[copy.from] ?? {};
    const base = join(context.root, copy.to);
    if (!existsSync(base)) {
      problems.push(`迁移副本目录缺失：${copy.to}`);
      continue;
    }
    const actual = new Map();
    for (const name of readdirSync(base).sort()) {
      if (name.endsWith(".sql"))
        actual.set(name, sha256(readFileSync(join(base, name))));
    }
    for (const [name, expected] of Object.entries(recorded)) {
      const digest = actual.get(name);
      if (digest === undefined) {
        problems.push(`迁移副本缺文件：${copy.to}/${name}（运行 go generate）`);
      } else if (digest !== expected) {
        problems.push(
          `迁移副本与 ${copy.from}/${name} 不一致：${copy.to}/${name}（运行 go generate）`,
        );
      }
    }
    for (const name of actual.keys()) {
      if (!(name in recorded)) {
        problems.push(`迁移副本多出文件：${copy.to}/${name}`);
      }
    }
  }
}

// ------------------------------------------------------- pinned-versions

const DEPENDENCY_FIELDS = [
  "dependencies",
  "devDependencies",
  "peerDependencies",
  "optionalDependencies",
];

/** `@orpc/*` 这类简写：`*` 只匹配一段包名，不跨 `/`。 */
function packageGlob(pattern) {
  const body = pattern
    .split("*")
    .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
    .join("[^/]+");
  return new RegExp(`^${body}$`);
}

/** 锁文件 `packages:` 段里解析出的 `名字@版本`（只读段首的键，不解析 YAML）。 */
function lockedPackages(text) {
  const start = text.search(/^packages:\s*$/m);
  if (start === -1) return [];
  const rest = text.slice(start).split(/^snapshots:\s*$/m)[0];
  const found = [];
  for (const match of rest.matchAll(
    /^ {2}'?((?:@[^@/\s']+\/)?[^@\s']+)@([^\s(':]+)/gm,
  )) {
    found.push({ name: match[1], version: match[2] });
  }
  return found;
}

/**
 * 上游锁版（工程规范化 §5、F15）：匹配的包在每个 package.json 里必须写精确版本、
 * 不得是预发布（beta / rc …），且整棵树——清单与锁文件——只允许同一个版本。
 * 一个都没有时规则直接通过。
 */
function checkPinnedVersions(context, problems) {
  for (const rule of context.rules.pinnedVersions ?? []) {
    const matcher = packageGlob(rule.packages);
    const seen = [];
    for (const path of context.files) {
      if (path !== "package.json" && !path.endsWith("/package.json")) continue;
      if (path.includes("/node_modules/")) continue;
      const text = readIfPresent(context.root, path);
      if (text === null) continue;
      const manifest = JSON.parse(text);
      for (const field of DEPENDENCY_FIELDS) {
        for (const [name, spec] of Object.entries(manifest[field] ?? {})) {
          if (!matcher.test(name)) continue;
          const where = `${path} ${field}.${name}`;
          if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(spec)) {
            problems.push(
              `${rule.packages} 必须写精确版本：${where} = "${spec}"`,
            );
            continue;
          }
          seen.push({ where, version: spec });
        }
      }
    }
    const lockPath = rule.lock ?? "pnpm-lock.yaml";
    const lock = readIfPresent(context.root, lockPath);
    if (lock !== null) {
      for (const { name, version } of lockedPackages(lock)) {
        if (matcher.test(name))
          seen.push({ where: `${lockPath} ${name}`, version });
      }
    }
    for (const { where, version } of seen) {
      if (version.includes("-")) {
        problems.push(
          `${rule.packages} 不得用预发布版本：${where} = "${version}"`,
        );
      }
    }
    const versions = [...new Set(seen.map((entry) => entry.version))].sort();
    if (versions.length > 1) {
      const detail = seen
        .map(({ where, version }) => `${where}@${version}`)
        .join("、");
      problems.push(
        `${rule.packages} 版本不一致（${versions.join(" / ")}）：${detail}`,
      );
    }
  }
}

// --------------------------------------------------- package-dependencies

/**
 * 包边界（清单层）：`packageDependencies` 的每一项给一个包目录与它不得声明的依赖
 * （名字或 `@scope/*` 简写），在依赖、开发依赖、对等与可选依赖里都不许出现。
 * 源码层的 import 由各包的边界测试与 ESLint 守，这里守住「依赖本身就不该进来」。
 */
function checkPackageDependencies(context, problems) {
  const tracked = new Set(context.files);
  for (const rule of context.rules.packageDependencies ?? []) {
    const path = `${rule.package}/package.json`;
    const text = tracked.has(path) ? readIfPresent(context.root, path) : null;
    if (text === null) {
      problems.push(`包边界规则指向的清单不存在：${path}`);
      continue;
    }
    const manifest = JSON.parse(text);
    const forbidden = (rule.forbid ?? []).map((pattern) => ({
      pattern,
      matcher: packageGlob(pattern),
    }));
    for (const field of DEPENDENCY_FIELDS) {
      for (const name of Object.keys(manifest[field] ?? {})) {
        const hit = forbidden.find(({ matcher }) => matcher.test(name));
        if (hit)
          problems.push(
            `${rule.package} 不得依赖 ${name}（${field}，规则 ${hit.pattern}）`,
          );
      }
    }
  }
}

// ------------------------------------------------------------ go-modules

/** go.mod 里 require 的模块（含 // indirect）与 replace / exclude 指令。 */
export function parseGoMod(text) {
  const modules = [];
  const directives = [];
  let module = null;
  let block = null;
  for (const raw of text.split("\n")) {
    const line = raw.replace(/\/\/.*$/, "").trim();
    if (line === "") continue;
    if (block !== null) {
      if (line === ")") {
        block = null;
        continue;
      }
      if (block === "require") {
        const [path, version] = line.split(/\s+/);
        modules.push({ path, version });
      } else directives.push(`${block} ${line}`);
      continue;
    }
    const opened = line.match(/^(require|replace|exclude|retract|tool)\s*\($/);
    if (opened) {
      block = opened[1];
      continue;
    }
    const single = line.match(
      /^(require|replace|exclude|retract|tool)\s+(.*)$/,
    );
    if (single) {
      if (single[1] === "require") {
        const [path, version] = single[2].split(/\s+/);
        modules.push({ path, version });
      } else directives.push(`${single[1]} ${single[2]}`);
      continue;
    }
    const named = line.match(/^module\s+(\S+)$/);
    if (named) module = named[1];
  }
  return { module, modules, directives };
}

/** .go 源码里 import 的路径（单行与括号块；注释里的不算）。 */
export function goImports(source) {
  const text = source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
  const found = [];
  for (const block of text.matchAll(/^import\s*\(([\s\S]*?)^\)/gm)) {
    for (const line of block[1].split("\n")) {
      const match = line.match(/^\s*(?:[\w.]+\s+)?"([^"]+)"/);
      if (match) found.push(match[1]);
    }
  }
  for (const match of text.matchAll(/^import\s+(?:[\w.]+\s+)?"([^"]+)"/gm)) {
    found.push(match[1]);
  }
  return found;
}

const underModule = (path, module) =>
  path === module || path.startsWith(`${module}/`);

/**
 * Go 代码的依赖边界：`go.mod` 的 require 只许登记过的模块（不得有 replace / exclude），
 * `.go` 里只许 import 标准库、本模块与登记过的模块；`internalBoundaries` 里某个目录下
 * 的代码不许 import 列出的本模块子路径（例如 `internal/core` 不碰 `internal/tunnel`）；
 * `testOnly` 里的模块只许出现在 `_test.go` 与 `testutil/` 下；`testOnlyPackages` 里的包（本模块的子路径）只许被 `_test.go` 与它自己 import。
 */
function checkGoModules(context, problems) {
  for (const rule of context.rules.goModules ?? []) {
    const modPath = `${rule.root}/go.mod`;
    const text = context.files.includes(modPath)
      ? readIfPresent(context.root, modPath)
      : null;
    if (text === null) {
      problems.push(`Go 模块规则指向的 go.mod 不存在：${modPath}`);
      continue;
    }
    const mod = parseGoMod(text);
    const allowed = rule.allow ?? [];
    for (const { path, version } of mod.modules) {
      if (!allowed.includes(path)) {
        problems.push(
          `${modPath} 引入了未登记的模块：${path}（先登记进 repo.rules.json 的 goModules）`,
        );
      }
      if (
        !/^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+incompatible)?$/.test(
          version ?? "",
        )
      ) {
        problems.push(`${modPath} 的 ${path} 必须写精确版本：${version}`);
      }
    }
    for (const directive of mod.directives) {
      if (/^(replace|exclude) /.test(directive))
        problems.push(
          `${modPath} 不许用 ${directive.split(" ")[0]}：${directive}`,
        );
    }
    for (const path of allowed) {
      if (!mod.modules.some((one) => one.path === path))
        problems.push(`goModules 登记了 go.mod 里没有的模块，可移除：${path}`);
    }
    if (!mod.module) {
      problems.push(`${modPath} 没有 module 行`);
      continue;
    }
    const prefix = `${rule.root}/`;
    for (const path of context.files) {
      if (!path.startsWith(prefix) || !path.endsWith(".go")) continue;
      const source = readIfPresent(context.root, path);
      if (source === null) continue;
      for (const target of goImports(source)) {
        const external = target.split("/")[0].includes(".");
        if (
          external &&
          !underModule(target, mod.module) &&
          !allowed.some((one) => underModule(target, one))
        ) {
          problems.push(`${path} import 了未登记的模块：${target}`);
        }
        const testOnly = (rule.testOnly ?? []).find((one) =>
          underModule(target, one),
        );
        if (
          testOnly &&
          !path.endsWith("_test.go") &&
          !path.includes("/testutil/")
        ) {
          problems.push(
            `${path} 在生产代码里 import 了只许测试用的模块：${target}`,
          );
        }
        for (const hidden of rule.testOnlyPackages ?? []) {
          const inside = path.startsWith(`${rule.root}/${hidden}/`);
          if (
            underModule(target, `${mod.module}/${hidden}`) &&
            !inside &&
            !path.endsWith("_test.go")
          ) {
            problems.push(
              `${path} 在生产代码里 import 了测试用的包：${target}`,
            );
          }
        }
        for (const boundary of rule.internalBoundaries ?? []) {
          if (!path.startsWith(`${rule.root}/${boundary.from}/`)) continue;
          for (const forbidden of boundary.forbid) {
            if (underModule(target, `${mod.module}/${forbidden}`))
              problems.push(
                `${path} 不得 import ${target}（${boundary.from} 不碰 ${forbidden}）`,
              );
          }
        }
      }
    }
  }
}

const CHECKS = {
  "docs-index": checkDocsIndex,
  links: checkLinks,
  blacklist: checkBlacklist,
  naming: checkNaming,
  "root-allowlist": checkRootAllowlist,
  "file-size": checkFileSize,
  migrations: checkMigrations,
  "pinned-versions": checkPinnedVersions,
  "package-dependencies": checkPackageDependencies,
  "go-modules": checkGoModules,
};

/** 对一个仓库根跑规则，返回问题列表；空列表表示通过。 */
export function checkRepository({ root, rules, files, only }) {
  const context = {
    root,
    rules,
    files: files ?? trackedFiles(root),
  };
  const problems = [];
  for (const name of only ?? RULES) {
    const check = CHECKS[name];
    if (!check) throw new Error(`未知规则：${name}`);
    check(context, problems);
  }
  return problems;
}

// ---------------------------------------------------------------------- cli

function parseArguments(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index];
    if (flag === "--root") options.root = argv[(index += 1)];
    else if (flag === "--rules") options.rules = argv[(index += 1)];
    else if (flag === "--only") options.only = argv[(index += 1)].split(",");
    else throw new Error(`未知参数：${flag}`);
  }
  return options;
}

const invokedDirectly =
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (invokedDirectly) {
  const options = parseArguments(process.argv.slice(2));
  const root = resolve(
    options.root ?? fileURLToPath(new URL("../", import.meta.url)),
  );
  const rulesPath = resolve(root, options.rules ?? "repo.rules.json");
  const rules = JSON.parse(readFileSync(rulesPath, "utf8"));
  const started = Date.now();
  const problems = checkRepository({ root, rules, only: options.only });
  if (problems.length > 0) {
    for (const problem of problems) console.error(`✗ ${problem}`);
    console.error(`\n仓库校验失败：${problems.length} 处问题`);
    process.exit(1);
  }
  console.log(
    `仓库校验通过（${(options.only ?? RULES).length} 条规则，${Date.now() - started}ms）`,
  );
}
