/**
 * Workers 中继的流量统计（cloud-api §19）：`SourceDO` 在自己的 DO SQLite 里记，`ControlDO` 的
 * `/v1/me/traffic` 逐源取回合并；检查周期落库；休眠唤醒后接着记；保留期清理。
 */
import {
  env,
  evictDurableObject,
  runDurableObjectAlarm,
  runInDurableObject,
} from "cloudflare:test";
import { describe, expect, it } from "vitest";

import {
  ConfigError,
  readConfig,
  sourceObjectName,
  type RelayEnv,
} from "../src/env.ts";
import type { SourceDO } from "../src/source-do.ts";
import {
  assertion,
  call,
  heartbeatHint,
  json,
  echoChannel,
  login,
  openChannel,
  registerSource,
  type Session,
  type TestSource,
} from "./api.ts";
import { FakeCore } from "./fake-core.ts";

interface Bucket {
  bucketStartMs: number;
  node: string;
  sourceId: string;
  subject: string;
  deviceId: string;
  kind: string;
  connections: number;
  bytesToSource: number;
  bytesToClient: number;
}

const stubOf = (sourceId: string) =>
  env.SOURCE.get(env.SOURCE.idFromName(sourceObjectName(sourceId)));

async function online(capabilities: string[] = ["e2e"]): Promise<{
  session: Session;
  source: TestSource;
  relayToken: string;
}> {
  const session = await login();
  const source = await registerSource(session);
  const hint = await heartbeatHint(source);
  const core = new FakeCore(source);
  const outcome = await core.connect(
    hint.nodes[0]?.url ?? "",
    hint.tunnelToken,
    { capabilities },
  );
  if (!outcome.ready) throw new Error(`没连上：${JSON.stringify(outcome)}`);
  const { relayToken } = await assertion(session, source.sourceId);
  return { session, source, relayToken };
}

async function traffic(
  session: Session,
  query: string,
): Promise<{ status: number; buckets: Bucket[] }> {
  const response = await call(`/v1/me/traffic?${query}`, {
    headers: { authorization: `Bearer ${session.accessToken}` },
  });
  const body = await json<{ buckets?: Bucket[] }>(response);
  return { status: response.status, buckets: body.buckets ?? [] };
}

const range = (sourceId: string) =>
  `fromMs=0&toMs=${Date.now() + 3_600_000}&sourceId=${sourceId}`;

const sum = (rows: Bucket[], field: keyof Bucket) =>
  rows.reduce((total, row) => total + Number(row[field]), 0);

describe("流量统计", () => {
  it("通道按 e2e 记到源的对象里，/v1/me/traffic 合并答出；明文请求不记；不记路径", async () => {
    const { session, source, relayToken } = await online();
    for (let index = 0; index < 3; index += 1) {
      const { ws } = await openChannel(source.sourceId, relayToken);
      if (!ws) throw new Error("没有 WS");
      await echoChannel(ws, Uint8Array.of(index));
      ws.close(1000, "done");
    }
    // 明文请求 404，不开流、不计。
    await (await call(`/s/${source.sourceId}/echo-request`)).arrayBuffer();

    const { status, buckets } = await traffic(session, range(source.sourceId));
    expect(status).toBe(200);
    expect(buckets.every((row) => row.kind === "e2e")).toBe(true);
    expect(sum(buckets, "connections")).toBe(3);
    expect(sum(buckets, "bytesToSource")).toBe(3);
    for (const row of buckets)
      expect(row).toMatchObject({
        node: "cf",
        sourceId: source.sourceId,
        subject: "acct:owner",
      });
    expect(JSON.stringify(buckets)).not.toContain("echo");
    const daily = await traffic(
      session,
      `${range(source.sourceId)}&granularity=day`,
    );
    expect(sum(daily.buckets, "connections")).toBe(3);
    expect((await traffic(session, "fromMs=5&toMs=5")).status).toBe(400);
  });

  it("端到端通道记为 e2e：字节是边缘 WS 载荷之和，设备取令牌的 dev", async () => {
    const { session, source, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    let got = 0;
    const echoed = new Promise<void>((resolve) =>
      ws.addEventListener("message", (event) => {
        got += (event.data as ArrayBuffer).byteLength;
        if (got >= 10_000) resolve();
      }),
    );
    for (let index = 0; index < 10; index += 1)
      ws.send(new Uint8Array(1000).fill(7));
    await echoed;
    ws.close(1000, "done");
    const { buckets } = await traffic(session, range(source.sourceId));
    expect(buckets).toEqual([
      expect.objectContaining({
        kind: "e2e",
        subject: "acct:owner",
        connections: 1,
        bytesToSource: 10_000,
        bytesToClient: 10_000,
      }),
    ]);
    expect(buckets[0]?.deviceId).not.toBe("");
  });

  it("检查周期把内存里的计数写进对象的 DO SQLite", async () => {
    const { source, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    await echoChannel(ws, Uint8Array.of(1));
    ws.close(1000, "done");
    const stub = stubOf(source.sourceId);
    await runDurableObjectAlarm(stub);
    const rows = await runInDurableObject(stub, (_instance, state) =>
      state.storage.sql
        .exec<{
          kind: string;
          connections: number;
        }>("SELECT kind, connections FROM relay_traffic_buckets")
        .toArray(),
    );
    expect(rows).toEqual([{ kind: "e2e", connections: 1 }]);
  });

  it("休眠唤醒后经还原的通道接着记字节（不重复计连接）", async () => {
    const { session, source, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    await echoChannel(ws, Uint8Array.of(1, 2));
    const before = await traffic(session, range(source.sourceId));
    const wsBefore = before.buckets.filter((row) => row.kind === "e2e");
    await evictDurableObject(stubOf(source.sourceId), {
      webSockets: "hibernate",
    });
    await echoChannel(ws, Uint8Array.of(3, 4, 5));
    const after = await traffic(session, range(source.sourceId));
    const wsAfter = after.buckets.filter((row) => row.kind === "e2e");
    expect(sum(wsAfter, "connections")).toBe(1);
    expect(sum(wsAfter, "bytesToSource")).toBeGreaterThan(
      sum(wsBefore, "bytesToSource"),
    );
    ws.close(1000, "done");
  });

  it("保留期：查询前一天清一次，早于 90 天的桶删掉", async () => {
    const { session, source, relayToken } = await online();
    const { ws } = await openChannel(source.sourceId, relayToken);
    if (!ws) throw new Error("没有 WS");
    await echoChannel(ws, Uint8Array.of(1));
    ws.close(1000, "done");
    const stub = stubOf(source.sourceId);
    await runInDurableObject(stub, async (instance: SourceDO, state) => {
      await instance.trafficQuery({
        fromMs: 0,
        toMs: Date.now() + 1,
        granularity: "hour",
        limit: 10,
      });
      state.storage.sql.exec(
        `INSERT INTO relay_traffic_buckets
          (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind,
           connections, bytes_to_source, bytes_to_client)
          VALUES ('default', ?, 'cf', ?, 'acct:owner', '', 'e2e', 1, 1, 1)`,
        Date.now() - 91 * 86_400_000,
        source.sourceId,
      );
      state.storage.kv.put("traffic:prunedAtMs", 0);
    });
    const { buckets } = await traffic(session, range(source.sourceId));
    expect(buckets).toHaveLength(1);
    expect(buckets[0]?.bucketStartMs).toBeGreaterThan(
      Date.now() - 2 * 86_400_000,
    );
  });
});

describe("配置", () => {
  const base = {
    RELAY_ISSUER: "https://relay.armadra.test",
  } as unknown as RelayEnv;
  it("RELAY_TRAFFIC_RETENTION_DAYS：缺省 90，0 关闭，超出范围或不是整数拒绝", () => {
    expect(readConfig(base).trafficRetentionDays).toBe(90);
    expect(
      readConfig({ ...base, RELAY_TRAFFIC_RETENTION_DAYS: "0" })
        .trafficRetentionDays,
    ).toBe(0);
    for (const bad of ["-1", "1.5", "3651", "x"])
      expect(() =>
        readConfig({ ...base, RELAY_TRAFFIC_RETENTION_DAYS: bad }),
      ).toThrow(ConfigError);
  });
});
