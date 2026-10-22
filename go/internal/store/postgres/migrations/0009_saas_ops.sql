-- 0009_saas_ops.sql：作业队列（SKIP LOCKED）、邮件发件箱、平台设置、签发索引；流量桶加团队维度。
-- 撤销表沿用 0002 的 relay_revoked_tokens（只有 jti 与到期时间）。
CREATE TABLE jobs (
  job_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id text NOT NULL,
  kind text NOT NULL,
  -- 只有标识与参数，不含令牌、口令与内容。
  payload jsonb NOT NULL,
  -- 同一 kind 下未完成的作业按它去重（例如 team.purge 按团队）。
  dedupe_key text,
  -- 定时作业的时间槽（例如 audit.retention:2026-10-23）：不论完成与否都唯一，同一槽只跑一次。
  slot_key text,
  run_at_ms bigint NOT NULL,
  attempts integer NOT NULL DEFAULT 0,
  max_attempts integer NOT NULL DEFAULT 8,
  locked_by text,
  locked_until_ms bigint,
  -- 只记错误类名或错误码。
  last_error text,
  created_at_ms bigint NOT NULL,
  done_at_ms bigint,
  failed_at_ms bigint
);

CREATE INDEX jobs_due ON jobs (run_at_ms) WHERE done_at_ms IS NULL AND failed_at_ms IS NULL;
-- 已完成 / 已失败作业的清理（jobs.gc）按时间扫这两个索引。
CREATE INDEX jobs_done ON jobs (done_at_ms) WHERE done_at_ms IS NOT NULL;
CREATE INDEX jobs_failed ON jobs (failed_at_ms) WHERE failed_at_ms IS NOT NULL;
CREATE UNIQUE INDEX jobs_dedupe ON jobs (tenant_id, kind, dedupe_key)
  WHERE dedupe_key IS NOT NULL AND done_at_ms IS NULL AND failed_at_ms IS NULL;
CREATE UNIQUE INDEX jobs_slot ON jobs (tenant_id, kind, slot_key) WHERE slot_key IS NOT NULL;

-- 发件箱：业务事务里插一行 + 一个 mail.send 作业；发信成功记 sent_at_ms，重试前先看它（幂等）。
-- 含收件地址（个人信息）：发出或失败满 30 天由 outbox.gc 删除。
CREATE TABLE outbox_emails (
  tenant_id text NOT NULL,
  email_id text NOT NULL,
  job_id bigint REFERENCES jobs (job_id) ON DELETE SET NULL,
  template text NOT NULL,
  locale text NOT NULL CHECK (locale IN ('zh-CN', 'en')),
  to_email text NOT NULL CHECK (to_email = lower(to_email)),
  -- 渲染参数：标识与显示名；需要一次性令牌的模板在发信时现生成（§6.3），这里没有令牌。
  params jsonb NOT NULL,
  message_id text NOT NULL,
  created_at_ms bigint NOT NULL,
  sent_at_ms bigint,
  failed_at_ms bigint,
  PRIMARY KEY (tenant_id, email_id)
);

CREATE INDEX outbox_emails_gc ON outbox_emails (created_at_ms);
-- 删作业时 ON DELETE SET NULL 要按 job_id 找发件箱行。
CREATE INDEX outbox_emails_job ON outbox_emails (job_id) WHERE job_id IS NOT NULL;

-- 平台级开关（事故开关等），运营后台写；键见设计 §4.9。
CREATE TABLE platform_settings (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  key text NOT NULL,
  value jsonb NOT NULL,
  updated_at_ms bigint NOT NULL,
  updated_by text,
  PRIMARY KEY (tenant_id, key)
);

-- 已签令牌的索引（Redis cloud:issued:* 的落盘副本，§7.2）：只有标识，没有令牌本身。
CREATE TABLE cloud_issued_tokens (
  tenant_id text NOT NULL,
  jti text NOT NULL,
  account_id text NOT NULL,
  source_id text NOT NULL,
  device_id text NOT NULL,
  exp_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, jti)
);

CREATE INDEX cloud_issued_tokens_account ON cloud_issued_tokens (tenant_id, account_id, exp_ms);
CREATE INDEX cloud_issued_tokens_source ON cloud_issued_tokens (tenant_id, source_id, exp_ms);
CREATE INDEX cloud_issued_tokens_exp ON cloud_issued_tokens (tenant_id, exp_ms);

-- 流量按团队计（能力设计 §11.14）：入口节点写中继令牌的 team；个人空间为空串。
-- 不进主键：同一小时同一主机换团队只在转移（M2）时发生，按首次写入的团队计。
ALTER TABLE relay_traffic_buckets ADD COLUMN team_id text NOT NULL DEFAULT '';
CREATE INDEX relay_traffic_team ON relay_traffic_buckets (tenant_id, team_id, bucket_start_ms)
  WHERE team_id <> '';
