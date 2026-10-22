-- 0006_saas_hosts.sql：主机的空间与配额。引用 sources 的外键延迟到提交时检查（Put 是同事务删了再插，设计 §3.2）。
CREATE TABLE source_spaces (
  tenant_id text NOT NULL,
  source_id text NOT NULL,
  -- NULL = 个人空间（归 owner_account_id）。
  team_id text,
  -- 登记它的账号；离队后改成团队 owner（能力设计 §1.2）。
  owner_account_id text NOT NULL,
  tags text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 16),
  -- hostSettings；NULL = 缺省（§4.5）。
  settings jsonb,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, source_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id),
  FOREIGN KEY (tenant_id, owner_account_id) REFERENCES accounts (tenant_id, account_id)
);

CREATE INDEX source_spaces_team ON source_spaces (tenant_id, team_id) WHERE team_id IS NOT NULL;
CREATE INDEX source_spaces_owner ON source_spaces (tenant_id, owner_account_id);
CREATE INDEX source_spaces_tags ON source_spaces USING gin (tags);

-- 注册令牌带空间：NULL = 个人空间（个人中转的令牌照旧）。
ALTER TABLE registration_tokens ADD COLUMN team_id text;

-- 空间的计划（M1 只有 Free；没有行 = free / active）。额度表在代码里（§4.10），overrides 给运营方单独放宽。
CREATE TABLE entitlements (
  tenant_id text NOT NULL,
  space_kind text NOT NULL CHECK (space_kind IN ('personal', 'team')),
  -- 个人空间是 account_id，团队空间是 team_id。
  space_id text NOT NULL,
  plan text NOT NULL DEFAULT 'free',
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'trialing', 'past_due', 'canceled')),
  overrides jsonb NOT NULL DEFAULT '{}',
  period_start_ms bigint,
  updated_at_ms bigint NOT NULL,
  updated_by text,
  PRIMARY KEY (tenant_id, space_kind, space_id)
);
