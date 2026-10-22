-- 中继流量统计（cloud-api §19）：按 租户 × UTC 整小时桶 × 节点 × 源 × 中继令牌 `sub` × `dev` × 类别
-- 聚合的连接数与两个方向的字节。中继在内存里累加，每 60 秒（Workers 在一阵流量之后）批量 upsert，
-- 不按帧写库。只记元数据：没有路径、头、IP 与正文。Workers 的 `SourceDO` 在自己的 DO SQLite 里写同一张表。
-- 保留期由中继按天清理（`bucket_start_ms` 早于保留期的行整行删掉）。

CREATE TABLE relay_traffic_buckets (
  tenant_id TEXT NOT NULL,
  -- UTC 整小时的起点（毫秒）。
  bucket_start_ms INTEGER NOT NULL,
  node TEXT NOT NULL,
  source_id TEXT NOT NULL,
  -- 中继令牌的 `sub`：`acct:<账号名>` / 账号 id / `guest:<linkId>:<随机>`。
  subject TEXT NOT NULL,
  -- 中继令牌的 `dev`；令牌没带时为空串。
  device_id TEXT NOT NULL DEFAULT '',
  -- `e2e` | `http` | `ws` | `media`。
  kind TEXT NOT NULL,
  connections INTEGER NOT NULL DEFAULT 0,
  bytes_to_source INTEGER NOT NULL DEFAULT 0,
  bytes_to_client INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
) STRICT;

CREATE INDEX relay_traffic_source ON relay_traffic_buckets (tenant_id, source_id, bucket_start_ms);
