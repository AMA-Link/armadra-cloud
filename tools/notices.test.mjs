import { strict as assert } from "node:assert";
import { test } from "node:test";
import { packagesFrom, render } from "./notices.mjs";

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
