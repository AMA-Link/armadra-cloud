/**
 * Workers 中继的端到端通道（cloud-api §20，workerd 本地）：`_relay/e2e` 的 WS 消息与隧道
 * `OPEN kind: "e2e"` 的 `DATA` 原样互转，不经 HTTP 转发；拒绝路径与 Node 中继相同。
 */
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { env, evictDurableObject, runInDurableObject } from "cloudflare:test";
import { beforeAll, describe, expect, it } from "vitest";

import { sourceObjectName } from "../src/env.ts";
import type { SourceDO } from "../src/source-do.ts";

import {
  assertion,
  call,
  closed,
  heartbeatHint,
  login,
  nextMessage,
  openClientWs,
  registerSource,
  type Session,
  type TestSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

let session: Session;

async function online(capabilities: string[]) {
  const source = await registerSource(session);
  const hint = await heartbeatHint(source);
  const core = new FakeCore(source);
  const outcome = await core.connect(
    hint.nodes[0]?.url ?? "",
    hint.tunnelToken,
    {
      capabilities,
    },
  );
  if (!outcome.ready) throw new Error(`没连上：${JSON.stringify(outcome)}`);
  const { relayToken } = await assertion(session, source.sourceId);
  return { source, core, relayToken };
}

function e2e(source: TestSource, token: string, protocols = ["armadra-e2e.1"]) {
  return openClientWs(source.sourceId, "/_relay/e2e", token, protocols);
}

beforeAll(async () => {
  session = await login();
});

describe("端到端通道", () => {
  it("源报 e2e：选 armadra-e2e.1，OPEN kind e2e 不带头，二进制字节双向原样（1 MiB）", async () => {
    const { source, core, relayToken } = await online(["e2e"]);
    const { response, ws } = await e2e(source, relayToken);
    expect(response.status).toBe(101);
    expect(response.headers.get("sec-websocket-protocol")).toBe(
      "armadra-e2e.1",
    );
    if (!ws) throw new Error("没有 WS");
    const open = core.opens.at(-1);
    expect(open).toMatchObject({
      kind: "e2e",
      method: "GET",
      path: "/_relay/e2e",
      headers: [],
      relayNode: "cf",
    });
    const total = 1024 * 1024;
    const sent = new Uint8Array(total);
    for (let index = 0; index < total; index += 1)
      sent[index] = (index * 131 + 17) & 0xff;
    const received: Uint8Array[] = [];
    let got = 0;
    const all = new Promise<void>((resolve) => {
      ws.addEventListener("message", (event) => {
        const chunk = new Uint8Array(event.data as ArrayBuffer);
        received.push(chunk);
        got += chunk.length;
        if (got >= total) resolve();
      });
    });
    for (let at = 0; at < total; at += 60_000)
      ws.send(sent.subarray(at, Math.min(total, at + 60_000)));
    await all;
    const joined = new Uint8Array(got);
    let offset = 0;
    for (const chunk of received) {
      joined.set(chunk, offset);
      offset += chunk.length;
    }
    expect(joined).toEqual(sent);
    expect(core.e2eBytes).toBe(total);
    const done = closed(ws);
    ws.close(1000, "done");
    expect((await done).code).toBe(1000);
  }, 30_000);

  it("拒绝：令牌无效 4401、缺版本子协议 4409、源没报 e2e 4404 e2e_unsupported；普通 HTTP 404 不进隧道", async () => {
    const plain = await online([]);
    const opens = plain.core.opens.length;
    const invalid = await e2e(plain.source, "bad");
    expect((await closed(invalid.ws as WebSocket)).code).toBe(
      CLOSE.unauthenticated,
    );
    const noVersion = await e2e(plain.source, plain.relayToken, [
      "armadra-ticket.t",
    ]);
    expect(await closed(noVersion.ws as WebSocket)).toEqual({
      code: CLOSE.protocolUnsupported,
      reason: "protocol_unsupported",
    });
    const unsupported = await e2e(plain.source, plain.relayToken);
    expect(await closed(unsupported.ws as WebSocket)).toEqual({
      code: CLOSE.sourceOffline,
      reason: "e2e_unsupported",
    });
    const http = await call(`/s/${plain.source.sourceId}/_relay/e2e`, {
      headers: { "armadra-relay-token": plain.relayToken },
    });
    expect(http.status).toBe(404);
    expect(plain.core.opens.length).toBe(opens);
  });

  it("文本消息以 4400 关闭；源下线以 4404 结束", async () => {
    const { source, core, relayToken } = await online(["e2e"]);
    const text = await e2e(source, relayToken);
    const ws = text.ws as WebSocket;
    const done = closed(ws);
    ws.send("plain");
    expect((await done).code).toBe(CLOSE.badFrame);
    const live = (await e2e(source, relayToken)).ws as WebSocket;
    live.send(Uint8Array.of(7, 8, 9));
    expect([
      ...new Uint8Array((await nextMessage(live)) as ArrayBuffer),
    ]).toEqual([7, 8, 9]);
    const gone = closed(live);
    core.disconnect();
    expect((await gone).code).toBe(CLOSE.sourceOffline);
  });

  it("休眠后唤醒：能力与 e2e 桥照样接上（字节仍原样透传，新通道照常开）", async () => {
    const { source, relayToken } = await online(["e2e"]);
    const ws = (await e2e(source, relayToken)).ws as WebSocket;
    ws.send(Uint8Array.of(1));
    expect([...new Uint8Array((await nextMessage(ws)) as ArrayBuffer)]).toEqual(
      [1],
    );
    const stub = env.SOURCE.get(
      env.SOURCE.idFromName(sourceObjectName(source.sourceId)),
    );
    await evictDurableObject(stub, { webSockets: "hibernate" });
    expect(
      await runInDurableObject(stub, (instance: SourceDO) => instance.stats()),
    ).toMatchObject({ tunnels: 1, streams: 1, clients: 1 });
    ws.send(Uint8Array.of(2, 3));
    expect([...new Uint8Array((await nextMessage(ws)) as ArrayBuffer)]).toEqual(
      [2, 3],
    );
    const second = (await e2e(source, relayToken)).ws as WebSocket;
    second.send(Uint8Array.of(4));
    expect([
      ...new Uint8Array((await nextMessage(second)) as ArrayBuffer),
    ]).toEqual([4]);
    ws.close(1000, "done");
    second.close(1000, "done");
  });
});
