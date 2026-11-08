import assert from "node:assert/strict";
import { test } from "node:test";

import { planGoScope, planVitest } from "./verify.mjs";

const BASE = "abc123";
const PACKAGES = [
  {
    importPath: "m/internal/obs",
    dir: "go/internal/obs",
    deps: [],
    testImports: [],
  },
];

test("没有改动：协议包的测试不跑，Go 一个包都不查", () => {
  assert.equal(planVitest([], { base: BASE }), null);
  assert.deepEqual(planGoScope([], PACKAGES), { full: false, packages: [] });
});

test("只改 Go：协议包的测试不跑", () => {
  assert.equal(planVitest(["go/internal/obs/log.go"], { base: BASE }), null);
});

test("改协议包源码或测试：交给 vitest --changed 挑文件", () => {
  for (const file of [
    "packages/platform-protocol/src/tunnel/frames.ts",
    "packages/platform-protocol/test/frames.test.ts",
  ]) {
    assert.deepEqual(planVitest([file], { base: BASE }), [
      "exec",
      "vitest",
      "run",
      "--changed",
      BASE,
      "--passWithNoTests",
    ]);
  }
});

test("测试按路径读的夹具、schema、JSON 向量与配置变了：协议包整包跑", () => {
  for (const file of [
    "packages/platform-protocol/fixtures/tunnel/open.json",
    "packages/platform-protocol/schema/errors.json",
    "packages/platform-protocol/test/format-vectors.json",
    "packages/platform-protocol/src/identity-vectors/vectors.json",
    "packages/platform-protocol/vitest.config.mts",
    "packages/platform-protocol/package.json",
  ]) {
    assert.deepEqual(
      planVitest([file], { base: BASE }),
      ["exec", "vitest", "run"],
      file,
    );
  }
});

test("锁文件、根配置变了，--full，或拿不到基点：全部全量", () => {
  for (const file of ["pnpm-lock.yaml", "package.json", "tsconfig.base.json"]) {
    assert.deepEqual(
      planVitest([file], { base: BASE }),
      ["exec", "vitest", "run"],
      file,
    );
    assert.equal(planGoScope([file], PACKAGES).full, true, file);
  }
  assert.deepEqual(planVitest([], { base: BASE, full: true }), [
    "exec",
    "vitest",
    "run",
  ]);
  assert.equal(planGoScope([], PACKAGES, { full: true }).full, true);
  assert.deepEqual(planVitest(null, { base: null }), ["exec", "vitest", "run"]);
  assert.equal(planGoScope(null, PACKAGES).full, true);
  // go list 失败：全量，让 go vet 报出原因。
  assert.equal(planGoScope(["go/internal/obs/log.go"], null).full, true);
});

test("Go 的挑选交给 planGo", () => {
  assert.deepEqual(planGoScope(["go/internal/obs/log.go"], PACKAGES), {
    full: false,
    packages: ["m/internal/obs"],
  });
});
