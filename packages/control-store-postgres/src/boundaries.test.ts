/**
 * 包边界：PostgreSQL 存储只实现控制核的端口——不 import 中继内核、app、网络、Redis 与别的存储实现；
 * 驱动只经 `postgres`（与 `@armadra/cloud-shared/db` 建池）。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { expect, it } from "vitest";

const SRC = fileURLToPath(new URL("./", import.meta.url));
const MANIFEST = fileURLToPath(new URL("../package.json", import.meta.url));

const imports = (specifier: string) =>
  new RegExp(`(?:from\\s+|import\\(\\s*)["']${specifier}["']`);

const FORBIDDEN = [
  imports("node:(?:http|https|net|sqlite)"),
  imports("ws"),
  imports("redis"),
  imports("(?:better-)?sqlite3"),
  imports("@armadra/cloud-shared/(?:redis|http)"),
  imports("@armadra/control-store-sqlite(?:/[^\"']*)?"),
  imports("@armadra/relay-kernel(?:/[^\"']*)?"),
  imports("@armadra/relay-runtime-(?:node|workers)(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay)(?:/[^\"']*)?"),
];

it("PostgreSQL 存储源码不 import 中继内核、app、网络、Redis 与别的存储实现", () => {
  const files = readdirSync(SRC)
    .filter((name) => name.endsWith(".ts") && !name.endsWith(".test.ts"))
    .map((name) => join(SRC, name));
  expect(files.length).toBeGreaterThan(5);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return FORBIDDEN.filter((pattern) => pattern.test(text)).map(
      (pattern) => `${relative(SRC, file)}: ${pattern}`,
    );
  });
  expect(offenders).toEqual([]);
});

it("清单只依赖控制核、SQL 账本、共享的连接池与 postgres 驱动", () => {
  const manifest = JSON.parse(readFileSync(MANIFEST, "utf8")) as {
    dependencies?: Record<string, string>;
  };
  expect(Object.keys(manifest.dependencies ?? {}).sort()).toEqual([
    "@armadra/cloud-shared",
    "@armadra/control-core",
    "@armadra/control-store-sql",
    "postgres",
  ]);
});
