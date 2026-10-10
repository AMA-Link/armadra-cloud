import { strict as assert } from "node:assert";
import { test } from "node:test";
import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { packagesFrom, render, withResolvedPeers } from "./notices.mjs";

test("按名字与版本排序，跳过工作区自己的包", () => {
  const rows = packagesFrom({
    MIT: [
      { name: "zod", versions: ["4.4.3"], paths: ["/z"], license: "MIT" },
      { name: "@armadra/cloud-shared", versions: ["0.1.0"], paths: ["/a"] },
      { name: "postgres", versions: ["3.4.9", "3.4.8"], paths: ["/p2", "/p1"] },
    ],
  });
  assert.deepEqual(
    rows.map((row) => `${row.name}@${row.version}:${row.path}`),
    ["postgres@3.4.8:/p1", "postgres@3.4.9:/p2", "zod@4.4.3:/z"],
  );
});

test("输出不含绝对路径，许可证正文原样进代码块", () => {
  const output = render(
    [
      {
        name: "x",
        version: "1.0.0",
        license: "MIT",
        homepage: "",
        path: "/abs/x",
      },
    ],
    () => ["Copyright (c) X\r\nPermission..."],
  );
  assert.ok(!output.includes("/abs/x"));
  assert.ok(output.includes("```text\nCopyright (c) X\r\nPermission...\n```"));
  assert.ok(output.endsWith("```\n"));
});

test("pnpm 12 漏列的已解析 peer 与可选依赖连同它们的依赖被补回", () => {
  const dir = mkdtempSync(join(tmpdir(), "cloud-notices-peers-"));
  const install = (store, name, manifest) => {
    const path = join(dir, ".pnpm", store, "node_modules", name);
    mkdirSync(path, { recursive: true });
    writeFileSync(
      join(path, "package.json"),
      JSON.stringify({ name, ...manifest }),
    );
    return path;
  };
  try {
    const debug = install("debug@4.4.3_supports-color@7.2.0", "debug", {
      version: "4.4.3",
      peerDependenciesMeta: { "supports-color": { optional: true } },
    });
    const color = install("supports-color@7.2.0", "supports-color", {
      version: "7.2.0",
      license: "MIT",
      dependencies: { "has-flag": "^4.0.0" },
    });
    symlinkSync(
      color,
      join(
        dir,
        ".pnpm",
        "debug@4.4.3_supports-color@7.2.0",
        "node_modules",
        "supports-color",
      ),
    );
    const flag = install("has-flag@4.0.0", "has-flag", {
      version: "4.0.0",
      license: "MIT",
    });
    symlinkSync(
      flag,
      join(dir, ".pnpm", "supports-color@7.2.0", "node_modules", "has-flag"),
    );
    const rows = packagesFrom(
      withResolvedPeers({
        MIT: [
          {
            name: "debug",
            versions: ["4.4.3"],
            paths: [debug],
            license: "MIT",
          },
        ],
      }),
    );
    assert.deepEqual(
      rows.map((row) => `${row.name}@${row.version} ${row.license}`),
      ["debug@4.4.3 MIT", "has-flag@4.0.0 MIT", "supports-color@7.2.0 MIT"],
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
