-- 0008_saas_audit.sql：团队审计与账号安全日志（一张表）。只记主体、动作、对象、时间、jti、IP 前缀与运营理由。
-- 按月分区：serve 启动时同步建当月与后两个月（失败拒绝启动），每天的作业再往后补；
-- 保留期按计划逐团队删，账号安全日志固定 90 天；超过 13 个月的整月分区直接 DROP。
CREATE TABLE audit_log (
  at_ms bigint NOT NULL,
  event_id text NOT NULL,
  tenant_id text NOT NULL,
  team_id text,
  -- 安全日志的本人（action 以 security. 开头）。
  account_id text,
  actor_kind text NOT NULL CHECK (actor_kind IN ('account', 'api_token', 'system', 'operator')),
  actor_id text,
  action text NOT NULL CHECK (char_length(action) BETWEEN 1 AND 64),
  target_kind text,
  target_id text,
  -- 涉及的主机（audit.list 的 sourceId 筛选用）。
  source_id text,
  jti text,
  ip_prefix text,
  -- 设备名快照（me.securityLog 的 deviceName；设备改名或删除后日志里仍是当时的名字）。
  device_name text CHECK (char_length(device_name) <= 128),
  -- 运营写操作的理由（契约 §28 必填）；其余为 NULL。
  reason text CHECK (char_length(reason) <= 500),
  PRIMARY KEY (at_ms, event_id)
) PARTITION BY RANGE (at_ms);

-- 兜底分区：正常情况下恒为空（见 audit_ensure_partition）。
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;

CREATE INDEX audit_log_team ON audit_log (tenant_id, team_id, at_ms DESC) WHERE team_id IS NOT NULL;
CREATE INDEX audit_log_account ON audit_log (tenant_id, account_id, at_ms DESC) WHERE account_id IS NOT NULL;
CREATE INDEX audit_log_actor ON audit_log (tenant_id, actor_kind, actor_id, at_ms DESC);
CREATE INDEX audit_log_target ON audit_log (tenant_id, target_kind, target_id, at_ms DESC);
CREATE INDEX audit_log_source ON audit_log (tenant_id, team_id, source_id, at_ms DESC) WHERE source_id IS NOT NULL;

-- 建一个月的分区（幂等）。分区名必须是 audit_log_YYYY_MM，边界必须是该 UTC 月的月初与下月月初，否则拒绝（22023）。
-- 兜底分区里若已有这个月的行：在同一事务里先把兜底分区摘下，建新分区，把那些行搬过去，再把兜底分区挂回。
-- 调用方持 pg_advisory_xact_lock，并先 SET LOCAL lock_timeout（摘 / 挂分区要父表的排他锁，见 §6）。
CREATE FUNCTION audit_ensure_partition(month_start_ms bigint, month_end_ms bigint, part_name text)
RETURNS void LANGUAGE plpgsql
SET search_path FROM CURRENT AS $$
DECLARE
  m timestamp := date_trunc('month', to_timestamp(month_start_ms / 1000.0) AT TIME ZONE 'UTC');
BEGIN
  IF month_start_ms <> (extract(epoch FROM m) * 1000)::bigint
     OR month_end_ms <> (extract(epoch FROM m + interval '1 month') * 1000)::bigint
     OR part_name <> 'audit_log_' || to_char(m, 'YYYY_MM') THEN
    RAISE EXCEPTION 'invalid audit partition' USING ERRCODE = '22023';
  END IF;
  IF to_regclass(part_name) IS NOT NULL THEN
    RETURN;
  END IF;
  IF EXISTS (SELECT 1 FROM audit_log_default WHERE at_ms >= month_start_ms AND at_ms < month_end_ms) THEN
    ALTER TABLE audit_log DETACH PARTITION audit_log_default;
    EXECUTE format('CREATE TABLE %I PARTITION OF audit_log FOR VALUES FROM (%s) TO (%s)',
                   part_name, month_start_ms, month_end_ms);
    EXECUTE format('INSERT INTO %I SELECT * FROM audit_log_default WHERE at_ms >= %s AND at_ms < %s',
                   part_name, month_start_ms, month_end_ms);
    DELETE FROM audit_log_default WHERE at_ms >= month_start_ms AND at_ms < month_end_ms;
    ALTER TABLE audit_log ATTACH PARTITION audit_log_default DEFAULT;
  ELSE
    EXECUTE format('CREATE TABLE %I PARTITION OF audit_log FOR VALUES FROM (%s) TO (%s)',
                   part_name, month_start_ms, month_end_ms);
  END IF;
END
$$;
