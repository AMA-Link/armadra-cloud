// 错误码扫描（AGENTS.md「错误码只从协议包取」）：控制核、中继内核与共享包源码里
// 出现的每个 `code: "…"` 字面量都必须在协议包的错误码注册表里。
import { strict as assert } from "node:assert";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const ROOTS = [
  "packages/cloud-shared/src",
  "packages/control-core/src",
  "packages/relay-kernel/src",
];
const registry = JSON.parse(
  readFileSync(
    join(root, "packages/platform-protocol/fixtures/errors.json"),
    "utf8",
  ),
);

/** 不是线上错误码的 `code:`：zod 的 issue 码（`ctx.addIssue({ code: "custom" })`）。 */
const NOT_WIRE = new Set(["custom"]);

function sources(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return sources(path);
    return /\.ts$/.test(entry.name) && !/\.test\.ts$/.test(entry.name)
      ? [path]
      : [];
  });
}

export function codeLiterals(text) {
  return [...text.matchAll(/\bcode:\s*["']([a-z0-9_]+)["']/g)].map(
    (match) => match[1],
  );
}

test("扫描规则认得出 code 字面量", () => {
  assert.deepEqual(
    codeLiterals(`x({ code: "not_found", message }); y = { code: 'a_b' }`),
    ["not_found", "a_b"],
  );
});

test("源码里的 code 字面量都在协议包注册表里", () => {
  const unknown = [];
  let seen = 0;
  for (const base of ROOTS) {
    for (const file of sources(join(root, base))) {
      for (const code of codeLiterals(readFileSync(file, "utf8"))) {
        if (NOT_WIRE.has(code)) continue;
        seen += 1;
        if (!(code in registry))
          unknown.push(`${relative(root, file)}: ${code}`);
      }
    }
  }
  assert.ok(seen > 0, "一个 code 字面量都没扫到，扫描规则可能失效了");
  assert.deepEqual(unknown, []);
});
