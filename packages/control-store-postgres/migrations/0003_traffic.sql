-- 中继流量统计（cloud-api §19）：与 SQLite 的 `packages/control-store-sql/migrations/0002_traffic.sql`
-- 同一张表、同一套列（计数用 bigint）。按 租户 × UTC 整小时桶 × 节点 × 源 × 中继令牌 `sub` × `dev`
-- × 类别聚合；中继在内存里累加、每 60 秒批量 upsert，不按帧写库。只记元数据：没有路径、头、IP 与正文。
-- 保留期由中继按天清理。

CREATE TABLE relay_traffic_buckets (
  tenant_id text NOT NULL,
  -- UTC 整小时的起点（毫秒）。
  bucket_start_ms bigint NOT NULL,
  node text NOT NULL,
  source_id text NOT NULL,
  -- 中继令牌的 `sub`：`acct:<账号名>` / 账号 id / `guest:<linkId>:<随机>`。
  subject text NOT NULL,
  -- 中继令牌的 `dev`；令牌没带时为空串。
  device_id text NOT NULL DEFAULT '',
  -- `e2e` | `http` | `ws` | `media`。
  kind text NOT NULL,
  connections bigint NOT NULL DEFAULT 0,
  bytes_to_source bigint NOT NULL DEFAULT 0,
  bytes_to_client bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
);

CREATE INDEX relay_traffic_source ON relay_traffic_buckets (tenant_id, source_id, bucket_start_ms);
