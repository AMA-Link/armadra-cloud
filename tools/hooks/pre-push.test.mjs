import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { parseYaml } from "../ci/workflow-yaml.mjs";
import {
  classify,
  cleanEnv,
  generatedDrift,
  parseStatus,
  parsePushLines,
  parseSkip,
  planChecks,
  refsToCheck,
  workingTreeReport,
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
  it("按路径精确拆出 code、go、images、terraform 四路开关", () => {
    assert.deepEqual(classify([]), {
      code: true,
      go: true,
      images: true,
      terraform: true,
    });
    assert.deepEqual(
      classify([
        "docs/a.md",
        "README.md",
        ".github/ISSUE_TEMPLATE/bug_report.yml",
      ]),
      {
        code: false,
        go: false,
        images: false,
        terraform: false,
      },
    );
    assert.deepEqual(classify(["docs/contracts/cloud-api.md"]), {
      code: true,
      go: false,
      images: false,
      terraform: false,
    });
    assert.deepEqual(classify(["THIRD_PARTY_NOTICES.md"]), {
      code: true,
      go: false,
      images: false,
      terraform: false,
    });
    assert.deepEqual(classify(["go/go.mod"]), {
      code: true,
      go: true,
      images: true,
      terraform: false,
    });
    assert.deepEqual(classify(["go/go.sum"]), {
      code: true,
      go: true,
      images: true,
      terraform: false,
    });
    assert.deepEqual(classify(["go/Dockerfile", "go/Dockerfile.cloud"]), {
      code: false,
      go: false,
      images: true,
      terraform: false,
    });
    assert.deepEqual(classify(["go/internal/api/x.go"]), {
      code: false,
      go: true,
      images: true,
      terraform: false,
    });
    assert.deepEqual(classify(["deploy/cloudflare/terraform/dns.tf"]), {
      code: false,
      go: false,
      images: false,
      terraform: true,
    });
    assert.deepEqual(classify(["deploy/README.md"]), {
      code: false,
      go: true,
      images: false,
      terraform: false,
    });
    assert.deepEqual(
      classify(["packages/platform-protocol/schema/frames.json"]),
      { code: true, go: true, images: false, terraform: false },
    );
    assert.deepEqual(classify(["scripts/go-check.mjs"]), {
      code: true,
      go: true,
      images: false,
      terraform: false,
    });
    assert.deepEqual(classify(["packages/platform-protocol/src/x.ts"]), {
      code: true,
      go: false,
      images: false,
      terraform: false,
    });
    for (const global of [
      ".github/workflows/ci.yml",
      "package.json",
      "pnpm-lock.yaml",
      "repo.rules.json",
    ]) {
      assert.deepEqual(
        classify([global]),
        { code: true, go: true, images: true, terraform: true },
        global,
      );
    }
  });

  it("ci.yml 的 changes 输出键与各作业门控同 classify 的四路一致", () => {
    const root = fileURLToPath(new URL("../../", import.meta.url));
    const ci = parseYaml(
      readFileSync(join(root, ".github/workflows/ci.yml"), "utf8"),
    );
    assert.deepEqual(Object.keys(ci.jobs.changes.outputs).sort(), [
      "code",
      "go",
      "images",
      "terraform",
    ]);
    assert.ok(String(ci.jobs.check.if).includes("outputs.code == 'true'"));
    assert.ok(String(ci.jobs.go.if).includes("outputs.go == 'true'"));
    assert.ok(String(ci.jobs.images.if).includes("outputs.images == 'true'"));
    assert.ok(
      String(ci.jobs.terraform.if).includes("outputs.terraform == 'true'"),
    );
  });
});

describe("工作区", () => {
  it("porcelain -z：首行前导空格与改名都解析对；未跟踪的 .go 算源码", () => {
    const entries = parseStatus(
      [
        " M go/a.go",
        "?? go/internal/x/new.go",
        "?? notes.txt",
        "R  b.ts",
        "a.ts",
        "",
      ].join("\0"),
    );
    assert.deepEqual(
      entries.map((entry) => entry.path),
      ["go/a.go", "go/internal/x/new.go", "notes.txt", "b.ts"],
    );
    assert.deepEqual(workingTreeReport(entries, new Set(["b.ts"])), {
      untrackedCode: ["go/internal/x/new.go"],
      untrackedOther: ["notes.txt"],
      outside: ["go/a.go"],
      inside: ["b.ts"],
    });
  });

  it("go generate 之后与 HEAD 的差异与未跟踪文件都算", () => {
    assert.deepEqual(
      generatedDrift("go/internal/protocol/gen/a.go\n", "go/x.go\n"),
      ["go/internal/protocol/gen/a.go", "go/x.go"],
    );
    assert.deepEqual(generatedDrift("", ""), []);
  });

  it("子进程环境去掉全部 GIT_*", () => {
    assert.deepEqual(cleanEnv({ GIT_DIR: "/x", HOME: "/h" }), { HOME: "/h" });
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

  it("只改 Go：跳过 TS 构建与单测，跑 Go 与 repo:check；改 Dockerfile 给提示", () => {
    const plan = planChecks(["go/internal/api/x.go", "go/Dockerfile"], {
      base: SHA,
    });
    assert.equal(plan.docsOnly, false);
    assert.equal(plan.go, true);
    assert.equal(plan.images, true);
    assert.equal(plan.libs, null);
    assert.equal(plan.vitest, null);
    assert.deepEqual(ids(plan.statics), ["format", "repo:check"]);
    assert.ok(plan.hints.some((hint) => hint.includes("images:build")));

    const dockerOnly = planChecks(["go/Dockerfile.cloud"], { base: SHA });
    assert.equal(dockerOnly.docsOnly, false);
    assert.equal(dockerOnly.go, false);
    assert.equal(dockerOnly.images, true);
    assert.equal(dockerOnly.libs, null);
    assert.ok(dockerOnly.hints.some((hint) => hint.includes("images:build")));
  });

  it("改 terraform 记下，跳过 TS 与 Go，留给钩子按本机有没有 terraform 决定", () => {
    const plan = planChecks(["deploy/cloudflare/terraform/dns.tf"], {
      base: SHA,
    });
    assert.equal(plan.docsOnly, false);
    assert.equal(plan.go, false);
    assert.equal(plan.libs, null);
    assert.equal(plan.terraform, true);
  });
});
