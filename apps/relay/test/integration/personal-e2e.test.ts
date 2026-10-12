/**
 * 个人中转端到端（中继设计 §9 `personal-e2e.test.ts`）：真 TLS（自签 CA）、真状态文件、R1 的假 core。
 *
 * init → serve → 登录 → 注册令牌 → 假 core 注册 + 源 JWS 换隧道令牌 + 握手 → owner 取断言 →
 * 经 `/s/<id>/_relay/e2e` 开端到端通道（明文 HTTP、预检一律 404）→ `me/stream` → 分享链接 → 访客
 * accept → 访客开通道 →
 * 撤销链接（访客 WS 断开、令牌失效）→ 撤销源（隧道与 owner WS 断开、`sourceRevoked`）→ 锁定。
 *
 * 每一步的响应都对协议包契约的 output schema 校验；最后核对日志里没有口令、令牌、秘密与正文。
 */
import { once } from "node:events";
import {
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { request } from "node:https";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "@armadra/cloud-shared/log";
import {
  assertionClaimsSchema,
  relayTokenClaimsSchema,
} from "@armadra/platform-protocol/assertion";
import {
  cloudContract,
  meStreamEventSchema,
  type MeStreamEvent,
} from "@armadra/platform-protocol/cloud-api";
import { wireErrorSchema } from "@armadra/platform-protocol/errors";
import { FakeCore } from "@armadra/relay-runtime-node/testing/fake-core";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { WebSocket } from "ws";
import type { z } from "zod";

import {
  startPersonalRelay,
  type RunningPersonalRelay,
} from "../../src/personal/serve.ts";
import { openPersonalStore } from "../../src/personal/store.ts";
import { PASSWORD, registerInput, SOURCE_ID, sourceJws } from "../personal.ts";

const MARKER = "PAYLOAD-MARKER-e2e-91c";
const DEVICE = { platform: "browser", name: "e2e" } as const;

const root = mkdtempSync(join(tmpdir(), "relay-e2e-"));
const logs: string[] = [];
const secrets: string[] = [PASSWORD];
let relay: RunningPersonalRelay;
let ca: string;
let issuer: string;
let core: FakeCore;
const sockets: WebSocket[] = [];

async function freePort(): Promise<number> {
  return new Promise((resolve) => {
    const server = createServer();
    server.listen(0, "127.0.0.1", () => {
      const port = (server.address() as { port: number }).port;
      server.close(() => resolve(port));
    });
  });
}

interface Reply {
  status: number;
  body: unknown;
  headers: Record<string, string | string[] | undefined>;
}

/** HTTPS 请求，信任中继的自签 CA。 */
function call(
  method: string,
  url: string,
  options: { body?: unknown; headers?: Record<string, string> } = {},
): Promise<Reply> {
  const payload =
    options.body === undefined ? undefined : JSON.stringify(options.body);
  return new Promise((resolve, reject) => {
    const req = request(
      url,
      {
        method,
        ca,
        headers: {
          ...(payload
            ? {
                "content-type": "application/json",
                "content-length": String(Buffer.byteLength(payload)),
              }
            : {}),
          ...options.headers,
        },
      },
      (res) => {
        const chunks: Buffer[] = [];
        res.on("data", (chunk: Buffer) => chunks.push(chunk));
        res.on("end", () => {
          const text = Buffer.concat(chunks).toString("utf8");
          let body: unknown = text;
          try {
            body = JSON.parse(text);
          } catch {
            // 不是 JSON 就留原文。
          }
          resolve({ status: res.statusCode ?? 0, body, headers: res.headers });
        });
      },
    );
    req.on("error", reject);
    req.end(payload);
  });
}

type Procedures = typeof cloudContract;
function output<D extends keyof Procedures, K extends keyof Procedures[D]>(
  domain: D,
  key: K,
  reply: Reply,
): unknown {
  expect(
    reply.status,
    `${String(domain)}.${String(key)} ${JSON.stringify(reply.body)}`,
  ).toBe(200);
  const procedure = Procedures(domain, key);
  const schema = procedure["~orpc"].outputSchema as z.ZodType;
  const parsed = schema.safeParse(reply.body);
  expect(
    parsed.error,
    `${String(domain)}.${String(key)} 输出不符契约`,
  ).toBeUndefined();
  return parsed.data;
}
function Procedures(domain: keyof Procedures, key: PropertyKey) {
  return (
    cloudContract[domain] as Record<
      PropertyKey,
      { "~orpc": { outputSchema: unknown } }
    >
  )[key] as {
    "~orpc": { outputSchema: unknown };
  };
}

function errorCode(reply: Reply): string {
  return wireErrorSchema.parse(reply.body).code;
}

/** 令牌载荷（只拆不验；验签在中继与 core）。 */
const payloadOf = (token: string): Record<string, unknown> =>
  JSON.parse(
    Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"),
  ) as Record<string, unknown>;

const bearer = (token: string) => ({ authorization: `Bearer ${token}` });

function openWs(url: string, protocols: string[]): WebSocket {
  const ws = new WebSocket(url.replace(/^https/, "wss"), protocols, { ca });
  ws.on("error", () => {});
  sockets.push(ws);
  return ws;
}

/** 开一条端到端通道（假 core 把字节回显）。 */
function channel(base: string, token: string): WebSocket {
  return openWs(`${base}/_relay/e2e`, [
    "armadra-e2e.1",
    `armadra-relay.${token}`,
  ]);
}

/** 通道开成答 `null`（随后关掉）；被拒答关闭码（中继先完成升级再关）。 */
async function channelOutcome(
  base: string,
  token: string,
): Promise<number | null> {
  const ws = channel(base, token);
  const closed = once(ws, "close").then(([code]) => code as number);
  const opened = await Promise.race([
    once(ws, "open").then(() => true),
    closed.then(() => false),
  ]);
  if (!opened) return closed;
  const outcome = await Promise.race([
    closed,
    new Promise<null>((resolve) => setTimeout(() => resolve(null), 300)),
  ]);
  if (outcome === null) ws.close();
  return outcome;
}

async function closeCode(ws: WebSocket): Promise<number> {
  if (ws.readyState === WebSocket.CLOSED) return -1;
  const [code] = (await once(ws, "close")) as [number];
  return code;
}

beforeAll(async () => {
  const port = await freePort();
  const passwordFile = join(root, "pw");
  writeFileSync(passwordFile, `${PASSWORD}\n`, { mode: 0o600 });
  relay = await startPersonalRelay(
    {
      dataDir: join(root, "data"),
      host: "127.0.0.1",
      port,
      listen: { host: "127.0.0.1", port },
      tls: "self-signed",
      logLevel: "debug",
      account: "me",
      passwordFile,
    },
    {
      log: createLogger({
        name: "relay",
        level: "debug",
        write: (line) => logs.push(line),
      }),
      streamPingMs: 200,
    },
  );
  issuer = relay.issuer;
  expect(issuer).toBe(`https://127.0.0.1:${port}`);
  const caReply = await new Promise<string>((resolve, reject) => {
    // 第一次取 CA 不验证（用户比对指纹），之后一律信任这张 CA。
    request(`${issuer}/ca.crt`, { rejectUnauthorized: false }, (res) => {
      let text = "";
      res.on("data", (chunk) => (text += chunk));
      res.on("end", () => resolve(text));
    })
      .on("error", reject)
      .end();
  });
  ca = caReply;
  expect(relay.fingerprint).toMatch(/^[0-9a-f]{64}$/);
}, 30_000);

afterAll(async () => {
  for (const ws of sockets) ws.terminate();
  await core?.stop();
  await relay?.close();
  rmSync(root, { recursive: true, force: true });
});

describe("个人中转端到端", () => {
  let ownerSession: { accessToken: string; refreshToken?: string };
  let relayToken: string;
  let relayBaseUrl: string;
  let ownerWs: WebSocket;
  let stream: WebSocket;
  const streamEvents: MeStreamEvent[] = [];

  it("平台信息与 JWKS；契约里 personal 不实现的路由答 404 not_found", async () => {
    const info = output(
      "platform",
      "info",
      await call("GET", `${issuer}/.well-known/armadra-platform`),
    );
    expect(info).toMatchObject({
      mode: "personal",
      issuer,
      relay: { addressing: "path" },
      webApp: null,
    });
    const jwks = output(
      "platform",
      "jwks",
      await call("GET", `${issuer}/.well-known/jwks.json`),
    ) as {
      keys: { kid?: string }[];
    };
    expect(jwks.keys.map((key) => key.kid)).toEqual(["k1"]);
    const orgs = await call("POST", `${issuer}/v1/orgs`, {
      body: { slug: "a", name: "a" },
    });
    expect(orgs.status).toBe(404);
    expect(errorCode(orgs)).toBe("not_found");
    const health = await call("GET", `${issuer}/health`);
    expect(health.status).toBe(200);
  });

  it("账号口令登录拿会话；源注册；源 JWS 换隧道令牌；假 core 经 wss 握手上线", async () => {
    const login = output(
      "auth",
      "login",
      await call("POST", `${issuer}/v1/auth/login`, {
        body: { account: "me", password: PASSWORD, device: DEVICE },
      }),
    ) as { session: typeof ownerSession };
    ownerSession = login.session;
    secrets.push(ownerSession.accessToken, ownerSession.refreshToken as string);

    const ticket = output(
      "me",
      "streamTicket",
      await call("POST", `${issuer}/v1/me/stream/ticket`, {
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { ticket: string };
    stream = openWs(`${issuer}/v1/me/stream`, [
      `armadra-ticket.${ticket.ticket}`,
    ]);
    stream.on("message", (data) => {
      const event = meStreamEventSchema.parse(JSON.parse(String(data)));
      streamEvents.push(event);
    });
    await once(stream, "open");
    expect(stream.protocol).toBe(`armadra-ticket.${ticket.ticket}`);
    stream.send(JSON.stringify({ type: "ping" }));

    const registration = output(
      "sources",
      "registrationToken",
      await call("POST", `${issuer}/v1/sources/registration-tokens`, {
        body: {},
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { registrationToken: string };
    secrets.push(registration.registrationToken);
    const registered = output(
      "sources",
      "register",
      await call("POST", `${issuer}/v1/sources/register`, {
        body: registerInput(registration.registrationToken),
      }),
    );
    expect(registered).toMatchObject({
      issuer,
      sourceId: SOURCE_ID,
      ownerAccountId: "acct:me",
    });

    const hint = output(
      "sources",
      "heartbeatHint",
      await call("GET", `${issuer}/v1/sources/me/relay`, {
        headers: { authorization: `Source ${sourceJws(issuer, Date.now())}` },
      }),
    ) as { tunnelToken: string; nodes: { url: string }[] };
    secrets.push(hint.tunnelToken);
    expect(hint.nodes[0]?.url).toBe(`${issuer.replace("https", "wss")}/t/v1`);
    core = new FakeCore({
      tunnelUrl: hint.nodes[0]?.url as string,
      token: () => hint.tunnelToken,
      ca,
      capabilities: ["e2e"],
    });
    const connected = await core.connect();
    expect(connected.ready).toBeDefined();

    await waitFor(() =>
      streamEvents.some((event) => event.type === "sourceOnline"),
    );
    expect(streamEvents.some((event) => event.type === "pong")).toBe(true);
    const listed = output(
      "me",
      "sources",
      await call("GET", `${issuer}/v1/me/sources`, {
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { sources: { online: boolean; via: string }[] };
    expect(listed.sources).toMatchObject([{ online: true, via: "owner" }]);
  });

  it("owner 取断言 + 中继令牌，经 /s/<id>/_relay/e2e 开通道；明文 HTTP 404", async () => {
    const assertion = output(
      "sources",
      "assertion",
      await call("POST", `${issuer}/v1/sources/${SOURCE_ID}/assertion`, {
        body: {},
        headers: bearer(ownerSession.accessToken),
      }),
    ) as {
      relayToken: string;
      relayBaseUrl: string;
      assertion: string;
      online: boolean;
    };
    expect(assertion.online).toBe(true);
    relayToken = assertion.relayToken;
    relayBaseUrl = assertion.relayBaseUrl;
    secrets.push(relayToken, assertion.assertion);
    expect(relayBaseUrl).toBe(`${issuer}/s/${SOURCE_ID}`);

    // 明文路径已删除（cloud-api §20）：带令牌的 HTTP 与 CORS 预检都 404，不进隧道。
    const before = core.requests;
    const plain = await call(
      "GET",
      `${relayBaseUrl}/echo-request?m=${MARKER}`,
      {
        headers: { "armadra-relay-token": relayToken },
      },
    );
    expect(plain.status).toBe(404);
    const preflight = await call(
      "OPTIONS",
      `${relayBaseUrl}/api/identity/cloud/login`,
      {
        headers: {
          origin: issuer,
          "access-control-request-method": "POST",
        },
      },
    );
    expect(preflight.status).toBe(404);
    expect(core.requests).toBe(before);
    expect(await channelOutcome(relayBaseUrl, "not-a-token")).toBe(4401);

    ownerWs = channel(relayBaseUrl, relayToken);
    await once(ownerWs, "open");
    expect(ownerWs.protocol).toBe("armadra-e2e.1");
    ownerWs.send(Buffer.from(MARKER));
    const [reply] = (await once(ownerWs, "message")) as [Buffer];
    expect(String(reply)).toBe(MARKER);
  });

  let guestWs: WebSocket;
  let guest: {
    relayToken: string;
    guestSession: { accessToken: string; refreshToken?: string };
  };
  let linkId: string;

  it("分享链接：秘密只在 # 片段里；访客匿名 accept 后经中继访问", async () => {
    const created = output(
      "links",
      "create",
      await call("POST", `${issuer}/v1/links`, {
        body: {
          kind: "source_invite",
          sourceId: SOURCE_ID,
          invitationId: "inv_e2e",
          label: "给同事",
          role: "editor",
          expiresAtMs: Date.now() + 3600_000,
          maxUses: 3,
        },
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { linkId: string; url: string; secret: string };
    linkId = created.linkId;
    secrets.push(created.secret);
    expect(created.url).toBe(`${issuer}/j/${linkId}`);
    // 分享出去的链接：`<url>#<secret>.<core 邀请令牌>`；打开落地页的请求里不带片段。
    const shared = new URL(`${created.url}#${created.secret}.core-invite`);
    const landing = await call("GET", `${shared.origin}${shared.pathname}`);
    expect(landing.status).toBe(200);
    expect(String(landing.body)).toContain("armadra://join?");

    output("links", "get", await call("GET", `${issuer}/v1/links/${linkId}`));
    const wrong = await call("POST", `${issuer}/v1/links/${linkId}/accept`, {
      body: { secret: "nope" },
    });
    expect(errorCode(wrong)).toBe("link_secret_invalid");
    guest = output(
      "links",
      "accept",
      await call("POST", `${issuer}/v1/links/${linkId}/accept`, {
        body: { secret: shared.hash.slice(1).split(".")[0], device: DEVICE },
      }),
    ) as typeof guest;
    secrets.push(
      guest.relayToken,
      guest.guestSession.accessToken,
      guest.guestSession.refreshToken as string,
    );
    await waitFor(() =>
      streamEvents.some((event) => event.type === "linkUsed"),
    );

    expect(await channelOutcome(relayBaseUrl, guest.relayToken)).toBeNull();
    const guestSources = output(
      "me",
      "sources",
      await call("GET", `${issuer}/v1/me/sources`, {
        headers: bearer(guest.guestSession.accessToken),
      }),
    ) as { sources: { via: string }[] };
    expect(guestSources.sources.map((row) => row.via)).toEqual(["link"]);
    const forbidden = await call(
      "POST",
      `${issuer}/v1/sources/registration-tokens`,
      {
        body: {},
        headers: bearer(guest.guestSession.accessToken),
      },
    );
    expect(errorCode(forbidden)).toBe("forbidden");

    guestWs = channel(relayBaseUrl, guest.relayToken);
    await once(guestWs, "open");
  });

  it("流量统计（§19）：owner 看到自己与访客经中继的连接与字节，访客只看到自己；不记路径", async () => {
    await relay.relay.traffic?.flush();
    const query = `fromMs=0&toMs=${Date.now() + 3_600_000}`;
    const owned = output(
      "me",
      "traffic",
      await call("GET", `${issuer}/v1/me/traffic?${query}`, {
        headers: bearer(ownerSession.accessToken),
      }),
    ) as {
      buckets: {
        subject: string;
        kind: string;
        sourceId: string;
        connections: number;
        bytesToSource: number;
        bytesToClient: number;
      }[];
    };
    const of = (subject: string, kind: string) =>
      owned.buckets.filter(
        (row) => row.subject === subject && row.kind === kind,
      );
    expect(owned.buckets.every((row) => row.kind === "e2e")).toBe(true);
    expect(of("acct:me", "e2e")[0]).toMatchObject({ sourceId: SOURCE_ID });
    expect(
      of("acct:me", "e2e").reduce((total, row) => total + row.connections, 0),
    ).toBeGreaterThanOrEqual(1);
    const guestSubject = owned.buckets.find((row) =>
      row.subject.startsWith(`guest:${linkId}:`),
    )?.subject;
    expect(guestSubject).toBeDefined();
    expect(JSON.stringify(owned)).not.toContain("echo");
    const mine = output(
      "me",
      "traffic",
      await call("GET", `${issuer}/v1/me/traffic?${query}&granularity=day`, {
        headers: bearer(guest.guestSession.accessToken),
      }),
    ) as { buckets: { subject: string }[] };
    expect(mine.buckets.length).toBeGreaterThan(0);
    expect(new Set(mine.buckets.map((row) => row.subject))).toEqual(
      new Set([guestSubject]),
    );
    const backwards = await call(
      "GET",
      `${issuer}/v1/me/traffic?fromMs=10&toMs=5`,
      { headers: bearer(ownerSession.accessToken) },
    );
    expect(errorCode(backwards)).toBe("bad_request");
  });

  it("改备注：owner 经 PATCH 只改 label，落地信息跟着变；访客改不了", async () => {
    const updated = output(
      "links",
      "update",
      await call("PATCH", `${issuer}/v1/links/${linkId}`, {
        body: { label: "  给设计组  " },
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { linkId: string; label: string; uses: number; revokedAtMs: unknown };
    expect(updated).toMatchObject({
      linkId,
      label: "给设计组",
      uses: 1,
      revokedAtMs: null,
    });
    const landing = output(
      "links",
      "get",
      await call("GET", `${issuer}/v1/links/${linkId}`),
    ) as { label: string };
    expect(landing.label).toBe("给设计组");
    const byGuest = await call("PATCH", `${issuer}/v1/links/${linkId}`, {
      body: { label: "x" },
      headers: bearer(guest.guestSession.accessToken),
    });
    expect(errorCode(byGuest)).toBe("forbidden");
    const missing = await call("PATCH", `${issuer}/v1/links/nope`, {
      body: { label: "x" },
      headers: bearer(ownerSession.accessToken),
    });
    expect(errorCode(missing)).toBe("not_found");
    const tooLong = await call("PATCH", `${issuer}/v1/links/${linkId}`, {
      body: { label: "x".repeat(129) },
      headers: bearer(ownerSession.accessToken),
    });
    expect(errorCode(tooLong)).toBe("bad_request");
  });

  it("工作区范围链接（协议 1.1）：断言带 scp、中继令牌带 ro；边缘不按 ro 过滤（照样开通道）；范围落进存储", async () => {
    const created = output(
      "links",
      "create",
      await call("POST", `${issuer}/v1/links`, {
        body: {
          kind: "source_invite",
          sourceId: SOURCE_ID,
          invitationId: "inv_scoped",
          label: "只看一个工作区",
          role: "viewer",
          expiresAtMs: Date.now() + 3600_000,
          scope: { workspaceId: "ws_e2e", readOnly: true },
        },
        headers: bearer(ownerSession.accessToken),
      }),
    ) as { linkId: string; secret: string };
    secrets.push(created.secret);
    const landing = output(
      "links",
      "get",
      await call("GET", `${issuer}/v1/links/${created.linkId}`),
    ) as { scope?: unknown };
    expect(landing.scope).toEqual({ workspaceId: "ws_e2e", readOnly: true });
    const persisted = await openPersonalStore(join(root, "data"));
    const state = await persisted.snapshot();
    await persisted.close();
    expect(
      state.links.find((row) => row.linkId === created.linkId)?.scope,
    ).toEqual({ workspaceId: "ws_e2e", readOnly: true });

    const scoped = output(
      "links",
      "accept",
      await call("POST", `${issuer}/v1/links/${created.linkId}/accept`, {
        body: { secret: created.secret, device: DEVICE },
      }),
    ) as {
      assertion: string;
      relayToken: string;
      guestSession: { accessToken: string; refreshToken?: string };
    };
    secrets.push(
      scoped.assertion,
      scoped.relayToken,
      scoped.guestSession.accessToken,
      scoped.guestSession.refreshToken as string,
    );
    const assertion = assertionClaimsSchema.parse(payloadOf(scoped.assertion));
    expect(assertion.scp).toEqual(["ws:ws_e2e", "ro"]);
    expect(assertion.link?.invitationId).toBe("inv_scoped");
    expect(relayTokenClaimsSchema.parse(payloadOf(scoped.relayToken)).ro).toBe(
      true,
    );

    // 边缘不按 ro 过滤：ro 令牌照样开得了通道，读写由 core 判定。
    expect(await channelOutcome(relayBaseUrl, scoped.relayToken)).toBeNull();
    // 再要断言仍带同一份范围。
    const again = output(
      "sources",
      "assertion",
      await call("POST", `${issuer}/v1/sources/${SOURCE_ID}/assertion`, {
        body: {},
        headers: bearer(scoped.guestSession.accessToken),
      }),
    ) as { assertion: string; relayToken: string };
    secrets.push(again.assertion, again.relayToken);
    expect(payloadOf(again.assertion).scp).toEqual(["ws:ws_e2e", "ro"]);
    expect(payloadOf(again.relayToken).ro).toBe(true);

    const badScope = await call("POST", `${issuer}/v1/links`, {
      body: {
        kind: "source_invite",
        sourceId: SOURCE_ID,
        invitationId: "inv_bad",
        label: "",
        role: "viewer",
        expiresAtMs: Date.now() + 3600_000,
        scope: { sessionId: "sess_1" },
      },
      headers: bearer(ownerSession.accessToken),
    });
    expect(errorCode(badScope)).toBe("bad_request");
    await call("DELETE", `${issuer}/v1/links/${created.linkId}`, {
      headers: bearer(ownerSession.accessToken),
    });
  });

  it("撤销链接：访客 WS 断开，访客的中继令牌与会话立即失效，owner 不受影响", async () => {
    const revoked = await call("DELETE", `${issuer}/v1/links/${linkId}`, {
      headers: bearer(ownerSession.accessToken),
    });
    expect(revoked.status).toBe(200);
    await closeCode(guestWs);
    expect(guestWs.readyState).toBe(WebSocket.CLOSED);
    expect(await channelOutcome(relayBaseUrl, guest.relayToken)).toBe(4401);
    const refreshed = await call("POST", `${issuer}/v1/auth/refresh`, {
      body: { refreshToken: guest.guestSession.refreshToken },
    });
    expect(errorCode(refreshed)).toBe("session_revoked");
    expect(ownerWs.readyState).toBe(WebSocket.OPEN);
  });

  it("撤销源：隧道关闭、owner WS 断开、me/stream 收到 sourceRevoked；中继令牌失效，源再要隧道令牌答 source_revoked", async () => {
    const revoked = await call("DELETE", `${issuer}/v1/sources/${SOURCE_ID}`, {
      headers: bearer(ownerSession.accessToken),
    });
    expect(revoked.status).toBe(200);
    await closeCode(ownerWs);
    await core.closed();
    await waitFor(() =>
      streamEvents.some((event) => event.type === "sourceRevoked"),
    );
    expect(await channelOutcome(relayBaseUrl, relayToken)).toBe(4401);
    const hint = await call("GET", `${issuer}/v1/sources/me/relay`, {
      headers: { authorization: `Source ${sourceJws(issuer, Date.now())}` },
    });
    expect(errorCode(hint)).toBe("source_revoked");
    const reconnect = await core.connect();
    expect(reconnect.ready).toBeUndefined();
  });

  it("刷新旋转；登出后访问令牌失效；me/stream 随之以 4401 关闭", async () => {
    const refreshed = output(
      "auth",
      "refresh",
      await call("POST", `${issuer}/v1/auth/refresh`, {
        body: { refreshToken: ownerSession.refreshToken },
      }),
    ) as { session: typeof ownerSession };
    secrets.push(
      refreshed.session.accessToken,
      refreshed.session.refreshToken as string,
    );
    // stream 绑在旧会话的设备上：刷新同一台设备不影响它。
    const logout = await call("POST", `${issuer}/v1/auth/logout`, {
      body: {},
      headers: bearer(refreshed.session.accessToken),
    });
    expect(logout.status).toBe(200);
    const me = await call("GET", `${issuer}/v1/me`, {
      headers: bearer(refreshed.session.accessToken),
    });
    expect(errorCode(me)).toBe("unauthenticated");
    expect(await closeCode(stream)).toBe(4401);
  });

  it("改口令（§18）：旧口令错 401；成功后其它设备失效、当前设备换新刷新令牌；旧口令登不上", async () => {
    const NEXT = "saffron-glacier-meadow-77";
    secrets.push(NEXT);
    const signIn = async (password: string) =>
      call("POST", `${issuer}/v1/auth/login`, {
        body: { account: "me", password, device: DEVICE },
      });
    type Answer = { session: typeof ownerSession };
    const current = (output("auth", "login", await signIn(PASSWORD)) as Answer)
      .session;
    const other = (output("auth", "login", await signIn(PASSWORD)) as Answer)
      .session;
    secrets.push(current.accessToken, other.accessToken);
    const wrong = await call("POST", `${issuer}/v1/auth/password`, {
      body: { password: "wrong-old-password", newPassword: NEXT },
      headers: bearer(current.accessToken),
    });
    expect(wrong.status).toBe(401);
    expect(errorCode(wrong)).toBe("credentials_invalid");
    const changed = output(
      "auth",
      "changePassword",
      await call("POST", `${issuer}/v1/auth/password`, {
        body: { password: PASSWORD, newPassword: NEXT },
        headers: bearer(current.accessToken),
      }),
    ) as Answer;
    secrets.push(changed.session.refreshToken as string);
    const meOf = (token: string) =>
      call("GET", `${issuer}/v1/me`, { headers: bearer(token) });
    expect((await meOf(current.accessToken)).status).toBe(200);
    expect(errorCode(await meOf(other.accessToken))).toBe("unauthenticated");
    const stale = await call("POST", `${issuer}/v1/auth/refresh`, {
      body: { refreshToken: other.refreshToken },
    });
    expect(errorCode(stale)).toBe("session_revoked");
    output(
      "auth",
      "refresh",
      await call("POST", `${issuer}/v1/auth/refresh`, {
        body: { refreshToken: changed.session.refreshToken },
      }),
    );
    expect(errorCode(await signIn(PASSWORD))).toBe("credentials_invalid");
    output("auth", "login", await signIn(NEXT));
  });

  it("错 5 次锁定：第 6 次答 429 account_locked，带 retryAfterMs 与 Retry-After，不再比对口令", async () => {
    for (let index = 0; index < 5; index += 1) {
      const reply = await call("POST", `${issuer}/v1/auth/login`, {
        body: {
          account: "me",
          password: `wrong-password-${index}`,
          device: DEVICE,
        },
      });
      expect(errorCode(reply)).toBe("credentials_invalid");
      expect(reply.status).toBe(401);
    }
    const locked = await call("POST", `${issuer}/v1/auth/login`, {
      body: { account: "me", password: PASSWORD, device: DEVICE },
    });
    expect(locked.status).toBe(429);
    const body = wireErrorSchema.parse(locked.body);
    expect(body.code).toBe("account_locked");
    expect(
      (body.details as { retryAfterMs: number }).retryAfterMs,
    ).toBeGreaterThan(55_000);
    expect(locked.headers["retry-after"]).toBe("60");
  });

  it("零日志：日志里没有口令、令牌、秘密、正文与路径", () => {
    const text = logs.join("\n");
    expect(text).toContain("个人中转已启动");
    expect(text).not.toContain(MARKER);
    expect(text).not.toContain("/echo");
    for (const secret of secrets) {
      expect(secret.length).toBeGreaterThan(8);
      expect(text).not.toContain(secret.slice(-16));
    }
  });

  it("零落盘：SQLite 库（连同 WAL）里没有口令、令牌、秘密与正文", () => {
    const data = join(root, "data");
    const files = readdirSync(data).filter((name) =>
      name.startsWith("relay.db"),
    );
    expect(files).toContain("relay.db");
    const bytes = files
      .map((name) => readFileSync(join(data, name)).toString("latin1"))
      .join("\n");
    expect(bytes).not.toContain(MARKER);
    expect(bytes).not.toContain(PASSWORD);
    expect(bytes).not.toContain("saffron-glacier-meadow-77");
    for (const secret of secrets)
      expect(bytes).not.toContain(secret.slice(-16));
  });
});

async function waitFor(check: () => boolean, timeoutMs = 5_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!check()) {
    if (Date.now() > deadline) throw new Error("等待超时");
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}
