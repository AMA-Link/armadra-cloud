/**
 * 流量统计的内存累加器：聚合（同键相加、跨小时分桶）、批量写（一次 flush 一批、没东西不写）、
 * 写失败并回、并发下计数不丢、隧道按流计字节、边缘按令牌记连接；热路径开销。
 */

import { describe, expect, it } from "vitest";

import { fakeTunnel, OPEN } from "../test/fake-transport.ts";
import { relayToken, SOURCE_ID, testControlPlane } from "../test/tokens.ts";
import { createAuthContext } from "./auth/context.ts";
import { pathAddressing } from "./edge/addressing.ts";
import { EdgePolicy, type EdgeRequest } from "./edge/policy.ts";
import { RelayMetrics } from "./metrics.ts";
import {
  TRAFFIC_BUCKET_MS,
  TrafficMeter,
  type TrafficKey,
  type TrafficRow,
} from "./traffic.ts";

const T0 = Date.UTC(2026, 9, 12, 8);
const KEY: TrafficKey = {
  sourceId: SOURCE_ID,
  subject: "acct:alice",
  deviceId: "",
  kind: "http",
};

function meter(
  options: {
    fail?: () => boolean;
    delayMs?: number;
    maxPendingRows?: number;
  } = {},
) {
  const clock = { now: T0 };
  const batches: TrafficRow[][] = [];
  const errors: { dropped: number }[] = [];
  const traffic = new TrafficMeter({
    node: "relay-1",
    now: () => clock.now,
    maxPendingRows: options.maxPendingRows,
    write: async (rows) => {
      if (options.delayMs !== undefined)
        await new Promise((resolve) => setTimeout(resolve, options.delayMs));
      if (options.fail?.()) throw new Error("db down");
      batches.push(rows.map((row) => ({ ...row })));
    },
    onError: (_error, dropped) => errors.push({ dropped }),
  });
  return { traffic, clock, batches, errors };
}

/** 全部批次按键加总（模拟存储的 upsert）。 */
function totals(batches: TrafficRow[][]): Map<string, TrafficRow> {
  const out = new Map<string, TrafficRow>();
  for (const row of batches.flat()) {
    const key = [
      row.bucketStartMs,
      row.node,
      row.sourceId,
      row.subject,
      row.deviceId,
      row.kind,
    ].join("|");
    const found = out.get(key);
    if (found) {
      found.connections += row.connections;
      found.bytesToSource += row.bytesToSource;
      found.bytesToClient += row.bytesToClient;
    } else out.set(key, { ...row });
  }
  return out;
}

describe("聚合", () => {
  it("同一键的流加到同一行，不同键各一行；跨小时分到下一桶", async () => {
    const { traffic, clock, batches } = meter();
    const a = traffic.counter(KEY);
    const b = traffic.counter(KEY);
    const ws = traffic.counter({ ...KEY, kind: "ws", deviceId: "dev-1" });
    a.connection();
    b.connection();
    a.bytes("to_source", 100);
    b.bytes("to_client", 1000);
    ws.connection();
    ws.bytes("to_client", 7);
    clock.now = T0 + TRAFFIC_BUCKET_MS + 5;
    a.bytes("to_client", 1);
    traffic.add({ ...KEY, atMs: T0, bytesToSource: 3 });
    await traffic.flush();
    expect(batches).toHaveLength(1);
    expect(batches[0]).toEqual([
      {
        bucketStartMs: T0,
        node: "relay-1",
        ...KEY,
        connections: 2,
        bytesToSource: 103,
        bytesToClient: 1000,
      },
      {
        bucketStartMs: T0,
        node: "relay-1",
        ...KEY,
        kind: "ws",
        deviceId: "dev-1",
        connections: 1,
        bytesToSource: 0,
        bytesToClient: 7,
      },
      {
        bucketStartMs: T0 + TRAFFIC_BUCKET_MS,
        node: "relay-1",
        ...KEY,
        connections: 0,
        bytesToSource: 0,
        bytesToClient: 1,
      },
    ]);
  });
});

describe("批量写", () => {
  it("一次 flush 一批；没东西不写；flush 之后的字节进下一批", async () => {
    const { traffic, batches } = meter();
    await traffic.flush();
    expect(batches).toHaveLength(0);
    const counter = traffic.counter(KEY);
    for (let index = 0; index < 1000; index += 1)
      counter.bytes("to_source", 10);
    expect(batches).toHaveLength(0);
    expect(traffic.pending).toBe(1);
    await traffic.flush();
    counter.bytes("to_source", 5);
    await traffic.flush();
    await traffic.flush();
    expect(batches.map((batch) => batch[0]?.bytesToSource)).toEqual([
      10_000, 5,
    ]);
    expect(traffic.pending).toBe(0);
  });

  it("新的一行才通知（运行时据此安排延后的 flush），同一行再加不通知", async () => {
    let rows = 0;
    const traffic = new TrafficMeter({
      node: "n",
      now: () => T0,
      write: async () => {},
      onRow: () => (rows += 1),
    });
    const counter = traffic.counter(KEY);
    counter.connection();
    counter.bytes("to_source", 1);
    traffic.counter({ ...KEY, kind: "ws" }).connection();
    expect(rows).toBe(2);
    await traffic.flush();
    counter.bytes("to_client", 1);
    expect(rows).toBe(3);
  });

  it("写失败：并回内存，下次连同新的一起写；积压超限丢掉这一批并报告", async () => {
    let failing = true;
    const { traffic, batches, errors } = meter({
      fail: () => failing,
      maxPendingRows: 2,
    });
    const counter = traffic.counter(KEY);
    counter.bytes("to_source", 1);
    await traffic.flush();
    expect(errors).toEqual([{ dropped: 0 }]);
    counter.bytes("to_source", 2);
    failing = false;
    await traffic.flush();
    expect(batches).toEqual([[expect.objectContaining({ bytesToSource: 3 })]]);
    failing = true;
    for (const subject of ["a", "b", "c"])
      traffic.counter({ ...KEY, subject }).connection();
    await traffic.flush();
    expect(errors.at(-1)).toEqual({ dropped: 3 });
    expect(traffic.pending).toBe(0);
  });
});

describe("并发", () => {
  it("上千条流交错计数、慢存储上多次并发 flush：写进去的总数与加上的一字不差", async () => {
    const { traffic, clock, batches } = meter({ delayMs: 1 });
    const keys = Array.from({ length: 20 }, (_, index) => ({
      ...KEY,
      subject: `acct:${index % 5}`,
      kind: index % 2 === 0 ? ("http" as const) : ("ws" as const),
      deviceId: `dev-${index % 4}`,
    }));
    const counters = Array.from({ length: 2000 }, (_, index) =>
      traffic.counter(keys[index % keys.length] as TrafficKey),
    );
    let expectedUp = 0;
    let expectedDown = 0;
    let expectedConnections = 0;
    const flushes: Promise<void>[] = [];
    let seed = 42;
    const random = () => {
      seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
      return seed / 2 ** 31;
    };
    for (let round = 0; round < 50; round += 1) {
      for (const counter of counters) {
        const up = Math.floor(random() * 1000);
        const down = Math.floor(random() * 5000);
        counter.bytes("to_source", up);
        counter.bytes("to_client", down);
        expectedUp += up;
        expectedDown += down;
        if (random() < 0.1) {
          counter.connection();
          expectedConnections += 1;
        }
      }
      if (round % 7 === 0) flushes.push(traffic.flush());
      if (round === 25) clock.now += TRAFFIC_BUCKET_MS;
      await Promise.resolve();
    }
    flushes.push(traffic.flush());
    await Promise.all(flushes);
    await traffic.flush();
    const rows = [...totals(batches).values()];
    expect(rows.reduce((sum, row) => sum + row.bytesToSource, 0)).toBe(
      expectedUp,
    );
    expect(rows.reduce((sum, row) => sum + row.bytesToClient, 0)).toBe(
      expectedDown,
    );
    expect(rows.reduce((sum, row) => sum + row.connections, 0)).toBe(
      expectedConnections,
    );
    // 20 个键 × 两个小时桶。
    expect(rows).toHaveLength(40);
  });
});

describe("隧道与边缘", () => {
  it("隧道按流记两个方向的 DATA 载荷，与 relay_bytes_total 同源；流结束后不再记", async () => {
    const { traffic, batches } = meter();
    const metrics: Record<string, number> = { to_source: 0, to_client: 0 };
    const { tunnel, feed } = fakeTunnel(
      {},
      {
        onBytes: (direction, bytes) => {
          metrics[direction] = (metrics[direction] ?? 0) + bytes;
        },
      },
    );
    const counted = tunnel.openStream(OPEN, traffic.counter(KEY));
    const plain = tunnel.openStream(OPEN);
    const writer = counted.writable.getWriter();
    await writer.write(new Uint8Array(1200));
    await plain.writable.getWriter().write(new Uint8Array(50));
    feed({
      type: "data",
      streamId: counted.streamId,
      bytes: new Uint8Array(3000),
    });
    feed({ type: "data", streamId: plain.streamId, bytes: new Uint8Array(9) });
    feed({ type: "rst", streamId: counted.streamId, code: 1 });
    feed({
      type: "data",
      streamId: counted.streamId,
      bytes: new Uint8Array(77),
    });
    await traffic.flush();
    expect(batches[0]).toEqual([
      expect.objectContaining({ bytesToSource: 1200, bytesToClient: 3000 }),
    ]);
    expect(metrics).toEqual({ to_source: 1250, to_client: 3086 });
  });

  it("边缘开通道：按令牌的 sub / dev 记一次 e2e 连接；没开统计就不记", async () => {
    const control = testControlPlane();
    const { traffic, batches } = meter();
    const { tunnel } = fakeTunnel();
    tunnel.capabilities = new Set(["e2e"]);
    const metrics = new RelayMetrics(() => ({ tunnels: 0, streams: 0 }));
    const edge = new EdgePolicy({
      nodeId: "relay-1",
      addressing: pathAddressing(),
      auth: createAuthContext(control),
      control,
      lookup: async () => tunnel,
      metrics,
      traffic,
    });
    const request: EdgeRequest = {
      method: "GET",
      url: `/s/${SOURCE_ID}/_relay/e2e`,
      headers: {
        host: "relay",
        upgrade: "websocket",
        "sec-websocket-protocol": `armadra-e2e.1, armadra-relay.${relayToken({ sub: "acct:alice", dev: "dev_1" })}`,
      },
      clientIp: "203.0.113.7",
    };
    const decision = await edge.decideUpgrade(request);
    if (decision.kind !== "accept") throw new Error(decision.kind);
    const identity = {
      subject: decision.subject,
      deviceId: decision.device ?? "",
    };
    expect(identity).toEqual({ subject: "acct:alice", deviceId: "dev_1" });
    expect(edge.openE2e(tunnel, request, decision.remoteIp, identity).ok).toBe(
      true,
    );
    edge.openE2e(tunnel, request, decision.remoteIp);
    await traffic.flush();
    expect(
      batches[0]?.map((row) => [
        row.subject,
        row.deviceId,
        row.kind,
        row.connections,
      ]),
    ).toEqual([["acct:alice", "dev_1", "e2e", 1]]);
  });
});

describe("开销", () => {
  it("热路径每次计数是常数级的加法（百万次在百毫秒量级内）", () => {
    const { traffic } = meter();
    const counter = traffic.counter(KEY);
    const rounds = 1_000_000;
    // 预热。
    for (let index = 0; index < 10_000; index += 1)
      counter.bytes("to_source", 1);
    const started = performance.now();
    for (let index = 0; index < rounds; index += 1)
      counter.bytes(index % 2 === 0 ? "to_source" : "to_client", 16_384);
    const perCallNs = ((performance.now() - started) * 1e6) / rounds;
    expect(traffic.pending).toBe(1);
    // 本机实测每次约几十纳秒；这里只防退化成按帧拼键、查表或写库（那会是微秒级）。
    expect(perCallNs).toBeLessThan(1_000);
  });
});
