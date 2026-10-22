-- 0004_saas_accounts.sql：SaaS 账号（存储租户 cloud）。个人中转的行这些列取缺省、不受影响。
ALTER TABLE accounts
  ADD COLUMN email text CHECK (email = lower(email)),
  ADD COLUMN email_verified_at_ms bigint,
  ADD COLUMN display_name text NOT NULL DEFAULT '',
  ADD COLUMN locale text NOT NULL DEFAULT 'zh-CN' CHECK (locale IN ('zh-CN', 'en')),
  ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  ADD COLUMN status_reason text,
  -- 特殊范围（如运营方的 operator）；me.get.scopes。
  ADD COLUMN scopes text[] NOT NULL DEFAULT '{}';

CREATE UNIQUE INDEX accounts_email ON accounts (tenant_id, email) WHERE email IS NOT NULL;

-- 会话的传输（bearer：原生、core、CLI；cookie：控制台）与范围（session：普通；operator：运营，
-- 1 小时绝对寿命）。两者正交：Bearer 的运营会话照样能调 admin.*（契约 §28）。
ALTER TABLE devices
  ADD COLUMN transport text NOT NULL DEFAULT 'bearer' CHECK (transport IN ('bearer', 'cookie')),
  ADD COLUMN scope text NOT NULL DEFAULT 'session' CHECK (scope IN ('session', 'operator')),
  ADD COLUMN elevated_until_ms bigint;

CREATE TABLE account_credentials (
  tenant_id text NOT NULL,
  credential_id text NOT NULL,
  account_id text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('totp', 'passkey', 'oauth')),
  label text NOT NULL DEFAULT '',
  -- 秘密（TOTP 种子等）用主密钥包裹，形如 aes256gcm-v1:<base64>；明文从不落库。
  secret_wrap text,
  public_json jsonb,
  created_at_ms bigint NOT NULL,
  -- TOTP：最后一次用过的时间步，同一步不重用。
  last_step bigint,
  last_used_at_ms bigint,
  PRIMARY KEY (tenant_id, credential_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX account_credentials_account ON account_credentials (tenant_id, account_id, kind);
-- 一个账号至多一个 TOTP。
CREATE UNIQUE INDEX account_credentials_totp ON account_credentials (tenant_id, account_id) WHERE kind = 'totp';

-- 邮件里的一次性令牌：只存哈希；verify 24 小时，reset 30 分钟，change 24 小时。
CREATE TABLE email_tokens (
  tenant_id text NOT NULL,
  token_hash text NOT NULL,
  account_id text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('verify', 'reset', 'change')),
  -- 发往的地址；change 是新地址，确认时据此改邮箱。
  email text NOT NULL CHECK (email = lower(email)),
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  consumed_at_ms bigint,
  PRIMARY KEY (tenant_id, token_hash),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX email_tokens_account ON email_tokens (tenant_id, account_id, purpose);

-- 设备码（能力设计 §11.5）：pending → approved → consumed，或 denied / expired。
CREATE TABLE device_codes (
  tenant_id text NOT NULL,
  device_code_hash text NOT NULL,
  user_code text NOT NULL CHECK (user_code ~ '^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$'),
  status text NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'consumed', 'expired')),
  platform text NOT NULL,
  name text NOT NULL,
  push_public_key text,
  account_id text,
  ip_prefix text,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  last_poll_at_ms bigint,
  decided_at_ms bigint,
  PRIMARY KEY (tenant_id, device_code_hash),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX device_codes_user_code ON device_codes (tenant_id, user_code)
  WHERE status IN ('pending', 'approved');
CREATE INDEX device_codes_expires ON device_codes (expires_at_ms);
