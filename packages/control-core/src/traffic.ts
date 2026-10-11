/**
 * 中继流量统计（cloud-api §19）：存储端口 {@link TrafficStore} 的内存实现（`--store file` 与测试参照）
 * 与 `me.traffic` 的查询服务。
 *
 * 一行 = 一个 UTC 整小时桶 × 节点 × 源 × 中继令牌 `sub` × `dev` × 类别的累计。中继内核在内存里累加，
 * 按批 `upsert`（同键相加）；这里不做时间换算之外的任何事：不记路径、头、IP 与正文。
 */
import { TRAFFIC_MAX_ROWS } from "@armadra/platform-protocol/cloud-api";

import { ControlError } from "./errors.ts";
import type {
  ControlContext,
  TrafficQuery,
  TrafficRow,
  TrafficStore,
} from "./ports.ts";
import type { Caller } from "./sessions.ts";

export const HOUR_MS = 3_600_000;
export const DAY_MS = 86_400_000;

/** 桶起点：按粒度向下取整（UTC）。 */
export function bucketStart(atMs: number, sizeMs: number = HOUR_MS): number {
  return Math.floor(atMs / sizeMs) * sizeMs;
}

/** 一行的维度键（不含计数）；NUL 分隔，维度里不会出现 NUL。 */
export function trafficKey(row: Omit<TrafficRow, Counter>): string {
  return [
    row.bucketStartMs,
    row.node,
    row.sourceId,
    row.subject,
    row.deviceId,
    row.kind,
  ].join("\u0000");
}

type Counter = "connections" | "bytesToSource" | "bytesToClient";

/** 维度排序：桶、节点、源、`sub`、`dev`、类别（按 UTF-16 码元，与 SQL 的二进制排序一致）。 */
export function compareTraffic(left: TrafficRow, right: TrafficRow): number {
  if (left.bucketStartMs !== right.bucketStartMs)
    return left.bucketStartMs - right.bucketStartMs;
  for (const field of [
    "node",
    "sourceId",
    "subject",
    "deviceId",
    "kind",
  ] as const) {
    if (left[field] < right[field]) return -1;
    if (left[field] > right[field]) return 1;
  }
  return 0;
}

/** 把行按 `granularity` 合并、排序（内存实现与 Workers 的跨对象合并共用）。 */
export function rollUp(
  rows: Iterable<TrafficRow>,
  granularity: TrafficQuery["granularity"],
): TrafficRow[] {
  const size = granularity === "day" ? DAY_MS : HOUR_MS;
  const merged = new Map<string, TrafficRow>();
  for (const row of rows) {
    const bucketStartMs = bucketStart(row.bucketStartMs, size);
    const key = trafficKey({ ...row, bucketStartMs });
    const found = merged.get(key);
    if (found) {
      found.connections += row.connections;
      found.bytesToSource += row.bytesToSource;
      found.bytesToClient += row.bytesToClient;
    } else merged.set(key, { ...row, bucketStartMs });
  }
  return [...merged.values()].sort(compareTraffic);
}

/** 行是否落在查询范围里（桶起点 ∈ `[fromMs, toMs)`，再按源与 `sub` 过滤）。 */
export function matchesTraffic(row: TrafficRow, query: TrafficQuery): boolean {
  return (
    row.bucketStartMs >= query.fromMs &&
    row.bucketStartMs < query.toMs &&
    (query.sourceId === undefined || row.sourceId === query.sourceId) &&
    (query.subject === undefined || row.subject === query.subject)
  );
}

/** 内存里的 {@link TrafficStore}（多租户）：`--store file` 用它，重启归零；也是合同测试的参照。 */
export function createMemoryTrafficStore(): TrafficStore {
  const tenants = new Map<string, Map<string, TrafficRow>>();
  const table = (tenantId: string) => {
    let rows = tenants.get(tenantId);
    if (!rows) {
      rows = new Map();
      tenants.set(tenantId, rows);
    }
    return rows;
  };
  return {
    async upsert(tenantId, rows) {
      const stored = table(tenantId);
      for (const row of rows) {
        const key = trafficKey(row);
        const found = stored.get(key);
        if (found) {
          found.connections += row.connections;
          found.bytesToSource += row.bytesToSource;
          found.bytesToClient += row.bytesToClient;
        } else stored.set(key, { ...row });
      }
    },
    async query(tenantId, query) {
      const rows = [...table(tenantId).values()].filter((row) =>
        matchesTraffic(row, query),
      );
      return rollUp(rows, query.granularity).slice(0, query.limit);
    },
    async prune(tenantId, beforeMs) {
      const stored = table(tenantId);
      let removed = 0;
      for (const [key, row] of stored)
        if (row.bucketStartMs < beforeMs) {
          stored.delete(key);
          removed += 1;
        }
      return removed;
    },
  };
}

export interface TrafficSummaryInput {
  fromMs: number;
  toMs: number;
  sourceId?: string;
  granularity: TrafficQuery["granularity"];
}

/**
 * `me.traffic`：账号本人看本租户全部源（含已撤销的源留下的历史），访客只看 `subject` 是自己的行。
 * `fromMs ≥ toMs` 或超过 {@link TRAFFIC_MAX_ROWS} 行答 `bad_request`。
 */
export function createTraffic(context: ControlContext) {
  const { tenantId, ports } = context;
  return {
    async summary(
      caller: Caller,
      input: TrafficSummaryInput,
    ): Promise<{ buckets: TrafficRow[] }> {
      if (input.fromMs >= input.toMs) throw new ControlError("bad_request");
      const rows = await ports.traffic.query(tenantId, {
        fromMs: input.fromMs,
        toMs: input.toMs,
        granularity: input.granularity,
        limit: TRAFFIC_MAX_ROWS + 1,
        ...(input.sourceId === undefined ? {} : { sourceId: input.sourceId }),
        ...(caller.role === "owner" ? {} : { subject: caller.subject }),
      });
      if (rows.length > TRAFFIC_MAX_ROWS) throw new ControlError("bad_request");
      return { buckets: rows };
    },
  };
}
