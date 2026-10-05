/**
 * 代码边界（AGENTS.md）：relay 不 import 控制面、不碰数据库；personal 模式不引入
 * PostgreSQL / Redis 的运行时依赖；cli 只按需 import 各模式。ESLint 也守前两条；
 * 这里扫源码文本，连动态 import 一起拦。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";

const SRC = fileURLToPath(new URL("./", import.meta.url));

function sources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return sources(path);
    return entry.name.endsWith(".ts") && !entry.name.endsWith(".test.ts")
      ? [path]
      : [];
  });
}

function offenders(files: string[], patterns: RegExp[]): string[] {
  return files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return patterns
      .filter((pattern) => pattern.test(text))
      .map((pattern) => `${relative(SRC, file)}: ${pattern}`);
  });
}

const DATABASE = [/["']postgres["']/, /["']@armadra\/cloud-shared\/db["']/];
const REDIS = [/["']redis["']/, /["']@armadra\/cloud-shared\/redis["']/];

it("relay 源码不 import 控制面、不 import 数据库客户端", () => {
  const files = sources(SRC);
  expect(files.length).toBeGreaterThan(0);
  expect(
    offenders(files, [
      /["']@armadra\/cloud["'/]/,
      /["'](\.\.\/)+cloud\//,
      ...DATABASE,
    ]),
  ).toEqual([]);
});

it("personal 模式及它依赖的内核（除 saas/、control/redis.ts 与按需分派的 cli.ts 外的全部源码）不 import Redis 与 saas", () => {
  const kernel = sources(SRC).filter((file) => {
    const path = relative(SRC, file).split("\\").join("/");
    return (
      !path.startsWith("saas/") &&
      path !== "control/redis.ts" &&
      path !== "cli.ts"
    );
  });
  expect(kernel.some((file) => file.includes("personal"))).toBe(true);
  expect(kernel.some((file) => file.endsWith("main.ts"))).toBe(true);
  expect(offenders(kernel, [...REDIS, /["']\.\.?\/(\.\.\/)?saas\//])).toEqual(
    [],
  );
});

it("cli 不静态 import 任何模式（各模式按需动态 import）", () => {
  const cli = readFileSync(join(SRC, "cli.ts"), "utf8");
  const staticImports = [
    ...cli.matchAll(/^import .* from ["']([^"']+)["'];?$/gm),
  ].map((match) => match[1]);
  expect(
    staticImports.filter((path) =>
      /saas|personal|redis|config\.ts/.test(path ?? ""),
    ),
  ).toEqual([]);
});
