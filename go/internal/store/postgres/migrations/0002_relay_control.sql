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
