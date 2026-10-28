// node --test scripts/e2e-libs.test.mjs
import assert from "node:assert/strict";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { ensureBuiltLibs, staleReason } from "./e2e/libs.mjs";

const lib = {
  name: "@armadra/x",
  dir: "pkg",
  inputs: ["src", "package.json"],
  output: "dist",
};

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "e2e-libs-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "pkg/src"), { recursive: true });
  writeFileSync(join(root, "pkg/package.json"), "{}");
  writeFileSync(join(root, "pkg/src/index.ts"), "export const a = 1;");
  writeFileSync(join(root, "pkg/src/index.test.ts"), "");
  return root;
}

function at(path, seconds) {
  utimesSync(path, seconds, seconds);
}

function writeDist(root, seconds) {
  mkdirSync(join(root, "pkg/dist/sub"), { recursive: true });
  for (const file of ["index.js", "sub/index.js"]) {
    writeFileSync(join(root, "pkg/dist", file), "");
    at(join(root, "pkg/dist", file), seconds);
  }
}

function inputsAt(root, seconds) {
  for (const file of ["package.json", "src/index.ts", "src/index.test.ts"])
    at(join(root, "pkg", file), seconds);
}

test("没有 dist 算不新鲜", (t) => {
  const root = fixture(t);
  assert.match(staleReason(root, lib), /没有 pkg\/dist/);
});

test("产物都比源码新算新鲜，测试文件不算输入", (t) => {
  const root = fixture(t);
  inputsAt(root, 1000);
  at(join(root, "pkg/src/index.test.ts"), 5000);
  writeDist(root, 2000);
  assert.equal(staleReason(root, lib), null);
});

test("源码比最旧的产物新算不新鲜，并指出文件", (t) => {
  const root = fixture(t);
  inputsAt(root, 1000);
  writeDist(root, 2000);
  at(join(root, "pkg/dist/sub/index.js"), 500);
  assert.match(staleReason(root, lib), /源码比产物新：pkg\/src\/index\.ts/);
});

test("旧了就构建，新鲜了不再构建", (t) => {
  const root = fixture(t);
  inputsAt(root, 1000);
  const built = [];
  const logs = [];
  ensureBuiltLibs({
    root,
    libs: [lib],
    log: (line) => logs.push(line),
    build: (target) => {
      built.push(target.name);
      writeDist(root, 2000);
      return true;
    },
  });
  assert.deepEqual(built, ["@armadra/x"]);
  assert.match(logs[0], /构建 @armadra\/x（没有 pkg\/dist）/);
  ensureBuiltLibs({
    root,
    libs: [lib],
    build: () => assert.fail("不该再构建"),
  });
});

test("构建失败或构建后仍旧时报错", (t) => {
  const root = fixture(t);
  inputsAt(root, 1000);
  assert.throws(
    () => ensureBuiltLibs({ root, libs: [lib], build: () => false }),
    /构建 @armadra\/x 失败/,
  );
  assert.throws(
    () =>
      ensureBuiltLibs({
        root,
        libs: [lib],
        build: () => {
          writeDist(root, 500);
          return true;
        },
      }),
    /构建后仍不新鲜：源码比产物新/,
  );
});

test("e2e.mjs 在用协议包之前先查产物（#143）", () => {
  const text = readFileSync(new URL("./e2e.mjs", import.meta.url), "utf8");
  assert.doesNotMatch(text, /^import .* from "@armadra\/platform-protocol/m);
  const ensure = text.indexOf("ensureBuiltLibs({");
  assert.ok(ensure > 0);
  for (const target of [
    '"@armadra/platform-protocol/e2e"',
    '"./e2e/saas-nodes.mjs"',
    '"./e2e/saas.mjs"',
    '"./e2e/workers.mjs"',
  ])
    assert.ok(text.indexOf(target) > ensure, target);
});
