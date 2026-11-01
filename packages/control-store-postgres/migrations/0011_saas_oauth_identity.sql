-- 0011_saas_oauth_identity.sql：第三方登录身份（kind = 'oauth'）。external_id 是 "<提供方>:<主体>"（GitHub 的数字 id、
-- Google 的 sub），TOTP 行为 NULL；public_json 只存 { provider, email, login / name }，不存任何令牌。
ALTER TABLE account_credentials ADD COLUMN external_id text;

-- 一个第三方身份只属于一个账号。
CREATE UNIQUE INDEX account_credentials_external ON account_credentials (tenant_id, kind, external_id) WHERE external_id IS NOT NULL;
-- 一个账号在同一提供方至多一个身份（解绑按提供方，不会有歧义）。
CREATE UNIQUE INDEX account_credentials_oauth_provider ON account_credentials (tenant_id, account_id, (split_part(external_id, ':', 1))) WHERE kind = 'oauth';
-- 第三方身份必须带 external_id，且形如 <提供方>:<主体>。
ALTER TABLE account_credentials ADD CONSTRAINT account_credentials_oauth_external
  CHECK (kind <> 'oauth' OR (external_id IS NOT NULL AND external_id ~ '^[a-z][a-z0-9-]*:.+$'));
