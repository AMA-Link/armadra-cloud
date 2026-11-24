import assert from "node:assert/strict";
import { test } from "node:test";

import { selectStaticSteps } from "./check-ci.mjs";
import { STATICS } from "./verify.mjs";

test("CI 缺省包含全部静态检查与 repo:test", () => {
  const steps = selectStaticSteps([]);
  assert.deepEqual(steps, STATICS);
  assert.ok(steps.includes("repo:test"));
  assert.ok(steps.includes("notices:check"));
  assert.ok(steps.includes("protocol:check"));
});

test("--no-repo-test 保留其余全部静态检查（供根 pnpm check 复用）", () => {
  const steps = selectStaticSteps(["--no-repo-test"]);
  assert.equal(steps.includes("repo:test"), false);
  assert.deepEqual([...steps, "repo:test"], STATICS);
});
