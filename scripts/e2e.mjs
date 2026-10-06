// 跨仓端到端（C0-3）：个人中转 + Armadra 服务器壳 + Node 客户端。
//
//   pnpm e2e --mode personal [--skip-build] [--relay-image <镜像>] [--server-image <镜像>]
//                            [--output <目录>] [--keep]
//
// 编排（deploy/e2e/compose.yml，项目名 armadra-e2e-<随机>，跑完 down -v）：
//   1. 本地构建个人中转镜像（本仓）与服务器壳镜像（Armadra 主仓，见下）；
//   2. 起中继（首次自动 init）与服务器壳；服务器壳不发布端口，只能经中继到达；
//   3. 服务器壳登记到中继（注册令牌）、隧道连上；
//   4. 客户端用账号口令登录中继 → 断言 + 中继令牌 → 换 core 会话 → 经中继调 HTTP API、
//      事件流、终端收发、协作板；
//   5. 分享链接访客访问；撤销链接、源离线、撤销源，各自断开；
//   6. 全部日志里查不到任何秘密。
//
// Armadra 主仓：环境变量 ARMADRA_REPO（缺省 ../ai-coding-canvas）；ARMADRA_REF（缺省 origin/main）
// 用 `git archive` 取那一版构建，不动主仓的检出；ARMADRA_REF=worktree 则直接用目录现状；
// ARMADRA_FETCH=0 不先 fetch。`--server-image` 直接用现成镜像（nightly 的 compat.min）。
//
// 产物：<输出目录>/result.json（缺省 .data/e2e/，每步耗时与结果）。任一步失败非零退出。
import { createHash, randomBytes, X509Certificate } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { readFileSync } from "node:fs";

import {
  Report,
  exec,
  expect,
  expectEqual,
  jsonFrames,
  log,
  redact,
  relayEndpoint,
  secret,
  secretEntries,
  until,
} from "./e2e/lib.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const USAGE = `用法: pnpm e2e --mode personal [选项]

  --mode personal        个人中转（saas 模式随控制面落地再加）
  --skip-build           不构建镜像，用已有的 armadra-relay:e2e 与 armadra-server:e2e
  --relay-image IMAGE    用现成的中继镜像
  --server-image IMAGE   用现成的服务器壳镜像（如 ghcr.io/owlbay/armadra-server:<compat.min>）
  --output DIR           result.json 的目录，缺省 .data/e2e
  --keep                 跑完不清理（调试用，之后手动 docker compose -p <项目> down -v）`;

function parseArgs(argv) {
  const options = { flags: new Set(), values: new Map() };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--help" || arg === "-h") {
      console.log(USAGE);
      process.exit(0);
    }
    if (["--skip-build", "--keep"].includes(arg)) options.flags.add(arg);
    else if (
      ["--mode", "--relay-image", "--server-image", "--output"].includes(arg)
    ) {
      const value = argv[(index += 1)];
      if (value === undefined || value.startsWith("--")) {
        console.error(`${arg} 缺少取值\n${USAGE}`);
        process.exit(2);
      }
      options.values.set(arg, value);
    } else {
      console.error(`未知选项：${arg}\n${USAGE}`);
      process.exit(2);
    }
  }
  return options;
}

const options = parseArgs(process.argv.slice(2));
const mode = options.values.get("--mode");
if (mode !== "personal") {
  console.error(
    mode === undefined
      ? `需要 --mode personal\n${USAGE}`
      : `--mode ${mode}：现在只支持 personal`,
  );
  process.exit(2);
}

const project = `armadra-e2e-${randomBytes(4).toString("hex")}`;
const composeFile = join(root, "deploy/e2e/compose.yml");
const output = resolve(root, options.values.get("--output") ?? ".data/e2e");
const relayImage = options.values.get("--relay-image") ?? "armadra-relay:e2e";
const serverImage =
  options.values.get("--server-image") ?? "armadra-server:e2e";
const keep = options.flags.has("--keep");
const report = new Report();

const relayPort = await new Promise((done, failed) => {
  const probe = createServer();
  probe.once("error", failed);
  probe.listen(0, "127.0.0.1", () => {
    const { port } = probe.address();
    probe.close(() => done(port));
  });
});
const issuer = `https://relay:${relayPort}`;
const secretsDir = mkdtempSync(join(tmpdir(), "armadra-e2e-"));
chmodSync(secretsDir, 0o755);
// 容器里是 uid 10002：口令文件要它读得到。随机口令、只活在这一次运行里。
const relayPassword = secret(
  randomBytes(18).toString("base64url"),
  "中继账号口令",
);
writeFileSync(join(secretsDir, "relay_password"), relayPassword, {
  mode: 0o644,
});

const composeEnv = {
  E2E_RELAY_IMAGE: relayImage,
  E2E_SERVER_IMAGE: serverImage,
  E2E_RELAY_PORT: String(relayPort),
  E2E_SECRETS_DIR: secretsDir,
};
const compose = (args, extra = {}) =>
  exec("docker", ["compose", "-p", project, "-f", composeFile, ...args], {
    env: composeEnv,
    ...extra,
  });

let caPem = "";
const relay = relayEndpoint({ issuer, getCa: () => caPem });

/* ------------------------------ 镜像 ------------------------------ */

async function buildImages() {
  if (!options.values.has("--relay-image")) {
    await exec(
      "docker",
      ["build", "-q", "-f", "apps/relay/Dockerfile", "-t", relayImage, "."],
      { cwd: root },
    );
  }
  if (options.values.has("--server-image")) return;
  const repo = resolve(root, process.env.ARMADRA_REPO ?? "../ai-coding-canvas");
  expect(
    existsSync(join(repo, "apps/server/docker/Dockerfile")),
    `ARMADRA_REPO 不是 Armadra 主仓：${repo}`,
  );
  const ref = process.env.ARMADRA_REF ?? "origin/main";
  if (ref === "worktree") {
    await exec("docker", [
      "build",
      "-q",
      "-f",
      join(repo, "apps/server/docker/Dockerfile"),
      "-t",
      serverImage,
      repo,
    ]);
    return `服务器壳取自目录现状 ${repo}`;
  }
  if (process.env.ARMADRA_FETCH !== "0")
    await exec("git", ["-C", repo, "fetch", "-q", "origin"], {
      allowFail: true,
    });
  const commit = (
    await exec("git", ["-C", repo, "rev-parse", "--short", ref])
  ).stdout.trim();
  const context = mkdtempSync(join(tmpdir(), "armadra-e2e-src-"));
  try {
    // 只读主仓：git archive 把那一版导进临时目录再构建，不碰检出与工作区。
    await exec("sh", [
      "-c",
      `git -C "${repo}" archive "${ref}" | tar -x -C "${context}"`,
    ]);
    await exec(
      "docker",
      [
        "build",
        "-q",
        "-f",
        "apps/server/docker/Dockerfile",
        "-t",
        serverImage,
        ".",
      ],
      { cwd: context },
    );
  } finally {
    rmSync(context, { recursive: true, force: true });
  }
  return `服务器壳取自 ${ref}@${commit}`;
}

/* ------------------------------ 中继 /v1 ------------------------------ */

function v1(method, path, body, token) {
  return relay.http(method, path, {
    ...(body === undefined ? {} : { body }),
    headers: token ? { authorization: `Bearer ${token}` } : {},
  });
}

function ok(answer, what) {
  expect(
    answer.status >= 200 && answer.status < 300,
    `${what}：HTTP ${answer.status} ${redact(answer.text).slice(0, 200)}`,
  );
  return answer.body;
}

/* ------------------------------ 服务器壳（容器里） ------------------------------ */

const serverOrigin = "https://armadra-server:8443";
const adminSource = readFileSync(
  join(root, "scripts/e2e/server-admin.mjs"),
  "utf8",
);

async function admin(input) {
  const script = `const input = ${JSON.stringify(input)};\n${adminSource}`;
  const out = await compose(
    ["exec", "-T", "armadra-server", "node", "--input-type=module", "-"],
    { input: script },
  );
  const line = out.stdout.trim().split("\n").at(-1) ?? "";
  try {
    return JSON.parse(line);
  } catch {
    throw new Error(
      `管理员工具没有给出 JSON：${redact(out.stdout).slice(-300)}`,
    );
  }
}

const adminCall = (method, path, body) =>
  admin({
    action: "call",
    origin: serverOrigin,
    method,
    path,
    ...(body === undefined ? {} : { body }),
  });

/** 经中继打 core（客户端视角）：中继令牌 + 来源 = 中继自己。 */
function viaRelay(client, method, path, body, extra = {}) {
  return relay.http(method, `${client.baseUrl}${path}`, {
    ...(body === undefined ? {} : { body }),
    headers: {
      origin: issuer,
      "armadra-relay-token": client.relayToken,
      ...(client.access ? { authorization: `Bearer ${client.access}` } : {}),
      ...extra,
    },
  });
}

async function wsTicket(client) {
  const answer = await viaRelay(client, "POST", "/api/identity/ws-ticket");
  return secret(ok(answer, "换 WS 票").ticket);
}

async function openWs(client, path, { relayToken } = {}) {
  const ticket = await wsTicket(client);
  return relay.ws(
    `${client.baseUrl.replace(/^https:/, "wss:")}${path}`,
    [
      `armadra-relay.${relayToken ?? client.relayToken}`,
      `armadra-ticket.${ticket}`,
    ],
    issuer,
  );
}

/** 断言换会话：owner 与访客共用这一段（访客多一张邀请令牌）。 */
async function coreLogin(client, assertion, invitationToken) {
  const login = await viaRelay(
    { ...client, access: "" },
    "POST",
    "/api/identity/cloud/login",
    { assertion, ...(invitationToken ? { invitationToken } : {}) },
  );
  const body = ok(login, "cloud/login");
  expect(
    login.headers["set-cookie"] === undefined,
    "经中继的 cloud/login 不发 Cookie",
  );
  expect(body.session?.native?.accessToken, "cloud/login 答了原生会话");
  secret(body.session.native.accessToken, "core 访问令牌");
  secret(body.session.native.refreshToken, "core 刷新令牌");
  client.access = body.session.native.accessToken;
  client.refresh = body.session.native.refreshToken;
  client.csrf = body.session.csrfToken;
  return body;
}

/* ------------------------------ 主流程 ------------------------------ */

const state = { terminals: [] };
let failure;
const wsToClose = [];

async function main() {
  await report.step(
    "构建镜像（中继取本仓，服务器壳取 Armadra 主仓）",
    async () =>
      options.flags.has("--skip-build") ? "跳过构建" : buildImages(),
  );

  await report.step("起中继与服务器壳（服务器壳不发布端口）", async () => {
    await compose(["up", "-d", "--wait", "--wait-timeout", "180"]);
    const ps = await compose(["ps", "--format", "json"]);
    const rows = ps.stdout
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((line) => JSON.parse(line));
    const server = rows.find((row) => row.Service === "armadra-server");
    expect(server !== undefined, "服务器壳容器在");
    expect(
      !(server.Publishers ?? []).some((p) => (p.PublishedPort ?? 0) > 0),
      "服务器壳没有发布任何端口",
    );
  });

  await report.step("中继 init：自动初始化、CA 指纹与 TLS 校验", async () => {
    const ca = await relay.http("GET", "/ca.crt", { insecure: true });
    expectEqual(ca.status, 200, "GET /ca.crt");
    caPem = ca.text;
    state.fingerprint = createHash("sha256")
      .update(new X509Certificate(caPem).raw)
      .digest("hex");
    const status = await compose([
      "exec",
      "-T",
      "relay",
      "node",
      "/app/out/cli.js",
      "personal",
      "status",
      "--data-dir",
      "/data",
    ]);
    expect(
      status.stdout.includes(state.fingerprint),
      "status 打印的指纹与 /ca.crt 一致",
    );
    const info = await relay.http("GET", "/.well-known/armadra-platform");
    expectEqual(info.status, 200, "platform.info（按 CA 校验过 TLS）");
    expectEqual(info.body.mode, "personal", "mode");
    expectEqual(info.body.issuer, issuer, "issuer");
    return `指纹 ${state.fingerprint.slice(0, 16)}…`;
  });

  const owner = { baseUrl: "", relayToken: "", access: "", refresh: "" };
  await report.step("账号口令登录中继；错口令被拒", async () => {
    const wrong = await v1("POST", "/v1/auth/login", {
      account: "e2e",
      password: "not-the-password-0",
      device: { platform: "desktop", name: "e2e" },
    });
    expectEqual(wrong.status, 401, "错口令的状态码");
    expectEqual(wrong.body.code, "credentials_invalid", "错口令的错误码");
    const login = await v1("POST", "/v1/auth/login", {
      account: "e2e",
      password: relayPassword,
      device: { platform: "desktop", name: "e2e-owner" },
    });
    const body = ok(login, "auth.login");
    state.relayAccess = secret(body.session.accessToken, "中继访问令牌");
    secret(body.session.refreshToken, "中继刷新令牌");
    expectEqual(body.session.account.accountId, "acct:e2e", "accountId");
    const me = ok(
      await v1("GET", "/v1/me", undefined, state.relayAccess),
      "me.get",
    );
    expectEqual(
      me.account?.accountId ?? me.accountId,
      "acct:e2e",
      "me.get 的账号",
    );
  });

  await report.step("服务器壳：配对管理员、建工作空间", async () => {
    const logs = await compose(["logs", "--no-log-prefix", "armadra-server"]);
    const link = /armadra-server pairing (\S+)/.exec(logs.stdout)?.[1] ?? "";
    const ticket = /#pair=([^&\s]+)/.exec(link)?.[1] ?? "";
    expect(ticket !== "", "启动日志里有配对链接");
    secret(ticket, "配对票");
    const paired = await admin({
      action: "pair",
      origin: serverOrigin,
      ticket,
    });
    expectEqual(paired.status, 200, "配对");
    const workspace = await adminCall("POST", "/api/workspaces", {
      name: "e2e",
      rootPath: "/data/e2e-ws",
      createDirectory: true,
    });
    expect(
      workspace.status < 300,
      `建工作空间：HTTP ${workspace.status} ${JSON.stringify(workspace.body).slice(0, 200)}`,
    );
    state.workspaceId = workspace.body.id;
    expect(typeof state.workspaceId === "string", "工作空间 id");
  });

  await report.step(
    "服务器壳用注册令牌登记到中继，隧道连上、me.sources 在线",
    async () => {
      const token = ok(
        await v1(
          "POST",
          "/v1/sources/registration-tokens",
          {},
          state.relayAccess,
        ),
        "sources.registrationToken",
      );
      secret(token.registrationToken, "注册令牌");
      expectEqual(token.issuer, issuer, "令牌的 issuer");
      const added = await adminCall("POST", "/api/rpc/sources/remoteAdd", {
        json: {
          kind: "personal",
          issuer,
          account: "e2e",
          password: relayPassword,
          fingerprint: state.fingerprint,
        },
      });
      expectEqual(
        added.status,
        200,
        `remoteAdd ${JSON.stringify(added.body).slice(0, 200)}`,
      );
      const registered = await adminCall(
        "POST",
        "/api/identity/cloud/register",
        {
          issuer,
          registrationToken: token.registrationToken,
        },
      );
      expectEqual(
        registered.status,
        200,
        `register ${JSON.stringify(registered.body).slice(0, 200)}`,
      );
      state.sourceId = registered.body.sourceId;
      expect(
        /^[0-9a-f]{32}$/.test(state.sourceId),
        "sourceId 是 32 位十六进制",
      );
      await until(
        "本机登记的隧道 ready",
        async () => {
          const status = await adminCall("GET", "/api/identity/cloud");
          return status.body.registrations?.[0]?.tunnel?.state === "ready";
        },
        30_000,
      );
      await until(
        "me.sources 里该源在线",
        async () => {
          const sources = ok(
            await v1("GET", "/v1/me/sources", undefined, state.relayAccess),
            "me.sources",
          );
          const list = sources.sources ?? sources;
          return list.find(
            (row) =>
              (row.sourceId ?? row.id) === state.sourceId &&
              row.online === true,
          );
        },
        30_000,
      );
      return `源 ${state.sourceId.slice(0, 8)}… online`;
    },
  );

  await report.step(
    "断言 + 中继令牌：owner 绑定后换 core 会话，经中继调 HTTP API",
    async () => {
      const assertion = async () =>
        ok(
          await v1(
            "POST",
            `/v1/sources/${state.sourceId}/assertion`,
            { device: { platform: "desktop", name: "e2e-owner" } },
            state.relayAccess,
          ),
          "sources.assertion",
        );
      const first = await assertion();
      secret(first.assertion, "断言");
      secret(first.relayToken, "中继令牌");
      const bound = await adminCall("POST", "/api/identity/cloud/bind", {
        assertion: first.assertion,
      });
      expectEqual(
        bound.status,
        200,
        `bind ${JSON.stringify(bound.body).slice(0, 200)}`,
      );
      const second = await assertion();
      secret(second.assertion, "断言");
      secret(second.relayToken, "中继令牌");
      owner.relayToken = second.relayToken;
      owner.baseUrl = second.relayBaseUrl;
      expectEqual(
        owner.baseUrl,
        `${issuer}/s/${state.sourceId}`,
        "relayBaseUrl",
      );
      expectEqual(second.online, true, "assertion.online");
      await coreLogin(owner, second.assertion);
      // 同一张断言第二次：core 拒绝重放。
      const replay = await viaRelay(
        { ...owner, access: "" },
        "POST",
        "/api/identity/cloud/login",
        { assertion: second.assertion },
      );
      expectEqual(replay.status, 401, "重放断言的状态码");
      expectEqual(
        replay.body.code,
        "cloud_assertion_replayed",
        "重放断言的错误码",
      );
      const refreshed = await viaRelay(
        { ...owner, access: owner.refresh },
        "POST",
        "/api/identity/session/refresh",
        undefined,
        { "x-armadra-csrf": owner.csrf },
      );
      const refreshedBody = ok(refreshed, "session/refresh");
      owner.access = secret(refreshedBody.native.accessToken);
      owner.refresh = secret(
        refreshedBody.native.refreshToken ?? owner.refresh,
      );
      const workspaces = await viaRelay(owner, "GET", "/api/workspaces");
      ok(workspaces, "GET /api/workspaces");
      expect(
        JSON.stringify(workspaces.body).includes(state.workspaceId),
        "工作空间列表含 e2e 工作空间",
      );
      expectEqual(
        (await viaRelay(owner, "GET", "/hook/health")).status,
        403,
        "回环专用路径经中继",
      );
      expectEqual(
        (await viaRelay({ ...owner, access: "" }, "GET", "/api/workspaces"))
          .status,
        401,
        "没有 Bearer",
      );
      const noToken = await relay.http(
        "GET",
        `${owner.baseUrl}/api/workspaces`,
        {
          headers: { origin: issuer, authorization: `Bearer ${owner.access}` },
        },
      );
      expectEqual(noToken.status, 401, "没有中继令牌的状态码");
      expectEqual(
        noToken.body.code,
        "relay_token_invalid",
        "没有中继令牌的错误码",
      );
    },
  );

  await report.step(
    "经中继：终端收发 echo，shell 退出时事件流收到 terminal.exit",
    async () => {
      const events = await openWs(
        owner,
        `/api/workspaces/${state.workspaceId}/events`,
      );
      wsToClose.push(events.socket);
      expectEqual(events.status, 101, "事件流升级");
      expect(events.closeCode === undefined, "事件流没有被立刻关");
      state.ownerEvents = events;
      const created = ok(
        await viaRelay(owner, "POST", "/api/terminals", {
          workspaceId: state.workspaceId,
          cwd: "/data/e2e-ws",
          shell: "/bin/sh",
        }),
        "建终端",
      );
      const id = created.id;
      state.terminalId = id;
      const terminal = await openWs(
        owner,
        `/api/terminals/${id}/ws?writer=e2e`,
      );
      wsToClose.push(terminal.socket);
      expectEqual(terminal.status, 101, "终端 WS 升级");
      await until("终端 hello", () =>
        jsonFrames(terminal.messages).some((frame) => frame.type === "hello"),
      );
      const marker = `e2e-${randomBytes(6).toString("hex")}`;
      terminal.socket.send(
        JSON.stringify({ type: "input", data: `echo ${marker}\r`, inputId: 1 }),
      );
      await until("终端回显标记", () =>
        jsonFrames(terminal.messages).some(
          (frame) =>
            frame.type === "output" &&
            String(frame.data ?? "").includes(marker),
        ),
      );
      // shell 退出时 core 发 terminal.exit 到工作空间事件流。
      terminal.socket.send(
        JSON.stringify({ type: "input", data: "exit\r", inputId: 2 }),
      );
      await until(
        `事件流收到 terminal.exit（目前只有：${[...new Set(jsonFrames(events.messages).map((frame) => frame.type))].join("、") || "无"}）`,
        () =>
          jsonFrames(events.messages).some(
            (frame) => frame.type === "terminal.exit",
          ),
      );
      terminal.socket.close();
    },
  );

  await report.step("经中继：协作板同步握手（收到 sync step2）", async () => {
    const boards = ok(
      await viaRelay(
        owner,
        "GET",
        `/api/workspaces/${state.workspaceId}/boards`,
      ),
      "列画板",
    );
    const list = Array.isArray(boards) ? boards : boards.boards;
    const boardId = list?.[0]?.id;
    expect(typeof boardId === "string", "工作空间有一块画板");
    const sync = await openWs(
      owner,
      `/api/workspaces/${state.workspaceId}/boards/${boardId}/sync`,
    );
    wsToClose.push(sync.socket);
    expectEqual(sync.status, 101, "sync 升级");
    // y-protocols：消息类型 0（sync）、子类型 0（step1）、空文档的状态向量 [0]。
    sync.socket.send(Buffer.from([0, 0, 1, 0]));
    await until("收到 sync step2", () =>
      sync.messages.some((data) => data[0] === 0 && data[1] === 1),
    );
    sync.socket.close();
  });

  await report.step("4401：坏中继令牌被关，换新断言重连成功", async () => {
    const bad = await openWs(
      owner,
      `/api/workspaces/${state.workspaceId}/events`,
      {
        relayToken: "not-a-relay-token",
      },
    );
    expectEqual(bad.closeCode ?? (await bad.closed()), 4401, "坏令牌的关闭码");
    const fresh = ok(
      await v1(
        "POST",
        `/v1/sources/${state.sourceId}/assertion`,
        { device: { platform: "desktop", name: "e2e-owner" } },
        state.relayAccess,
      ),
      "sources.assertion",
    );
    owner.relayToken = secret(fresh.relayToken);
    secret(fresh.assertion, "断言");
    const again = await openWs(
      owner,
      `/api/workspaces/${state.workspaceId}/events`,
    );
    expectEqual(again.status, 101, "新令牌重连");
    expect(again.closeCode === undefined, "新令牌没有被关");
    again.socket.close();
  });

  const guest = { baseUrl: "", relayToken: "", access: "", refresh: "" };
  await report.step(
    "分享链接访客：links.create → accept → 访客 cloud/login，只读",
    async () => {
      const invitation = await adminCall("POST", "/api/identity/invitations", {
        role: "viewer",
        targetWorkspaceId: state.workspaceId,
        ttlMs: 3_600_000,
      });
      expect(
        invitation.status < 300,
        `core 邀请：HTTP ${invitation.status} ${JSON.stringify(invitation.body).slice(0, 200)}`,
      );
      const invitationToken = secret(invitation.body.token);
      const invitationId =
        invitation.body.invitationId ??
        invitation.body.id ??
        invitationToken.split(".")[0];
      const created = ok(
        await v1(
          "POST",
          "/v1/links",
          {
            kind: "source_invite",
            sourceId: state.sourceId,
            invitationId,
            label: "e2e",
            role: "viewer",
            expiresAtMs: Date.now() + 3_600_000,
          },
          state.relayAccess,
        ),
        "links.create",
      );
      secret(created.secret, "链接秘密");
      state.linkId = created.linkId;
      expectEqual(created.url, `${issuer}/j/${created.linkId}`, "链接地址");
      const info = ok(
        await v1("GET", `/v1/links/${created.linkId}`),
        "links.get",
      );
      expectEqual(info.kind, "source_invite", "links.get.kind");
      const accepted = ok(
        await v1("POST", `/v1/links/${created.linkId}/accept`, {
          secret: created.secret,
          device: { platform: "browser", name: "e2e-guest" },
        }),
        "links.accept",
      );
      secret(accepted.assertion, "断言");
      secret(accepted.relayToken, "中继令牌");
      state.guestRelayAccess = secret(accepted.guestSession.accessToken);
      secret(accepted.guestSession.refreshToken);
      expectEqual(accepted.sourceId, state.sourceId, "accept 的 sourceId");
      guest.relayToken = accepted.relayToken;
      guest.baseUrl = accepted.relayBaseUrl;
      const login = await coreLogin(guest, accepted.assertion, invitationToken);
      expectEqual(login.created, true, "访客账号是新建的");
      const workspaces = ok(
        await viaRelay(guest, "GET", "/api/workspaces"),
        "访客列工作空间",
      );
      expect(
        JSON.stringify(workspaces).includes(state.workspaceId),
        "访客看得到被分享的工作空间",
      );
      expectEqual(
        (
          await viaRelay(guest, "POST", "/api/terminals", {
            workspaceId: state.workspaceId,
            cwd: "/data/e2e-ws",
            shell: "/bin/sh",
          })
        ).status,
        403,
        "只读访客不能开终端",
      );
      // 中继侧：访客只看得到链接指向的源，要不了注册令牌。
      const mine = ok(
        await v1("GET", "/v1/me/sources", undefined, state.guestRelayAccess),
        "访客 me.sources",
      );
      expectEqual((mine.sources ?? mine).length, 1, "访客只看到一个源");
      expectEqual(
        (
          await v1(
            "POST",
            "/v1/sources/registration-tokens",
            {},
            state.guestRelayAccess,
          )
        ).status,
        403,
        "访客要注册令牌",
      );
      const events = await openWs(
        guest,
        `/api/workspaces/${state.workspaceId}/events`,
      );
      wsToClose.push(events.socket);
      expectEqual(events.status, 101, "访客事件流升级");
      state.guestEvents = events;
    },
  );

  await report.step(
    "撤销链接：访客的流被断开、令牌作废，链接不能再兑换",
    async () => {
      const started = Date.now();
      ok(
        await v1(
          "DELETE",
          `/v1/links/${state.linkId}`,
          undefined,
          state.relayAccess,
        ),
        "links.revoke",
      );
      const code = await state.guestEvents.closed();
      expect(
        Date.now() - started < 5000,
        `访客流在 5 秒内被断开（${Date.now() - started} ms）`,
      );
      expect(code >= 4400 || code === 1006, `访客流的关闭码 ${code}`);
      const after = await viaRelay(guest, "GET", "/api/workspaces");
      expect(
        [401, 403].includes(after.status),
        `撤销后访客的请求被拒：${after.status}`,
      );
      const again = await v1("POST", `/v1/links/${state.linkId}/accept`, {
        secret: "x".repeat(20),
        device: { platform: "browser", name: "e2e-guest-2" },
      });
      expect(again.status >= 400, `撤销后的链接不能再兑换：${again.status}`);
      // owner 不受影响。
      ok(await viaRelay(owner, "GET", "/api/workspaces"), "owner 仍可用");
    },
  );

  await report.step(
    "源下线：503 source_offline、WS 4404；重启后恢复",
    async () => {
      const events = await openWs(
        owner,
        `/api/workspaces/${state.workspaceId}/events`,
      );
      expectEqual(events.status, 101, "下线前的事件流");
      const started = Date.now();
      await compose(["stop", "-t", "10", "armadra-server"]);
      const code = await events.closed();
      expectEqual(code, 4404, "源下线时事件流的关闭码");
      const offline = await until(
        "经中继 503 source_offline",
        async () => {
          const answer = await viaRelay(owner, "GET", "/api/workspaces");
          return answer.status === 503 ? answer : undefined;
        },
        10_000,
      );
      expectEqual(offline.body.code, "source_offline", "下线的错误码");
      const down = Date.now() - started;
      await compose(["start", "armadra-server"]);
      await until(
        "隧道重连、请求恢复",
        async () => {
          const answer = await viaRelay(owner, "GET", "/api/workspaces");
          return answer.status === 200;
        },
        90_000,
        500,
      );
      return `停 ${down} ms 后断开可见，重启后 ${Date.now() - started} ms 恢复`;
    },
  );

  await report.step(
    "撤销源：流被断开，经中继 404，源列表清空，本机登记随之撤销",
    async () => {
      const events = await openWs(
        owner,
        `/api/workspaces/${state.workspaceId}/events`,
      );
      expectEqual(events.status, 101, "撤销前的事件流");
      ok(
        await v1(
          "DELETE",
          `/v1/sources/${state.sourceId}`,
          undefined,
          state.relayAccess,
        ),
        "sources.revoke",
      );
      const code = await events.closed();
      expect(code >= 4400 || code === 1006, `撤销源后事件流的关闭码 ${code}`);
      const gone = await viaRelay(owner, "GET", "/api/workspaces");
      expect(
        [404, 410, 403, 401].includes(gone.status),
        `撤销后的经中继请求被拒：${gone.status}`,
      );
      const sources = ok(
        await v1("GET", "/v1/me/sources", undefined, state.relayAccess),
        "me.sources",
      );
      expectEqual((sources.sources ?? sources).length, 0, "源列表清空");
      await until(
        "本机登记随之撤销（tunnel 不再 ready）",
        async () => {
          const status = await adminCall("GET", "/api/identity/cloud");
          return (status.body.registrations ?? []).length === 0;
        },
        60_000,
        1000,
      );
    },
  );

  await report.step("日志里没有任何秘密", async () => {
    const all = await compose(["logs", "--no-color", "--no-log-prefix"]);
    // 服务器壳启动时按设计在日志里打印一次配对链接（给运维取票用，两分钟、一次性），
    // 只有这一行不算；其余行里出现任何已登记的秘密都是泄漏。
    const lines = `${all.stdout}\n${all.stderr}`
      .split("\n")
      .filter((line) => !line.includes("armadra-server pairing"));
    const entries = secretEntries();
    for (const [value, label] of entries) {
      const hit = lines.find((line) => line.includes(value));
      expect(
        hit === undefined,
        `容器日志泄漏了「${label}」：${redact(hit ?? "").slice(0, 160)}`,
      );
    }
    return `查了 ${entries.length} 个秘密、${lines.length} 行日志`;
  });
}

async function teardown() {
  for (const socket of wsToClose) {
    try {
      socket.terminate();
    } catch {
      // 已经关了。
    }
  }
  if (!keep) {
    await compose(["down", "-v", "--remove-orphans", "-t", "5"], {
      allowFail: true,
    }).catch(() => undefined);
    rmSync(secretsDir, { recursive: true, force: true });
  } else {
    log(
      `保留现场：docker compose -p ${project} -f deploy/e2e/compose.yml down -v`,
    );
  }
}

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    teardown().finally(() => process.exit(130));
  });
}

log(`armadra-cloud e2e（personal），项目 ${project}`);
try {
  await main();
} catch (error) {
  failure = error;
} finally {
  await teardown();
  mkdirSync(output, { recursive: true });
  const result = {
    mode: "personal",
    project,
    ok: failure === undefined,
    totalMs: Date.now() - report.startedAt,
    steps: report.steps,
    ...(failure
      ? { error: redact(failure instanceof Error ? failure.message : failure) }
      : {}),
  };
  writeFileSync(
    join(output, "result.json"),
    `${JSON.stringify(result, null, 2)}\n`,
  );
  log(
    failure
      ? `失败：${result.error}（用时 ${result.totalMs} ms，${join(output, "result.json")}）`
      : `全部通过（用时 ${result.totalMs} ms，${join(output, "result.json")}）`,
  );
}
process.exit(failure ? 1 : 0);
