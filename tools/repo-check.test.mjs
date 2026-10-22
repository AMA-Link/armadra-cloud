// node --test tools/repo-check.test.mjs
//
// 每条规则各验证一次「通过」与一次「被拦下」；文件树写在临时目录里，
// 跟踪列表直接传入，避免测试依赖 git 仓库状态。
import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import test from "node:test";
import assert from "node:assert/strict";

import {
  RULES,
  checkRepository,
  goImports,
  parseGoMod,
  trackedFiles,
} from "./repo-check.mjs";

const roots = [];

function repo(tree) {
  const root = mkdtempSync(join(tmpdir(), "repo-check-"));
  roots.push(root);
  for (const [path, content] of Object.entries(tree)) {
    const absolute = join(root, path);
    mkdirSync(dirname(absolute), { recursive: true });
    writeFileSync(absolute, content);
  }
  return root;
}

function run(root, rules, only, files) {
  return checkRepository({
    root,
    rules,
    only,
    files: files ?? Object.keys(rules.$files ?? {}),
  });
}

function sha256(text) {
  return createHash("sha256").update(text).digest("hex");
}

test.after(() => {
  for (const root of roots) rmSync(root, { recursive: true, force: true });
});

test("导出的规则名与实现一一对应", () => {
  assert.deepEqual([...RULES].sort(), [
    "blacklist",
    "docs-index",
    "file-size",
    "go-modules",
    "links",
    "migrations",
    "naming",
    "package-dependencies",
    "pinned-versions",
    "root-allowlist",
  ]);
  assert.throws(
    () => checkRepository({ root: ".", rules: {}, files: [], only: ["nope"] }),
    /未知规则/,
  );
});

test("docs-index：未登记的文档被拦下，history/ 只需登记目录", () => {
  const index =
    "# 索引\n\n[指南](guides/development.md)\n\n`history/` 保存旧文档。\n";
  const root = repo({ "docs/README.md": index });
  const rules = {
    docs: { root: "docs", index: "docs/README.md", directoryOnly: ["history"] },
  };
  const files = [
    "docs/README.md",
    "docs/guides/development.md",
    "docs/history/old.md",
  ];
  assert.deepEqual(run(root, rules, ["docs-index"], files), []);
  assert.deepEqual(
    run(root, rules, ["docs-index"], [...files, "docs/design/new.md"]),
    ["文档未在 docs/README.md 登记：docs/design/new.md"],
  );
});

test("docs-index：有 history/ 文件但索引没提到目录时报告", () => {
  const root = repo({ "docs/README.md": "# 索引\n" });
  const problems = run(
    root,
    {
      docs: {
        root: "docs",
        index: "docs/README.md",
        directoryOnly: ["history"],
      },
    },
    ["docs-index"],
    ["docs/README.md", "docs/history/old.md"],
  );
  assert.deepEqual(problems, ["目录未在 docs/README.md 登记：docs/history/"]);
});

test("links：相对链接必须可解析，外链与锚点跳过", () => {
  const root = repo({
    "docs/a.md":
      '# A\n\n[近邻](./b.md#锚)、[上级](../README.md)、[外链](https://example.com)\n<img src="img.png" />\n',
    "docs/b.md": "# B\n",
    "docs/img.png": "",
    "README.md": "# 根\n",
    "docs/c.md": "[断链](missing.md)\n",
  });
  const rules = { links: { include: ["docs/", "*.md"] } };
  const files = ["docs/a.md", "docs/b.md", "README.md"];
  assert.deepEqual(run(root, rules, ["links"], files), []);
  assert.deepEqual(run(root, rules, ["links"], [...files, "docs/c.md"]), [
    "链接无法解析：docs/c.md → missing.md",
  ]);
  assert.deepEqual(
    run(
      root,
      { links: { include: ["docs/"], exclude: ["docs/"] } },
      ["links"],
      [...files, "docs/c.md"],
    ),
    [],
  );
});

test("blacklist：黑名单文件一旦被跟踪就报告", () => {
  const root = repo({ "README.md": "" });
  const rules = { blacklist: ["output/", "*.db", "target/"] };
  assert.deepEqual(
    run(root, rules, ["blacklist"], ["README.md", "apps/x.rs"]),
    [],
  );
  assert.deepEqual(
    run(
      root,
      rules,
      ["blacklist"],
      ["output/report.json", "apps/canvas.db", "apps/desktop/target/x"],
    ),
    [
      "禁止跟踪的文件：output/report.json（output/）",
      "禁止跟踪的文件：apps/canvas.db（*.db）",
      "禁止跟踪的文件：apps/desktop/target/x（target/）",
    ],
  );
});

test("naming：目录名必须等于包名去掉前缀，豁免到期后报告", () => {
  const root = repo({
    "packages/shared/package.json": '{ "name": "@armadra/shared" }',
    "packages/shared-ts/package.json": '{ "name": "@armadra/extras" }',
    "apps/shell/package.json": '{ "name": "@armadra/desktop" }',
  });
  const base = [
    { directory: "packages", manifest: "package.json", prefix: "@armadra/" },
    { directory: "apps", manifest: "package.json", prefix: "@armadra/" },
  ];
  assert.deepEqual(run(root, { naming: base }, ["naming"], []), [
    "目录名与包名不一致：packages/shared-ts 应为 packages/extras（@armadra/extras）",
    "目录名与包名不一致：apps/shell 应为 apps/desktop（@armadra/desktop）",
  ]);
  const exempted = structuredClone(base);
  exempted[0].exemptions = [
    { directory: "packages/shared-ts", note: "待改名" },
  ];
  exempted[1].exemptions = [{ directory: "apps/shell", note: "待改名" }];
  assert.deepEqual(run(root, { naming: exempted }, ["naming"], []), []);
  const stale = structuredClone(exempted);
  stale[1].exemptions = [{ directory: "apps/desktop", note: "已改完" }];
  assert.deepEqual(run(root, { naming: stale }, ["naming"], []), [
    "目录名与包名不一致：apps/shell 应为 apps/desktop（@armadra/desktop）",
    "改名豁免已过期，可从 repo.rules.json 移除：apps/desktop",
  ]);
});

test("root-allowlist：根目录条目必须在白名单里", () => {
  const root = repo({ "README.md": "" });
  const rules = { root: ["README.md", "apps", "docs"] };
  assert.deepEqual(
    run(root, rules, ["root-allowlist"], ["README.md", "apps/web/main.ts"]),
    [],
  );
  assert.deepEqual(
    run(
      root,
      rules,
      ["root-allowlist"],
      ["README.md", "output/x", "notes.txt"],
    ),
    ["根目录不在白名单：notes.txt", "根目录不在白名单：output"],
  );
});

test("file-size：超限文件必须登记豁免，且只允许变小", () => {
  const root = repo({
    "apps/small.rs": "a\n".repeat(3),
    "apps/big.rs": "a\n".repeat(10),
    "apps/gen/huge.rs": "a\n".repeat(99),
  });
  const rule = {
    max: 5,
    roots: ["apps"],
    extensions: [".rs"],
    excludeSegments: ["/gen/"],
  };
  const files = ["apps/small.rs", "apps/big.rs", "apps/gen/huge.rs"];
  assert.deepEqual(run(root, { fileSize: rule }, ["file-size"], files), [
    "源码单文件超过 5 行：apps/big.rs（10 行）",
  ]);
  assert.deepEqual(
    run(
      root,
      {
        fileSize: { ...rule, exemptions: [{ path: "apps/big.rs", lines: 10 }] },
      },
      ["file-size"],
      files,
    ),
    [],
  );
  assert.deepEqual(
    run(
      root,
      {
        fileSize: { ...rule, exemptions: [{ path: "apps/big.rs", lines: 8 }] },
      },
      ["file-size"],
      files,
    ),
    ["豁免文件继续增长：apps/big.rs（10 行 > 登记的 8 行）"],
  );
  assert.deepEqual(
    run(
      root,
      {
        fileSize: {
          ...rule,
          exemptions: [
            { path: "apps/big.rs", lines: 10 },
            { path: "apps/small.rs", lines: 3 },
            { path: "apps/gone.rs", lines: 9 },
          ],
        },
      },
      ["file-size"],
      files,
    ),
    [
      "豁免已过期，可从 repo.rules.json 移除：apps/small.rs（3 行）",
      "豁免登记的文件不存在：apps/gone.rs",
    ],
  );
});

test("migrations：编号连续、已发布文件 sha256 不变", () => {
  const first = "create table a(id);\n";
  const second = "create table b(id);\n";
  const root = repo({
    "db/0001_a.sql": first,
    "db/0002_b.sql": second,
    "migrations.lock": JSON.stringify({
      db: { "0001_a.sql": sha256(first), "0002_b.sql": sha256(second) },
    }),
  });
  const rules = {
    migrations: {
      lock: "migrations.lock",
      sources: [{ path: "db", kind: "directory" }],
    },
  };
  assert.deepEqual(run(root, rules, ["migrations"], []), []);

  const drifted = repo({
    "db/0001_a.sql": first,
    "db/0003_c.sql": second,
    "migrations.lock": JSON.stringify({
      db: { "0001_a.sql": sha256("已改动\n") },
    }),
  });
  const problems = run(drifted, rules, ["migrations"], []);
  assert.deepEqual(problems, [
    "迁移编号不连续：db 第 2 个为 3",
    "已发布迁移被改动：db/0001_a.sql",
    `新迁移未登记到 migrations.lock：db/0003_c.sql（sha256 ${sha256(second)}）`,
  ]);
});

test("migrations：副本（Go embed）与 lock 逐字节一致、文件集合一样", () => {
  const first = "create table a(id);\n";
  const second = "create table b(id);\n";
  const lock = JSON.stringify({
    db: { "0001_a.sql": sha256(first), "0002_b.sql": sha256(second) },
  });
  const rules = {
    migrations: {
      lock: "migrations.lock",
      sources: [{ path: "db", kind: "directory" }],
      copies: [{ from: "db", to: "go/migrations" }],
    },
  };
  const good = repo({
    "db/0001_a.sql": first,
    "db/0002_b.sql": second,
    "go/migrations/0001_a.sql": first,
    "go/migrations/0002_b.sql": second,
    "migrations.lock": lock,
  });
  assert.deepEqual(run(good, rules, ["migrations"], []), []);

  const bad = repo({
    "db/0001_a.sql": first,
    "db/0002_b.sql": second,
    "go/migrations/0001_a.sql": "create table a(id); -- 改过\n",
    "go/migrations/0003_c.sql": second,
    "migrations.lock": lock,
  });
  assert.deepEqual(run(bad, rules, ["migrations"], []), [
    "迁移副本与 db/0001_a.sql 不一致：go/migrations/0001_a.sql（运行 go generate）",
    "迁移副本缺文件：go/migrations/0002_b.sql（运行 go generate）",
    "迁移副本多出文件：go/migrations/0003_c.sql",
  ]);

  const missing = repo({
    "db/0001_a.sql": first,
    "db/0002_b.sql": second,
    "migrations.lock": lock,
  });
  assert.deepEqual(run(missing, rules, ["migrations"], []), [
    "迁移副本目录缺失：go/migrations",
  ]);
});

test("trackedFiles：读得到本仓库自己的跟踪列表", () => {
  const files = trackedFiles(fileURLToPath(new URL("../", import.meta.url)));
  assert.ok(files.includes("package.json"));
  assert.ok(files.every((path) => !path.startsWith("/")));
});

test("pinned-versions：一个都没有时通过，精确且同版本时通过", () => {
  const rules = { pinnedVersions: [{ packages: "@orpc/*" }] };
  const empty = repo({
    "package.json": JSON.stringify({ devDependencies: { vitest: "^4.1.0" } }),
    "pnpm-lock.yaml":
      "lockfileVersion: '9.0'\n\npackages:\n\n  vitest@4.1.11:\n    resolution: {}\n",
  });
  assert.deepEqual(
    run(empty, rules, ["pinned-versions"], ["package.json", "pnpm-lock.yaml"]),
    [],
  );
  const pinned = repo({
    "packages/shared/package.json": JSON.stringify({
      dependencies: { "@orpc/contract": "1.15.4", "@orpc/zod": "1.15.4" },
    }),
    "apps/web/package.json": JSON.stringify({
      dependencies: { "@orpc/client": "1.15.4" },
    }),
    "pnpm-lock.yaml": [
      "packages:",
      "",
      "  '@orpc/client@1.15.4':",
      "    resolution: {}",
      "",
      "  '@orpc/shared@1.15.4':",
      "    resolution: {}",
      "",
      "snapshots:",
      "",
      "  '@orpc/client@1.15.4':",
      "",
    ].join("\n"),
  });
  assert.deepEqual(
    run(
      pinned,
      rules,
      ["pinned-versions"],
      [
        "packages/shared/package.json",
        "apps/web/package.json",
        "pnpm-lock.yaml",
      ],
    ),
    [],
  );
});

test("pinned-versions：范围版本、beta 与版本不一致都被拦下", () => {
  const rules = { pinnedVersions: [{ packages: "@orpc/*" }] };
  const root = repo({
    "apps/web/package.json": JSON.stringify({
      dependencies: { "@orpc/client": "^1.15.4", "@orpc-like/x": "^1.0.0" },
    }),
    "apps/desktop/package.json": JSON.stringify({
      dependencies: { "@orpc/server": "2.0.0-beta.42" },
    }),
    "packages/shared/package.json": JSON.stringify({
      dependencies: { "@orpc/contract": "1.15.4" },
    }),
    "pnpm-lock.yaml":
      "packages:\n\n  '@orpc/shared@1.15.3':\n    resolution: {}\n",
  });
  const problems = run(
    root,
    rules,
    ["pinned-versions"],
    [
      "apps/web/package.json",
      "apps/desktop/package.json",
      "packages/shared/package.json",
      "pnpm-lock.yaml",
    ],
  );
  assert.deepEqual(problems.slice(0, 2), [
    '@orpc/* 必须写精确版本：apps/web/package.json dependencies.@orpc/client = "^1.15.4"',
    '@orpc/* 不得用预发布版本：apps/desktop/package.json dependencies.@orpc/server = "2.0.0-beta.42"',
  ]);
  assert.equal(problems.length, 3);
  assert.match(
    problems[2],
    /^@orpc\/\* 版本不一致（1\.15\.3 \/ 1\.15\.4 \/ 2\.0\.0-beta\.42）/,
  );
  assert.match(problems[2], /pnpm-lock\.yaml @orpc\/shared@1\.15\.3/);
});

test("pinned-versions：本仓库的规则在现状上通过", () => {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const rules = JSON.parse(readFileSync(join(root, "repo.rules.json"), "utf8"));
  assert.ok(rules.pinnedVersions.some((rule) => rule.packages === "@orpc/*"));
  assert.deepEqual(
    checkRepository({ root, rules, only: ["pinned-versions"] }),
    [],
  );
});

test("package-dependencies：清单里出现被禁止的依赖（名字或 scope 简写）就报告", () => {
  const rules = {
    packageDependencies: [
      { package: "packages/kernel", forbid: ["redis", "@armadra/core-*"] },
      { package: "packages/missing", forbid: ["x"] },
    ],
  };
  const root = repo({
    "packages/kernel/package.json": JSON.stringify({
      dependencies: { ws: "8.21.0", redis: "5.12.1" },
      devDependencies: { "@armadra/core-x": "workspace:*" },
    }),
  });
  assert.deepEqual(
    run(
      root,
      rules,
      ["package-dependencies"],
      ["packages/kernel/package.json"],
    ),
    [
      "packages/kernel 不得依赖 redis（dependencies，规则 redis）",
      "packages/kernel 不得依赖 @armadra/core-x（devDependencies，规则 @armadra/core-*）",
      "包边界规则指向的清单不存在：packages/missing/package.json",
    ],
  );
  assert.deepEqual(run(root, {}, ["package-dependencies"], []), []);
});

test("package-dependencies：本仓库的规则在现状上通过", () => {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const rules = JSON.parse(readFileSync(join(root, "repo.rules.json"), "utf8"));
  assert.ok(
    rules.packageDependencies.some(
      (rule) => rule.package === "packages/relay-kernel",
    ),
  );
  assert.deepEqual(
    checkRepository({ root, rules, only: ["package-dependencies"] }),
    [],
  );
});

const GO_MOD = `module example.test/m

go 1.27.1

require (
	golang.org/x/crypto v0.58.0
	github.com/lib/test/v6 v6.0.3 // indirect
)

require golang.org/x/text v0.43.0
`;

const goRule = {
  root: "go",
  allow: ["golang.org/x/crypto", "github.com/lib/test/v6", "golang.org/x/text"],
  testOnly: ["github.com/lib/test/v6"],
  internalBoundaries: [
    { from: "internal/core", forbid: ["internal/tunnel", "internal/edge"] },
  ],
};

test("go-modules：解析 go.mod 的 require 块、单行 require 与 replace", () => {
  const parsed = parseGoMod(
    `${GO_MOD}\nreplace a => ../b\nexclude (\n\tx v1.0.0\n)\n`,
  );
  assert.equal(parsed.module, "example.test/m");
  assert.deepEqual(parsed.modules, [
    { path: "golang.org/x/crypto", version: "v0.58.0" },
    { path: "github.com/lib/test/v6", version: "v6.0.3" },
    { path: "golang.org/x/text", version: "v0.43.0" },
  ]);
  assert.deepEqual(parsed.directives, [
    "replace a => ../b",
    "exclude x v1.0.0",
  ]);
});

test("go-modules：只取真正的 import，注释与字符串里的不算", () => {
  const source = `package x

// import "fake.test/comment"
import (
	"fmt"
	_ "embed"
	alias "golang.org/x/crypto/scrypt"
	/* "fake.test/block" */
)

import "os"

var s = "import \\"fake.test/string\\""
`;
  assert.deepEqual(goImports(source).sort(), [
    "embed",
    "fmt",
    "golang.org/x/crypto/scrypt",
    "os",
  ]);
});

test("go-modules：登记过的模块与标准库、本模块通过", () => {
  const root = repo({
    "go/go.mod": GO_MOD,
    "go/a/a.go": `package a

import (
	"fmt"
	"example.test/m/internal/b"
	"golang.org/x/crypto/scrypt"
)
`,
    "go/a/a_test.go": `package a

import "github.com/lib/test/v6/sub"
`,
    "go/internal/testutil/t.go": `package testutil

import "github.com/lib/test/v6"
`,
    "go/internal/core/c.go": `package core

import "example.test/m/internal/store"
`,
  });
  assert.deepEqual(
    run(
      root,
      { goModules: [goRule] },
      ["go-modules"],
      [
        "go/go.mod",
        "go/a/a.go",
        "go/a/a_test.go",
        "go/internal/testutil/t.go",
        "go/internal/core/c.go",
      ],
    ),
    [],
  );
  assert.deepEqual(run(root, {}, ["go-modules"], []), []);
});

test("go-modules：未登记的模块、replace、测试专用模块进生产代码、越过内部边界都被拦下", () => {
  const root = repo({
    "go/go.mod": `${GO_MOD}
require github.com/other/dep v1.2.3
require github.com/loose/dep v1.2
replace golang.org/x/text => ../text
`,
    "go/a/a.go": `package a

import (
	"github.com/sneaky/pkg"
	"github.com/lib/test/v6"
)
`,
    "go/internal/core/c.go": `package core

import (
	"example.test/m/internal/tunnel"
	"example.test/m/internal/edge/sub"
	"example.test/m/internal/store"
)
`,
  });
  const files = ["go/go.mod", "go/a/a.go", "go/internal/core/c.go"];
  assert.deepEqual(run(root, { goModules: [goRule] }, ["go-modules"], files), [
    "go/go.mod 引入了未登记的模块：github.com/other/dep（先登记进 repo.rules.json 的 goModules）",
    "go/go.mod 引入了未登记的模块：github.com/loose/dep（先登记进 repo.rules.json 的 goModules）",
    "go/go.mod 的 github.com/loose/dep 必须写精确版本：v1.2",
    "go/go.mod 不许用 replace：replace golang.org/x/text => ../text",
    "go/a/a.go import 了未登记的模块：github.com/sneaky/pkg",
    "go/a/a.go 在生产代码里 import 了只许测试用的模块：github.com/lib/test/v6",
    "go/internal/core/c.go 不得 import example.test/m/internal/tunnel（internal/core 不碰 internal/tunnel）",
    "go/internal/core/c.go 不得 import example.test/m/internal/edge/sub（internal/core 不碰 internal/edge）",
  ]);
  const stale = repo({ "go/go.mod": "module example.test/m\n\ngo 1.27.1\n" });
  assert.deepEqual(
    run(
      stale,
      { goModules: [{ root: "go", allow: ["golang.org/x/crypto"] }] },
      ["go-modules"],
      ["go/go.mod"],
    ),
    ["goModules 登记了 go.mod 里没有的模块，可移除：golang.org/x/crypto"],
  );
  assert.deepEqual(
    run(
      stale,
      { goModules: [{ root: "nope", allow: [] }] },
      ["go-modules"],
      [],
    ),
    ["Go 模块规则指向的 go.mod 不存在：nope/go.mod"],
  );
});

test("go-modules：本仓库的规则在现状上通过", () => {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const rules = JSON.parse(readFileSync(join(root, "repo.rules.json"), "utf8"));
  assert.ok(rules.goModules.some((rule) => rule.root === "go"));
  assert.deepEqual(checkRepository({ root, rules, only: ["go-modules"] }), []);
});
