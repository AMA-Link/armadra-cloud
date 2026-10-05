/**
 * 代码边界（AGENTS.md）：relay 不得 import apps/cloud，不碰数据库。
 * ESLint 也守同一条；这里扫源码文本，连动态 import 一起拦。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
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

const FORBIDDEN = [
  /["']@armadra\/cloud["'/]/,
  /["'](\.\.\/)+cloud\//,
  /["']postgres["']/,
  /["']@armadra\/cloud-shared\/db["']/,
];

it("relay 源码不 import 控制面、不 import 数据库客户端", () => {
  const files = sources(SRC);
  expect(files.length).toBeGreaterThan(0);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return FORBIDDEN.filter((pattern) => pattern.test(text)).map(
      (pattern) => `${file}: ${pattern}`,
    );
  });
  expect(offenders).toEqual([]);
});
