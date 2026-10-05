import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";

const CLI = fileURLToPath(new URL("./cli.ts", import.meta.url));

function run(...args: string[]) {
  return spawnSync(process.execPath, [CLI, ...args], {
    encoding: "utf8",
    env: {},
  });
}

it("personal --help 打印用法并以 0 退出", () => {
  const result = run("personal", "--help");
  expect(result.status).toBe(0);
  expect(result.stdout).toContain("armadra-relay personal");
  expect(result.stdout).toContain("init");
});

it("saas serve 缺配置以 64 退出，消息只列变量名", () => {
  const result = run("saas", "serve");
  expect(result.status).toBe(64);
  expect(result.stderr).toContain("RELAY_CLOUD_ISSUER");
});

it("未知命令以 64 退出", () => {
  expect(run("bogus").status).toBe(64);
});
