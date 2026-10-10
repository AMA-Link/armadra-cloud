/**
 * 包边界（AGENTS.md「代码边界」）：Node 运行时只装配中继内核，不 import 控制核、控制面与
 * 任何 app。PostgreSQL / Redis 只出现在多节点控制面的两个实现里（`control/postgres.ts`、
 * `control/redis.ts`，经 `@armadra/cloud-shared/db|redis`），包内别的文件不 import 它们，由装配方
 * 按 `--control` 动态 import。ESLint 守静态 import；这里扫源码文本（连动态 import 一起）并核对清单的依赖。
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

const imports = (specifier: string) =>
  new RegExp(`(?:from\\s+|import\\(\\s*)["']${specifier}["']`);

const FORBIDDEN = [
  imports("@armadra/control-core(?:/[^\"']*)?"),
  imports("@armadra/control-store-(?:postgres|sqlite)(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay)(?:/[^\"']*)?"),
  imports("(?:\\.\\./)+apps/[^\"']*"),
  imports("(?:\\.\\./)+relay-kernel/[^\"']*"),
  imports("postgres"),
  imports("redis"),
];
const DATABASE = imports("@armadra/cloud-shared/db");
const REDIS = imports("@armadra/cloud-shared/redis");
/** 多节点控制面的两个实现：只有它们碰存储客户端。 */
const CLUSTER: Record<string, RegExp> = {
  "control/postgres.ts": DATABASE,
  "control/redis.ts": REDIS,
};

const relativePath = (file: string) =>
  relative(SRC, file).split("\\").join("/");

it("Node 运行时不 import 控制核、控制面、app 与存储驱动；内核只经包名引用", () => {
  const files = sources(SRC);
  expect(files.length).toBeGreaterThan(5);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    const allowed = CLUSTER[relativePath(file)];
    return [...FORBIDDEN, DATABASE, REDIS]
      .filter((pattern) => pattern !== allowed && pattern.test(text))
      .map((pattern) => `${relativePath(file)}: ${pattern}`);
  });
  expect(offenders).toEqual([]);
});

it("包内没有文件 import 多节点控制面的两个实现（只由装配方按需动态 import）", () => {
  const offenders = sources(SRC)
    .filter((file) => !(relativePath(file) in CLUSTER))
    .filter((file) =>
      /["']\.{1,2}\/(?:[^"']*\/)?control\/(?:postgres|redis)(?:\.ts)?["']|["']\.\/(?:postgres|redis)\.ts["']/.test(
        readFileSync(file, "utf8"),
      ),
    )
    .map(relativePath);
  expect(offenders).toEqual([]);
});

it("Node 运行时清单不依赖控制核与 SaaS 专有的包", () => {
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
