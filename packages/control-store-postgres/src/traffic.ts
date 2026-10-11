/**
 * 流量统计端口的 PostgreSQL 实现（表 `relay_traffic_buckets`，迁移 0003）：cloud 与 Node 中继
 * `--store postgres` 共用。一批行先按键合并（同一条 `INSERT … ON CONFLICT` 不能两次命中同一行），
 * 再按维度排序（固定加锁顺序，多节点同时写不死锁）、按块多行插入；累加在库里做，同一桶不丢计数。
 *
 * 写入不锁租户行、不碰修订号（统计不是「状态被改」，个人中转的外部改动检测不该被它触发）。
 */
import type {
  TrafficKind,
  TrafficRow,
  TrafficStore,
} from "@armadra/control-core/ports";
import { compareTraffic, trafficKey } from "@armadra/control-core/traffic";

import { transaction, type Row, type Sql } from "./db.ts";

const DAY_MS = 86_400_000;
/** 每条 INSERT 最多这么多行（10 列，远低于 65 535 个参数的上限）。 */
const CHUNK = 1_000;

function merge(rows: readonly TrafficRow[]): TrafficRow[] {
  const merged = new Map<string, TrafficRow>();
  for (const row of rows) {
    const key = trafficKey(row);
    const found = merged.get(key);
    if (found) {
      found.connections += row.connections;
      found.bytesToSource += row.bytesToSource;
      found.bytesToClient += row.bytesToClient;
    } else merged.set(key, { ...row });
  }
  // 固定的加锁顺序：几个节点同时写重叠的桶时不会互相死锁。
  return [...merged.values()].sort(compareTraffic);
}

function toRow(row: Row): TrafficRow {
  return {
    bucketStartMs: Number(row.bucket_start_ms),
    node: String(row.node),
    sourceId: String(row.source_id),
    subject: String(row.subject),
    deviceId: String(row.device_id),
    kind: String(row.kind) as TrafficKind,
    connections: Number(row.connections),
    bytesToSource: Number(row.bytes_to_source),
    bytesToClient: Number(row.bytes_to_client),
  };
}

export function createPostgresTraffic(sql: Sql): TrafficStore {
  return {
    async upsert(tenantId, rows) {
      const merged = merge(rows);
      if (merged.length === 0) return;
      await transaction(sql, async (tx) => {
        for (let from = 0; from < merged.length; from += CHUNK) {
          const values = merged.slice(from, from + CHUNK).map((row) => ({
            tenant_id: tenantId,
            bucket_start_ms: row.bucketStartMs,
            node: row.node,
            source_id: row.sourceId,
            subject: row.subject,
            device_id: row.deviceId,
            kind: row.kind,
            connections: row.connections,
            bytes_to_source: row.bytesToSource,
            bytes_to_client: row.bytesToClient,
          }));
          await tx`
            INSERT INTO relay_traffic_buckets ${tx(values)}
            ON CONFLICT (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
            DO UPDATE SET
              connections = relay_traffic_buckets.connections + excluded.connections,
              bytes_to_source = relay_traffic_buckets.bytes_to_source + excluded.bytes_to_source,
              bytes_to_client = relay_traffic_buckets.bytes_to_client + excluded.bytes_to_client`;
        }
      });
    },
    async query(tenantId, query) {
      const size = query.granularity === "day" ? DAY_MS : 1;
      const rows = await sql`
        SELECT (bucket_start_ms / ${size}::bigint) * ${size}::bigint AS bucket_start_ms,
          node, source_id, subject, device_id, kind,
          sum(connections) AS connections, sum(bytes_to_source) AS bytes_to_source,
          sum(bytes_to_client) AS bytes_to_client
        FROM relay_traffic_buckets
        WHERE tenant_id = ${tenantId}
          AND bucket_start_ms >= ${query.fromMs} AND bucket_start_ms < ${query.toMs}
          ${query.sourceId === undefined ? sql`` : sql`AND source_id = ${query.sourceId}`}
          ${query.subject === undefined ? sql`` : sql`AND subject = ${query.subject}`}
        GROUP BY 1, node, source_id, subject, device_id, kind
        ORDER BY 1, node COLLATE "C", source_id COLLATE "C", subject COLLATE "C",
          device_id COLLATE "C", kind COLLATE "C"
        LIMIT ${query.limit}`;
      return rows.map(toRow);
    },
    async prune(tenantId, beforeMs) {
      const result = await sql`
        DELETE FROM relay_traffic_buckets
        WHERE tenant_id = ${tenantId} AND bucket_start_ms < ${beforeMs}`;
      return result.count;
    },
  };
}
