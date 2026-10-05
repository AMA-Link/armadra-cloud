import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "@armadra/cloud-shared/log";
import { afterAll, expect, it } from "vitest";

import { PASSWORD } from "../../test/personal.ts";
import type { PersonalServeOptions } from "./args.ts";
import { runRotateKey } from "./commands.ts";
import { startPersonalRelay } from "./serve.ts";
import { StateError, StateStore } from "./state.ts";

const root = mkdtempSync(join(tmpdir(), "relay-personal-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));
const passwordFile = join(root, "pw");
writeFileSync(passwordFile, PASSWORD);

const base: Omit<PersonalServeOptions, "dataDir"> = {
  host: "127.0.0.1",
  port: 8443,
  listen: { host: "127.0.0.1", port: 0 },
  tls: "plain",
  logLevel: "error",
  account: "me",
  passwordFile,
};

const quiet = {
  log: createLogger({ name: "relay", level: "error", write: () => {} }),
};

it("首次自动初始化：数据目录 0700、状态文件在；/health 与 /ready 200", async () => {
  const dataDir = join(root, "a", "data");
  const relay = await startPersonalRelay({ ...base, dataDir }, quiet);
  try {
    expect(existsSync(join(dataDir, "state.json"))).toBe(true);
    if (process.platform !== "win32")
      expect(statSync(dataDir).mode & 0o777).toBe(0o700);
    const health = await fetch(`${relay.url}/health`);
    expect(await health.json()).toEqual({
      ok: true,
      mode: "personal",
      node: "127.0.0.1",
      version: "0.1.0",
      tunnels: 0,
      streams: 0,
    });
    expect((await fetch(`${relay.url}/ready`)).status).toBe(200);
    // 明文模式（反向代理之后）：issuer 仍是 https，没有要钉的指纹。
    expect(relay.issuer).toBe("https://127.0.0.1:8443");
    expect(relay.fingerprint).toBeNull();
  } finally {
    await relay.close();
  }
});

it("没初始化又没给账号口令：拒绝启动", async () => {
  await expect(
    startPersonalRelay(
      {
        ...base,
        dataDir: join(root, "b"),
        account: undefined,
        passwordFile: undefined,
      },
      quiet,
    ),
  ).rejects.toThrow(StateError);
});

it("--public-origin 变了：issuer 跟着改并写回状态文件", async () => {
  const dataDir = join(root, "c");
  const first = await startPersonalRelay({ ...base, dataDir }, quiet);
  await first.close();
  const second = await startPersonalRelay(
    { ...base, dataDir, publicOrigin: "https://relay.example" },
    quiet,
  );
  try {
    expect(second.issuer).toBe("https://relay.example");
    expect(StateStore.open(dataDir).current.issuer).toBe(
      "https://relay.example",
    );
    const info = (await (
      await fetch(`${second.url}/.well-known/armadra-platform`)
    ).json()) as { issuer: string; webApp: string | null };
    expect(info).toMatchObject({
      issuer: "https://relay.example",
      webApp: null,
    });
  } finally {
    await second.close();
  }
});

it("--web-root 有产物：/app/ 托管，platform.info 报 webApp", async () => {
  const web = join(root, "web");
  mkdirSync(web, { recursive: true });
  writeFileSync(
    join(web, "index.html"),
    "<!doctype html><title>armadra</title>",
  );
  const relay = await startPersonalRelay(
    { ...base, dataDir: join(root, "d"), webRoot: web },
    quiet,
  );
  try {
    expect(await (await fetch(`${relay.url}/app/`)).text()).toContain(
      "armadra",
    );
    const info = (await (
      await fetch(`${relay.url}/.well-known/armadra-platform`)
    ).json()) as { webApp: string | null };
    expect(info.webApp).toBe("https://127.0.0.1:8443/app/");
  } finally {
    await relay.close();
  }
});

it("运行中另一个进程轮换签名钥：一秒内 JWKS 就有新钥", async () => {
  const dataDir = join(root, "e");
  const relay = await startPersonalRelay({ ...base, dataDir }, quiet);
  try {
    runRotateKey(dataDir, () => {});
    const deadline = Date.now() + 5_000;
    let kids: string[] = [];
    while (Date.now() < deadline) {
      const jwks = (await (
        await fetch(`${relay.url}/.well-known/jwks.json`)
      ).json()) as { keys: { kid: string }[] };
      kids = jwks.keys.map((key) => key.kid);
      if (kids.includes("k2")) break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    expect(kids).toEqual(["k1", "k2"]);
  } finally {
    await relay.close();
  }
}, 10_000);
