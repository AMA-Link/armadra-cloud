// node --test apps/relay-workers/scripts/rotate-password.test.mjs
//
// 改口令脚本：对本机的假中继跑一遍——登录（要挑战时带令牌）→ 改口令 → 登出 → 新口令只打印一次；
// 中继不报能力时直接停；旧口令不对时不改口令。只用测试值，不连外网。
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { fileURLToPath } from "node:url";
import test from "node:test";
import assert from "node:assert/strict";

const SCRIPT = fileURLToPath(new URL("./rotate-password.sh", import.meta.url));
const OLD = "test-old-passphrase-1";

/** 假中继：记下收到的请求。 */
async function fakeRelay({ capabilities, scope = [] }) {
  const requests = [];
  const server = createServer((req, res) => {
    let text = "";
    req.on("data", (chunk) => (text += chunk));
    req.on("end", () => {
      const body = text === "" ? null : JSON.parse(text);
      requests.push({
        method: req.method,
        url: req.url,
        authorization: req.headers.authorization ?? null,
        body,
      });
      const send = (status, value) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify(value));
      };
      if (req.url === "/.well-known/armadra-platform")
        return send(200, {
          mode: "personal",
          capabilities,
          ...(scope.length
            ? { challenge: { provider: "turnstile", siteKey: "site", scope } }
            : {}),
        });
      if (req.url === "/v1/auth/login") {
        if (body.password !== OLD)
          return send(401, { code: "credentials_invalid", message: "x" });
        return send(200, { session: { accessToken: "access-1" } });
      }
      if (req.url === "/v1/auth/password")
        return send(200, { session: { accessToken: "access-2" } });
      if (req.url === "/v1/auth/logout") return send(200, {});
      send(404, { code: "not_found", message: "x" });
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  return {
    origin: `http://127.0.0.1:${port}`,
    requests,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

/** 跑脚本（新会话，没有控制终端：新口令落到标准输出）。 */
function run(origin, input) {
  return new Promise((resolve) => {
    const child = spawn("bash", [SCRIPT], {
      detached: true,
      env: {
        PATH: process.env.PATH,
        ARMADRA_RELAY_ORIGIN: origin,
        ARMADRA_RELAY_ACCOUNT: "owner",
        ARMADRA_RELAY_NO_OPEN: "1",
      },
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("close", (code) => resolve({ code, stdout, stderr }));
    child.stdin.end(input);
  });
}

test("要挑战的中继：登录与改口令各带一次令牌，改完登出，新口令只打印一次", async () => {
  const relay = await fakeRelay({
    capabilities: ["auth.password", "auth.password-change"],
    scope: ["auth.login", "auth.changePassword"],
  });
  try {
    const result = await run(relay.origin, `${OLD}\nlogin-token\npw-token\n`);
    assert.equal(result.code, 0, result.stderr);
    const [, login, change, logout] = relay.requests;
    assert.deepEqual(login.body, {
      account: "owner",
      password: OLD,
      device: { platform: "server", name: "rotate-password" },
      challenge: { provider: "turnstile", token: "login-token" },
    });
    assert.equal(change.url, "/v1/auth/password");
    assert.equal(change.authorization, "Bearer access-1");
    assert.equal(change.body.password, OLD);
    assert.deepEqual(change.body.challenge, {
      provider: "turnstile",
      token: "pw-token",
    });
    const next = change.body.newPassword;
    assert.match(next, /^[A-Za-z0-9]{28}$/);
    assert.equal(logout.url, "/v1/auth/logout");
    assert.equal(logout.authorization, "Bearer access-2");
    assert.equal(result.stdout.split(next).length - 1, 1);
    assert.ok(!result.stderr.includes(next));
    assert.ok(!result.stdout.includes(OLD) && !result.stderr.includes(OLD));
    assert.match(result.stderr, /action=armadra-login/);
    assert.match(result.stderr, /action=armadra-password/);
  } finally {
    await relay.close();
  }
});

test("不要挑战：不提示令牌，请求里不带 challenge", async () => {
  const relay = await fakeRelay({
    capabilities: ["auth.password-change"],
  });
  try {
    const result = await run(relay.origin, `${OLD}\n`);
    assert.equal(result.code, 0, result.stderr);
    assert.equal(relay.requests[1].body.challenge, undefined);
    assert.equal(relay.requests[2].body.challenge, undefined);
    assert.ok(!result.stderr.includes("人机验证"));
  } finally {
    await relay.close();
  }
});

test("中继不报 auth.password-change：不问口令、不登录，直接停", async () => {
  const relay = await fakeRelay({ capabilities: ["auth.password"] });
  try {
    const result = await run(relay.origin, `${OLD}\n`);
    assert.equal(result.code, 1);
    assert.equal(relay.requests.length, 1);
    assert.match(result.stderr, /还不支持改口令/);
  } finally {
    await relay.close();
  }
});

test("旧口令不对：登录失败即停，不调改口令", async () => {
  const relay = await fakeRelay({ capabilities: ["auth.password-change"] });
  try {
    const result = await run(relay.origin, "wrong-passphrase\n");
    assert.equal(result.code, 1);
    assert.deepEqual(
      relay.requests.map((one) => one.url),
      ["/.well-known/armadra-platform", "/v1/auth/login"],
    );
    assert.match(result.stderr, /账号或口令不对/);
  } finally {
    await relay.close();
  }
});
