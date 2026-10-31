/**
 * 包边界（AGENTS.md「代码边界」）：控制核只经端口读写状态——不 import 文件系统、数据库与
 * Redis 客户端；与传输无关——不 import HTTP、WebSocket 与中继内核；也不 import 任何 app。
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
  imports("node:fs(?:/promises)?"),
  imports("node:(?:http|https|net)"),
  imports("ws"),
  imports("postgres"),
  imports("redis"),
  imports("@armadra/cloud-shared/(?:db|redis|http)"),
  imports("@armadra/relay-kernel(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay)(?:/[^\"']*)?"),
  imports("(?:\\.\\./)+apps/[^\"']*"),
];

it("控制核源码只经端口：不 import 文件系统、数据库、Redis、HTTP / WS、中继内核与 app", () => {
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

it("控制核清单不依赖中继内核与存储客户端", () => {
  const manifest = JSON.parse(readFileSync(MANIFEST, "utf8")) as Record<
    string,
    Record<string, string> | undefined
  >;
  const names = ["dependencies", "devDependencies", "peerDependencies"]
    .flatMap((field) => Object.keys(manifest[field] ?? {}))
    .sort();
  expect(
    names.filter((name) =>
      ["@armadra/relay-kernel", "postgres", "redis", "ws"].includes(name),
    ),
  ).toEqual([]);
});
