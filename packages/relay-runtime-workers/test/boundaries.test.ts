/**
 * 包边界（AGENTS.md「代码边界」）：Workers 运行时只装配内核与控制核，不带 Node 专有的东西
 * （`ws`、`node:fs` / `http` / `net` / `sqlite`…、`node:sqlite` 版存储的打开与迁移读目录）、
 * PostgreSQL / Redis 与 app。源码经 Vite 的 `import.meta.glob` 读进来（workerd 里没有文件系统）。
 */
import { expect, it } from "vitest";

import manifest from "../package.json" with { type: "json" };

const SOURCES = import.meta.glob("../src/**/*.ts", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const imports = (specifier: string) =>
  new RegExp(`(?:from\\s+|import\\(\\s*)["']${specifier}["']`);

const FORBIDDEN = [
  imports("ws"),
  imports("postgres"),
  imports("redis"),
  imports("node:(?!(?:crypto|buffer)[\"'])[^\"']+"),
  imports("@armadra/cloud-shared/(?:db|redis|http|config)"),
  imports("@armadra/control-store-sqlite/(?:open|migrate)"),
  imports("@armadra/relay-runtime-node(?:/[^\"']*)?"),
  imports("@armadra/(?:cloud|relay|relay-workers)(?:/[^\"']*)?"),
  imports("(?:\\.\\./)+apps/[^\"']*"),
];

it("源码只许 node:crypto / node:buffer，不碰 ws、PostgreSQL / Redis、Node 版存储的打开与 app", () => {
  const files = Object.keys(SOURCES).filter((file) => !file.endsWith(".d.ts"));
  expect(files.length).toBeGreaterThan(15);
  const offenders = files.flatMap((file) =>
    FORBIDDEN.filter((pattern) => pattern.test(SOURCES[file] ?? "")).map(
      (pattern) => `${file}: ${pattern}`,
    ),
  );
  expect(offenders).toEqual([]);
});

it("清单不依赖 ws、PostgreSQL / Redis 与 Node 运行时", () => {
  const names = Object.keys({
    ...manifest.dependencies,
    ...manifest.devDependencies,
  });
  expect(
    names.filter((name) =>
      [
        "ws",
        "@types/ws",
        "postgres",
        "redis",
        "@armadra/relay-runtime-node",
      ].includes(name),
    ),
  ).toEqual([]);
});
