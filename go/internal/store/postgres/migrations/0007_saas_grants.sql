-- 0007_saas_grants.sql：授予、邀请（团队与主机两类）、新设备待信任。有效权限按查询合成，不落库。
CREATE TABLE source_grants (
  tenant_id text NOT NULL,
  grant_id text NOT NULL,
  source_id text NOT NULL,
  -- 主机属于团队时是该团队；个人空间的主机为 NULL。
  team_id text,
  subject_kind text NOT NULL CHECK (subject_kind IN ('team', 'member', 'group', 'guest')),
  -- team → team_id；member / guest → account_id；group → group_id。
  subject_id text NOT NULL,
  role text NOT NULL CHECK (role IN ('viewer', 'editor', 'operator', 'driver')),
  -- 空 = 整台主机。
  workspace_ids text[] NOT NULL DEFAULT '{}' CHECK (cardinality(workspace_ids) <= 64),
  expires_at_ms bigint,
  granted_by text NOT NULL,
  -- 来路：grant（grants.create）或 invitation（主机邀请被接受）。
  origin text NOT NULL DEFAULT 'grant' CHECK (origin IN ('grant', 'invitation')),
  invitation_id text,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  -- 撤销与到期都写这里（到期由 grant.expire 写，revoked_by = 'system'）。
  revoked_at_ms bigint,
  revoked_by text,
  PRIMARY KEY (tenant_id, grant_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  -- team 主体就是主机所属的团队；team / member 主体只出现在团队主机上。
  CHECK (subject_kind <> 'team' OR subject_id = team_id),
  CHECK (subject_kind NOT IN ('team', 'member') OR team_id IS NOT NULL)
);

-- 同一主机对同一主体只有一条有效授予（重复建答 409 conflict）。
CREATE UNIQUE INDEX source_grants_active ON source_grants (tenant_id, source_id, subject_kind, subject_id)
  WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_subject ON source_grants (tenant_id, subject_kind, subject_id)
  WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_team ON source_grants (tenant_id, team_id) WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_expiry ON source_grants (expires_at_ms)
  WHERE revoked_at_ms IS NULL AND expires_at_ms IS NOT NULL;

CREATE TABLE invitations (
  tenant_id text NOT NULL,
  invitation_id text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('team', 'host')),
  email text NOT NULL CHECK (email = lower(email)),
  -- team：必有；host：主机属于团队时有。
  team_id text,
  team_role text CHECK (team_role IN ('admin', 'member', 'billing', 'guest')),
  source_id text,
  host_role text CHECK (host_role IN ('viewer', 'editor', 'operator', 'driver')),
  workspace_ids text[] NOT NULL DEFAULT '{}' CHECK (cardinality(workspace_ids) <= 64),
  -- 邮件作业发信时生成令牌、只存哈希；NULL = 还没发出。resend 换新哈希。
  token_hash text,
  state text NOT NULL CHECK (state IN ('pending', 'accepted', 'declined', 'revoked', 'expired')),
  invited_by text NOT NULL,
  decided_by text,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  -- 主机邀请被接受后授予的到期；NULL = 不过期。与邀请自身的有效期（expires_at_ms）无关。
  grant_expires_at_ms bigint,
  decided_at_ms bigint,
  sent_at_ms bigint,
  resend_count integer NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, invitation_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  CHECK (kind <> 'team' OR (team_id IS NOT NULL AND team_role IS NOT NULL AND source_id IS NULL
                            AND host_role IS NULL AND cardinality(workspace_ids) = 0)),
  CHECK (kind <> 'host' OR (source_id IS NOT NULL AND host_role IS NOT NULL AND team_role IS NULL))
);

CREATE UNIQUE INDEX invitations_pending_team ON invitations (tenant_id, team_id, email)
  WHERE kind = 'team' AND state = 'pending';
CREATE UNIQUE INDEX invitations_pending_host ON invitations (tenant_id, source_id, email)
  WHERE kind = 'host' AND state = 'pending';
CREATE INDEX invitations_received ON invitations (tenant_id, email) WHERE state = 'pending';
CREATE INDEX invitations_team ON invitations (tenant_id, team_id, created_at_ms DESC);
CREATE UNIQUE INDEX invitations_token ON invitations (tenant_id, token_hash) WHERE token_hash IS NOT NULL;

-- 新设备待信任（能力设计 §11.4）：主机上报（sources.trustRequest），云端只存通知所需的元数据，没有校验码。
CREATE TABLE trust_requests (
  tenant_id text NOT NULL,
  request_id text NOT NULL,
  source_id text NOT NULL,
  -- 主机侧的待信任 id，作幂等键：同一主机同一 pending_id 重报答原请求。
  pending_id text NOT NULL,
  team_id text,
  account_id text NOT NULL,
  -- 断言里的 device.deviceId（云端设备）；设备名与平台由云端从 devices 表填，不信主机报的。
  device_id text NOT NULL,
  device_name text NOT NULL,
  platform text NOT NULL,
  state text NOT NULL CHECK (state IN ('pending', 'trusted', 'rejected', 'expired')),
  requested_at_ms bigint NOT NULL,
  -- 不超过请求时刻 + 24 小时。
  expires_at_ms bigint NOT NULL CHECK (expires_at_ms <= requested_at_ms + 86400000),
  resolved_at_ms bigint,
  resolved_by text,
  PRIMARY KEY (tenant_id, request_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX trust_requests_idem ON trust_requests (tenant_id, source_id, pending_id);
CREATE UNIQUE INDEX trust_requests_pending ON trust_requests (tenant_id, source_id, device_id)
  WHERE state = 'pending';
-- 拒绝后的冷却期按 (source, device) 查最近一次 rejected。
CREATE INDEX trust_requests_device ON trust_requests (tenant_id, source_id, device_id, resolved_at_ms DESC);
CREATE INDEX trust_requests_team ON trust_requests (tenant_id, team_id, state);
