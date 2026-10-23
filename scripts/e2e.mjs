// 跨仓端到端（C0-3）：个人中转 + Armadra 服务器壳 + Node 客户端。
//
//   pnpm e2e --mode personal [--skip-build] [--relay-image <镜像>] [--server-image <镜像>]
//                            [--output <目录>] [--keep]
//   pnpm e2e --mode personal-workers [--output <目录>]
//   pnpm e2e --mode saas [--redis-url <专用 Redis>] [--output <目录>]
//
// `personal-workers` 是本地 workerd 上的 Workers 中继（`wrangler dev --local`，不连 Cloudflare
// 账号）对假 core 与 Node 客户端，见 scripts/e2e/workers.mjs；`saas` 是三个 SaaS 中继节点 + Redis
// （节点间转发、撤销跨节点、下线再平衡），见 scripts/e2e/saas.mjs；下面的编排只属于 `personal`。
//
// 编排（deploy/e2e/compose.yml，项目名 armadra-e2e-<随机>，跑完 down -v）：
//   1. 本地构建个人中转镜像（本仓）与服务器壳镜像（Armadra 主仓，见下）；
//   2. 起中继（首次自动 init）与服务器壳；服务器壳不发布端口，只能经中继到达；
//   3. 服务器壳登记到中继（注册令牌）、隧道连上；
//   4. 客户端用账号口令登录中继 → 断言 + 中继令牌 → 开端到端通道（`_relay/e2e`，新设备由
//      服务器壳的主人核对放行）→ 经通道换 core 会话、调 HTTP API、事件流、终端收发、协作板；
//      中继上的明文路径（HTTP、预检、媒体票）一律 404；
//   5. 分享链接访客访问；撤销链接、源离线、撤销源，各自断开；
//   6. 全部日志里查不到任何秘密。
//
// Armadra 主仓：环境变量 ARMADRA_REPO（缺省 ../ai-coding-canvas）；ARMADRA_REF（缺省 origin/main）
// 用 `git archive` 取那一版构建，不动主仓的检出；ARMADRA_REF=worktree 则直接用目录现状；
// ARMADRA_FETCH=0 不先 fetch。`--server-image` 直接用现成镜像（nightly 的 compat.min）。
//
// 中继镜像缺省由 go/Dockerfile 构建（Go 实现）。对上一版 Node 镜像跑同一套用例（回滚演练），
// 不改用例，只换镜像与容器里的命令行：
//   E2E_RELAY_CLI="node /app/out/cli.js" pnpm e2e --mode personal --relay-image ghcr.io/ama-link/armadra-relay:<上一版>
//   E2E_RELAY_DOCKERFILE  构建中继镜像用的 Dockerfile（缺省 go/Dockerfile）
//   E2E_RELAY_CLI         容器里中继命令行的写法（缺省 `armadra-relay`，空格分隔）
//   已有镜像时用 --relay-image <镜像>，不构建。
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

import { ed25519SignerFromSeed } from "@armadra/platform-protocol/e2e";
import { WebSocket } from "ws";

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

  --mode personal        个人中转
  --mode personal-workers  本地 workerd 上的 Workers 中继（wrangler dev --local）对假 core
  --mode saas            三个 SaaS 中继节点 + Redis（一次性容器）对假 core：节点间转发、撤销、再平衡
  --redis-url URL        saas 用这个 Redis（必须是专用的空库），不起容器
  --skip-build           不构建镜像，用已有的 armadra-relay:e2e 与 armadra-server:e2e
  --relay-image IMAGE    用现成的中继镜像
  --server-image IMAGE   用现成的服务器壳镜像（如 ghcr.io/ama-link/armadra-server:<compat.min>）
  --output DIR           result.json 的目录，缺省 .data/e2e
  --relay-data DIR       中继的数据目录用宿主机上的 DIR（绑定挂载，跑完保留），缺省是一次性的卷；
                         升级与回滚演练用：同一个目录先后交给两个镜像（Linux 上先 chown 给 uid 10002）
  --relay-password-file F  中继账号口令文件：有就读，没有就生成（0600）；配 --relay-data 复用账号
  --relay-port N         中继端口（缺省随机）
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
      [
        "--mode",
        "--relay-image",
        "--server-image",
        "--output",
        "--relay-data",
        "--relay-password-file",
        "--relay-port",
        "--redis-url",
      ].includes(arg)
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
if (mode === "personal-workers") {
  const { runWorkersE2e } = await import("./e2e/workers.mjs");
  const workersReport = new Report();
  const workersOutput = resolve(
    root,
    options.values.get("--output") ?? ".data/e2e",
  );
  log("armadra-cloud e2e（personal-workers）");
  let workersFailure;
  try {
    await runWorkersE2e({ root, report: workersReport });
  } catch (error) {
    workersFailure = error;
  }
  mkdirSync(workersOutput, { recursive: true });
  const result = {
    mode,
    ok: workersFailure === undefined,
    totalMs: Date.now() - workersReport.startedAt,
    steps: workersReport.steps,
    ...(workersFailure
      ? {
          error: redact(
            workersFailure instanceof Error
              ? workersFailure.message
              : workersFailure,
          ),
        }
      : {}),
  };
  writeFileSync(
    join(workersOutput, "result.json"),
    `${JSON.stringify(result, null, 2)}\n`,
  );
  log(
    workersFailure
      ? `失败：${result.error}（用时 ${result.totalMs} ms）`
      : `全部通过（用时 ${result.totalMs} ms，${join(workersOutput, "result.json")}）`,
  );
  process.exit(workersFailure ? 1 : 0);
}
if (mode === "saas") {
  const { runSaasE2e } = await import("./e2e/saas.mjs");
  const saasReport = new Report();
  const saasOutput = resolve(
    root,
    options.values.get("--output") ?? ".data/e2e",
  );
  log("armadra-cloud e2e（saas）");
  let saasFailure;
  try {
    await runSaasE2e({
      root,
      report: saasReport,
      redisUrl: options.values.get("--redis-url"),
    });
  } catch (error) {
    saasFailure = error;
  }
  mkdirSync(saasOutput, { recursive: true });
  const result = {
    mode,
    ok: saasFailure === undefined,
    totalMs: Date.now() - saasReport.startedAt,
    steps: saasReport.steps,
    ...(saasFailure
      ? {
          error: redact(
            saasFailure instanceof Error ? saasFailure.message : saasFailure,
          ),
        }
      : {}),
  };
  writeFileSync(
    join(saasOutput, "result.json"),
    `${JSON.stringify(result, null, 2)}\n`,
  );
  log(
    saasFailure
      ? `失败：${result.error}（用时 ${result.totalMs} ms）`
      : `全部通过（用时 ${result.totalMs} ms，${join(saasOutput, "result.json")}）`,
  );
  process.exit(saasFailure ? 1 : 0);
}
if (mode !== "personal") {
  console.error(
    mode === undefined
      ? `需要 --mode personal\n${USAGE}`
      : `--mode ${mode}：只支持 personal、personal-workers 与 saas`,
  );
  process.exit(2);
}

const project = `armadra-e2e-${randomBytes(4).toString("hex")}`;
const composeFile = join(root, "deploy/e2e/compose.yml");
const output = resolve(root, options.values.get("--output") ?? ".data/e2e");
const relayImage = options.values.get("--relay-image") ?? "armadra-relay:e2e";
const relayDockerfile = process.env.E2E_RELAY_DOCKERFILE ?? "go/Dockerfile";
const relayCli = (process.env.E2E_RELAY_CLI ?? "armadra-relay")
  .split(" ")
  .filter(Boolean);
const serverImage =
  options.values.get("--server-image") ?? "armadra-server:e2e";
const keep = options.flags.has("--keep");
const report = new Report();

const relayPort = options.values.has("--relay-port")
  ? Number(options.values.get("--relay-port"))
  : await new Promise((done, failed) => {
      const probe = createServer();
      probe.once("error", failed);
      probe.listen(0, "127.0.0.1", () => {
        const { port } = probe.address();
        probe.close(() => done(port));
      });
    });
const relayData = options.values.has("--relay-data")
  ? resolve(options.values.get("--relay-data"))
  : undefined;
if (relayData) mkdirSync(relayData, { recursive: true });
const issuer = `https://relay:${relayPort}`;
const secretsDir = mkdtempSync(join(tmpdir(), "armadra-e2e-"));
chmodSync(secretsDir, 0o755);
// 容器里是 uid 10002：口令文件要它读得到。缺省是随机口令、只活在这一次运行里；
// 给了 --relay-password-file 就沿用那个文件（没有时生成），同一个数据目录换镜像后还能登录。
const passwordFile = options.values.get("--relay-password-file");
let passwordText = randomBytes(18).toString("base64url");
if (passwordFile) {
  if (existsSync(passwordFile))
    passwordText = readFileSync(passwordFile, "utf8").trim();
  else writeFileSync(passwordFile, passwordText, { mode: 0o600 });
}
const relayPassword = secret(passwordText, "中继账号口令");
writeFileSync(join(secretsDir, "relay_password"), relayPassword, {
  mode: 0o644,
});

const composeEnv = {
  E2E_RELAY_IMAGE: relayImage,
  E2E_SERVER_IMAGE: serverImage,
  E2E_RELAY_PORT: String(relayPort),
  E2E_SECRETS_DIR: secretsDir,
  ...(relayData ? { E2E_RELAY_DATA: relayData } : {}),
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
      ["build", "-q", "-f", relayDockerfile, "-t", relayImage, "."],
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

/* ------------------------------ 端到端通道 ------------------------------ */

// 通道的客户端（握手、记录层之上的内层复用，`fetch` 与 WebSocket 的形状），与 Armadra 页面
// 那一份同形；字节编解码全在协议包。
const { E2eChannelError, E2eClientChannel, channelFetch, channelWebSocket } =
  await import("./e2e/channel/index.ts");

/** 底层 WebSocket：信任中继的自签 CA，主机名一律解析到回环（compose 里的 `relay`）。 */
class ChannelSocket extends WebSocket {
  constructor(url, protocols) {
    super(url, protocols, {
      ca: caPem,
      perMessageDeflate: false,
      lookup: (_host, options, callback) =>
        options?.all
          ? callback(null, [{ address: "127.0.0.1", family: 4 }])
          : callback(null, "127.0.0.1", 4),
    });
  }
}

/** 中继拒绝开通道的关闭码 → 等价的 HTTP 拒绝（给只看状态码的步骤用）。 */
const REFUSED = {
  4401: [401, "relay_token_invalid"],
  4403: [403, "relay_source_mismatch"],
  4404: [503, "source_offline"],
  4429: [429, "rate_limited"],
};

/** 开一条通道（不缓存）；被拒抛 `E2eChannelError`（带中继的关闭码）。 */
async function openChannel(client, relayToken = client.relayToken) {
  client.device ??= await ed25519SignerFromSeed(randomBytes(32));
  return E2eClientChannel.open({
    url: `${client.baseUrl.replace(/^https:/, "wss:")}/_relay/e2e`,
    relayToken,
    sourceId: state.sourceId,
    device: client.device,
    platform: "desktop",
    name: client.name ?? "e2e",
    WebSocket: ChannelSocket,
    pingIntervalMs: 0,
  });
}

/** 这个客户端开着的通道；没有、关了或换了中继令牌就重开。 */
async function channelOf(client) {
  const current = client.channel;
  if (
    current &&
    client.channelToken === client.relayToken &&
    !current.closed &&
    !current.expired
  )
    return current;
  current?.close();
  client.channel = await openChannel(client);
  client.channelToken = client.relayToken;
  return client.channel;
}

/** 经通道打 core（客户端视角）；中继拒绝开通道时按关闭码答一个等价的拒绝。 */
async function viaRelay(client, method, path, body, extra = {}) {
  let channel;
  try {
    channel = await channelOf(client);
  } catch (error) {
    if (!(error instanceof E2eChannelError)) throw error;
    const [status, code] = REFUSED[error.closeCode] ?? [502, "unreachable"];
    return { status, headers: {}, text: "", body: { code }, refused: error };
  }
  const fetcher = channelFetch(channel, { origin: null, base: client.baseUrl });
  const response = await fetcher(`${client.baseUrl}${path}`, {
    method,
    headers: {
      ...(body === undefined ? {} : { "content-type": "application/json" }),
      ...(client.access ? { authorization: `Bearer ${client.access}` } : {}),
      ...extra,
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const text = await response.text();
  let parsed = {};
  try {
    parsed = text === "" ? {} : JSON.parse(text);
  } catch {
    parsed = {};
  }
  return {
    status: response.status,
    headers: Object.fromEntries(response.headers.entries()),
    text,
    body: parsed,
  };
}

async function wsTicket(client) {
  const answer = await viaRelay(client, "POST", "/api/identity/ws-ticket");
  return secret(ok(answer, "换 WS 票").ticket);
}

/**
 * 经通道升级 WebSocket（形状同 `relay.ws` 的答案）。通道断了内层流以 1006 结束；`closed()`
 * 这时答中继关通道的关闭码（4403 撤销、4404 源下线）。
 */
async function openWs(client, path) {
  const ticket = await wsTicket(client);
  const channel = await channelOf(client);
  let channelCode;
  channel.onClose((error) => {
    channelCode = error.closeCode;
  });
  const Socket = channelWebSocket(channel, {
    origin: null,
    base: client.baseUrl,
  });
  const socket = new Socket(
    `${client.baseUrl.replace(/^https:/, "wss:")}${path}`,
    [`armadra-ticket.${ticket}`],
  );
  socket.binaryType = "arraybuffer";
  const messages = [];
  socket.onmessage = (event) =>
    messages.push(
      typeof event.data === "string"
        ? Buffer.from(event.data)
        : Buffer.from(new Uint8Array(event.data)),
    );
  let closeCode;
  const closing = new Promise((done) => {
    socket.onclose = (event) => {
      closeCode = event.code;
      done(event.code);
    };
  });
  const opened = await Promise.race([
    new Promise((done) => {
      socket.onopen = () => done(true);
    }),
    closing.then(() => false),
  ]);
  if (opened)
    await Promise.race([closing, new Promise((done) => setTimeout(done, 300))]);
  const settle = async () => {
    const code = await closing;
    // 通道的关闭事件与内层流的结束谁先到不一定：等一下再看通道的码。
    await new Promise((done) => setTimeout(done, 100));
    return code === 1006 && channelCode !== undefined ? channelCode : code;
  };
  return {
    socket: {
      send: (data) => socket.send(data),
      close: () => socket.close(),
      terminate: () => socket.close(),
    },
    status: opened ? 101 : 0,
    messages,
    get closeCode() {
      return closeCode;
    },
    closed: settle,
  };
}

/**
 * 断言换会话：owner 与访客共用这一段（访客多一张邀请令牌）。这台设备第一次来时 core 答
 * `device_pending`：服务器壳的主人按通道的校验码放行，再登录一次（同一张断言）。
 */
async function coreLogin(client, assertion, invitationToken) {
  await channelOf(client);
  const send = () =>
    viaRelay({ ...client, access: "" }, "POST", "/api/identity/cloud/login", {
      assertion,
      ...(invitationToken ? { invitationToken } : {}),
    });
  let login = await send();
  if (login.status === 403 && login.body?.code === "device_pending") {
    const approved = await adminCall("POST", "/api/devices/approve", {
      pendingId: login.body.details.pendingId,
      sas: client.channel.sas,
    });
    expectEqual(
      approved.status,
      200,
      `放行新设备 ${JSON.stringify(approved.body).slice(0, 200)}`,
    );
    login = await send();
    // 账号在第一次（待确认的那次）就建好了：再登录时 `created` 是 false。
    client.approvedAfterPending = true;
  }
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
    // --relay-data：容器里是 uid 10002，Linux 上写不进当前用户的 0755 目录（macOS 的 Docker
    // Desktop 会映射属主）。用一次性容器（同一个中继镜像，root）把它交给 10002。
    if (relayData && process.platform === "linux")
      await exec("docker", [
        "run",
        "--rm",
        "--user",
        "0",
        "--entrypoint",
        "chown",
        "-v",
        `${relayData}:/data`,
        relayImage,
        "-R",
        "10002:10002",
        "/data",
      ]);
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
      ...relayCli,
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
    "断言 + 中继令牌：owner 绑定后开端到端通道（新设备由主人放行）、换 core 会话、调 HTTP API",
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
    },
  );

  await report.step(
    "明文路径已删除：带令牌的 HTTP、预检、媒体票与普通 WS 升级都由中继答 404",
    async () => {
      const plain = await relay.http("GET", `${owner.baseUrl}/api/workspaces`, {
        headers: {
          origin: issuer,
          "armadra-relay-token": owner.relayToken,
          authorization: `Bearer ${owner.access}`,
        },
      });
      expectEqual(plain.status, 404, "带令牌的明文请求");
      expectEqual(plain.body?.code, "not_found", "明文请求的错误码");
      const preflight = await relay.http(
        "OPTIONS",
        `${owner.baseUrl}/api/identity/cloud/login`,
        {
          headers: {
            origin: "https://localhost",
            "access-control-request-method": "POST",
          },
        },
      );
      expectEqual(preflight.status, 404, "预检");
      expectEqual(
        preflight.headers["access-control-allow-origin"],
        "https://localhost",
        "中继自己的 404 按名单回显来源",
      );
      const media = await relay.http(
        "POST",
        `${owner.baseUrl}/_relay/media-tickets`,
        {
          body: { path: "/api/media/x" },
          headers: { "armadra-relay-token": owner.relayToken },
        },
      );
      expectEqual(media.status, 404, "签媒体票");
      const upgrade = await relay.ws(
        `${owner.baseUrl.replace(/^https:/, "wss:")}/api/workspaces/${state.workspaceId}/events`,
        [`armadra-relay.${owner.relayToken}`, "armadra-ticket.x"],
        issuer,
      );
      expectEqual(upgrade.status, 404, "普通 WS 升级");
      return "HTTP、预检、媒体票、普通 WS 都 404";
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

  await report.step(
    "4401：坏中继令牌开不了通道，换新断言重连成功",
    async () => {
      const bad = await openChannel(owner, "not-a-relay-token").then(
        (channel) => {
          channel.close();
          return undefined;
        },
        (error) => error,
      );
      expect(bad instanceof E2eChannelError, "坏令牌的通道被拒");
      expectEqual(bad.closeCode, 4401, "坏令牌的关闭码");
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
    },
  );

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
      expectEqual(
        login.created || guest.approvedAfterPending === true,
        true,
        "访客账号是新建的（经放行的那次在待确认时就建了）",
      );
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
    "工作区范围链接（协议 1.1）：断言带 scp、中继令牌带 ro；只读访客经通道登录、读与开事件流，写被 core 挡下",
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
      const info = ok(
        await v1("GET", "/.well-known/armadra-platform"),
        "平台信息",
      );
      expect(
        (info.capabilities ?? []).includes("links.scope"),
        "中继报 links.scope",
      );
      const created = ok(
        await v1(
          "POST",
          "/v1/links",
          {
            kind: "source_invite",
            sourceId: state.sourceId,
            invitationId,
            label: "e2e-scoped",
            role: "viewer",
            expiresAtMs: Date.now() + 3_600_000,
            scope: { workspaceId: state.workspaceId, readOnly: true },
          },
          state.relayAccess,
        ),
        "links.create（带范围）",
      );
      secret(created.secret, "链接秘密");
      const landing = ok(
        await v1("GET", `/v1/links/${created.linkId}`),
        "links.get",
      );
      expectEqual(
        JSON.stringify(landing.scope),
        JSON.stringify({ workspaceId: state.workspaceId, readOnly: true }),
        "links.get.scope",
      );
      const accepted = ok(
        await v1("POST", `/v1/links/${created.linkId}/accept`, {
          secret: created.secret,
          device: { platform: "browser", name: "e2e-scoped" },
        }),
        "links.accept",
      );
      secret(accepted.assertion, "断言");
      secret(accepted.relayToken, "中继令牌");
      secret(accepted.guestSession.accessToken);
      secret(accepted.guestSession.refreshToken);
      const claims = (token) =>
        JSON.parse(Buffer.from(token.split(".")[1], "base64url").toString());
      expectEqual(
        JSON.stringify(claims(accepted.assertion).scp),
        JSON.stringify([`ws:${state.workspaceId}`, "ro"]),
        "断言 scp",
      );
      expectEqual(claims(accepted.relayToken).ro, true, "中继令牌 ro");
      const scoped = {
        relayToken: accepted.relayToken,
        baseUrl: accepted.relayBaseUrl,
        access: "",
        refresh: "",
      };
      // 只读令牌下 core 的 cloud/login（身份域 POST）照常通过。
      const login = await coreLogin(
        scoped,
        accepted.assertion,
        invitationToken,
      );
      expectEqual(
        login.created || scoped.approvedAfterPending === true,
        true,
        "范围访客账号是新建的（经放行的那次在待确认时就建了）",
      );
      const workspaces = ok(
        await viaRelay(scoped, "GET", "/api/workspaces"),
        "范围访客列工作空间",
      );
      expect(
        JSON.stringify(workspaces).includes(state.workspaceId),
        "范围访客看得到被分享的工作空间",
      );
      const write = await viaRelay(scoped, "POST", "/api/terminals", {
        workspaceId: state.workspaceId,
        cwd: "/data/e2e-ws",
        shell: "/bin/sh",
      });
      expectEqual(write.status, 403, "只读令牌的写请求");
      expectEqual(write.body?.code, "forbidden", "只读令牌的写请求 code");
      const events = await openWs(
        scoped,
        `/api/workspaces/${state.workspaceId}/events`,
      );
      wsToClose.push(events.socket);
      expectEqual(events.status, 101, "范围访客事件流升级");
      events.socket.close();
      ok(
        await v1(
          "DELETE",
          `/v1/links/${created.linkId}`,
          undefined,
          state.relayAccess,
        ),
        "撤销范围链接",
      );
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
    "源下线：通道 4404（等价 503 source_offline）；重启后恢复",
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
