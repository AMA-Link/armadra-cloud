// 跨进程端到端的 Workers 版（`pnpm e2e --mode personal-workers`）：本地 workerd 上的 Workers 中继。
//
//   1. `wrangler dev --local` 按 apps/relay-workers/wrangler.jsonc 起中继（本机回环、临时存储目录，
//      secret 用这一次随机生成的测试值；不连 Cloudflare 账号）；
//   2. 账号口令登录 → 注册令牌 → 登记源 → 源 JWS 换隧道令牌，节点地址带 `?src=`；
//   3. 假 core（relay-runtime-node 的测试件，真 `ws` 客户端与真 HTTP 解析）连隧道：握手、45 秒心跳；
//   4. 经中继的 HTTP（剥令牌、X-Forwarded-*、1 MiB 大体）与 WebSocket（子协议、回显）；
//      端到端通道 `_relay/e2e`（假 core 报 `e2e`）：10 MB 随机字节双向原样；
//   5. 撤销源：隧道 4492、客户端 WS 断开、旧中继令牌 401；
//   6. 中继日志里查不到任何秘密。
//
// 真 core 的那一半（登记、隧道、画布、终端、分享链接含只读范围、媒体票 Range、撤销）在
// Armadra 仓的 `tools/probes/personal-roundtrip.mjs --workerd`，夜间作业 e2e-workers-armadra 跑。
//
// 与 `--mode personal` 的差别：服务器壳（真 core）对 workerd 的场景要 TLS 与容器可达的 issuer，
// 留给 Workers 探针批次；这里覆盖中继这一侧的线上行为。
import { spawn } from "node:child_process";
import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign,
} from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  buildSigningInput,
  joinCompactJws,
  TOKEN_TYPES,
} from "@armadra/platform-protocol/assertion";
import { WebSocket } from "ws";

import {
  expect,
  expectEqual,
  log,
  redact,
  secret,
  secretEntries,
  until,
} from "./lib.mjs";

const freePort = () =>
  new Promise((done, failed) => {
    const probe = createServer();
    probe.once("error", failed);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => done(port));
    });
  });

/** 起 `wrangler dev --local`；答 `{ origin, logs, stop }`。 */
async function startWorkerd(root) {
  const port = await freePort();
  const origin = `http://127.0.0.1:${port}`;
  const persist = mkdtempSync(join(tmpdir(), "armadra-workers-e2e-"));
  const cwd = join(root, "apps/relay-workers");
  // apps/relay-workers 的 devDependency（pnpm 的软链），版本由锁文件定。
  const wrangler = join(cwd, "node_modules/wrangler/bin/wrangler.js");
  const password = secret(
    `${randomBytes(12).toString("base64url")}-Aq7`,
    "中继账号口令",
  );
  const masterKey = secret(randomBytes(32).toString("hex"), "主密钥");
  const metricsToken = secret(randomBytes(18).toString("hex"), "指标令牌");
  const child = spawn(
    process.execPath,
    [
      wrangler,
      "dev",
      "--local",
      "--ip",
      "127.0.0.1",
      "--port",
      String(port),
      "--inspector-port",
      "0",
      "--show-interactive-dev-session=false",
      "--persist-to",
      persist,
      "--var",
      `RELAY_ISSUER:${origin}`,
      "--var",
      `RELAY_MASTER_KEY:${masterKey}`,
      "--var",
      "RELAY_PERSONAL_ACCOUNT:owner",
      "--var",
      `RELAY_PERSONAL_PASSWORD:${password}`,
      "--var",
      `RELAY_METRICS_TOKEN:${metricsToken}`,
    ],
    {
      cwd,
      env: { ...process.env, WRANGLER_SEND_METRICS: "false", CI: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  const logs = [];
  child.stdout.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  child.stderr.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  const exited = new Promise((done) => child.once("exit", done));
  const stop = async () => {
    if (child.exitCode === null) {
      child.kill("SIGINT");
      const timer = setTimeout(() => child.kill("SIGKILL"), 10_000);
      await exited;
      clearTimeout(timer);
    }
    rmSync(persist, { recursive: true, force: true });
  };
  try {
    await until(
      "workerd 就绪",
      async () => {
        if (child.exitCode !== null)
          throw new Error(
            `wrangler 提前退出：${redact(logs.join("").slice(-800))}`,
          );
        try {
          return (await fetch(`${origin}/health`)).ok;
        } catch {
          return false;
        }
      },
      120_000,
      500,
    );
  } catch (error) {
    await stop();
    throw error;
  }
  return { origin, password, metricsToken, logs, stop };
}

async function api(
  origin,
  path,
  { method = "GET", body, token, authorization } = {},
) {
  const headers = {};
  if (body !== undefined) headers["content-type"] = "application/json";
  if (token) headers.authorization = `Bearer ${token}`;
  if (authorization) headers.authorization = authorization;
  const response = await fetch(`${origin}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let json;
  try {
    json = JSON.parse(text);
  } catch {
    json = undefined;
  }
  return { status: response.status, json, text, headers: response.headers };
}

function sourceJws(origin, sourceId, key) {
  const iat = Math.floor(Date.now() / 1000);
  const { signingInput, encodedHeader, encodedPayload } = buildSigningInput(
    { alg: "EdDSA", typ: TOKEN_TYPES.source, kid: sourceId },
    {
      iss: sourceId,
      aud: origin,
      iat,
      exp: iat + 60,
      jti: randomBytes(12).toString("hex"),
    },
  );
  return joinCompactJws(
    encodedHeader,
    encodedPayload,
    new Uint8Array(sign(null, signingInput, key)),
  );
}

function openWs(url, protocols) {
  return new Promise((done, failed) => {
    const ws = new WebSocket(url, protocols, { perMessageDeflate: false });
    ws.once("open", () => done(ws));
    ws.once("error", failed);
  });
}

const nextMessage = (ws) =>
  new Promise((done) =>
    ws.once("message", (data, binary) => done({ data, binary })),
  );
const closeOf = (ws) =>
  new Promise((done) => ws.once("close", (code) => done(code)));

/** 跑完全部步骤；`report` 是 e2e.mjs 的步骤记录。 */
export async function runWorkersE2e({ root, report }) {
  const { FakeCore } = await import(
    "../../packages/relay-runtime-node/test/fake-core.ts"
  );
  let relay;
  let core;
  try {
    await report.step("wrangler dev --local 起 Workers 中继", async () => {
      relay = await startWorkerd(root);
      const health = await api(relay.origin, "/health");
      expectEqual(health.json?.node, "cf", "/health 的 node");
      return relay.origin;
    });
    const { origin } = relay;

    await report.step("平台信息与公钥集：/.well-known、/health", async () => {
      const info = await api(origin, "/.well-known/armadra-platform");
      expectEqual(info.status, 200, "/.well-known/armadra-platform");
      expectEqual(info.json?.mode, "personal", "mode");
      expectEqual(info.json?.issuer, origin, "issuer 与探测地址一致");
      const jwks = await api(origin, "/.well-known/jwks.json");
      expectEqual(jwks.status, 200, "/.well-known/jwks.json");
      expect(
        Array.isArray(jwks.json?.keys) &&
          jwks.json.keys.length > 0 &&
          jwks.json.keys.every((key) => key.d === undefined),
        "公钥集有公钥且不含私钥分量",
      );
      const health = await api(origin, "/health");
      expect(!health.headers.has("set-cookie"), "/health 不种 Cookie");
    });

    let session;
    await report.step("账号口令登录；错口令被拒", async () => {
      const wrong = await api(origin, "/v1/auth/login", {
        method: "POST",
        body: {
          account: "owner",
          password: "wrong-password-1",
          device: { platform: "desktop", name: "e2e" },
        },
      });
      expectEqual(wrong.status, 401, "错口令");
      const ok = await api(origin, "/v1/auth/login", {
        method: "POST",
        body: {
          account: "owner",
          password: relay.password,
          device: { platform: "desktop", name: "e2e" },
        },
      });
      expectEqual(ok.status, 200, "登录");
      secret(ok.json.session.accessToken, "访问令牌");
      secret(ok.json.session.refreshToken, "刷新令牌");
      session = ok.json.session;
    });

    let source;
    await report.step("登记源；节点地址带 ?src=", async () => {
      const issued = await api(origin, "/v1/sources/registration-tokens", {
        method: "POST",
        body: {},
        token: session.accessToken,
      });
      secret(issued.json.registrationToken, "注册令牌");
      const { privateKey, publicKey } = generateKeyPairSync("ed25519");
      const sourceId = randomBytes(16).toString("hex");
      const registered = await api(origin, "/v1/sources/register", {
        method: "POST",
        body: {
          registrationToken: issued.json.registrationToken,
          sourceId,
          publicKey: {
            kty: "OKP",
            crv: "Ed25519",
            x: publicKey.export({ format: "jwk" }).x,
          },
          name: "e2e",
          kind: "server",
          coreVersion: "0.2.0-e2e",
          capabilities: [],
          protocol: { major: 1, minor: 2 },
        },
      });
      expectEqual(registered.status, 200, "登记");
      const hint = await api(origin, "/v1/sources/me/relay", {
        authorization: `Source ${sourceJws(origin, sourceId, privateKey)}`,
      });
      expectEqual(hint.status, 200, "换隧道令牌");
      secret(hint.json.tunnelToken, "隧道令牌");
      expectEqual(
        hint.json.nodes[0].url,
        `${origin.replace(/^http/, "ws")}/t/v1?src=${sourceId}`,
        "节点地址",
      );
      source = { sourceId, privateKey, hint: hint.json };
      return sourceId;
    });

    await report.step(
      "假 core 连隧道：握手、ready.heartbeatMs = 45 秒",
      async () => {
        core = new FakeCore({
          tunnelUrl: source.hint.nodes[0].url,
          sourceId: source.sourceId,
          sourceKey: source.privateKey,
          token: () => source.hint.tunnelToken,
          capabilities: ["e2e"],
        });
        const outcome = await core.connect();
        expect(outcome.ready, `握手被拒：${JSON.stringify(outcome)}`);
        expectEqual(outcome.ready.heartbeatMs, 45_000, "heartbeatMs");
      },
    );

    let relayToken;
    await report.step("换断言与中继令牌：源在线", async () => {
      const issued = await api(
        origin,
        `/v1/sources/${source.sourceId}/assertion`,
        {
          method: "POST",
          body: { device: { platform: "desktop", name: "e2e" } },
          token: session.accessToken,
        },
      );
      expectEqual(issued.status, 200, "断言");
      expectEqual(issued.json.online, true, "online");
      secret(issued.json.relayToken, "中继令牌");
      secret(issued.json.assertion, "断言");
      relayToken = issued.json.relayToken;
    });

    await report.step(
      "经中继的 HTTP：剥令牌、X-Forwarded-*；1 MiB 大体长度不丢",
      async () => {
        const response = await fetch(
          `${origin}/s/${source.sourceId}/echo-request`,
          {
            headers: { "armadra-relay-token": relayToken },
          },
        );
        expectEqual(response.status, 200, "echo-request");
        const body = await response.json();
        expect(!("armadra-relay-token" in body.headers), "令牌没剥掉");
        expectEqual(
          body.headers["x-forwarded-proto"],
          "https",
          "x-forwarded-proto",
        );
        const big = await fetch(
          `${origin}/s/${source.sourceId}/big?bytes=1048576`,
          {
            headers: { "armadra-relay-token": relayToken },
          },
        );
        let received = 0;
        try {
          for await (const chunk of big.body) received += chunk.length;
        } catch (error) {
          throw new Error(
            `大体只收到 ${received} 字节：${error.message}；${error.cause?.message ?? ""}`,
          );
        }
        expectEqual(received, 1_048_576, "大体长度");
      },
    );

    let ws;
    await report.step("经中继的 WebSocket：子协议选 ticket、回显", async () => {
      const wsUrl = `${origin.replace(/^http/, "ws")}/s/${source.sourceId}/ws`;
      const socket = await openWs(wsUrl, [
        `armadra-relay.${relayToken}`,
        "armadra-ticket.e2e",
      ]);
      expectEqual(socket.protocol, "armadra-ticket.e2e", "子协议");
      socket.send("hello");
      const { data } = await nextMessage(socket);
      expectEqual(data.toString(), "hello", "回显");
      ws = socket;
    });

    await report.step(
      "端到端通道 _relay/e2e：选 armadra-e2e.1，10 MB 随机字节双向原样",
      async () => {
        const url = `${origin.replace(/^http/, "ws")}/s/${source.sourceId}/_relay/e2e`;
        const socket = await openWs(url, [
          "armadra-e2e.1",
          `armadra-relay.${relayToken}`,
        ]);
        expectEqual(socket.protocol, "armadra-e2e.1", "子协议");
        const total = 10 * 1024 * 1024;
        const sent = createHash("sha256");
        const got = createHash("sha256");
        let received = 0;
        socket.on("message", (data) => {
          got.update(data);
          received += data.length;
        });
        for (let offset = 0; offset < total; ) {
          const size = Math.min(total - offset, 48 * 1024);
          const chunk = randomBytes(size);
          sent.update(chunk);
          socket.send(chunk);
          offset += size;
          if (socket.bufferedAmount > 4 * 1024 * 1024)
            await until(
              "客户端缓冲排空",
              () => socket.bufferedAmount < 1024 * 1024,
            );
        }
        await until("回显收全", () => received >= total, 60_000, 50);
        expectEqual(received, total, "回显字节数");
        expectEqual(got.digest("hex"), sent.digest("hex"), "回显内容");
        const closed = closeOf(socket);
        socket.close(1000);
        await closed;
        const unsupported = await openWs(url, [`armadra-relay.${relayToken}`]);
        expectEqual(await closeOf(unsupported), 4409, "缺版本子协议");
      },
    );

    await report.step(
      "撤销源：隧道 4492、客户端 WS 断开、旧中继令牌 401",
      async () => {
        const wsClosed = closeOf(ws);
        const revoked = await api(origin, `/v1/sources/${source.sourceId}`, {
          method: "DELETE",
          token: session.accessToken,
        });
        expectEqual(revoked.status, 200, "撤销");
        expectEqual(await core.closed(), 4492, "隧道关闭码");
        expectEqual(await wsClosed, 4403, "客户端 WS 关闭码");
        const after = await fetch(
          `${origin}/s/${source.sourceId}/echo-request`,
          {
            headers: { "armadra-relay-token": relayToken },
          },
        );
        expectEqual(after.status, 401, "撤销后的中继令牌");
      },
    );

    await report.step("指标要令牌；中继日志里没有任何秘密", async () => {
      const metrics = await fetch(`${origin}/metrics`, {
        headers: { authorization: `Bearer ${relay.metricsToken}` },
      });
      expectEqual(metrics.status, 200, "/metrics");
      const text = relay.logs.join("");
      const leaked = secretEntries().filter(([value]) => text.includes(value));
      expectEqual(
        leaked.map(([, label]) => label).join("、"),
        "",
        "日志里的秘密",
      );
    });
  } catch (error) {
    // 排查用：E2E_DEBUG=1 时打印中继日志的最后一段（照样脱敏）。
    if (process.env.E2E_DEBUG) log(relay?.logs.join("").slice(-4000) ?? "");
    throw error;
  } finally {
    await core?.stop().catch(() => {});
    await relay?.stop?.();
  }
}
