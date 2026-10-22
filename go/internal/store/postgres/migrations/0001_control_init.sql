-- 控制面端口的 PostgreSQL 存储：cloud 与 Node 中继 `--store postgres` 共用。
--
-- 列与 control-core `ports.ts` 的记录一一对应（camelCase ↔ snake_case），与 SQLite 的
-- `packages/control-store-sql/migrations/0001_init.sql` 同一套列；`*_json` 存 JSON 文本（字节原样
-- 往返）；时间都是毫秒 bigint，可空的列 NULL 即「没有」。`seq` 是创建顺序（SQLite 的 rowid）。
-- 账本 `cloud_migrations` 由迁移器维护，本文件只建业务表；不写事务语句（迁移器包在一个事务里）。

CREATE TABLE tenants (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text PRIMARY KEY,
  issuer text NOT NULL,
  allow_guests boolean NOT NULL,
  trusted_origins_json text NOT NULL,
  -- 边缘限流额度（次 / 分钟）；NULL = 没有配置。
  rate_limits_json text,
  -- 受信代理（Node 中继）；NULL = 不信任任何代理。
  trusted_proxy_json text,
  -- 服务端名称（`platform.info.name`）；NULL = 没有配置。
  display_name text,
  -- 每次写入加一：外部看「有没有人改过」。
  revision bigint NOT NULL DEFAULT 0
);

CREATE TABLE accounts (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  account_id text NOT NULL,
  name text NOT NULL,
  password_json text NOT NULL,
  failures integer NOT NULL,
  locked_until_ms bigint NOT NULL,
  last_failure_at_ms bigint NOT NULL,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, account_id)
);

CREATE UNIQUE INDEX accounts_name ON accounts (tenant_id, name);

CREATE TABLE signing_keys (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  kid text NOT NULL,
  -- PKCS#8 DER 的 base64；`private_key_wrap` 说明有没有再包一层（Node 中继 `none`）。
  private_key_pkcs8 text NOT NULL,
  private_key_wrap text NOT NULL DEFAULT 'none',
  public_jwk_json text NOT NULL,
  active_from_ms bigint NOT NULL,
  retired_at_ms bigint,
  PRIMARY KEY (tenant_id, kid)
);

CREATE TABLE sources (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  source_id text NOT NULL,
  name text NOT NULL,
  kind text NOT NULL,
  public_jwk_json text NOT NULL,
  core_version text NOT NULL,
  capabilities_json text NOT NULL,
  registered_at_ms bigint NOT NULL,
  last_seen_at_ms bigint,
  revoked_at_ms bigint,
  trusted_origins_json text NOT NULL,
  PRIMARY KEY (tenant_id, source_id)
);

CREATE TABLE registration_tokens (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  token_hash text NOT NULL,
  account_id text NOT NULL,
  expires_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, token_hash)
);

CREATE TABLE devices (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  device_id text NOT NULL,
  subject text NOT NULL,
  platform text NOT NULL,
  name text NOT NULL,
  refresh_hash text NOT NULL,
  previous_refresh_hash text,
  rotation integer NOT NULL,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  revoked_at_ms bigint,
  last_seen_at_ms bigint,
  PRIMARY KEY (tenant_id, device_id)
);

CREATE INDEX devices_subject ON devices (tenant_id, subject);

CREATE TABLE links (
  seq bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  link_id text NOT NULL,
  source_id text NOT NULL,
  invitation_id text NOT NULL,
  label text NOT NULL,
  role text NOT NULL,
  secret_hash text NOT NULL,
  max_uses integer,
  uses integer NOT NULL,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  revoked_at_ms bigint,
  -- 分享范围；NULL = 整台。
  scope_json text,
  PRIMARY KEY (tenant_id, link_id)
);

CREATE INDEX links_source ON links (tenant_id, source_id);
