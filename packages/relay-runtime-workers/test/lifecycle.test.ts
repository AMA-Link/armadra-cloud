/**
 * 对象的生命周期：休眠后唤醒接着用、检查周期判死、同源替换、`?src=` 与令牌不符、撤销即时生效。
 */
import {
  env,
  evictDurableObject,
  runDurableObjectAlarm,
  runInDurableObject,
} from "cloudflare:test";
import { beforeAll, describe, expect, it } from "vitest";

import { sourceObjectName } from "../src/env.ts";
import type { SourceDO } from "../src/source-do.ts";
import {
  assertion,
  byteDiff,
  call,
  channelOutcome,
  closed,
  DEVICE,
  echoChannel,
  heartbeatHint,
  json,
  login,
  openChannel,
  postJson,
  registerSource,
  REQUESTS_TIMEOUT_MS,
  scryptTimeout,
  type Session,
  type TestSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

const stubOf = (sourceId: string) =>
  env.SOURCE.get(env.SOURCE.idFromName(sourceObjectName(sourceId)));

// 登录只是铺垫（这里测的是对象的生命周期）：整个文件做一次，不让每条用例各付一次 scrypt。
let session: Session;

beforeAll(async () => {
  session = await login();
}, scryptTimeout(1));

async function online(): Promise<{
  session: Session;
  source: TestSource;
  core: FakeCore;
  relayToken: string;
}> {
  const source = await registerSource(session);
  const hint = await heartbeatHint(source);
  const core = new FakeCore(source);
  const outcome = await core.connect(
    hint.nodes[0]?.url ?? "",
    hint.tunnelToken,
  );
  if (!outcome.ready) throw new Error(`没连上：${JSON.stringify(outcome)}`);
  const { relayToken } = await assertion(session, source.sourceId);
  return { session, source, core, relayToken };
}

/**
 * 唤醒到接上（还原隧道与流表、答第一条消息、代答 ping）的上限。本地 workerd 实测：安静时约
 * 10 ms，load 110–180 时不到 100 ms；给 2 秒是二十倍以上的余量，机器忙不会误报，而唤醒路径真慢到
 * 秒级（例如还原时多了一轮存储往返或等待）就失败。
 */
const WAKE_BUDGET_MS = 2_000;

describe("休眠", () => {
  // 整条用例的成本主要不在唤醒：登记源、接隧道、600 KiB 过信用窗口回显、`channelOutcome` 固定等
  // 200 ms（登录已在文件的 `beforeAll` 里做掉）。load 110 时约 2.5 秒、load 180 时近 5 秒，给 15 秒；唤醒那段另有 WAKE_BUDGET_MS。
  it("对象换出内存后唤醒：隧道、流表与客户端通道原样接上，信用不漏", async () => {
    const { source, core, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    const before = Uint8Array.of(1, 2, 3);
    expect(await echoChannel(ws, before)).toEqual(before);

    // 攒着没写的流量统计会让对象留在内存里等定时写（不带着计数休眠）：先让检查周期写掉。
    await runDurableObjectAlarm(stubOf(source.sourceId));
    await evictDurableObject(stubOf(source.sourceId), {
      webSockets: "hibernate",
    });
    const wakeStarted = performance.now();
    expect(
      await runInDurableObject(stubOf(source.sourceId), (instance: SourceDO) =>
        instance.stats(),
      ),
    ).toMatchObject({ tunnels: 1, streams: 1, clients: 1 });
    const after = Uint8Array.of(4, 5, 6);
    expect(await echoChannel(ws, after)).toEqual(after);
    await core.ping();
    expect(
      performance.now() - wakeStarted,
      "唤醒到接上的耗时（ms）",
    ).toBeLessThan(WAKE_BUDGET_MS);

    // 多来几轮（> 半个窗口）：唤醒后补窗口的账照样对得上；隧道还能开新通道。
    const big = new Uint8Array(600 * 1024).map((_, index) => index & 0xff);
    expect(byteDiff(await echoChannel(ws, big), big)).toEqual({
      length: big.length,
      firstMismatch: -1,
    });
    expect(await channelOutcome(source.sourceId, relayToken)).toBeNull();
    ws.close(1000, "done");
  }, 15_000);
});

describe("检查周期（alarm）", { timeout: REQUESTS_TIMEOUT_MS }, () => {
  it("超过 2 × heartbeatMs 没收到源的帧：以 1001 关闭隧道，源变离线", async () => {
    const { session, source, core } = await online();
    await runInDurableObject(stubOf(source.sourceId), (instance: SourceDO) => {
      const internals = instance as unknown as {
        tunnels: Map<unknown, { attachment: { lastHeardAtMs: number } }>;
      };
      for (const record of internals.tunnels.values())
        record.attachment.lastHeardAtMs = Date.now() - 200_000;
    });
    expect(await runDurableObjectAlarm(stubOf(source.sourceId))).toBe(true);
    expect(await core.closed()).toEqual({
      code: 1001,
      reason: "heartbeat_timeout",
    });
    expect((await assertion(session, source.sourceId)).online).toBe(false);
  });

  it("有心跳就不判死，并排下一次检查", async () => {
    const { source, core } = await online();
    await core.ping();
    expect(await runDurableObjectAlarm(stubOf(source.sourceId))).toBe(true);
    await core.ping();
    expect(core.closeInfo).toBeNull();
  });
});

describe("隧道替换与身份", { timeout: REQUESTS_TIMEOUT_MS }, () => {
  it("同源第二条隧道就绪：旧的收 GOAWAY replaced、以 4491 关闭，新的接着服务", async () => {
    const { source, core, relayToken } = await online();
    const hint = await heartbeatHint(source);
    const second = new FakeCore(source);
    expect(
      (await second.connect(hint.nodes[0]?.url ?? "", hint.tunnelToken)).ready,
    ).toBeTruthy();
    expect((await core.closed()).code).toBe(4491);
    expect(await channelOutcome(source.sourceId, relayToken)).toBeNull();
    expect(second.opens.filter((open) => open.kind === "e2e")).toHaveLength(1);
  });

  it("?src= 指向别的源而令牌是这个源的：拒绝 tunnel_token_invalid（4490）", async () => {
    const mine = await registerSource(session);
    const other = await registerSource(session);
    const hint = await heartbeatHint(mine);
    const core = new FakeCore(mine);
    const outcome = await core.connect(
      `wss://relay.armadra.test/t/v1?src=${other.sourceId}`,
      hint.tunnelToken,
    );
    expect(outcome.reject?.code).toBe("tunnel_token_invalid");
    expect(outcome.closeCode).toBe(4490);
  });

  it("不是升级、src 不是 32 位 hex：404", async () => {
    expect((await call("/t/v1?src=xyz")).status).toBe(404);
  });
});

/**
 * 撤销后已签出的令牌立即答 4401：源对象在断开 / 关隧道前先作废撤销判断的缓存（#180），所以不用等，
 * 撤销请求一答 200 就该成立；答 4404 说明隧道先关了、缓存还没作废。
 */
const tokenRejected = async (sourceId: string, token: string) =>
  expect(await channelOutcome(sourceId, token)).toBe(4401);

describe("撤销即时生效", { timeout: REQUESTS_TIMEOUT_MS }, () => {
  it("撤销源：隧道 4492、经中继的通道 4403、已签出的中继令牌 4401", async () => {
    const { session, source, core, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    await echoChannel(ws, Uint8Array.of(1));
    const wsClosed = closed(ws);
    const revoked = await call(`/v1/sources/${source.sourceId}`, {
      method: "DELETE",
      headers: { authorization: `Bearer ${session.accessToken}` },
    });
    expect(revoked.status).toBe(200);
    expect((await core.closed()).code).toBe(4492);
    expect((await wsClosed).code).toBe(4403);
    await tokenRejected(source.sourceId, relayToken);
    // 撤销后的源换不到隧道令牌。
    expect(
      await heartbeatHint(source).catch((error: Error) => error.message),
    ).toMatch(/^relay 410/);
  });

  it("撤销分享链接：访客的通道断开，令牌失效；owner 不受影响", async () => {
    const { session, source, relayToken } = await online();
    const created = await postJson(
      "/v1/links",
      {
        kind: "source_invite",
        sourceId: source.sourceId,
        invitationId: "inv_guest",
        label: "guest",
        role: "viewer",
        expiresAtMs: Date.now() + 3_600_000,
      },
      { token: session.accessToken },
    );
    const link = await json<{ linkId: string; secret: string }>(created);
    const accepted = await postJson(`/v1/links/${link.linkId}/accept`, {
      secret: link.secret,
      device: DEVICE,
    });
    const guest = await json<{ relayToken: string }>(accepted);
    const { ws } = await openChannel(source.sourceId, guest.relayToken);
    if (!ws) throw new Error("没有 WS");
    await echoChannel(ws, Uint8Array.of(1));
    const guestClosed = closed(ws);
    const revoked = await call(`/v1/links/${link.linkId}`, {
      method: "DELETE",
      headers: { authorization: `Bearer ${session.accessToken}` },
    });
    expect(revoked.status).toBe(200);
    expect((await guestClosed).code).toBe(4403);
    await tokenRejected(source.sourceId, guest.relayToken);
    expect(await channelOutcome(source.sourceId, relayToken)).toBeNull();
  });
});
