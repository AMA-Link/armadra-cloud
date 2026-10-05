import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, expect, it } from "vitest";

const CLI = fileURLToPath(new URL("./cli.ts", import.meta.url));
const dirs: string[] = [];
afterAll(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
});

function run(...args: string[]) {
  return spawnSync(process.execPath, [CLI, ...args], {
    encoding: "utf8",
    env: {},
  });
}

it("personal --help 打印用法并以 0 退出", () => {
  const result = run("personal", "--help");
  expect(result.status).toBe(0);
  expect(result.stdout).toContain("用法: armadra-relay personal");
  expect(result.stdout).toContain("--data-dir");
});

it("personal serve 缺省 --tls self-signed 尚未实现，以 69 退出", () => {
  const dir = mkdtempSync(join(tmpdir(), "relay-cli-"));
  dirs.push(dir);
  const result = run(
    "personal",
    "serve",
    "--data-dir",
    dir,
    "--host",
    "127.0.0.1",
  );
  expect(result.status).toBe(69);
  expect(result.stderr).toContain("--tls self-signed 尚未实现");
});

it("personal serve 缺 --host 以 64 退出", () => {
  expect(run("personal", "serve", "--data-dir", "/tmp/x").status).toBe(64);
});

it("personal 的未实现子命令以 69 退出，未知子命令以 64 退出", () => {
  expect(run("personal", "init").status).toBe(69);
  expect(run("personal", "bogus").status).toBe(64);
});

it("saas serve 缺配置以 64 退出，消息只列变量名", () => {
  const result = run("saas", "serve");
  expect(result.status).toBe(64);
  expect(result.stderr).toContain("RELAY_CLOUD_ISSUER");
});

it("未知命令以 64 退出", () => {
  expect(run("bogus").status).toBe(64);
});

it("personal serve --tls plain 不要任何 PostgreSQL / Redis 配置就起得来", async () => {
  const dir = mkdtempSync(join(tmpdir(), "relay-cli-"));
  dirs.push(dir);
  const child = spawn(
    process.execPath,
    [
      CLI,
      "personal",
      "serve",
      "--data-dir",
      dir,
      "--host",
      "127.0.0.1",
      "--tls",
      "plain",
      "--listen",
      "127.0.0.1:0",
    ],
    { env: {}, stdio: ["ignore", "pipe", "pipe"] },
  );
  try {
    const url = await new Promise<string>((resolve, reject) => {
      let out = "";
      child.stdout.setEncoding("utf8");
      child.stdout.on("data", (chunk: string) => {
        out += chunk;
        const match = /"url":"(http:\/\/[^"]+)"/.exec(out);
        if (match?.[1]) resolve(match[1]);
      });
      child.once("exit", (code) => reject(new Error(`提前退出 ${code}`)));
    });
    const response = await fetch(`${url}/health`);
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      ok: true,
      mode: "personal",
      node: "127.0.0.1",
    });
  } finally {
    child.kill("SIGTERM");
  }
});
