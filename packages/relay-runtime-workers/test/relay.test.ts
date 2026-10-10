/**
 * Workers 中继端到端（workerd 本地）：初始化 → 登录 → 登记源 → 隧道握手 → 经中继的 HTTP 与 WS。
 */
import { beforeAll, describe, expect, it } from "vitest";

import {
  assertion,
  call,
  closed,
  DEVICE,
  heartbeatHint,
  json,
  login,
  nextMessage,
  openClientWs,
  postJson,
  registerSource,
  viaRelay,
  type Session,
  type TestSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

let session: Session;
let source: TestSource;
let core: FakeCore;
let relayToken: string;

beforeAll(async () => {
  session = await login();
  source = await registerSource(session);
  const hint = await heartbeatHint(source);
  core = new FakeCore(source);
  const outcome = await core.connect(
    hint.nodes[0]?.url ?? "",
    hint.tunnelToken,
  );
  if (!outcome.ready) throw new Error(`没连上：${JSON.stringify(outcome)}`);
  relayToken = (await assertion(session, source.sourceId)).relayToken;
});

describe("隧道", () => {
  it("节点地址带 ?src=<sourceId>；ready 下发 45 秒心跳，中继不主动 PING、源的 PING 立即回 PONG", async () => {
    const hint = await heartbeatHint(source);
    expect(hint.nodes).toEqual([
      {
        url: `wss://relay.armadra.test/t/v1?src=${source.sourceId}`,
        weight: 1,
      },
    ]);
    expect(core.ready?.heartbeatMs).toBe(45_000);
    expect(core.ready?.protocol).toEqual({ major: 1, minor: 2 });
    await core.ping();
  });

  it("源在线：assertion 报 online", async () => {
    expect((await assertion(session, source.sourceId)).online).toBe(true);
  });
});

describe("HTTP 转发", () => {
  it("剥令牌与 Cloudflare 头、加 X-Forwarded-*，路径去掉 /s/<id>", async () => {
    const response = await viaRelay(source.sourceId, "/echo-request?x=1", {
      token: relayToken,
      headers: { "cf-ray": "abc", "x-forwarded-for": "6.6.6.6" },
    });
    expect(response.status).toBe(200);
    const body = await json<{
      method: string;
      url: string;
      headers: Record<string, string>;
    }>(response);
    expect(body.url).toBe("/echo-request?x=1");
    expect(body.headers["armadra-relay-token"]).toBeUndefined();
    expect(body.headers["cf-ray"]).toBeUndefined();
    expect(body.headers["cf-connecting-ip"]).toBeUndefined();
    expect(body.headers["x-forwarded-for"]).toBe("203.0.113.1");
    expect(body.headers["x-forwarded-proto"]).toBe("https");
    expect(body.headers.connection).toBe("close");
    expect(response.headers.get("set-cookie")).toBe("a=1");
  });
});

describe("HTTP 转发（续）", () => {
  it("请求体：带长度的原样转、没有长度的走 chunked；响应 chunked 照样拼回", async () => {
    const fixed = await viaRelay(source.sourceId, "/echo", {
      token: relayToken,
      method: "POST",
      body: "hello relay",
      headers: { "content-type": "text/plain" },
    });
    expect(fixed.status).toBe(200);
    expect(await fixed.text()).toBe("hello relay");
    const lastFixed = core.requests.at(-1);
    expect(lastFixed?.headers["content-length"]).toBe("11");

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("part-1;"));
        controller.enqueue(new TextEncoder().encode("part-2"));
        controller.close();
      },
    });
    const chunked = await viaRelay(source.sourceId, "/echo", {
      token: relayToken,
      method: "POST",
      body: stream,
      // @ts-expect-error -- workerd 要 duplex 才能流式上传
      duplex: "half",
    });
    expect(await chunked.text()).toBe("part-1;part-2");
    expect(core.requests.at(-1)?.headers["transfer-encoding"]).toBe("chunked");

    const response = await viaRelay(source.sourceId, "/chunked", {
      token: relayToken,
    });
    expect(await response.text()).toBe("chunked body here");
  });

  it("大响应体按信用背压、长度不丢（1 MiB，经 FixedLengthStream 带 Content-Length）", async () => {
    const total = 1024 * 1024;
    const response = await viaRelay(source.sourceId, `/big?bytes=${total}`, {
      token: relayToken,
    });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-length")).toBe(String(total));
    const bytes = new Uint8Array(await response.arrayBuffer());
    expect(bytes.length).toBe(total);
    let mismatch = -1;
    for (let index = 0; index < total; index += 1)
      if (bytes[index] !== ((index * 31 + 7) & 0xff)) {
        mismatch = index;
        break;
      }
    expect(mismatch).toBe(-1);
    expect(core.openStreams).toBe(0);
  });

  it("源的响应不合法：重置流，答 502 bad_gateway", async () => {
    const response = await viaRelay(source.sourceId, "/bad", {
      token: relayToken,
    });
    expect(response.status).toBe(502);
    expect((await json(response)).code).toBe("bad_gateway");
  });

  it("没令牌 401、令牌是别的源的 403、地址不是源 404；预检由中继自己答、不进隧道", async () => {
    expect((await viaRelay(source.sourceId, "/echo-request")).status).toBe(401);
    const other = await registerSource(session);
    const otherToken = (await assertion(session, other.sourceId)).relayToken;
    const mismatch = await viaRelay(source.sourceId, "/echo-request", {
      token: otherToken,
    });
    expect(mismatch.status).toBe(403);
    expect((await json(mismatch)).code).toBe("relay_source_mismatch");
    // 别的源没连隧道：源离线。
    const offline = await viaRelay(other.sourceId, "/echo-request", {
      token: otherToken,
    });
    expect(offline.status).toBe(503);
    expect((await json(offline)).code).toBe("source_offline");

    const before = core.requests.length;
    const preflight = await viaRelay(source.sourceId, "/api/x", {
      method: "OPTIONS",
      headers: {
        origin: "https://app.armadra.test",
        "access-control-request-method": "POST",
      },
    });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("access-control-allow-origin")).toBe(
      "https://app.armadra.test",
    );
    const denied = await viaRelay(source.sourceId, "/api/x", {
      method: "OPTIONS",
      headers: {
        origin: "https://evil.example",
        "access-control-request-method": "POST",
      },
    });
    expect(denied.status).toBe(403);
    expect(core.requests.length).toBe(before);
    expect((await call("/s/nothex/echo")).status).toBe(404);
  });
});

describe("媒体票", () => {
  it("签票 → 兑票按 Range 转到签票时的路径（206）；票不认识 404；只认 GET / HEAD", async () => {
    const issued = await viaRelay(source.sourceId, "/_relay/media-tickets", {
      token: relayToken,
      method: "POST",
      body: JSON.stringify({ path: "/media?v=1" }),
      headers: { "content-type": "application/json" },
    });
    expect(issued.status).toBe(200);
    const ticket = await json<{ ticket: string; path: string }>(issued);
    expect(ticket.path).toBe(`/_relay/m/${ticket.ticket}`);
    const ranged = await viaRelay(source.sourceId, ticket.path, {
      headers: { range: "bytes=10-19" },
    });
    expect(ranged.status).toBe(206);
    expect(ranged.headers.get("content-range")).toBe("bytes 10-19/1000");
    expect(ranged.headers.get("content-length")).toBe("10");
    const bytes = new Uint8Array(await ranged.arrayBuffer());
    expect([...bytes]).toEqual(
      Array.from({ length: 10 }, (_, index) => ((index + 10) * 31 + 7) & 0xff),
    );
    expect(core.requests.at(-1)?.path).toBe("/media?v=1");
    // 再取一次（播放器拖动）：票可多次使用。
    const again = await viaRelay(source.sourceId, ticket.path, {
      headers: { range: "bytes=0-0" },
    });
    expect(again.status).toBe(206);
    await again.arrayBuffer();
    const unknown = await viaRelay(
      source.sourceId,
      `/_relay/m/${"A".repeat(43)}`,
    );
    expect(unknown.status).toBe(404);
    const posted = await viaRelay(source.sourceId, ticket.path, {
      method: "POST",
    });
    expect(posted.status).toBe(400);
  });
});

describe("只读令牌（ro）", () => {
  it("只放行安全方法与身份域；写方法 403 不进隧道", async () => {
    const created = await postJson(
      "/v1/links",
      {
        kind: "source_invite",
        sourceId: source.sourceId,
        invitationId: "inv_ro",
        label: "ro",
        role: "viewer",
        expiresAtMs: Date.now() + 3_600_000,
        scope: { workspaceId: "ws_1", readOnly: true },
      },
      { token: session.accessToken },
    );
    expect(created.status).toBe(200);
    const link = await json<{ linkId: string; secret: string }>(created);
    const accepted = await postJson(`/v1/links/${link.linkId}/accept`, {
      secret: link.secret,
      device: DEVICE,
    });
    expect(accepted.status).toBe(200);
    const guest = await json<{ relayToken: string }>(accepted);
    const read = await viaRelay(source.sourceId, "/echo-request", {
      token: guest.relayToken,
    });
    expect(read.status).toBe(200);
    await read.arrayBuffer();
    const before = core.requests.length;
    const write = await viaRelay(source.sourceId, "/api/write", {
      token: guest.relayToken,
      method: "POST",
      body: "{}",
    });
    expect(write.status).toBe(403);
    expect((await json(write)).code).toBe("forbidden");
    expect(core.requests.length).toBe(before);
    const identity = await viaRelay(source.sourceId, "/api/identity/login", {
      token: guest.relayToken,
      method: "POST",
      body: "{}",
    });
    expect(identity.status).toBe(200);
    await identity.arrayBuffer();
  });
});

describe("客户端 WebSocket", () => {
  it("升级经隧道到源：子协议剥掉令牌、选 ticket；文本 / 二进制回显", async () => {
    const { response, ws } = await openClientWs(
      source.sourceId,
      "/ws/echo?q=1",
      relayToken,
    );
    expect(response.status).toBe(101);
    expect(response.headers.get("sec-websocket-protocol")).toBe(
      "armadra-ticket.test",
    );
    if (!ws) throw new Error("没有 WS");
    ws.send("whoami");
    const whoami = JSON.parse(String(await nextMessage(ws))) as {
      url: string;
      protocol: string;
    };
    expect(whoami.url).toBe("/ws/echo?q=1");
    expect(whoami.protocol).toBe("armadra-ticket.test");
    ws.send("hello");
    expect(await nextMessage(ws)).toBe("hello");
    ws.send(Uint8Array.of(1, 2, 3));
    expect([...new Uint8Array((await nextMessage(ws)) as ArrayBuffer)]).toEqual(
      [1, 2, 3],
    );
    ws.close(1000, "done");
  });

  it("core 发 ping 由中继代答 pong；core 关闭（4000）原样转给客户端", async () => {
    const { ws } = await openClientWs(source.sourceId, "/ws", relayToken);
    if (!ws) throw new Error("没有 WS");
    const pongs = core.pongsFromRelay;
    ws.send("ping-me");
    ws.send("sync");
    expect(await nextMessage(ws)).toBe("sync");
    expect(core.pongsFromRelay).toBe(pongs + 1);
    const done = closed(ws);
    ws.send("bye");
    expect(await done).toEqual({ code: 4000, reason: "bye" });
  });

  it("令牌无效：先完成升级再以 4401 关闭（浏览器读不到升级失败的状态码）", async () => {
    const { response, ws } = await openClientWs(source.sourceId, "/ws", "bad");
    expect(response.status).toBe(101);
    if (!ws) throw new Error("没有 WS");
    expect((await closed(ws)).code).toBe(4401);
  });

  it("客户端单条消息超过 1 MiB：以 1009 关闭", async () => {
    const { ws } = await openClientWs(source.sourceId, "/ws", relayToken);
    if (!ws) throw new Error("没有 WS");
    const done = closed(ws);
    ws.send(new Uint8Array(1024 * 1024 + 1));
    expect((await done).code).toBe(1009);
  });
});
