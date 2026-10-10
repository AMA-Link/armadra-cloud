/**
 * 包边界：SQLite 存储只实现控制核的端口——不 import 中继内核、app、网络与其它存储客户端
 * （PostgreSQL / Redis）；SQLite 只经 `node:sqlite`，不带第三方驱动。
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
  imports("node:(?:http|https|net)"),
  imports("ws"),
  imports("postgres"),
  imports("redis"),
  imports("(?:better-)?sqlite3"),
  imports("@armadra/cloud-shared/(?:db|redis|http)"),
  imports("@armadra/relay-kernel(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay)(?:/[^\"']*)?"),
];

it("SQLite 存储源码不 import 中继内核、app、网络与别的存储客户端", () => {
  const files = readdirSync(SRC)
    .filter((name) => name.endsWith(".ts") && !name.endsWith(".test.ts"))
    .map((name) => join(SRC, name));
  expect(files.length).toBeGreaterThan(4);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return FORBIDDEN.filter((pattern) => pattern.test(text)).map(
      (pattern) => `${relative(SRC, file)}: ${pattern}`,
    );
  });
  expect(offenders).toEqual([]);
});

it("清单只依赖控制核与 SQL 包", () => {
  const manifest = JSON.parse(readFileSync(MANIFEST, "utf8")) as {
    dependencies?: Record<string, string>;
  };
  expect(Object.keys(manifest.dependencies ?? {}).sort()).toEqual([
    "@armadra/control-core",
    "@armadra/control-store-sql",
  ]);
});
