import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
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

const PASSWORD = "plover-quartz-lantern-41";

function tempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), "relay-cli-"));
  dirs.push(dir);
  return dir;
}

function passwordFile(dir: string, value = PASSWORD): string {
  const file = join(dir, "pw");
  writeFileSync(file, `${value}\n`, { mode: 0o600 });
  return file;
}

/** 起 serve，等启动日志里的 url；答 url、日志与子进程。 */
async function serve(args: string[], env: Record<string, string> = {}) {
  const child = spawn(process.execPath, [CLI, "personal", "serve", ...args], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  const url = await new Promise<string>((resolve, reject) => {
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk: string) => {
      out += chunk;
      const match = /"url":"(https?:\/\/[^"]+)"/.exec(out);
      if (match?.[1]) resolve(match[1]);
    });
    child.once("exit", (code) => reject(new Error(`提前退出 ${code}`)));
  });
  return { child, url, output: () => out };
}

it("personal --help 打印用法并以 0 退出", () => {
  const result = run("personal", "--help");
  expect(result.status).toBe(0);
  expect(result.stdout).toContain("用法: armadra-relay personal");
  expect(result.stdout).toContain("--data-dir");
});

it("personal serve 没初始化、也没给首次初始化的账号口令，以 64 退出", () => {
  const dir = tempDir();
  const result = run(
    "personal",
    "serve",
    "--data-dir",
    dir,
    "--host",
    "127.0.0.1",
  );
  expect(result.status).toBe(64);
  expect(result.stderr).toContain("还没有初始化");
});

it("personal serve 缺 --host 以 64 退出", () => {
  expect(run("personal", "serve", "--data-dir", "/tmp/x").status).toBe(64);
});

it("personal 子命令缺参数、未知子命令以 64 退出", () => {
  expect(run("personal", "init").status).toBe(64);
  expect(run("personal", "status").status).toBe(64);
  expect(run("personal", "bogus").status).toBe(64);
});

it("init → status → passwd → rotate-key → links：只打印元数据，口令策略按 identity-vectors", () => {
  const dir = tempDir();
  const data = join(dir, "data");
  const weak = run(
    "personal",
    "init",
    "--data-dir",
    data,
    "--account",
    "me",
    "--host",
    "127.0.0.1",
    "--password-file",
    passwordFile(dir, "short"),
  );
  expect(weak.status).toBe(64);
  expect(weak.stderr).toContain("口令太短");
  const init = run(
    "personal",
    "init",
    "--data-dir",
    data,
    "--account",
    "me",
    "--host",
    "127.0.0.1",
    "--password-file",
    passwordFile(dir),
  );
  expect(init.status).toBe(0);
  const fingerprint = /CA 指纹：([0-9a-f]{64})/.exec(init.stdout)?.[1];
  expect(fingerprint).toBeDefined();
  expect(init.stdout).not.toContain(PASSWORD);
  const again = run(
    "personal",
    "init",
    "--data-dir",
    data,
    "--account",
    "me",
    "--host",
    "127.0.0.1",
    "--password-file",
    passwordFile(dir),
  );
  expect(again.status).toBe(64);
  expect(again.stderr).toContain("已经初始化过");

  const status = run("personal", "status", "--data-dir", data);
  expect(status.status).toBe(0);
  expect(status.stdout).toContain("账号：me");
  expect(status.stdout).toContain(`CA 指纹：${fingerprint}`);

  const passwd = run(
    "personal",
    "passwd",
    "--data-dir",
    data,
    "--password-file",
    passwordFile(dir, "saffron-glacier-meadow-77"),
  );
  expect(passwd.status).toBe(0);
  const rotate = run("personal", "rotate-key", "--data-dir", data);
  expect(rotate.stdout).toContain("已启用新签名钥：k2");
  expect(run("personal", "status", "--data-dir", data).stdout).toMatch(
    /签名钥：k1（退役于 [^）]+）、k2/,
  );
  expect(run("personal", "links", "list", "--data-dir", data).stdout).toContain(
    "没有链接",
  );
  const revoke = run(
    "personal",
    "links",
    "revoke",
    "0123456789abcdef",
    "--data-dir",
    data,
  );
  expect(revoke.status).toBe(64);
});

it("saas serve 缺配置以 64 退出，消息只列变量名", () => {
  const result = run("saas", "serve");
  expect(result.status).toBe(64);
  expect(result.stderr).toContain("RELAY_CLOUD_ISSUER");
});

it("未知命令以 64 退出", () => {
  expect(run("bogus").status).toBe(64);
});

it("personal serve --tls plain（回环）首次自动初始化，不要任何 PostgreSQL / Redis 配置就起得来", async () => {
  const dir = tempDir();
  const { child, url } = await serve(
    [
      "--data-dir",
      join(dir, "data"),
      "--host",
      "127.0.0.1",
      "--tls",
      "plain",
      "--listen",
      "127.0.0.1:0",
    ],
    {
      RELAY_PERSONAL_ACCOUNT: "me",
      RELAY_PERSONAL_PASSWORD_FILE: passwordFile(dir),
    },
  );
  try {
    expect(url).toMatch(/^http:\/\//);
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

it("personal serve --tls plain 绑非回环地址：拒绝启动", () => {
  const result = run(
    "personal",
    "serve",
    "--data-dir",
    tempDir(),
    "--host",
    "203.0.113.5",
    "--tls",
    "plain",
  );
  expect(result.status).toBe(64);
  expect(result.stderr).toContain("--tls plain 只允许监听回环地址");
});

it("personal serve 缺省自签 TLS：启动日志打印 CA 指纹，与 status 一致，日志里没有口令", async () => {
  const dir = tempDir();
  const data = join(dir, "data");
  const { child, url, output } = await serve(
    ["--data-dir", data, "--host", "127.0.0.1", "--listen", "127.0.0.1:0"],
    {
      RELAY_PERSONAL_ACCOUNT: "me",
      RELAY_PERSONAL_PASSWORD_FILE: passwordFile(dir),
    },
  );
  try {
    expect(url).toMatch(/^https:\/\//);
    const logged = /"fingerprint":"([0-9a-f]{64})"/.exec(output())?.[1];
    expect(logged).toBeDefined();
    expect(run("personal", "status", "--data-dir", data).stdout).toContain(
      `CA 指纹：${logged}`,
    );
    expect(output()).not.toContain(PASSWORD);
  } finally {
    child.kill("SIGTERM");
  }
});
