/**
 * 代码边界（AGENTS.md）：relay 不 import 控制面；personal 的**缺省装配**（file / sqlite 存储 + 内存
 * 控制面）不静态依赖 PostgreSQL / Redis——`--store postgres` 与 `--control redis|postgres` 是显式选项，
 * 它们的模块只经动态 import 按需加载；cli 只按需 import 各模式。
 *
 * 这里沿静态 import（`import … from`、`export … from`、`import "…"`；`import type` 编译后不存在，
 * 不算）从 personal 的每个源文件走遍整张图，跨进本仓的包（按 `package.json` 的 `exports` 解析），
 * 核对图里没有存储驱动与它们的封装；再核对动态 import 它们的只有两个装配点。ESLint 也守一部分。
 */
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";

const SRC = fileURLToPath(new URL("./", import.meta.url));
const REPO = fileURLToPath(new URL("../../../", import.meta.url));

function sources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return sources(path);
    return entry.name.endsWith(".ts") && !entry.name.endsWith(".test.ts")
      ? [path]
      : [];
  });
}

const rel = (file: string) => relative(REPO, file).split("\\").join("/");

// ------------------------------------------------------------ 本仓的包

interface Workspace {
  dir: string;
  /** 源码导出（值是字符串的那些）；协议包导出构建产物（条件导出），当作叶子。 */
  exports: Record<string, string>;
}

const workspaces = new Map<string, Workspace>();
for (const group of ["packages", "apps"])
  for (const name of readdirSync(join(REPO, group))) {
    const manifest = join(REPO, group, name, "package.json");
    if (!existsSync(manifest)) continue;
    const json = JSON.parse(readFileSync(manifest, "utf8")) as {
      name: string;
      exports?: Record<string, unknown>;
    };
    const exports = Object.entries(json.exports ?? {});
    if (exports.some(([, target]) => typeof target !== "string")) continue;
    workspaces.set(json.name, {
      dir: join(REPO, group, name),
      exports: Object.fromEntries(exports) as Record<string, string>,
    });
  }

/** `@armadra/x/sub` → 源文件；不是本仓的包答 null。 */
function resolveWorkspace(specifier: string): string | null {
  const match = /^(@armadra\/[^/]+)(\/.*)?$/.exec(specifier);
  const workspace = match ? workspaces.get(match[1] ?? "") : undefined;
  if (!match || !workspace) return null;
  const subpath = `.${match[2] ?? ""}`;
  const exact = workspace.exports[subpath];
  if (exact) return join(workspace.dir, exact);
  for (const [pattern, target] of Object.entries(workspace.exports)) {
    if (!pattern.includes("*")) continue;
    const [head, tail] = pattern.split("*") as [string, string];
    if (subpath.startsWith(head) && subpath.endsWith(tail))
      return join(
        workspace.dir,
        target.replace(
          "*",
          subpath.slice(head.length, subpath.length - tail.length),
        ),
      );
  }
  throw new Error(`包 ${match[1]} 没有导出 ${subpath}`);
}

// ------------------------------------------------------------ 静态 import 图

const STATIC_IMPORT =
  /(?:^|\n)\s*(?:import|export)\s+(type\s+)?(?:[^;'"]*?\sfrom\s+)?["']([^"']+)["']/g;

function staticImports(file: string): string[] {
  const text = readFileSync(file, "utf8");
  return [...text.matchAll(STATIC_IMPORT)]
    .filter((match) => !match[1])
    .map((match) => match[2] ?? "");
}

/** 从 `entries` 沿静态 import 走：答走到的本仓文件与外部包（含 `node:*`）。 */
function walk(entries: string[]): {
  files: Set<string>;
  externals: Map<string, string>;
} {
  const files = new Set<string>();
  const externals = new Map<string, string>();
  const queue = [...entries];
  while (queue.length > 0) {
    const file = queue.pop() as string;
    if (files.has(file)) continue;
    files.add(file);
    if (!file.endsWith(".ts")) continue;
    for (const specifier of staticImports(file)) {
      if (specifier.startsWith(".")) {
        queue.push(resolve(dirname(file), specifier));
        continue;
      }
      const target = resolveWorkspace(specifier);
      if (target) queue.push(target);
      else if (!externals.has(specifier)) externals.set(specifier, rel(file));
    }
  }
  return { files, externals };
}

// ------------------------------------------------------------ 规则

/** 存储驱动与它们的封装：缺省装配一个也不能静态带进来。 */
const OPTIONAL = [
  /^postgres$/,
  /^redis$/,
  /^@redis\//,
  /^@armadra\/cloud-shared\/(?:db|redis)$/,
  /^@armadra\/control-store-postgres(?:\/.*)?$/,
  /^@armadra\/relay-runtime-node\/control\/(?:postgres|redis)$/,
];
const OPTIONAL_FILES = [
  "apps/relay/src/personal/postgres-store.ts",
  "packages/relay-runtime-node/src/control/postgres.ts",
  "packages/relay-runtime-node/src/control/redis.ts",
  "packages/cloud-shared/src/db/client.ts",
  "packages/cloud-shared/src/redis.ts",
];
/** 只有这两个装配点按需动态 import 可选模块。 */
const DYNAMIC_OWNERS: Record<string, RegExp> = {
  "apps/relay/src/personal/store.ts": /^\.\/postgres-store\.ts$/,
  "apps/relay/src/personal/control.ts":
    /^@armadra\/(?:cloud-shared\/redis|control-store-postgres\/(?:open|migrate|errors)|relay-runtime-node\/control\/(?:postgres|redis))$/,
};

const personalEntries = sources(join(SRC, "personal")).filter(
  (file) => rel(file) !== "apps/relay/src/personal/postgres-store.ts",
);

it("personal 的缺省装配（全部 personal 源码沿静态 import 走遍）不带 PostgreSQL / Redis", () => {
  expect(personalEntries.length).toBeGreaterThan(10);
  const { files, externals } = walk(personalEntries);
  // 走进了内核、Node 运行时、控制核与 SQLite 存储（图是真的跨包走了）。
  for (const expected of [
    "packages/relay-kernel/src/control/memory.ts",
    "packages/relay-runtime-node/src/start-relay.ts",
    "packages/control-core/src/accounts.ts",
    "packages/control-store-sqlite/src/open.ts",
  ])
    expect([...files].map(rel)).toContain(expected);
  expect(
    [...externals]
      .filter(([specifier]) =>
        OPTIONAL.some((pattern) => pattern.test(specifier)),
      )
      .map(([specifier, from]) => `${from}: ${specifier}`),
  ).toEqual([]);
  expect(
    [...files].map(rel).filter((file) => OPTIONAL_FILES.includes(file)),
  ).toEqual([]);
});

it("可选模块只在 store.ts 与 control.ts 里动态 import", () => {
  const offenders = sources(SRC).flatMap((file) => {
    const text = readFileSync(file, "utf8");
    const owner = DYNAMIC_OWNERS[rel(file)];
    return [...text.matchAll(/import\(\s*["']([^"']+)["']\s*\)/g)]
      .map((match) => match[1] ?? "")
      .filter(
        (specifier) =>
          (OPTIONAL.some((pattern) => pattern.test(specifier)) ||
            /postgres-store/.test(specifier)) &&
          !owner?.test(specifier),
      )
      .map((specifier) => `${rel(file)}: ${specifier}`);
  });
  expect(offenders).toEqual([]);
  // 两个装配点确实在用（规则没有空转）。
  for (const owner of Object.keys(DYNAMIC_OWNERS))
    expect(readFileSync(join(REPO, owner), "utf8")).toMatch(/import\(\s*["']/);
});

it("relay 源码不 import 控制面，也不直接 import 数据库驱动", () => {
  const files = sources(SRC);
  const patterns = [
    /["']@armadra\/cloud["'/]/,
    /["'](\.\.\/)+cloud\//,
    /from\s+["']postgres["']/,
    /["']@armadra\/cloud-shared\/db["']/,
  ];
  expect(
    files.flatMap((file) => {
      const text = readFileSync(file, "utf8");
      return patterns
        .filter((pattern) => pattern.test(text))
        .map((pattern) => `${rel(file)}: ${pattern}`);
    }),
  ).toEqual([]);
});

it("personal 不 import saas；saas 只经 cli 按需加载", () => {
  const { files } = walk(personalEntries);
  expect(
    [...files].map(rel).filter((file) => file.includes("/src/saas/")),
  ).toEqual([]);
});

it("cli 不静态 import 任何模式（各模式按需动态 import）", () => {
  const cli = readFileSync(join(SRC, "cli.ts"), "utf8");
  const staticImports = [
    ...cli.matchAll(/^import .* from ["']([^"']+)["'];?$/gm),
  ].map((match) => match[1]);
  expect(
    staticImports.filter((path) =>
      /saas|personal|redis|postgres|config\.ts/.test(path ?? ""),
    ),
  ).toEqual([]);
});

it("清单：PostgreSQL 存储是可选依赖，relay 不直接依赖驱动", () => {
  const manifest = JSON.parse(
    readFileSync(join(SRC, "../package.json"), "utf8"),
  ) as Record<string, Record<string, string> | undefined>;
  expect(Object.keys(manifest.optionalDependencies ?? {})).toEqual([
    "@armadra/control-store-postgres",
  ]);
  expect(
    manifest.dependencies?.["@armadra/control-store-postgres"],
  ).toBeUndefined();
  for (const field of ["dependencies", "optionalDependencies"])
    expect(
      Object.keys(manifest[field] ?? {}).filter((name) =>
        ["postgres", "redis"].includes(name),
      ),
    ).toEqual([]);
});
