import assert from "node:assert/strict";
import { test } from "node:test";

import { goSteps, planGo, snapshotDiff, TEST_TIMEOUT } from "./go-check.mjs";

const M = "github.com/AMA-Link/armadra-cloud/go";

/** 一个小模块：obs ← store ← server ← cmd；api 的测试 import testutil，testutil 依赖 store。 */
const PACKAGES = [
  {
    importPath: `${M}/internal/obs`,
    dir: "go/internal/obs",
    deps: ["fmt"],
    testImports: [],
  },
  {
    importPath: `${M}/internal/store`,
    dir: "go/internal/store",
    deps: ["fmt", `${M}/internal/obs`],
    testImports: [],
  },
  {
    importPath: `${M}/internal/server`,
    dir: "go/internal/server",
    deps: [`${M}/internal/obs`, `${M}/internal/store`],
    testImports: [],
  },
  {
    importPath: `${M}/cmd/relay`,
    dir: "go/cmd/relay",
    deps: [`${M}/internal/obs`, `${M}/internal/store`, `${M}/internal/server`],
    testImports: [],
  },
  {
    importPath: `${M}/internal/testutil`,
    dir: "go/internal/testutil",
    deps: [`${M}/internal/store`, `${M}/internal/obs`],
    testImports: [],
  },
  {
    importPath: `${M}/internal/api`,
    dir: "go/internal/api",
    deps: ["net/http"],
    testImports: [`${M}/internal/testutil`, "testing"],
  },
];

const names = (plan) => plan.packages.map((pkg) => pkg.slice(M.length + 1));

test("没有改动，或只改了 Go 以外的文件：一个包都不查", () => {
  for (const changed of [
    [],
    ["docs/README.md", "packages/e2e-fake-core/src/x.ts"],
  ]) {
    assert.deepEqual(planGo(changed, PACKAGES), { full: false, packages: [] });
  }
});

test("改一个包：它自己加上直接或间接 import 它的包", () => {
  const plan = planGo(["go/internal/store/store.go"], PACKAGES);
  assert.equal(plan.full, false);
  assert.deepEqual(names(plan), [
    "cmd/relay",
    "internal/api", // 测试经 testutil 间接依赖 store
    "internal/server",
    "internal/store",
    "internal/testutil",
  ]);
});

test("测试 import 的包（及其依赖）变了，测试方也要查", () => {
  assert.ok(
    names(planGo(["go/internal/testutil/x.go"], PACKAGES)).includes(
      "internal/api",
    ),
  );
  // api 的测试经 testutil 间接依赖 obs。
  assert.ok(
    names(planGo(["go/internal/obs/log.go"], PACKAGES)).includes(
      "internal/api",
    ),
  );
});

test("叶子包只查自己", () => {
  assert.deepEqual(names(planGo(["go/cmd/relay/main.go"], PACKAGES)), [
    "cmd/relay",
  ]);
  assert.deepEqual(names(planGo(["go/internal/api/api_test.go"], PACKAGES)), [
    "internal/api",
  ]);
});

test("go:embed 的迁移与 testdata 归最近的上层包", () => {
  assert.deepEqual(
    names(planGo(["go/internal/obs/testdata/golden.json"], PACKAGES)).sort(),
    names(planGo(["go/internal/obs/log.go"], PACKAGES)).sort(),
  );
  assert.ok(
    names(
      planGo(["go/internal/store/migrations/0002_x.sql"], PACKAGES),
    ).includes("internal/store"),
  );
});

test("go/ 根下的 Dockerfile 不归任何包，不触发", () => {
  assert.deepEqual(planGo(["go/Dockerfile.cloud"], PACKAGES), {
    full: false,
    packages: [],
  });
});

test(".go 文件落不进任何包（删了整个包）：全量", () => {
  assert.equal(planGo(["go/internal/gone/gone.go"], PACKAGES).full, true);
});

test("模块依赖、冻结夹具、测试按路径读的仓库文件、本脚本变了：全量", () => {
  for (const file of [
    "go/go.mod",
    "go/go.sum",
    "go/testdata/compat/state.json",
    "packages/platform-protocol/schema/errors.json",
    "packages/platform-protocol/fixtures/tunnel/open.json",
    "packages/platform-protocol/test/format-vectors.json",
    "packages/platform-protocol/src/identity-vectors/vectors.json",
    "deploy/README.md",
    "scripts/go-check.mjs",
  ]) {
    assert.equal(planGo([file], PACKAGES).full, true, file);
  }
});

test("拿不到基点：全量", () => {
  assert.equal(planGo(null, PACKAGES).full, true);
});

test("CI（缺省）全量并带 -count=1；本机保留测试缓存、只查挑出的包", () => {
  const ci = Object.fromEntries(
    goSteps().map((step) => [step.name, step.args]),
  );
  assert.deepEqual(ci.test, [
    "test",
    "-race",
    "-count=1",
    `-timeout=${TEST_TIMEOUT}`,
    "./...",
  ]);
  assert.deepEqual(ci.vet, ["vet", "./..."]);

  const local = Object.fromEntries(
    goSteps({
      scope: { full: false, packages: ["p/a", "p/b"] },
      local: true,
    }).map((step) => [step.name, step.args]),
  );
  assert.deepEqual(local.test, [
    "test",
    "-race",
    `-timeout=${TEST_TIMEOUT}`,
    "p/a",
    "p/b",
  ]);
  assert.deepEqual(local.staticcheck.slice(-2), ["p/a", "p/b"]);
});

test("没有受影响的包：vet、staticcheck、test 跳过，fmt 与 generate 照跑", async () => {
  const steps = goSteps({ scope: { full: false, packages: [] }, local: true });
  assert.deepEqual(
    steps.map((step) => step.name),
    ["fmt", "vet", "staticcheck", "test", "generate"],
  );
  for (const step of steps.filter((one) =>
    ["vet", "staticcheck", "test"].includes(one.name),
  )) {
    assert.equal(step.args, null);
    assert.deepEqual((await step.run()).problems, []);
  }
});

test("vet、staticcheck、test 同组并行，generate 最后单独跑", () => {
  const group = Object.fromEntries(
    goSteps().map((step) => [step.name, step.group]),
  );
  assert.equal(group.vet, group.test);
  assert.equal(group.staticcheck, group.test);
  assert.ok(group.generate > group.test);
  assert.ok(group.fmt < group.test);
});

test("生成物快照：改了、多了、少了都报", () => {
  const before = new Map([
    ["gen/a.go", Buffer.from("a")],
    ["gen/b.go", Buffer.from("b")],
  ]);
  assert.deepEqual(snapshotDiff(before, new Map(before)), []);
  const after = new Map([
    ["gen/a.go", Buffer.from("a2")],
    ["gen/c.go", Buffer.from("c")],
  ]);
  const problems = snapshotDiff(before, after);
  assert.equal(problems.length, 3);
  assert.ok(problems.some((p) => p.includes("gen/a.go")));
  assert.ok(problems.some((p) => p.includes("gen/b.go")));
  assert.ok(problems.some((p) => p.includes("gen/c.go")));
});
