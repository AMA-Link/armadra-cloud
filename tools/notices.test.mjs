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
import {
  detectLicense,
  goPackagesFrom,
  packagesFrom,
  parseJsonStream,
  render,
  withResolvedPeers,
} from "./notices.mjs";

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

test("go list 的 JSON 流：对象首尾相接，字符串里的花括号与转义不干扰", () => {
  const text = `{
	"ImportPath": "a",
	"Module": {"Path": "x.test/m", "Version": "v1.0.0", "Dir": "/cache/m"}
}
{"ImportPath": "b{", "Standard": true}
{"ImportPath": "c\\"}", "Module": {"Path": "y.test/n/v6", "Version": "v6.0.3", "Dir": "/cache/n"}}`;
  assert.deepEqual(
    parseJsonStream(text).map((entry) => entry.ImportPath),
    ["a", "b{", 'c"}'],
  );
});

test("Go 模块：去掉标准库与主模块，同一模块只记一次，按正文认许可证", () => {
  const texts = { "/cache/m": "MIT", "/cache/n": "BSD", "/cache/x": "" };
  const output = [
    { ImportPath: "fmt", Standard: true },
    {
      ImportPath: "me/go",
      Module: { Path: "me/go", Version: "", Main: true, Dir: "/src" },
    },
    {
      ImportPath: "x.test/m/a",
      Module: { Path: "x.test/m", Version: "v1.2.3", Dir: "/cache/m" },
    },
    {
      ImportPath: "x.test/m/b",
      Module: { Path: "x.test/m", Version: "v1.2.3", Dir: "/cache/m" },
    },
    {
      ImportPath: "y.test/n/v6",
      Module: { Path: "y.test/n/v6", Version: "v6.0.3", Dir: "/cache/n" },
    },
  ]
    .map((entry) => JSON.stringify(entry))
    .join("\n");
  const rows = goPackagesFrom(output, (dir) => [texts[dir]]);
  assert.deepEqual(
    rows.map((row) => [row.name, row.version, row.homepage, row.path]),
    [
      ["x.test/m", "v1.2.3", "https://x.test/m", "/cache/m"],
      ["y.test/n/v6", "v6.0.3", "https://y.test/n", "/cache/n"],
    ],
  );
  assert.deepEqual(
    rows.map((row) => row.license),
    ["见许可证文本", "见许可证文本"],
  );
});

test("按 LICENSE 正文认 SPDX 标识", () => {
  const bsd =
    "Redistribution and use in source and binary forms, with or without\nmodification, are permitted provided that:\n * Neither the name of Google";
  assert.equal(detectLicense(bsd), "BSD-3-Clause");
  assert.equal(
    detectLicense(bsd.replace("Neither the name of", "Nothing about")),
    "BSD-2-Clause",
  );
  assert.equal(
    detectLicense(
      "MIT License\n\nPermission is hereby granted,\nfree of charge, to any",
    ),
    "MIT",
  );
  assert.equal(
    detectLicense("Apache License\n  Version 2.0, January 2004"),
    "Apache-2.0",
  );
  assert.equal(detectLicense("all rights reserved"), "见许可证文本");
});
