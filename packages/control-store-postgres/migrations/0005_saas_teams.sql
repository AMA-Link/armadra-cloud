-- 0005_saas_teams.sql：团队是业务租户（能力设计 §1）。分组的表 M1 建好、M2 起使用。
CREATE TABLE teams (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  team_id text NOT NULL,
  slug text NOT NULL CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$'),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
  avatar_url text,
  -- 企业组织层预留（能力设计 §1.4），M1 恒为 NULL。
  organization_id text,
  -- teamSettings 的全部字段（缺省见 §4.4），按协议包形状存。
  settings jsonb NOT NULL,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
  status_reason text,
  created_by text NOT NULL,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  deleted_at_ms bigint,
  purge_at_ms bigint,
  PRIMARY KEY (tenant_id, team_id),
  CHECK ((deleted_at_ms IS NULL) = (purge_at_ms IS NULL))
);

-- slug 在恢复期内仍占用，清除后释放。
CREATE UNIQUE INDEX teams_slug ON teams (tenant_id, slug);
CREATE INDEX teams_purge ON teams (purge_at_ms) WHERE purge_at_ms IS NOT NULL;

CREATE TABLE team_members (
  tenant_id text NOT NULL,
  team_id text NOT NULL,
  account_id text NOT NULL,
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'billing', 'guest')),
  joined_at_ms bigint NOT NULL,
  invited_by text,
  PRIMARY KEY (tenant_id, team_id, account_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX team_members_account ON team_members (tenant_id, account_id);

-- 每个团队至少一个 owner：事务提交时检查（转移所有权可以在一个事务里先升后降）。
-- 读已提交下两个事务各降一个 owner 会写偏斜：触发器先锁团队行，再用新快照查，后提交的那个必然看到
-- 前一个的结果而失败。服务层另外在改成员前先 SELECT … FOR UPDATE 团队行，并先查后答 409
-- team_last_owner；这里是兜底，违反时 SQLSTATE 23514、约束名 team_has_owner。
-- UPDATE 时新旧两个团队都查（team_id 本不该被改，改了也守得住）。
CREATE FUNCTION team_owner_check(t_tenant text, t_team text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM teams WHERE tenant_id = t_tenant AND team_id = t_team FOR UPDATE;
  IF FOUND AND NOT EXISTS (SELECT 1 FROM team_members
                           WHERE tenant_id = t_tenant AND team_id = t_team AND role = 'owner') THEN
    RAISE EXCEPTION 'team has no owner' USING ERRCODE = '23514', CONSTRAINT = 'team_has_owner';
  END IF;
END
$$;

CREATE FUNCTION team_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') AND TG_TABLE_NAME = 'team_members' THEN
    PERFORM team_owner_check(OLD.tenant_id, OLD.team_id);
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') THEN
    PERFORM team_owner_check(NEW.tenant_id, NEW.team_id);
  END IF;
  RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER team_members_owner AFTER UPDATE OR DELETE ON team_members
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION team_owner_guard();
CREATE CONSTRAINT TRIGGER teams_owner AFTER INSERT ON teams
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION team_owner_guard();

CREATE TABLE groups (
  tenant_id text NOT NULL,
  group_id text NOT NULL,
  team_id text NOT NULL,
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
  created_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, group_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX groups_name ON groups (tenant_id, team_id, name);

-- 组员必须是团队成员（guest 不行，由服务层判）；成员离队即出组。
CREATE TABLE group_members (
  tenant_id text NOT NULL,
  group_id text NOT NULL,
  team_id text NOT NULL,
  account_id text NOT NULL,
  is_lead boolean NOT NULL DEFAULT false,
  added_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, group_id, account_id),
  FOREIGN KEY (tenant_id, group_id) REFERENCES groups (tenant_id, group_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, team_id, account_id)
    REFERENCES team_members (tenant_id, team_id, account_id) ON DELETE CASCADE
);
