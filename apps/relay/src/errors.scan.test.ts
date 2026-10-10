/**
 * 错误码只从协议包注册表取（中继设计 §8、§9）：扫源码里写给线上的码字面量，全部要在注册表里。
 * 类型系统已经挡住大部分（参数是 `ErrorCode`），这里再挡一道 `code: "…"` 这种对象字面量。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { isErrorCode } from "@armadra/platform-protocol/errors";
import { expect, it } from "vitest";

const SRC = fileURLToPath(new URL("./", import.meta.url));
/** 中继的线上错误由本 app 与它装配的内核、Node 运行时、控制核一起产生。 */
const ROOTS = [
  SRC,
  fileURLToPath(
    new URL("../../../packages/relay-kernel/src/", import.meta.url),
  ),
  fileURLToPath(
    new URL("../../../packages/relay-runtime-node/src/", import.meta.url),
  ),
  fileURLToPath(
    new URL("../../../packages/control-core/src/", import.meta.url),
  ),
];

function sources(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return sources(path);
    return entry.name.endsWith(".ts") && !entry.name.endsWith(".test.ts")
      ? [path]
      : [];
  });
}

const PATTERNS = [
  /\b(?:ControlError|sendRelayError|sendPersonalError|rejectUpgrade|rawErrorResponse|rawErrorBytes)\(\s*(?:[\w.]+,\s*)?"([a-z_]+)"/g,
  /\bcode: "([a-z_]+)"/g,
  /\berror: "([a-z_]+)" as const/g,
];

it("源码里写给线上的错误码都在协议包注册表里", () => {
  const found: string[] = [];
  const unknown: string[] = [];
  for (const file of ROOTS.flatMap(sources)) {
    const text = readFileSync(file, "utf8");
    for (const pattern of PATTERNS)
      for (const match of text.matchAll(pattern)) {
        const code = match[1] as string;
        found.push(code);
        if (!isErrorCode(code)) unknown.push(`${relative(SRC, file)}: ${code}`);
      }
  }
  expect(found.length).toBeGreaterThan(20);
  expect(unknown).toEqual([]);
});
