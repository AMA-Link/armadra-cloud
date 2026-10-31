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
      ["@armadra/control-core", "postgres", "redis"].includes(name),
    ),
  ).toEqual([]);
});

/**
 * 运行时无关（CF 设计 §2.9、§2.10）：Workers 上也要能跑，所以只许 `node:crypto` /
 * `node:buffer`（`nodejs_compat` 原生支持）；`ws`、`node:http` / `net` / `stream` / `fs` 等
 * 与 cloud-shared 的 HTTP 外壳都不进内核。
 */
const NODE_ONLY = [
  imports("node:(?!crypto[\"']|buffer[\"'])[^\"']+"),
  imports(
    "(?!node:)(?:http|https|net|tls|stream|fs|os|child_process|dgram|cluster|worker_threads)(?:/[^\"']*)?",
  ),
  imports("ws"),
  imports("@armadra/cloud-shared/http"),
];

it("内核源码与运行时无关：除 node:crypto / node:buffer 外不 import node:*，不 import ws 与 HTTP 外壳", () => {
  const files = sources(SRC);
  const offenders = files.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    return NODE_ONLY.filter((pattern) => pattern.test(text)).map(
      (pattern) => `${relative(SRC, file)}: ${pattern}`,
    );
  });
  expect(offenders).toEqual([]);
  // 自检：规则认得出要拦的写法、放过允许的写法。
  const blocked = [
    'import { request } from "node:http";',
    'import type { Duplex } from "node:stream";',
    'import { WebSocketServer } from "ws";',
    'const fs = await import("node:fs");',
    'import { listen } from "@armadra/cloud-shared/http";',
  ];
  for (const line of blocked)
    expect(
      NODE_ONLY.some((pattern) => pattern.test(line)),
      line,
    ).toBe(true);
  for (const line of [
    'import { randomBytes } from "node:crypto";',
    'import { Buffer } from "node:buffer";',
    'import { verifyJwt } from "@armadra/cloud-shared/jwt";',
  ])
    expect(
      NODE_ONLY.some((pattern) => pattern.test(line)),
      line,
    ).toBe(false);
});

it("内核清单不依赖 ws", () => {
  const manifest = JSON.parse(readFileSync(MANIFEST, "utf8")) as Record<
    string,
    Record<string, string> | undefined
  >;
  const names = ["dependencies", "devDependencies", "peerDependencies"].flatMap(
    (field) => Object.keys(manifest[field] ?? {}),
  );
  expect(names.filter((name) => ["ws", "@types/ws"].includes(name))).toEqual(
    [],
  );
});
