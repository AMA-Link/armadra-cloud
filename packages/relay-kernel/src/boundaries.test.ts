/**
 * 包边界（AGENTS.md「代码边界」）：中继内核是 personal 与 saas 共用的数据面，
 * 不 import 控制核、控制面与任何 app，也不带 SaaS 专有依赖（PostgreSQL / Redis）。
 * ESLint 守静态 import；这里扫源码文本（连动态 import 一起）并核对清单的依赖。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { expect, it } from "vitest";

const SRC = fileURLToPath(new URL("./", import.meta.url));
const MANIFEST = fileURLToPath(new URL("../package.json", import.meta.url));

function sources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return sources(path);
    return entry.name.endsWith(".ts") && !entry.name.endsWith(".test.ts")
      ? [path]
      : [];
  });
}

/** `from "<模块>"` 或 `import("<模块>")`：只认 import 位置，不误伤普通字符串。 */
const imports = (specifier: string) =>
  new RegExp(`(?:from\\s+|import\\(\\s*)["']${specifier}["']`);

const FORBIDDEN = [
  imports("@armadra/control-core(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay)(?:/[^\"']*)?"),
  imports("(?:\\.\\./)+apps/[^\"']*"),
  imports("postgres"),
  imports("redis"),
  imports("@armadra/cloud-shared/(?:db|redis)"),
];

it("内核源码不 import 控制核、控制面、app、数据库与 Redis 客户端", () => {
  const files = sources(SRC);
  expect(files.length).toBeGreaterThan(10);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return FORBIDDEN.filter((pattern) => pattern.test(text)).map(
      (pattern) => `${relative(SRC, file)}: ${pattern}`,
    );
  });
  expect(offenders).toEqual([]);
});

it("内核清单不依赖控制核与 SaaS 专有的包", () => {
  const manifest = JSON.parse(readFileSync(MANIFEST, "utf8")) as Record<
    string,
    Record<string, string> | undefined
  >;
  const names = ["dependencies", "devDependencies", "peerDependencies"]
    .flatMap((field) => Object.keys(manifest[field] ?? {}))
    .sort();
  expect(
    names.filter((name) =>
      [
        "@armadra/control-core",
        "@armadra/cloud",
        "@armadra/relay",
        "postgres",
        "redis",
      ].includes(name),
    ),
  ).toEqual([]);
});
