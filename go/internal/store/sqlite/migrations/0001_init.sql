-- 控制面端口的 SQLite 存储：Node 中继 `--store sqlite` 与将来的 DO SQLite 共用这份 SQL。
--
-- 列与 control-core `ports.ts` 的记录一一对应（camelCase ↔ snake_case）；`*_json` 存数组 / 对象；
-- 时间都是毫秒整数，可空的列 NULL 即「没有」。行的先后（rowid）就是记录的先后（按创建顺序）。
-- 账本 `relay_migrations` 由迁移器维护，本文件只建业务表；不写 PRAGMA 与事务语句（DO 不接受）。

CREATE TABLE tenants (
  tenant_id TEXT PRIMARY KEY,
  issuer TEXT NOT NULL,
  allow_guests INTEGER NOT NULL CHECK (allow_guests IN (0, 1)),
  trusted_origins_json TEXT NOT NULL,
  -- 边缘限流额度（次 / 分钟）；NULL = 没有配置。
  rate_limits_json TEXT,
  -- 受信代理（Node 中继）；NULL = 不信任任何代理。
  trusted_proxy_json TEXT,
  -- 服务端名称（`platform.info.name`）；NULL = 没有配置。
  display_name TEXT,
  -- 每次写入加一：外部看「有没有人改过」。
  revision INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE accounts (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  account_id TEXT NOT NULL,
  name TEXT NOT NULL,
  password_json TEXT NOT NULL,
  failures INTEGER NOT NULL,
  locked_until_ms INTEGER NOT NULL,
  last_failure_at_ms INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, account_id)
) STRICT;

CREATE UNIQUE INDEX accounts_name ON accounts (tenant_id, name);

CREATE TABLE signing_keys (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  kid TEXT NOT NULL,
  -- PKCS#8 DER 的 base64；`private_key_wrap` 说明有没有再包一层（Node 中继 `none`，靠文件 0600）。
  private_key_pkcs8 TEXT NOT NULL,
  private_key_wrap TEXT NOT NULL DEFAULT 'none',
  public_jwk_json TEXT NOT NULL,
  active_from_ms INTEGER NOT NULL,
  retired_at_ms INTEGER,
  PRIMARY KEY (tenant_id, kid)
) STRICT;

CREATE TABLE sources (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  source_id TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  public_jwk_json TEXT NOT NULL,
  core_version TEXT NOT NULL,
  capabilities_json TEXT NOT NULL,
  registered_at_ms INTEGER NOT NULL,
  last_seen_at_ms INTEGER,
  revoked_at_ms INTEGER,
  trusted_origins_json TEXT NOT NULL,
  PRIMARY KEY (tenant_id, source_id)
) STRICT;

CREATE TABLE registration_tokens (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  token_hash TEXT NOT NULL,
  account_id TEXT NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, token_hash)
) STRICT;

CREATE TABLE devices (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  device_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  platform TEXT NOT NULL,
  name TEXT NOT NULL,
  refresh_hash TEXT NOT NULL,
  previous_refresh_hash TEXT,
  rotation INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  revoked_at_ms INTEGER,
  last_seen_at_ms INTEGER,
  PRIMARY KEY (tenant_id, device_id)
) STRICT;

CREATE INDEX devices_subject ON devices (tenant_id, subject);

CREATE TABLE links (
  tenant_id TEXT NOT NULL REFERENCES tenants (tenant_id),
  link_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  invitation_id TEXT NOT NULL,
  label TEXT NOT NULL,
  role TEXT NOT NULL,
  secret_hash TEXT NOT NULL,
  max_uses INTEGER,
  uses INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  revoked_at_ms INTEGER,
  -- 分享范围；NULL = 整台。
  scope_json TEXT,
  PRIMARY KEY (tenant_id, link_id)
) STRICT;

CREATE INDEX links_source ON links (tenant_id, source_id);
