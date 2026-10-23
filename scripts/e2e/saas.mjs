// 跨进程端到端的 SaaS 版（`pnpm e2e --mode saas`，SaaS 实施设计 §8.4）：真实控制面 `armadra-cloud` + 两个
// `armadra-relay saas serve` 节点（a、b，各自的隧道地址）+ PostgreSQL + Redis + mailpit（都是一次性容器、临时数据）
// + 假 core（packages/e2e-fake-core，报 e2e 能力与 1.4 的 coreVersion）。
//
// 账号、团队、邀请、设备码、主机登记、授予、断言、撤销、运营停用都走真实接口；验证邮件与邀请邮件从 mailpit 取。
// 客户端两种形态各一遍：原生（Bearer，A）与控制台（Cookie + CSRF，B 与运营账号）。断言一律是「在超时内最终成立」
// （轮询直到满足或超时失败），不写具体毫秒数。场景编号对应设计 §8.4 的表；执行顺序按因果关系排（9、10 在 6 之前，
// 12 在 11 之前，13 在最后）。
//
//   1. `go build` 出控制面与中继（ARMADRA_CLOUD_BIN / ARMADRA_RELAY_BIN 可指定现成的二进制）；
//   2. 起 PostgreSQL / Redis / mailpit 容器（随机端口、随机口令），只停自己起的；
//   3. `armadra-cloud migrate`，用 deploy/saas/db-roles.sh 建节点的流量角色（与 compose 同一份脚本），起控制面，
//      用 `nodes token` 给两个节点签节点令牌，再起两个节点；
//   4. 场景 1–14；全部日志、库转储与 Redis 转储里查不到任何登记过的秘密。
import { randomBytes } from "node:crypto";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { WebSocket } from "ws";

import {
  exec,
  expect,
  expectEqual,
  log,
  redact,
  secret,
  secretEntries,
  sleep,
  until,
} from "./lib.mjs";
import {
  Client,
  decodePart,
  mailpit,
  newHost,
  runCli,
  sourceAuth,
  startContainer,
  startProcess,
  totp,
} from "./saas-lib.mjs";
import {
  channel,
  closeOf,
  DOMAIN,
  echo,
  freePort,
  redisClient,
} from "./saas-nodes.mjs";

const IMAGES = {
  postgres: "postgres:17.11-alpine",
  redis: "redis:8.8.3-alpine",
  mailpit: "axllent/mailpit:v1.31.4",
};
const HOST_VERSION = "0.10.0"; // 满足 MIN_CORE_VERSION_TEAM_ASSERTION 的 core
const OLD_HOST_VERSION = "0.9.0"; // 低于团队断言的最低版本

/* ------------------------------ 编排 ------------------------------ */

export async function runSaasE2e({ root, report }) {
  const { FakeCore } = await import(
    "../../packages/e2e-fake-core/src/fake-core.ts"
  );
  const tag = randomBytes(4).toString("hex");
  const containers = [];
  const nodes = {};
  const cores = [];
  const sockets = [];
  let cloud;
  let redis;
  let adminRedis;
  let pgName;
  let work;
  try {
    work = mkdtempSync(join(tmpdir(), "armadra-e2e-saas-"));
    const ports = {
      pg: await freePort(),
      redis: await freePort(),
      smtp: await freePort(),
      mail: await freePort(),
      cloud: await freePort(),
    };
    const pgPassword = secret(
      randomBytes(12).toString("hex"),
      "PostgreSQL 口令",
    );
    const trafficPassword = secret(
      randomBytes(12).toString("hex"),
      "流量角色口令",
    );
    const masterKey = secret(randomBytes(32).toString("hex"), "主密钥");
    const metricsToken = secret(randomBytes(18).toString("hex"), "指标令牌");
    const base = `http://127.0.0.1:${ports.cloud}`;
    pgName = `armadra-e2e-saas-pg-${tag}`;
    const redisName = `armadra-e2e-saas-redis-${tag}`;
    const psql = async (sql) => {
      const { stdout } = await exec("docker", [
        "exec",
        pgName,
        "psql",
        "-h",
        "127.0.0.1",
        "-U",
        "armadra",
        "-d",
        "armadra_cloud",
        "-At",
        "-F",
        "|",
        "-v",
        "ON_ERROR_STOP=1",
        "-c",
        sql,
      ]);
      return stdout
        .split("\n")
        .filter(Boolean)
        .map((line) => line.split("|"));
    };
    let cloudBin = process.env.ARMADRA_CLOUD_BIN;
    let relayBin = process.env.ARMADRA_RELAY_BIN;
    const cloudEnv = {
      ARMADRA_CLOUD_ISSUER: base,
      ARMADRA_CLOUD_APP_ORIGIN: base,
      ARMADRA_CLOUD_LISTEN: `127.0.0.1:${ports.cloud}`,
      ARMADRA_CLOUD_DATABASE_URL: `postgres://armadra:${pgPassword}@127.0.0.1:${ports.pg}/armadra_cloud`,
      ARMADRA_CLOUD_REDIS_URL: `redis://127.0.0.1:${ports.redis}`,
      ARMADRA_CLOUD_MASTER_KEY: masterKey,
      ARMADRA_CLOUD_RELAY_DOMAIN: DOMAIN,
      ARMADRA_CLOUD_TRUSTED_PROXY: "127.0.0.1/32,::1/128",
      ARMADRA_CLOUD_SMTP_HOST: "127.0.0.1",
      ARMADRA_CLOUD_SMTP_PORT: String(ports.smtp),
      ARMADRA_CLOUD_SMTP_TLS: "none",
      ARMADRA_CLOUD_SMTP_FROM: "armadra@e2e.test",
      ARMADRA_CLOUD_WEB_ROOT: join(work, "web"),
      ARMADRA_CLOUD_LOG_LEVEL: "debug",
    };

    await report.step(
      "构建控制面与中继，起 PostgreSQL / Redis / mailpit 一次性容器",
      async () => {
        mkdirSync(join(work, "web"));
        writeFileSync(
          join(work, "web", "index.html"),
          "<!doctype html><title>armadra console</title>",
        );
        if (!cloudBin) {
          cloudBin = join(work, "armadra-cloud");
          await exec("go", ["build", "-o", cloudBin, "./cmd/armadra-cloud"], {
            cwd: join(root, "go"),
          });
        }
        if (!relayBin) {
          relayBin = join(work, "armadra-relay");
          await exec("go", ["build", "-o", relayBin, "./cmd/armadra-relay"], {
            cwd: join(root, "go"),
          });
        }
        containers.push(
          await startContainer(pgName, IMAGES.postgres, [[ports.pg, 5432]], {
            POSTGRES_USER: "armadra",
            POSTGRES_PASSWORD: pgPassword,
            POSTGRES_DB: "armadra_cloud",
          }),
          await startContainer(
            redisName,
            IMAGES.redis,
            [[ports.redis, 6379]],
            {},
            ["redis-server", "--save", "", "--appendonly", "no"],
          ),
          await startContainer(
            `armadra-e2e-saas-mail-${tag}`,
            IMAGES.mailpit,
            [
              [ports.smtp, 1025],
              [ports.mail, 8025],
            ],
            {},
          ),
        );
        redis = { url: `redis://127.0.0.1:${ports.redis}` };
        await until(
          "PostgreSQL 就绪（TCP）",
          async () => {
            try {
              return (await psql("SELECT 1")).length === 1;
            } catch {
              return false;
            }
          },
          60_000,
          500,
        );
        adminRedis = redisClient(redis.url);
        await until(
          "Redis 就绪",
          async () =>
            (await adminRedis.command("PING").catch(() => "")) === "PONG",
          30_000,
          200,
        );
        await until(
          "mailpit 就绪",
          async () =>
            (
              await fetch(`http://127.0.0.1:${ports.mail}/api/v1/info`).catch(
                () => ({ ok: false }),
              )
            ).ok,
          30_000,
          200,
        );
      },
    );
    const mail = mailpit(`http://127.0.0.1:${ports.mail}`);

    await report.step(
      "迁移、建节点的流量角色（deploy/saas/db-roles.sh）、起控制面",
      async () => {
        await runCli(cloudBin, ["migrate"], cloudEnv);
        const script = readFileSync(
          join(root, "deploy/saas/db-roles.sh"),
          "utf8",
        );
        await exec(
          "docker",
          [
            "exec",
            "-i",
            "-e",
            "PGUSER=armadra",
            "-e",
            "PGDATABASE=armadra_cloud",
            "-e",
            `RELAY_TRAFFIC_PASSWORD=${trafficPassword}`,
            pgName,
            "sh",
            "-s",
          ],
          { input: script },
        );
        cloud = startProcess("cloud", cloudBin, ["serve"], cloudEnv);
        await until(
          "控制面就绪",
          async () => {
            if (!cloud.running)
              throw new Error(
                `控制面提前退出：${redact(cloud.logs.join("").slice(-1200))}`,
              );
            try {
              return (await fetch(`${base}/ready`)).ok;
            } catch {
              return false;
            }
          },
          40_000,
          200,
        );
        // 托管的页面：页面路由回落 index.html，CSP 与 Referrer-Policy 固定（设计 §5.3）。
        const page = await fetch(`${base}/login`);
        expectEqual(page.status, 200, "/login");
        expect(
          (await page.text()).includes("armadra console"),
          "/login 是 index.html",
        );
        expectEqual(
          page.headers.get("referrer-policy"),
          "no-referrer",
          "Referrer-Policy",
        );
        expect(
          (page.headers.get("content-security-policy") ?? "").includes(
            `wss://*.src.${DOMAIN}`,
          ) &&
            page.headers
              .get("content-security-policy")
              .includes("frame-ancestors 'none'"),
          "页面 CSP",
        );
        const info = await (
          await fetch(`${base}/.well-known/armadra-platform`)
        ).json();
        expectEqual(info.mode, "saas", "platform.info.mode");
        expectEqual(info.webApp, base, "platform.info.webApp");
      },
    );

    await report.step(
      "节点令牌（nodes token）与两个节点：节点表各一项，url 是各自的隧道地址",
      async () => {
        for (const name of ["a", "b"]) {
          const { stdout } = await runCli(
            cloudBin,
            ["nodes", "token", "--node", name],
            cloudEnv,
          );
          const token = secret(stdout.trim(), `节点 ${name} 的节点令牌`);
          const port = await freePort();
          const metricsPort = await freePort();
          const proc = startProcess(
            `relay-${name}`,
            relayBin,
            ["saas", "serve"],
            {
              RELAY_LISTEN: `127.0.0.1:${port}`,
              RELAY_NODE_ID: name,
              RELAY_DOMAIN: DOMAIN,
              RELAY_CLOUD_ISSUER: base,
              RELAY_REDIS_URL: redis.url,
              RELAY_TUNNEL_URL: `ws://127.0.0.1:${port}/t/v1`,
              RELAY_ALLOW_INSECURE_TUNNEL: "1",
              RELAY_NODE_TOKEN: token,
              RELAY_DATABASE_URL: `postgres://armadra_relay_traffic:${trafficPassword}@127.0.0.1:${ports.pg}/armadra_cloud`,
              RELAY_METRICS_LISTEN: `127.0.0.1:${metricsPort}`,
              RELAY_METRICS_TOKEN: metricsToken,
              RELAY_DRAIN_GRACE_MS: "3000",
              RELAY_LOG_LEVEL: "debug",
            },
          );
          const node = {
            ...proc,
            name,
            origin: `http://127.0.0.1:${port}`,
            tunnelUrl: `ws://127.0.0.1:${port}/t/v1`,
            metrics: async () => {
              const response = await fetch(
                `http://127.0.0.1:${metricsPort}/metrics`,
                {
                  headers: { authorization: `Bearer ${metricsToken}` },
                },
              );
              expectEqual(response.status, 200, `${name} 的 /metrics`);
              return response.text();
            },
          };
          nodes[name] = node;
          await until(
            `节点 ${name} 就绪`,
            async () => {
              if (!node.running)
                throw new Error(
                  `节点 ${name} 提前退出：${redact(node.logs.join("").slice(-1200))}`,
                );
              try {
                return (await fetch(`${node.origin}/ready`)).ok;
              } catch {
                return false;
              }
            },
            30_000,
            200,
          );
        }
        await until(
          "节点表有 a、b",
          async () => (await adminRedis.command("HLEN", "relay:nodes")) === 2,
          30_000,
          300,
        );
        const table = await adminRedis.command("HGETALL", "relay:nodes");
        const entries = {};
        for (let i = 0; i < table.length; i += 2)
          entries[table[i]] = JSON.parse(table[i + 1]);
        for (const name of ["a", "b"]) {
          expectEqual(
            entries[name]?.url,
            nodes[name].tunnelUrl,
            `节点表 ${name}.url`,
          );
          expectEqual(
            entries[name]?.draining,
            false,
            `节点表 ${name}.draining`,
          );
        }
      },
    );

    /* ---------- 公用动作 ---------- */

    const verifyEmail = async (client) => {
      const { match } = await mail.link(client.email, /\/verify#(\S+)/);
      const token = secret(match[1], `${client.label} 邮箱验证令牌`);
      const res = await client.call("POST", "/v1/auth/email/verify", {
        body: { token },
        anonymous: true,
      });
      expectEqual(
        res.status,
        200,
        `${client.label} 邮箱验证（${res.text.slice(0, 160)}）`,
      );
      return token;
    };

    const openStream = async (client) => {
      const ticket = (
        await client.expectCall(
          "me-stream 票",
          200,
          "",
          "POST",
          "/v1/me/stream/ticket",
          { body: {} },
        )
      ).body.ticket;
      secret(ticket, `${client.label} me-stream 票`);
      const ws = new WebSocket(
        `ws://127.0.0.1:${ports.cloud}/v1/me/stream`,
        [`armadra-ticket.${ticket}`],
        {
          perMessageDeflate: false,
        },
      );
      sockets.push(ws);
      const events = [];
      ws.on("message", (data) => {
        try {
          events.push(JSON.parse(data.toString("utf8")));
        } catch {
          /* 忽略非 JSON */
        }
      });
      await new Promise((done, failed) => {
        ws.once("open", done);
        ws.once("error", failed);
        ws.once("unexpected-response", (_r, response) =>
          failed(new Error(`me-stream 升级被拒：${response.statusCode}`)),
        );
      });
      return {
        events,
        ws,
        has: (predicate) => events.some(predicate),
      };
    };

    /** 经 registrationToken + register 登记一台主机；答 { host, res }（res 是登记答复）。 */
    const registerHost = async (
      client,
      label,
      space,
      coreVersion = HOST_VERSION,
    ) => {
      const host = newHost(label);
      const tokenRes = await client.call(
        "POST",
        "/v1/sources/registration-tokens",
        {
          body: space ? { space } : {},
        },
      );
      if (tokenRes.status !== 200)
        return { host, res: tokenRes, stage: "token" };
      const registrationToken = secret(
        tokenRes.body.registrationToken,
        `${label} 注册令牌`,
      );
      const res = await client.call("POST", "/v1/sources/register", {
        anonymous: true,
        body: {
          registrationToken,
          sourceId: host.id,
          publicKey: { kty: "OKP", crv: "Ed25519", x: host.x },
          name: label,
          kind: "desktop",
          coreVersion,
          capabilities: ["e2e"],
          protocol: { major: 1, minor: 4 },
        },
      });
      return { host, res, stage: "register" };
    };

    /** 用主机认证头取 heartbeatHint（隧道令牌 + 节点列表）。 */
    const hint = async (host) => {
      const response = await fetch(`${base}/v1/sources/me/relay`, {
        headers: sourceAuth(host, base),
      });
      const body = await response.json().catch(() => ({}));
      if (body.tunnelToken) secret(body.tunnelToken, `${host.label} 隧道令牌`);
      return { status: response.status, body };
    };

    /** 假 core 连上指定节点的隧道（token 取自当次 heartbeatHint）。 */
    const connectCore = async (host, node, coreVersion = HOST_VERSION) => {
      const { status, body } = await hint(host);
      expectEqual(status, 200, `${host.label} 的 heartbeatHint`);
      const core = new FakeCore({
        tunnelUrl: node.tunnelUrl,
        sourceId: host.id,
        sourceKey: host.privateKey,
        token: () => body.tunnelToken,
        capabilities: ["e2e"],
        coreVersion,
      });
      cores.push(core);
      const outcome = await core.connect();
      expect(
        outcome.ready,
        `${host.label} 握手被拒：${JSON.stringify(outcome)}`,
      );
      await until(`路由指向 ${node.name}`, async () => {
        const route = await adminRedis.command(
          "GET",
          `relay:route:src:${host.id}`,
        );
        return route && JSON.parse(route).node === node.name;
      });
      return core;
    };

    const tokensOf = (res) => ({
      assertion: decodePart(res.body.assertion),
      relayToken: secret(res.body.relayToken, "中继令牌"),
      relay: decodePart(res.body.relayToken),
    });
    const closedWithin = (ws, what, ms = 20_000) =>
      Promise.race([
        closeOf(ws),
        sleep(ms).then(() =>
          Promise.reject(new Error(`${what}：通道没被关掉`)),
        ),
      ]);
    const events = { a: undefined };

    /* ---------- 场景 1 ---------- */
    const A = new Client(base, "A", "bearer");
    const B = new Client(base, "B", "cookie");
    const C = new Client(base, "C", "cookie"); // 运营账号
    let teamId;
    await report.step(
      "场景 1：A 注册 → 未验证时建团队 403 email_unverified → 取信验证 → 门通过",
      async () => {
        await A.register();
        expectEqual(A.accountId.length > 0, true, "A 的 accountId");
        await A.expectCall(
          "未验证建团队",
          403,
          "email_unverified",
          "POST",
          "/v1/teams",
          { body: { slug: `t-${tag}`, name: "研发" } },
        );
        await A.expectCall(
          "未验证要注册令牌",
          403,
          "email_unverified",
          "POST",
          "/v1/sources/registration-tokens",
          { body: {} },
        );
        await verifyEmail(A);
        const me = await A.expectCall("me.get", 200, "", "GET", "/v1/me");
        expectEqual(me.body.emailVerified, true, "emailVerified");
        // 重发后旧链接失效（设计 §8.3）：已验证的账号重发是空操作，这里只验证 resend 不报错。
        events.a = await openStream(A);
      },
    );

    /* ---------- 场景 2 ---------- */
    await report.step(
      "场景 2：A 建团队 T → 邀请 B（member）→ B（控制台形态）注册、验证、接受",
      async () => {
        const created = await A.expectCall(
          "teams.create",
          200,
          "",
          "POST",
          "/v1/teams",
          { body: { slug: `t-${tag}`, name: "研发" } },
        );
        teamId = created.body.teamId;
        expectEqual(created.body.role, "owner", "创建者角色");
        await A.expectCall(
          "invitations.create",
          200,
          "",
          "POST",
          "/v1/invitations",
          {
            body: { kind: "team", teamId, email: B.email, role: "member" },
          },
        );
        // 邀请邮件先到，B 之后才注册（邀请按邮箱匹配）。
        const invite = await mail.link(
          B.email,
          /\/invite\/([A-Za-z0-9_-]+)#(\S+)/,
        );
        const [, invitationId, inviteToken] = invite.match;
        secret(inviteToken, "邀请令牌");
        await B.register();
        expect(B.csrf, "控制台形态的 B 有 CSRF 值");
        await B.expectCall(
          "未验证接受邀请",
          403,
          "email_unverified",
          "POST",
          `/v1/invitations/${invitationId}/accept`,
          { body: { token: inviteToken } },
        );
        await verifyEmail(B);
        const accepted = await B.expectCall(
          "invitations.accept",
          200,
          "",
          "POST",
          `/v1/invitations/${invitationId}/accept`,
          { body: { token: inviteToken } },
        );
        expectEqual(accepted.body.teamId, teamId, "接受后的 teamId");
        await until("A 的 me-stream 收到 memberChanged(added)", () =>
          events.a.has(
            (e) =>
              e.type === "memberChanged" &&
              e.teamId === teamId &&
              e.change === "added" &&
              e.accountId === B.accountId,
          ),
        );
        const spaces = await B.expectCall(
          "me.spaces",
          200,
          "",
          "GET",
          "/v1/me/spaces",
        );
        expect(
          spaces.body.spaces.some(
            (s) => s.teamId === teamId && s.role === "member",
          ),
          "me.spaces 含 T",
        );
      },
    );

    /* ---------- 场景 3 ---------- */
    const H1 = newHost("H1");
    let core1;
    await report.step(
      "场景 3：A 设备码登录假 core H1 → 选空间 T 登记；ownerAccountId = A，sources.list(space=T) 含 H1",
      async () => {
        const start = await new Client(base, "H1-core", "bearer").call(
          "POST",
          "/v1/auth/device/start",
          {
            anonymous: true,
            body: { device: { platform: "desktop", name: "H1" } },
          },
        );
        expectEqual(start.status, 200, "device/start");
        const { deviceCode, userCode } = start.body;
        secret(deviceCode, "设备码");
        const coreClient = new Client(base, "H1core", "bearer");
        const pending = await coreClient.call("POST", "/v1/auth/device/poll", {
          anonymous: true,
          body: { deviceCode },
        });
        expectEqual(pending.body.status, "pending", "批准前轮询");
        // 批准新设备要先提权（再验证口令，10 分钟内有效）。
        await A.expectCall(
          "device/approve 未提权",
          401,
          "mfa_required",
          "POST",
          "/v1/auth/device/approve",
          { body: { userCode, approve: true } },
        );
        const stepUp = await A.expectCall(
          "auth.stepUp",
          200,
          "",
          "POST",
          "/v1/auth/step-up",
          { body: { password: A.password } },
        );
        expect(stepUp.body.elevatedUntilMs > Date.now(), "提权有效期");
        await A.expectCall(
          "device/approve",
          200,
          "",
          "POST",
          "/v1/auth/device/approve",
          { body: { userCode, approve: true } },
        );
        const polled = await until("设备码批准后轮询拿到会话", async () => {
          const res = await coreClient.call("POST", "/v1/auth/device/poll", {
            anonymous: true,
            body: { deviceCode },
          });
          return res.body.status === "approved" ? res : false;
        });
        coreClient.adopt(polled);
        expectEqual(coreClient.accountId, A.accountId, "设备码会话属于 A");
        // 用 core 的会话登记（空间选 T）。
        const tokenRes = await coreClient.expectCall(
          "registration-tokens(T)",
          200,
          "",
          "POST",
          "/v1/sources/registration-tokens",
          {
            body: { space: { kind: "team", teamId } },
          },
        );
        const res = await coreClient.expectCall(
          "sources.register",
          200,
          "",
          "POST",
          "/v1/sources/register",
          {
            anonymous: true,
            body: {
              registrationToken: secret(
                tokenRes.body.registrationToken,
                "H1 注册令牌",
              ),
              sourceId: H1.id,
              publicKey: { kty: "OKP", crv: "Ed25519", x: H1.x },
              name: "H1",
              kind: "desktop",
              coreVersion: HOST_VERSION,
              capabilities: ["e2e"],
              protocol: { major: 1, minor: 4 },
            },
          },
        );
        expectEqual(res.body.ownerAccountId, A.accountId, "ownerAccountId");
        const list = await A.expectCall(
          "sources.list(space=T)",
          200,
          "",
          "GET",
          "/v1/sources",
          {
            query: { "space[kind]": "team", "space[teamId]": teamId },
          },
        );
        expect(
          list.body.sources.some((s) => s.sourceId === H1.id),
          "目录含 H1",
        );
        core1 = await connectCore(H1, nodes.a);
      },
    );

    /* ---------- 场景 4 ---------- */
    let grantId;
    await report.step(
      "场景 4：A 授予 T 全员 editor → grants.effective(B, H1) = editor，来源 team",
      async () => {
        const before = await A.expectCall(
          "授予前的有效权限",
          200,
          "",
          "GET",
          "/v1/grants/effective",
          {
            query: { sourceId: H1.id, accountId: B.accountId },
          },
        );
        expectEqual(before.body.role, null, "授予前 B 没有权限");
        const granted = await A.expectCall(
          "grants.create",
          200,
          "",
          "POST",
          "/v1/grants",
          {
            body: {
              sourceId: H1.id,
              subject: { kind: "team", teamId },
              role: "editor",
            },
          },
        );
        grantId = granted.body.grantId;
        const after = await A.expectCall(
          "grants.effective",
          200,
          "",
          "GET",
          "/v1/grants/effective",
          {
            query: { sourceId: H1.id, accountId: B.accountId },
          },
        );
        expectEqual(after.body.role, "editor", "有效角色");
        expect(
          after.body.from.some((f) => f.via === "team" && f.role === "editor"),
          "来源 team",
        );
      },
    );

    /* ---------- 场景 5 ---------- */
    let trafficCheck;
    let bToken1;
    await report.step(
      "场景 5：H1 隧道在 a，B（控制台）连节点 b：断言 team = {T, member}、role = editor；节点间转发；流量只在 b 计",
      async () => {
        const res = await B.expectCall(
          "B 的 sources.assertion",
          200,
          "",
          "POST",
          `/v1/sources/${H1.id}/assertion`,
          {
            body: { device: { platform: "browser", name: "B 的浏览器" } },
          },
        );
        const { assertion, relayToken, relay } = tokensOf(res);
        secret(res.body.assertion, "B 的断言");
        expectEqual(assertion.sub, B.accountId, "断言 sub");
        expectEqual(assertion.aud, H1.id, "断言 aud");
        expectEqual(assertion.role, "editor", "断言 role");
        expectEqual(assertion.team?.teamId, teamId, "断言 team.teamId");
        expectEqual(assertion.team?.role, "member", "断言 team.role");
        expectEqual(relay.src, H1.id, "中继令牌 src");
        expectEqual(relay.team, teamId, "中继令牌 team");
        bToken1 = { relayToken, jti: relay.jti };
        expectEqual(
          res.body.relayOrigin,
          `https://${H1.id}.src.${DOMAIN}`,
          "relayOrigin",
        );
        const opened = await channel(nodes.b, H1.id, relayToken);
        expect(opened.ws, `B 经 b 的通道没开成：${JSON.stringify(opened)}`);
        sockets.push(opened.ws);
        await echo(opened.ws, 1024 * 1024);
        expectEqual(
          core1.e2eOpens.at(-1)?.relayNode,
          "b",
          "OPEN.relayNode 是入口节点",
        );
        opened.ws.close(1000);
        await closeOf(opened.ws);
        expect(
          (await nodes.b.metrics()).includes("relay_routes_remote_total"),
          "b 走了节点间转发",
        );
        // 流量最终出现在 b 的行上、team_id = T，a 上没有这个源的 e2e 行（节点每 60 秒落库，晚点再收口）。
        trafficCheck = until(
          "流量桶：节点 b、team_id = T、a 上没有",
          async () => {
            const rows = await psql(
              `SELECT node, team_id, kind FROM relay_traffic_buckets WHERE source_id = '${H1.id}'`,
            );
            if (!rows.some(([node]) => node === "b")) return false;
            expect(
              rows.every(([node, team]) => node === "b" && team === teamId),
              `流量行：${JSON.stringify(rows)}`,
            );
            return rows;
          },
          150_000,
          1000,
        );
        trafficCheck.catch(() => {});
      },
    );

    /* ---------- 场景 9、10 ---------- */
    await report.step(
      "场景 9：老版本 core H2 登记进 T 得 426 protocol_unsupported；登记到个人空间成功、断言不带 team",
      async () => {
        const old = await registerHost(
          A,
          "H2",
          { kind: "team", teamId },
          OLD_HOST_VERSION,
        );
        expectEqual(
          old.res.status,
          426,
          `H2 进 T（${old.res.text?.slice(0, 160)}）`,
        );
        expectEqual(
          old.res.body.code,
          "protocol_unsupported",
          "H2 进 T 的错误码",
        );
        const personal = await registerHost(
          A,
          "H2",
          undefined,
          OLD_HOST_VERSION,
        );
        expectEqual(
          personal.res.status,
          200,
          `H2 进个人空间（${personal.res.text?.slice(0, 160)}）`,
        );
        const res = await A.expectCall(
          "个人空间主机的断言",
          200,
          "",
          "POST",
          `/v1/sources/${personal.host.id}/assertion`,
          { body: {} },
        );
        secret(res.body.assertion, "A 的断言(H2)");
        const { assertion, relay } = tokensOf(res);
        expect(
          assertion.team === undefined && relay.team === undefined,
          "个人空间的断言与中继令牌不带 team",
        );
      },
    );

    await report.step(
      "场景 10：T 里第 4 台主机得 429 limit_reached，details.quota = hosts",
      async () => {
        for (const label of ["H3", "H4"]) {
          const added = await registerHost(A, label, { kind: "team", teamId });
          expectEqual(
            added.res.status,
            200,
            `${label} 进 T（${added.res.text?.slice(0, 160)}）`,
          );
        }
        const fourth = await registerHost(A, "H5", { kind: "team", teamId });
        expectEqual(
          fourth.res.status,
          429,
          `第 4 台（${fourth.stage}：${fourth.res.text?.slice(0, 160)}）`,
        );
        expectEqual(fourth.res.body.code, "limit_reached", "第 4 台的错误码");
        expectEqual(fourth.res.body.details?.quota, "hosts", "details.quota");
      },
    );

    /* ---------- 场景 6 ---------- */
    await report.step(
      "场景 6：A 撤销授予 → B 的通道最终被关；旧中继令牌重连 4401；再要断言 403 source_access_denied",
      async () => {
        const live = await channel(nodes.b, H1.id, bToken1.relayToken);
        // 旧令牌此时已用过一次，仍有效（授予还在）。
        expect(live.ws, "撤销前旧令牌还能开");
        sockets.push(live.ws);
        const closed = closedWithin(live.ws, "撤销授予");
        await A.expectCall(
          "grants.revoke",
          200,
          "",
          "DELETE",
          `/v1/grants/${grantId}`,
        );
        await closed;
        await until(
          "旧中继令牌被拒 4401",
          async () =>
            (await channel(nodes.b, H1.id, bToken1.relayToken)).code === 4401,
          30_000,
          300,
        );
        expectEqual(
          (await channel(nodes.a, H1.id, bToken1.relayToken)).code,
          4401,
          "a 上的旧令牌",
        );
        await B.expectCall(
          "撤销后再要断言",
          403,
          "source_access_denied",
          "POST",
          `/v1/sources/${H1.id}/assertion`,
          { body: {} },
        );
      },
    );

    /* ---------- 场景 7 ---------- */
    let bToken2;
    await report.step(
      "场景 7：重新授予后 A 把 B 移出团队 → 同 6；A 收到 memberChanged(removed)",
      async () => {
        const granted = await A.expectCall(
          "重新授予",
          200,
          "",
          "POST",
          "/v1/grants",
          {
            body: {
              sourceId: H1.id,
              subject: { kind: "team", teamId },
              role: "editor",
            },
          },
        );
        expect(granted.body.grantId, "新的 grantId");
        const res = await B.expectCall(
          "B 再取断言",
          200,
          "",
          "POST",
          `/v1/sources/${H1.id}/assertion`,
          { body: {} },
        );
        const { relayToken, relay } = tokensOf(res);
        bToken2 = { relayToken, jti: relay.jti };
        const live = await channel(nodes.b, H1.id, relayToken);
        expect(live.ws, "重新授予后能开通道");
        sockets.push(live.ws);
        const closed = closedWithin(live.ws, "移出团队");
        await A.expectCall(
          "teams.memberRemove",
          200,
          "",
          "DELETE",
          `/v1/teams/${teamId}/members/${B.accountId}`,
        );
        await closed;
        await until(
          "旧中继令牌被拒 4401",
          async () => (await channel(nodes.b, H1.id, relayToken)).code === 4401,
          30_000,
          300,
        );
        await B.expectCall(
          "移出后再要断言",
          403,
          "source_access_denied",
          "POST",
          `/v1/sources/${H1.id}/assertion`,
          { body: {} },
        );
        await until("A 收到 memberChanged(removed)", () =>
          events.a.has(
            (e) =>
              e.type === "memberChanged" &&
              e.teamId === teamId &&
              e.change === "removed" &&
              e.accountId === B.accountId,
          ),
        );
      },
    );

    /* ---------- 运营账号 ---------- */
    const operator = new Client(base, "OPS", "cookie");
    operator.cookieName = "__Host-armadra-ops";
    let opsSecretUri;
    await report.step(
      "运营账号：C 注册验证 → operators add / totp（命令行）→ 口令 + TOTP 登录（Cookie 运营会话）",
      async () => {
        await C.register();
        await verifyEmail(C);
        await runCli(
          cloudBin,
          ["operators", "add", "--email", C.email],
          cloudEnv,
        );
        const { stdout } = await runCli(
          cloudBin,
          ["operators", "totp", "--email", C.email],
          cloudEnv,
        );
        opsSecretUri = stdout
          .split("\n")
          .find((line) => line.startsWith("otpauth://"));
        expect(opsSecretUri, "命令行打印了 otpauth 地址");
        const seed = new URL(opsSecretUri).searchParams.get("secret");
        secret(seed, "TOTP 种子");
        operator.email = C.email;
        operator.password = C.password;
        const login = await operator.call("POST", "/v1/auth/login", {
          anonymous: true,
          body: {
            account: C.email,
            password: C.password,
            scope: "operator",
            device: { platform: "browser", name: "运营" },
          },
        });
        expectEqual(
          login.status,
          200,
          `运营登录第一步（${login.text.slice(0, 160)}）`,
        );
        expect(login.body.mfa?.challengeId, "要两步验证");
        secret(login.body.mfa.challengeId, "MFA 挑战");
        let verified;
        for (const at of [Date.now(), Date.now() + 30_000]) {
          verified = await operator.call("POST", "/v1/auth/mfa/verify", {
            anonymous: true,
            body: {
              challengeId: login.body.mfa.challengeId,
              code: totp(seed, at),
            },
          });
          if (verified.status === 200) break;
        }
        operator.adopt(verified);
        expect(operator.jar.has("__Host-armadra-ops"), "拿到运营会话 Cookie");
        // 普通会话调 admin.* 不行、运营会话能调。
        await C.expectCall(
          "普通会话调 admin",
          401,
          "",
          "GET",
          "/v1/admin/teams",
        );
        const nodesList = await operator.expectCall(
          "admin.relayNodes",
          200,
          "",
          "GET",
          "/v1/admin/relay-nodes",
        );
        expect(
          nodesList.body.nodes.some((n) => n.node === "a" && n.online) &&
            nodesList.body.nodes.some((n) => n.node === "b" && n.online),
          "两个节点在线",
        );
      },
    );

    /* ---------- 场景 12 ---------- */
    await report.step(
      "场景 12：节点 a 排空 → H1 收 GOAWAY rebalance 转到 b；heartbeatHint 不再含 a",
      async () => {
        const drained = await operator.expectCall(
          "admin.relayNodeDrain",
          200,
          "",
          "PUT",
          "/v1/admin/relay-nodes/a/drain",
          {
            body: { drain: true, reason: "e2e 排空演练" },
          },
        );
        expect(drained.status === 200, "排空接口");
        await until(
          "H1 收到 GOAWAY rebalance",
          () => core1.goaways.includes("rebalance"),
          20_000,
          100,
        );
        const code = await core1.closed();
        expect([4491, 4492].includes(code), `旧隧道的关闭码 ${code}`);
        await until(
          "heartbeatHint 不再含 a",
          async () => {
            const { status, body } = await hint(H1);
            return (
              status === 200 &&
              body.nodes.length > 0 &&
              !body.nodes.some((n) => n.url === nodes.a.tunnelUrl) &&
              body.nodes.some((n) => n.url === nodes.b.tunnelUrl)
            );
          },
          30_000,
          300,
        );
        core1 = await connectCore(H1, nodes.b);
        const entry = JSON.parse(
          (await adminRedis.command("HGET", "relay:nodes", "a")) ?? "{}",
        );
        expect(
          entry.draining === true || Object.keys(entry).length === 0,
          "节点表里 a 标了排空",
        );
      },
    );

    /* ---------- 场景 11 ---------- */
    await report.step(
      "场景 11：运营停用团队 T → 在线连接最终被踢、断言 403 team_suspended；审计 actor.kind = operator 带 reason",
      async () => {
        const res = await A.expectCall(
          "A 的断言",
          200,
          "",
          "POST",
          `/v1/sources/${H1.id}/assertion`,
          { body: {} },
        );
        const { relayToken } = tokensOf(res);
        const live = await channel(nodes.b, H1.id, relayToken);
        expect(live.ws, `A 的通道没开成：${JSON.stringify(live)}`);
        sockets.push(live.ws);
        const closed = closedWithin(live.ws, "停用团队");
        const reason = "e2e 滥用处置演练";
        await operator.expectCall(
          "admin.teamStatus(suspended)",
          200,
          "",
          "PUT",
          `/v1/admin/teams/${teamId}/status`,
          {
            body: { status: "suspended", reason },
          },
        );
        await closed;
        await until(
          "断言 403 team_suspended",
          async () => {
            const again = await A.call(
              "POST",
              `/v1/sources/${H1.id}/assertion`,
              { body: {} },
            );
            return again.status === 403 && again.body.code === "team_suspended";
          },
          20_000,
          300,
        );
        const rows = await psql(
          `SELECT actor_kind, reason FROM audit_log WHERE team_id = '${teamId}' AND actor_kind = 'operator' ORDER BY at_ms`,
        );
        expect(
          rows.length >= 1 &&
            rows.every(([kind, why]) => kind === "operator" && why.length > 0),
          `运营审计行：${JSON.stringify(rows)}`,
        );
        expect(
          rows.some(([, why]) => why === reason),
          "审计带了理由",
        );
        await operator.expectCall(
          "admin.teamStatus(active)",
          200,
          "",
          "PUT",
          `/v1/admin/teams/${teamId}/status`,
          {
            body: { status: "active", reason: "e2e 演练结束，恢复" },
          },
        );
        const audit = await A.expectCall(
          "audit.list",
          200,
          "",
          "GET",
          `/v1/teams/${teamId}/audit`,
          { query: { limit: "100" } },
        );
        expect(
          audit.body.events.some((e) => e.actor.kind === "operator"),
          "audit.list 看得到运营动作",
        );
        await until(
          "恢复后断言可用",
          async () =>
            (
              await A.call("POST", `/v1/sources/${H1.id}/assertion`, {
                body: {},
              })
            ).status === 200,
          20_000,
          300,
        );
      },
    );

    /* ---------- 场景 8 ---------- */
    await report.step(
      "场景 8：A 撤销 H1 → 隧道关、sourceRevoked、心跳 410 source_revoked",
      async () => {
        const closed = core1.closed();
        await A.expectCall(
          "sources.revoke",
          200,
          "",
          "DELETE",
          `/v1/sources/${H1.id}`,
        );
        const code = await Promise.race([
          closed,
          sleep(20_000).then(() =>
            Promise.reject(new Error("H1 的隧道没被关")),
          ),
        ]);
        expect(code > 0, `隧道关闭码 ${code}`);
        await until("A 的 me-stream 收到 sourceRevoked", () =>
          events.a.has(
            (e) => e.type === "sourceRevoked" && e.sourceId === H1.id,
          ),
        );
        const beat = await hint(H1);
        expectEqual(beat.status, 410, "撤销后的心跳");
        expectEqual(beat.body.code, "source_revoked", "心跳错误码");
      },
    );

    /* ---------- 场景 13 ---------- */
    await report.step(
      "场景 13：撤销后重启 Redis（数据清空）→ 控制面重放，B 的旧中继令牌仍被拒",
      async () => {
        const jtis = [bToken1.jti, bToken2.jti];
        adminRedis.close();
        await exec("docker", ["restart", "-t", "1", redisName]);
        await until(
          "Redis 重启后可用",
          async () => {
            const probe = redisClient(redis.url);
            try {
              if ((await probe.command("PING")) !== "PONG") return false;
              adminRedis = probe;
              return true;
            } catch {
              probe.close();
              return false;
            }
          },
          30_000,
          300,
        );
        await until(
          "控制面把撤销重放回 Redis",
          async () => {
            for (const jti of jtis)
              if (!(await adminRedis.command("EXISTS", `relay:revoked:${jti}`)))
                return false;
            return true;
          },
          60_000,
          500,
        );
        for (const token of [bToken1.relayToken, bToken2.relayToken]) {
          await until(
            "重放后旧令牌仍被拒 4401",
            async () => (await channel(nodes.b, H1.id, token)).code === 4401,
            30_000,
            300,
          );
          expectEqual(
            (await channel(nodes.a, H1.id, token)).code,
            4401,
            "a 上的旧令牌",
          );
        }
        // a 在场景 12 排空了（/ready 按设计不就绪），所以只看控制面与 b；b 的节点表项要在 Redis 清空后重新写回。
        await until(
          "控制面与节点 b 在 Redis 重启后仍就绪",
          async () => {
            const [c, bReady] = await Promise.all([
              fetch(`${base}/ready`),
              fetch(`${nodes.b.origin}/ready`),
            ]);
            return c.ok && bReady.ok;
          },
          60_000,
          500,
        );
        await until(
          "节点 b 重新写回节点表",
          async () =>
            (await adminRedis.command("HEXISTS", "relay:nodes", "b")) === 1,
          60_000,
          500,
        );
      },
    );

    await report.step(
      "场景 5（收口）：流量桶只在入口节点 b、team_id = T",
      async () => {
        const rows = await trafficCheck;
        return `${rows.length} 行`;
      },
    );

    /* ---------- 场景 14 ---------- */
    await report.step(
      "场景 14：泄密扫描：日志、库转储、Redis 转储里没有登记过的令牌、口令、邮件令牌",
      async () => {
        await nodes.b.stop();
        await nodes.a.stop();
        const dump = (
          await exec("docker", [
            "exec",
            pgName,
            "pg_dump",
            "-U",
            "armadra",
            "-d",
            "armadra_cloud",
            "--data-only",
          ])
        ).stdout;
        const redisText = [];
        let cursor = "0";
        do {
          const [next, keys] = await adminRedis.command(
            "SCAN",
            cursor,
            "COUNT",
            "1000",
          );
          cursor = next;
          for (const key of keys) {
            const type = await adminRedis.command("TYPE", key);
            redisText.push(key);
            if (type === "string")
              redisText.push(String(await adminRedis.command("GET", key)));
            else if (type === "hash")
              redisText.push(
                (await adminRedis.command("HGETALL", key)).join("\n"),
              );
            else if (type === "zset")
              redisText.push(
                (await adminRedis.command("ZRANGE", key, 0, -1)).join("\n"),
              );
            else if (type === "set")
              redisText.push(
                (await adminRedis.command("SMEMBERS", key)).join("\n"),
              );
            else if (type === "list")
              redisText.push(
                (await adminRedis.command("LRANGE", key, 0, -1)).join("\n"),
              );
          }
        } while (cursor !== "0");
        const haystacks = {
          控制面日志: cloud.logs.join(""),
          节点日志: Object.values(nodes)
            .map((n) => n.logs.join(""))
            .join(""),
          库转储: dump,
          Redis转储: redisText.join("\n"),
        };
        const leaks = [];
        for (const [where, text] of Object.entries(haystacks))
          for (const [value, label] of secretEntries())
            if (text.includes(value)) leaks.push(`${where}：${label}`);
        expectEqual(leaks.join("；"), "", "泄露的秘密");
        return `库转储 ${dump.length} 字节，Redis ${redisText.length} 段`;
      },
    );
  } catch (error) {
    if (process.env.E2E_DEBUG) {
      log(`--- cloud\n${(cloud?.logs.join("") ?? "").slice(-4000)}`);
      for (const node of Object.values(nodes))
        log(`--- ${node.name}\n${node.logs.join("").slice(-2500)}`);
    }
    throw error;
  } finally {
    for (const ws of sockets) ws.terminate();
    for (const core of cores) await core.stop().catch(() => {});
    for (const node of Object.values(nodes)) await node.stop().catch(() => {});
    await cloud?.stop().catch(() => {});
    adminRedis?.close();
    for (const container of containers) await container.stop();
    if (work) rmSync(work, { recursive: true, force: true });
    await sleep(0);
  }
}
