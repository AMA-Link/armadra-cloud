// 跨进程端到端的 SaaS 版（`pnpm e2e --mode saas`）：三个 `armadra-relay saas serve` 节点 + 一个 Redis。
//
//   1. `go build` 出中继（或用 ARMADRA_RELAY_BIN）；Redis 缺省起一个一次性容器（跑完 `docker stop`），
//      给了 `--redis-url` 就用它（必须是专用的空库：节点的键前缀固定是 relay:）；
//   2. 测试签发者：本进程里的 Ed25519 钥，`/.well-known/jwks.json` 走 HTTP；节点令牌、隧道令牌、
//      中继令牌都由它签（控制面落地之前代替 armadra-cloud）；
//   3. 假 core（packages/e2e-fake-core）连节点 a 的隧道，客户端连节点 b（`<sourceId>.src.<域>`）：
//      通道经 `/x/v1` 转到 a，10 MB 随机字节双向原样；`/x/v1` 没有有效节点令牌一律 401；
//      慢客户端（不读的下载）不拖住同一条节点间连接上别的源；下载中途掉线 64 次之后源照常出数据；
//   4. 撤销跨节点生效：照控制面的写法写 `relay:revoked:<jti>` 并广播、发 `kick`，在线通道被踢，
//      旧令牌在每个节点上都 4401；
//   5. 持有节点下线再平衡：给 a 发 SIGTERM → 隧道收到 GOAWAY rebalance、节点表里 a 先标 draining 再删掉、
//      经 b 转发的通道 4404 → 源按节点表重连到 c → 客户端重连经 b 落到 c；
//      持有节点 c 被 SIGKILL → 经 b 的通道立即 4404（节点间连接断了）；
//   6. 指标里有 relay_routes_remote_total 等；全部节点日志里查不到任何秘密。
//
// 没有控制面，所以 heartbeatHint、按团队踢与额度由 Go 的包测试覆盖（internal/saas），这里只走线上行为。
import { spawn } from "node:child_process";
import {
  createHash,
  generateKeyPairSync,
  randomBytes,
  sign,
} from "node:crypto";
import { createServer as createHttpServer } from "node:http";
import { connect, createServer } from "node:net";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  buildSigningInput,
  joinCompactJws,
  TOKEN_TYPES,
} from "@armadra/platform-protocol/assertion";
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

const DOMAIN = "e2e.test";
const REDIS_IMAGE = "redis:8.8.3-alpine";

const freePort = () =>
  new Promise((done, failed) => {
    const probe = createServer();
    probe.once("error", failed);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => done(port));
    });
  });

/* ------------------------------ Redis（最小 RESP 客户端） ------------------------------ */

/** 连 Redis，答 `{ command(...args), close() }`；只用来扮演控制面写键、发广播、读节点表。 */
function redisClient(url) {
  const target = new URL(url);
  const socket = connect(Number(target.port || 6379), target.hostname);
  let buffer = Buffer.alloc(0);
  const waiting = [];
  const parse = (buf, at) => {
    const end = buf.indexOf("\r\n", at);
    if (end < 0) return undefined;
    const kind = String.fromCharCode(buf[at]);
    const line = buf.toString("utf8", at + 1, end);
    const next = end + 2;
    if (kind === "+") return { value: line, next };
    if (kind === "-") return { value: new Error(line), next };
    if (kind === ":") return { value: Number(line), next };
    if (kind === "$") {
      const len = Number(line);
      if (len < 0) return { value: null, next };
      if (buf.length < next + len + 2) return undefined;
      return {
        value: buf.toString("utf8", next, next + len),
        next: next + len + 2,
      };
    }
    if (kind === "*") {
      const count = Number(line);
      if (count < 0) return { value: null, next };
      const items = [];
      let cursor = next;
      for (let i = 0; i < count; i += 1) {
        const item = parse(buf, cursor);
        if (!item) return undefined;
        items.push(item.value);
        cursor = item.next;
      }
      return { value: items, next: cursor };
    }
    throw new Error(`不认识的 RESP 答复：${kind}`);
  };
  socket.on("data", (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    for (;;) {
      const reply = parse(buffer, 0);
      if (!reply) return;
      buffer = buffer.subarray(reply.next);
      const pending = waiting.shift();
      if (reply.value instanceof Error) pending?.failed(reply.value);
      else pending?.done(reply.value);
    }
  });
  let broken;
  const fail = (error) => {
    broken ??= error ?? new Error("Redis 连接断开");
    for (const pending of waiting.splice(0)) pending.failed(broken);
  };
  socket.on("error", fail);
  socket.on("close", () => fail());
  const ready = new Promise((done, failed) => {
    socket.once("connect", done);
    socket.once("error", failed);
  });
  ready.catch(() => {});
  return {
    async command(...args) {
      await ready;
      if (broken) throw broken;
      const parts = [`*${args.length}\r\n`];
      for (const arg of args.map(String))
        parts.push(`$${Buffer.byteLength(arg)}\r\n${arg}\r\n`);
      return new Promise((done, failed) => {
        waiting.push({ done, failed });
        socket.write(parts.join(""));
      });
    },
    close() {
      socket.destroy();
    },
  };
}

/** 起一次性的 Redis 容器（只停自己起的这一个）。 */
async function startRedis() {
  const name = `armadra-e2e-saas-${randomBytes(4).toString("hex")}`;
  await exec("docker", [
    "run",
    "-d",
    "--rm",
    "--name",
    name,
    "-p",
    "127.0.0.1::6379",
    REDIS_IMAGE,
  ]);
  const { stdout } = await exec("docker", ["port", name, "6379/tcp"]);
  const port = stdout.trim().split("\n")[0].split(":").pop();
  const url = `redis://127.0.0.1:${port}`;
  await until(
    "Redis 就绪",
    async () => {
      const probe = redisClient(url);
      try {
        return (await probe.command("PING")) === "PONG";
      } catch {
        return false;
      } finally {
        probe.close();
      }
    },
    30_000,
    300,
  );
  return {
    url,
    stop: () => exec("docker", ["stop", name], { allowFail: true }),
  };
}

/* ------------------------------ 签发者 ------------------------------ */

async function startIssuer() {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const kid = `k-${randomBytes(4).toString("hex")}`;
  const jwk = { ...publicKey.export({ format: "jwk" }), kid };
  const server = createHttpServer((request, response) => {
    if (request.url === "/.well-known/jwks.json") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify({ keys: [jwk] }));
      return;
    }
    response.writeHead(404).end();
  });
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  const issuer = `http://127.0.0.1:${server.address().port}`;
  const signToken = (typ, claims) => {
    const iat = Math.floor(Date.now() / 1000);
    const { signingInput, encodedHeader, encodedPayload } = buildSigningInput(
      { alg: "EdDSA", typ, kid },
      { iss: issuer, iat, exp: iat + 3600, ver: 1, ...claims },
    );
    return secret(
      joinCompactJws(
        encodedHeader,
        encodedPayload,
        new Uint8Array(sign(null, signingInput, privateKey)),
      ),
      `${typ} 令牌`,
    );
  };
  return {
    issuer,
    nodeToken: (node) =>
      signToken(TOKEN_TYPES.node, {
        sub: node,
        aud: "relay-node",
        jti: `n.${node}.${randomBytes(6).toString("hex")}`,
      }),
    tunnelToken: (sourceId, sourcePublic) =>
      signToken(TOKEN_TYPES.tunnel, {
        sub: sourceId,
        aud: "relay-tunnel",
        jti: randomBytes(8).toString("hex"),
        cnf: {
          jwk: {
            kty: "OKP",
            crv: "Ed25519",
            x: sourcePublic.export({ format: "jwk" }).x,
          },
        },
      }),
    relayToken: (sourceId, over = {}) =>
      signToken(TOKEN_TYPES.relay, {
        sub: "acct_e2e",
        aud: "relay",
        src: sourceId,
        jti: randomBytes(8).toString("hex"),
        ...over,
      }),
    close: () => new Promise((done) => server.close(done)),
  };
}

/* ------------------------------ 节点 ------------------------------ */

async function startNode({
  bin,
  name,
  issuer,
  redisUrl,
  nodeToken,
  metricsToken,
}) {
  const port = await freePort();
  const metricsPort = await freePort();
  const origin = `http://127.0.0.1:${port}`;
  const logs = [];
  const child = spawn(bin, ["saas", "serve"], {
    env: {
      PATH: process.env.PATH,
      RELAY_LISTEN: `127.0.0.1:${port}`,
      RELAY_NODE_ID: name,
      RELAY_DOMAIN: DOMAIN,
      RELAY_CLOUD_ISSUER: issuer,
      RELAY_REDIS_URL: redisUrl,
      RELAY_TUNNEL_URL: `ws://127.0.0.1:${port}/t/v1`,
      RELAY_ALLOW_INSECURE_TUNNEL: "1",
      RELAY_NODE_TOKEN: nodeToken,
      RELAY_METRICS_LISTEN: `127.0.0.1:${metricsPort}`,
      RELAY_METRICS_TOKEN: metricsToken,
      RELAY_DRAIN_GRACE_MS: "3000",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  child.stderr.on("data", (chunk) => logs.push(chunk.toString("utf8")));
  const exited = new Promise((done) =>
    child.once("exit", (code, signal) => done({ code, signal })),
  );
  await until(
    `节点 ${name} 就绪`,
    async () => {
      if (child.exitCode !== null)
        throw new Error(
          `节点 ${name} 提前退出：${redact(logs.join("").slice(-800))}`,
        );
      try {
        return (await fetch(`${origin}/ready`)).ok;
      } catch {
        return false;
      }
    },
    30_000,
    200,
  );
  return {
    name,
    origin,
    tunnelUrl: `ws://127.0.0.1:${port}/t/v1`,
    metrics: async () => {
      const response = await fetch(`http://127.0.0.1:${metricsPort}/metrics`, {
        headers: { authorization: `Bearer ${metricsToken}` },
      });
      expectEqual(response.status, 200, `${name} 的 /metrics`);
      return response.text();
    },
    logs,
    exited,
    signal: (sig) => child.kill(sig),
    stop: async () => {
      if (child.exitCode === null && child.signalCode === null) {
        child.kill("SIGTERM");
        const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
        await exited;
        clearTimeout(timer);
      }
    },
  };
}

/* ------------------------------ 客户端 ------------------------------ */

/** 经节点 node 开一条到源的端到端通道（Host 寻址）；答 ws 或被拒的关闭码。 */
function channel(node, sourceId, relayToken) {
  return new Promise((done, failed) => {
    const ws = new WebSocket(
      `${node.origin.replace(/^http/, "ws")}/_relay/e2e`,
      ["armadra-e2e.1", `armadra-relay.${relayToken}`],
      {
        perMessageDeflate: false,
        headers: { host: `${sourceId}.src.${DOMAIN}` },
      },
    );
    let opened = false;
    ws.once("open", () => {
      opened = true;
      // 被拒的升级也先完成再以关闭码结束：等一小会儿看它是不是马上被关。
      const timer = setTimeout(() => done({ ws }), 300);
      ws.once("close", (code) => {
        clearTimeout(timer);
        done({ code });
      });
    });
    ws.once("unexpected-response", (_request, response) =>
      done({ status: response.statusCode }),
    );
    ws.once("error", (error) => {
      if (!opened) failed(error);
    });
  });
}

/** 开一条通道就答（不等看会不会被关）；paused 为真时一开就停读（慢客户端：下行字节堆在内核与中继里）。 */
function rawChannel(node, sourceId, relayToken, paused = false) {
  return new Promise((done, failed) => {
    const ws = new WebSocket(
      `${node.origin.replace(/^http/, "ws")}/_relay/e2e`,
      ["armadra-e2e.1", `armadra-relay.${relayToken}`],
      {
        perMessageDeflate: false,
        headers: { host: `${sourceId}.src.${DOMAIN}` },
      },
    );
    ws.once("open", () => {
      if (paused) ws.pause();
      done(ws);
    });
    ws.once("error", failed);
  });
}

const closeOf = (ws) =>
  ws.readyState === WebSocket.CLOSED
    ? Promise.resolve(-1)
    : new Promise((done) => ws.once("close", (code) => done(code)));

async function echo(ws, total) {
  const sent = createHash("sha256");
  const got = createHash("sha256");
  let received = 0;
  const onMessage = (data) => {
    got.update(data);
    received += data.length;
  };
  ws.on("message", onMessage);
  for (let offset = 0; offset < total; ) {
    const size = Math.min(total - offset, 48 * 1024);
    const chunk = randomBytes(size);
    sent.update(chunk);
    ws.send(chunk);
    offset += size;
    if (ws.bufferedAmount > 4 * 1024 * 1024)
      await until("客户端缓冲排空", () => ws.bufferedAmount < 1024 * 1024);
  }
  await until("回显收全", () => received >= total, 60_000, 50);
  ws.off("message", onMessage);
  expectEqual(received, total, "回显字节数");
  expectEqual(got.digest("hex"), sent.digest("hex"), "回显内容");
}

function metric(text, name) {
  const line = text.split("\n").find((l) => l.startsWith(`${name} `));
  return line === undefined ? undefined : Number(line.slice(name.length + 1));
}

/* ------------------------------ 编排 ------------------------------ */

/** 跑完全部步骤；`report` 是 e2e.mjs 的步骤记录。 */
export async function runSaasE2e({ root, report, redisUrl: givenRedis }) {
  const { FakeCore } = await import(
    "../../packages/e2e-fake-core/src/fake-core.ts"
  );
  const nodes = {};
  const cores = [];
  const sockets = [];
  let redis;
  let issuer;
  let admin;
  let buildDir;
  try {
    let bin = process.env.ARMADRA_RELAY_BIN;
    await report.step("构建中继、起 Redis 与测试签发者", async () => {
      if (!bin) {
        buildDir = mkdtempSync(join(tmpdir(), "armadra-e2e-saas-"));
        bin = join(buildDir, "armadra-relay");
        await exec("go", ["build", "-o", bin, "./cmd/armadra-relay"], {
          cwd: join(root, "go"),
        });
      }
      redis = givenRedis
        ? { url: givenRedis, stop: async () => {} }
        : await startRedis();
      admin = redisClient(redis.url);
      if (givenRedis) {
        const existing = await admin.command("KEYS", "relay:*");
        expect(
          existing.length === 0,
          "--redis-url 要是专用的空库（已有 relay: 前缀的键）",
        );
      }
      issuer = await startIssuer();
      return redis.url.replace(/\/\/[^@]*@/, "//…@");
    });

    const metricsToken = secret(randomBytes(18).toString("hex"), "指标令牌");
    await report.step(
      "起三个节点：节点表各一项，url 是各自的隧道地址",
      async () => {
        for (const name of ["a", "b", "c"])
          nodes[name] = await startNode({
            bin,
            name,
            issuer: issuer.issuer,
            redisUrl: redis.url,
            nodeToken: issuer.nodeToken(name),
            metricsToken,
          });
        const table = await admin.command("HGETALL", "relay:nodes");
        const entries = {};
        for (let i = 0; i < table.length; i += 2)
          entries[table[i]] = JSON.parse(table[i + 1]);
        for (const name of ["a", "b", "c"]) {
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
        const health = await (await fetch(`${nodes.a.origin}/health`)).json();
        expectEqual(health.mode, "saas", "/health.mode");
      },
    );

    const { privateKey, publicKey } = generateKeyPairSync("ed25519");
    const sourceId = randomBytes(16).toString("hex");
    const connectCore = async (node) => {
      const core = new FakeCore({
        tunnelUrl: node.tunnelUrl,
        sourceId,
        sourceKey: privateKey,
        token: () => issuer.tunnelToken(sourceId, publicKey),
        capabilities: ["e2e"],
      });
      cores.push(core);
      const outcome = await core.connect();
      expect(outcome.ready, `握手被拒：${JSON.stringify(outcome)}`);
      await until(`路由指向 ${node.name}`, async () => {
        const route = await admin.command("GET", `relay:route:src:${sourceId}`);
        return route && JSON.parse(route).node === node.name;
      });
      return core;
    };

    let core;
    await report.step(
      "源连节点 a；客户端连节点 b：经 /x/v1 转发，10 MB 随机字节双向原样",
      async () => {
        core = await connectCore(nodes.a);
        const opened = await channel(
          nodes.b,
          sourceId,
          issuer.relayToken(sourceId),
        );
        expect(opened.ws, `通道没开成：${JSON.stringify(opened)}`);
        sockets.push(opened.ws);
        expectEqual(opened.ws.protocol, "armadra-e2e.1", "子协议");
        await echo(opened.ws, 10 * 1024 * 1024);
        expectEqual(core.e2eOpens.length, 1, "源收到一条 e2e OPEN");
        expectEqual(
          core.e2eOpens[0].relayNode,
          "b",
          "OPEN.relayNode 是入口节点",
        );
        const closed = closeOf(opened.ws);
        opened.ws.close(1000);
        await closed;
        const metricsB = await nodes.b.metrics();
        expect(
          metric(metricsB, "relay_routes_remote_total") >= 1,
          "b 的 relay_routes_remote_total",
        );
        expect(
          metric(
            metricsB,
            'relay_internode_bytes_total{direction="to_source"}',
          ) >=
            10 * 1024 * 1024,
          "b 的节点间上行字节",
        );
        expect(
          metric(await nodes.a.metrics(), "relay_tunnels") === 1,
          "a 持有隧道",
        );
      },
    );

    await report.step(
      "慢客户端与下载中途断开：经 b 转发的 24 条不读的下载不拖住别的源；64 次下载中途掉线之后源照常出数据",
      async () => {
        // 第二个源：每条通道写 64 MB 的确定序列（下载），同样连 a。
        const keys = generateKeyPairSync("ed25519");
        const downloadId = randomBytes(16).toString("hex");
        const downloader = new FakeCore({
          tunnelUrl: nodes.a.tunnelUrl,
          sourceId: downloadId,
          sourceKey: keys.privateKey,
          token: () => issuer.tunnelToken(downloadId, keys.publicKey),
          capabilities: ["e2e"],
          e2eSend: 64 * 1024 * 1024,
        });
        cores.push(downloader);
        expect((await downloader.connect()).ready, "下载源握手");
        await until("下载源的路由", async () => {
          const route = await admin.command(
            "GET",
            `relay:route:src:${downloadId}`,
          );
          return route && JSON.parse(route).node === "a";
        });
        // 慢客户端：开了不读。
        const slow = [];
        for (let i = 0; i < 24; i += 1) {
          const ws = await rawChannel(
            nodes.b,
            downloadId,
            issuer.relayToken(downloadId),
            true,
          );
          slow.push(ws);
          sockets.push(ws);
        }
        await sleep(500);
        const echoing = await channel(
          nodes.b,
          sourceId,
          issuer.relayToken(sourceId),
        );
        expect(echoing.ws, "别的源的通道");
        sockets.push(echoing.ws);
        await echo(echoing.ws, 1024 * 1024);
        echoing.ws.close(1000);
        for (const ws of slow) ws.terminate();
        // 下载中途掉线 64 次。
        for (let i = 0; i < 64; i += 1) {
          const ws = await rawChannel(
            nodes.b,
            downloadId,
            issuer.relayToken(downloadId),
          );
          sockets.push(ws);
          await new Promise((done) => ws.once("message", done));
          await sleep(30);
          ws.terminate();
        }
        // 源照常出数据：读满 8 MB。
        const last = await rawChannel(
          nodes.b,
          downloadId,
          issuer.relayToken(downloadId),
        );
        sockets.push(last);
        let received = 0;
        last.on("message", (data) => {
          received += data.length;
        });
        await until(
          "掉线 64 次之后下载照常",
          () => received >= 8 * 1024 * 1024,
          30_000,
          50,
        );
        last.terminate();
        await downloader.stop();
      },
    );

    await report.step(
      "节点间 /x/v1：没有、伪造、错类型的令牌一律 401",
      async () => {
        const valid = issuer.nodeToken("b");
        for (const [label, token] of [
          ["没有令牌", undefined],
          ["伪造", `${valid.slice(0, -4)}AAAA`],
          ["中继令牌", issuer.relayToken(sourceId)],
        ]) {
          const status = await new Promise((done, failed) => {
            const ws = new WebSocket(
              `${nodes.a.origin.replace(/^http/, "ws")}/x/v1`,
              {
                headers: token ? { authorization: `Bearer ${token}` } : {},
              },
            );
            ws.once("unexpected-response", (_request, response) =>
              done(response.statusCode),
            );
            ws.once("open", () => {
              ws.terminate();
              done(101);
            });
            ws.once("error", (error) => {
              if (!ws.readyState) failed(error);
            });
          });
          expectEqual(status, 401, label);
        }
      },
    );

    await report.step(
      "撤销跨节点生效：写撤销表并广播、kick，在线通道被踢，旧令牌在每个节点 4401",
      async () => {
        const jti = randomBytes(8).toString("hex");
        const token = issuer.relayToken(sourceId, { jti, sub: "acct_revoked" });
        const online = await channel(nodes.b, sourceId, token);
        expect(online.ws, "撤销前能开");
        sockets.push(online.ws);
        const closed = closeOf(online.ws);
        const expMs = Date.now() + 3_600_000;
        // 控制面的写法（SaaS 实施设计 §2.4、§4.8）：先写键再广播；再按人 kick（sourceId 必填）。
        await admin.command("SET", `relay:revoked:${jti}`, "1", "PXAT", expMs);
        await admin.command(
          "PUBLISH",
          "relay:revoked",
          JSON.stringify({ origin: "cloud/e2e", kind: "revoked", jti, expMs }),
        );
        await admin.command(
          "PUBLISH",
          "relay:ctl",
          JSON.stringify({
            origin: "cloud/e2e",
            kind: "command",
            command: {
              type: "kick",
              sourceId,
              subject: "acct_revoked",
              reason: "revoked",
            },
          }),
        );
        await Promise.race([
          closed,
          sleep(15_000).then(() => {
            throw new Error("在线通道没被踢");
          }),
        ]);
        for (const name of ["a", "b", "c"]) {
          const again = await channel(nodes[name], sourceId, token);
          expectEqual(again.code, 4401, `${name} 上的旧令牌`);
        }
        const fresh = await channel(
          nodes.c,
          sourceId,
          issuer.relayToken(sourceId),
        );
        expect(fresh.ws, "别的令牌照常（经 c 转发）");
        fresh.ws.close(1000);
      },
    );

    await report.step(
      "持有节点 a 下线：GOAWAY rebalance、节点表先 draining 后删除；源重连到 c，客户端经 b 重连后落到 c",
      async () => {
        const ws = (
          await channel(nodes.b, sourceId, issuer.relayToken(sourceId))
        ).ws;
        expect(ws, "下线前的通道");
        sockets.push(ws);
        const wsClosed = closeOf(ws);
        nodes.a.signal("SIGTERM");
        await until("a 标成 draining", async () => {
          const value = await admin.command("HGET", "relay:nodes", "a");
          return value === null || JSON.parse(value).draining === true;
        });
        await until("源收到 GOAWAY rebalance", () =>
          core.goaways.includes("rebalance"),
        );
        expectEqual(await core.closed(), 4492, "旧隧道的关闭码");
        expectEqual(await wsClosed, 4404, "经 b 转发的通道随旧隧道结束");
        const { code } = await nodes.a.exited;
        expectEqual(code, 0, "a 正常退出");
        expectEqual(
          await admin.command("HEXISTS", "relay:nodes", "a"),
          0,
          "a 从节点表里删掉",
        );
        // 源按节点表（去掉 draining 的）选节点：这里固定选 c，让客户端经 b 转发。
        core = await connectCore(nodes.c);
        const again = await channel(
          nodes.b,
          sourceId,
          issuer.relayToken(sourceId),
        );
        expect(again.ws, `重连没开成：${JSON.stringify(again)}`);
        sockets.push(again.ws);
        await echo(again.ws, 256 * 1024);
        expectEqual(core.e2eOpens.at(-1)?.relayNode, "b", "落到 c、经 b 转发");
        // 持有节点 c 突然没了：节点间连接断开，经 b 的通道 4404。
        const lost = closeOf(again.ws);
        nodes.c.signal("SIGKILL");
        expectEqual(await lost, 4404, "持有节点没了，客户端 4404");
        const retry = await channel(
          nodes.b,
          sourceId,
          issuer.relayToken(sourceId),
        );
        expectEqual(retry.code, 4404, "源不在线时重连 4404");
      },
    );

    await report.step("节点日志里没有任何秘密", async () => {
      await nodes.b.stop();
      const text = Object.values(nodes)
        .map((node) => node.logs.join(""))
        .join("");
      const leaked = secretEntries().filter(([value]) => text.includes(value));
      expectEqual(
        leaked.map(([, label]) => label).join("、"),
        "",
        "日志里的秘密",
      );
      expect(text.includes("中继已停止"), "b 正常停下");
    });
  } catch (error) {
    if (process.env.E2E_DEBUG)
      for (const node of Object.values(nodes))
        log(`--- ${node.name}\n${node.logs.join("").slice(-3000)}`);
    throw error;
  } finally {
    for (const ws of sockets) ws.terminate();
    for (const core of cores) await core.stop().catch(() => {});
    for (const node of Object.values(nodes)) await node.stop().catch(() => {});
    admin?.close();
    await issuer?.close();
    await redis?.stop();
    if (buildDir) rmSync(buildDir, { recursive: true, force: true });
    await sleep(0);
  }
}
