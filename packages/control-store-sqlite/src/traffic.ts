/**
 * 流量统计端口的 SQLite 实现（表 `relay_traffic_buckets`，迁移 0002）：Node 中继 `--store sqlite`
 * 与 Workers 的 DO SQLite（`SourceDO` 写、`ControlDO` 合并）共用。可移植：只用 {@link SqlDatabase}。
 *
 * 写入不碰租户修订号（统计不是「状态被改」，`serve` 的外部改动检测也不该被它触发）。
 */
import type { TrafficKind, TrafficStore } from "@armadra/control-core/ports";

import type { Row, SqlDatabase } from "./db.ts";

const UPSERT = `INSERT INTO relay_traffic_buckets
  (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind,
   connections, bytes_to_source, bytes_to_client)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  ON CONFLICT (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
  DO UPDATE SET
    connections = connections + excluded.connections,
    bytes_to_source = bytes_to_source + excluded.bytes_to_source,
    bytes_to_client = bytes_to_client + excluded.bytes_to_client`;

const DAY_MS = 86_400_000;

function toRow(row: Row) {
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

export function createSqliteTraffic(db: SqlDatabase): TrafficStore {
  return {
    async upsert(tenantId, rows) {
      if (rows.length === 0) return;
      db.transaction(() => {
        for (const row of rows)
          db.run(
            UPSERT,
            tenantId,
            row.bucketStartMs,
            row.node,
            row.sourceId,
            row.subject,
            row.deviceId,
            row.kind,
            row.connections,
            row.bytesToSource,
            row.bytesToClient,
          );
      });
    },
    async query(tenantId, query) {
      const bucket =
        query.granularity === "day"
          ? `(bucket_start_ms / ${DAY_MS}) * ${DAY_MS}`
          : "bucket_start_ms";
      const where = [
        "tenant_id = ?",
        "bucket_start_ms >= ?",
        "bucket_start_ms < ?",
      ];
      const params: (string | number)[] = [tenantId, query.fromMs, query.toMs];
      if (query.sourceId !== undefined) {
        where.push("source_id = ?");
        params.push(query.sourceId);
      }
      if (query.subject !== undefined) {
        where.push("subject = ?");
        params.push(query.subject);
      }
      params.push(query.limit);
      return db
        .all(
          `SELECT ${bucket} AS bucket_start_ms, node, source_id, subject, device_id, kind,
             sum(connections) AS connections, sum(bytes_to_source) AS bytes_to_source,
             sum(bytes_to_client) AS bytes_to_client
           FROM relay_traffic_buckets
           WHERE ${where.join(" AND ")}
           GROUP BY 1, node, source_id, subject, device_id, kind
           ORDER BY 1, node, source_id, subject, device_id, kind
           LIMIT ?`,
          ...params,
        )
        .map(toRow);
    },
    async prune(tenantId, beforeMs) {
      return db.transaction(() =>
        db.run(
          "DELETE FROM relay_traffic_buckets WHERE tenant_id = ? AND bucket_start_ms < ?",
          tenantId,
          beforeMs,
        ),
      );
    },
  };
}
