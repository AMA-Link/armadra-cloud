import { strict as assert } from "node:assert";
import { test } from "node:test";
import { parseYaml } from "./workflow-yaml.mjs";
import {
  checkWorkflow,
  checkWorkflowDirectory,
  isPinnedAction,
} from "./validate-workflows.mjs";

const SHA = "11d5960a326750d5838078e36cf38b85af677262";

function check(yaml) {
  return checkWorkflow("test.yml", parseYaml(yaml));
}

const GOOD = `name: t
on:
  push:
permissions:
  contents: read
jobs:
  a:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@${SHA}
      - run: echo hi
`;

test("本仓的工作流全部通过", () => {
  const { files, problems } = checkWorkflowDirectory();
  assert.deepEqual(problems, []);
  assert.ok(files.includes("ci.yml"));
});

test("合格的工作流没有问题", () => {
  assert.deepEqual(check(GOOD), []);
});

test("action 必须钉 SHA", () => {
  assert.equal(isPinnedAction(`actions/checkout@${SHA}`), true);
  assert.equal(isPinnedAction("actions/checkout@v4"), false);
  assert.equal(isPinnedAction("./.github/actions/x"), true);
  const problems = check(GOOD.replace(`@${SHA}`, "@v4"));
  assert.deepEqual(problems, [
    "test.yml: job a step 1 uses actions/checkout@v4 without pinning a commit SHA",
  ]);
});

test("缺顶层 permissions 与 write-all 都被拦下", () => {
  const missing = GOOD.replace("permissions:\n  contents: read\n", "");
  assert.deepEqual(check(missing), [
    "test.yml: declares no top-level permissions",
  ]);
  const writeAll = GOOD.replace(
    "permissions:\n  contents: read\n",
    "permissions: write-all\n",
  );
  assert.deepEqual(check(writeAll), ["test.yml: grants write-all permissions"]);
});

test("作业必须写 timeout-minutes", () => {
  assert.deepEqual(check(GOOD.replace("    timeout-minutes: 5\n", "")), [
    "test.yml: job a has no timeout-minutes",
  ]);
});

test("needs 指向不存在的作业与成环", () => {
  const yaml = `${GOOD}  b:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    needs: [c]
    steps:
      - run: echo
  c:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    needs: [b, nope]
    steps:
      - run: echo
`;
  const problems = check(yaml);
  assert.ok(problems.some((p) => p.includes("needs nope")));
  assert.ok(problems.some((p) => p.includes("cycle")));
});
