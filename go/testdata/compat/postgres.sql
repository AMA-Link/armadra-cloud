-- 由 go/testdata/compat/make-fixtures.mjs 生成：TypeScript 的 PostgreSQL 迁移器建出来的 schema 的转储。
-- 还原进一个空 schema 即可（建表是迁移文件原文，账本行是 TypeScript 迁移器写的）。

CREATE TABLE cloud_migrations (id integer PRIMARY KEY, name text NOT NULL, sha384 text NOT NULL, applied_at_ms bigint NOT NULL);

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

-- 多节点中继的控制面状态（`--control postgres`，`relay-runtime-node` 的 `control/postgres.ts`）：
-- 路由、撤销的令牌与按窗口计数的限流。变化经 `NOTIFY relay_presence | relay_revoked | relay_ctl`
-- 广播给各节点。路由与限流桶丢了无妨（隧道重连、窗口重计），用 UNLOGGED 表；撤销要落盘。
-- 不存任何令牌明文：撤销表只有 `jti` 与到期时间。

CREATE UNLOGGED TABLE relay_routes (
  source_id text PRIMARY KEY,
  node text NOT NULL,
  tunnel_id text NOT NULL,
  since_ms bigint NOT NULL,
  core_version text NOT NULL,
  expires_at_ms bigint NOT NULL
);

CREATE TABLE relay_revoked_tokens (
  jti text PRIMARY KEY,
  exp_ms bigint NOT NULL
);

CREATE INDEX relay_revoked_tokens_exp ON relay_revoked_tokens (exp_ms);

CREATE UNLOGGED TABLE relay_limits (
  id text PRIMARY KEY,
  count integer NOT NULL,
  reset_at_ms bigint NOT NULL
);

CREATE INDEX relay_limits_reset ON relay_limits (reset_at_ms);

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

INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms) VALUES (1, '0001_control_init', '3f5fc09f43ab598b290e7315958d23db3e1e90ed1c7f0bf0e421dea02cb9f97cf2ad8b2c44057e48b71a14985eb99df4', 1760000000000);
INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms) VALUES (2, '0002_relay_control', '30ddc79e9170ab5d4f1e0b048573a2bed17dcf81c0bf5414ec86e81391647c42179e0d765b0ae179224c1344a1dd2ae3', 1760000000000);
INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms) VALUES (3, '0003_traffic', 'bba45afbf738f38e91f4797e12245997af6916f0d65e02ef0885a8eee1e039a5dc56985f8ee9ca003f712bf3bd18c2b1', 1760000000000);

INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, rate_limits_json, trusted_proxy_json, display_name, revision) VALUES ('default', 'https://relay.armadra.test', true, '["https://app.armadra.test"]', '{"token":600}', '{"proxies":["10.0.0.0/8"],"prefer":"forwarded-for"}', 'Fixture relay', 7);
INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms, last_failure_at_ms, created_at_ms, updated_at_ms) VALUES ('default', 'acct:owner', 'owner', '{"kdf":"scrypt","cost":32768,"block":8,"parallel":1,"length":32,"salt":"BwcHBwcHBwcHBwcHBwcHBw==","hash":"pnK6K1WmsZUGrhy11+YELD2Y8Dg1oAZlqs51re7fwkc="}', 2, 0, 1759996400000, 1759913600000, 1759913600000);
INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json, active_from_ms, retired_at_ms) VALUES ('default', 'k1', 'MC4CAQAwBQYDK2VwBCIEIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB', 'none', '{"kty":"OKP","crv":"Ed25519","x":"iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w","kid":"k1","alg":"EdDSA","use":"sig"}', 1759992800000, 1760001800000);
INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json, active_from_ms, retired_at_ms) VALUES ('default', 'k2', 'MC4CAQAwBQYDK2VwBCIEIAICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgIC', 'none', '{"kty":"OKP","crv":"Ed25519","x":"gTl3Dqh9F19Wo1Rmw0x-zMuNipG07jeiXfYPW4_Js5Q","kid":"k2","alg":"EdDSA","use":"sig"}', 1759996400000, NULL);
INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version, capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json) VALUES ('default', '0123456789abcdef0123456789abcdef', 'workstation', 'desktop', '{"kty":"OKP","crv":"Ed25519","x":"Zr5-Myx6RTMyvZ0Kf32wVfXF7xoGraZtmLOftoEMRzo"}', '0.9.2', '["terminal","e2e"]', 1759920000000, 1759999995000, NULL, '["https://app.armadra.test"]');
INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version, capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json) VALUES ('default', 'fedcba9876543210fedcba9876543210', 'old laptop', 'server', '{"kty":"OKP","crv":"Ed25519","x":"C1E62bSSQBXKCQLtB5BE06xdvsIwbwaUjBDajrbjny0"}', '0.8.0', '[]', 1759910000000, NULL, 1759999000000, '[]');
INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms) VALUES ('default', 'eb9e2ad9206bc929eb858ff813a1298d828987041b6685a623e179412feb1bd0', 'acct:owner', 1760000600000);
INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms) VALUES ('default', 'd1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1', 'acct:owner', 'desktop', 'laptop', '0b61ee9340f32fc84c2183b6a8c84e6c4dfa5b3422418c17c445f4ae484a05d1', NULL, 3, 1759914000000, 1762000000000, NULL, 1759999940000);
INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms) VALUES ('default', 'd2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2', 'acct:owner', 'browser', 'old browser', '22007b4f5b4452e0cb7c2d55a6fbaa8798b9ccf4bdf3b0e910b9dff3e6da0aae', '2ef798f52417af3ac7f6ce1f2371ea923cf41266d90af32e2548d5bd46aea1f3', 2, 1759910000000, 1762000000000, 1759998000000, NULL);
INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms) VALUES ('default', 'd3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3', 'guest:1111111111111111:abcd1234', 'browser', 'guest', 'a392f41eda7f275bdef479746920429113f82d8d54ad49a96e8622df7ea309f4', NULL, 1, 1759999000000, 1762000000000, NULL, NULL);
INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash, max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json) VALUES ('default', '1111111111111111', '0123456789abcdef0123456789abcdef', 'inv-fixture', 'friend', 'viewer', '9b17a4baad2a3d1cf365967565c9e0fc7c88fc6bdd2a2bf942a57708cfe6ab0a', 3, 1, 1759999000000, 1762000000000, NULL, '{"workspaceId":"ws_01","readOnly":true}');
INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash, max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json) VALUES ('default', '2222222222222222', '0123456789abcdef0123456789abcdef', 'inv-revoked', '', 'editor', 'ee3cc8c11635efbeb6756a4098189c9cd4b34624bb1c6e4362ddf7359979fbe6', NULL, 0, 1759998000000, 1762000000000, 1759999500000, NULL);
INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind, connections, bytes_to_source, bytes_to_client) VALUES ('default', 1759993200000, 'relay-1', '0123456789abcdef0123456789abcdef', 'acct:owner', 'd1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1', 'ws', 3, 18432, 2621440);
INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind, connections, bytes_to_source, bytes_to_client) VALUES ('default', 1759996800000, 'relay-1', '0123456789abcdef0123456789abcdef', 'guest:1111111111111111:abcd1234', '', 'http', 1, 100, 5000000000);
