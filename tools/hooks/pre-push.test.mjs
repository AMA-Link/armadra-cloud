import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  classify,
  parsePushLines,
  parseSkip,
  planChecks,
  refsToCheck,
} from "./pre-push.mjs";

const SHA = "a".repeat(40);
const ZERO = "0".repeat(40);
const ids = (steps) => steps.map((step) => step.id);

describe("推送的 ref", () => {
  it("删除分支与 tag 不查", () => {
    const updates = parsePushLines(
      [
        `refs/heads/x ${SHA} refs/heads/x ${ZERO}`,
        `(delete) ${ZERO} refs/heads/gone ${SHA}`,
        `refs/tags/v1 ${SHA} refs/tags/v1 ${ZERO}`,
      ].join("\n"),
    );
    assert.deepEqual(
      refsToCheck(updates).map((update) => update.remoteRef),
      ["refs/heads/x"],
    );
  });

  it("ARMADRA_SKIP_PREPUSH：1 跳过全部，逗号列表跳过指定步骤", () => {
    assert.equal(parseSkip("1").all, true);
    assert.deepEqual([...parseSkip("go:test,lint").steps], ["go:test", "lint"]);
  });
});

describe("与 CI 的 changes 作业同一判定", () => {
  it("文档、契约、Go 与触发 Go 的 TS 文件", () => {
    assert.deepEqual(classify(["docs/a.md", "README.md"]), {
      code: false,
      go: false,
    });
    assert.deepEqual(classify(["docs/contracts/cloud-api.md"]), {
      code: true,
      go: false,
    });
    assert.deepEqual(classify(["go/internal/api/x.go"]), {
      code: true,
      go: true,
    });
    assert.deepEqual(
      classify(["packages/platform-protocol/schema/frames.json"]),
      { code: true, go: true },
    );
    assert.deepEqual(classify(["packages/platform-protocol/src/x.ts"]), {
      code: true,
      go: false,
    });
    assert.deepEqual(classify([".github/workflows/ci.yml"]), {
      code: true,
      go: true,
    });
  });
});

describe("挑检查", () => {
  it("只改文档：改动文件的 prettier 与 repo:check", () => {
    const plan = planChecks(["docs/a.md"], { base: SHA });
    assert.equal(plan.docsOnly, true);
    assert.deepEqual(ids(plan.statics), ["format", "repo:check"]);
  });

  it("改协议包源码：lint 只看改动文件，协议包单测按模块图挑，不跑 Go", () => {
    const file = "packages/platform-protocol/src/frames.ts";
    const plan = planChecks([file], { base: SHA });
    assert.equal(plan.go, false);
    assert.ok(
      plan.statics.find((step) => step.id === "lint").args.includes(file),
    );
    assert.ok(ids(plan.statics).includes("protocol:check"));
    assert.deepEqual(plan.vitest.args.slice(-3), [
      "--changed",
      SHA,
      "--passWithNoTests",
    ]);
  });

  it("只改 Go：不跑协议包单测，跑 Go；改 Dockerfile 给提示", () => {
    const plan = planChecks(["go/internal/api/x.go", "go/Dockerfile"], {
      base: SHA,
    });
    assert.equal(plan.go, true);
    assert.equal(plan.vitest, null);
    assert.ok(plan.hints.some((hint) => hint.includes("images:build")));
  });

  it("改 terraform 记下，留给钩子按本机有没有 terraform 决定", () => {
    const plan = planChecks(["deploy/cloudflare/terraform/dns.tf"], {
      base: SHA,
    });
    assert.equal(plan.terraform, true);
  });
});
