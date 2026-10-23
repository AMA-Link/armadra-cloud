#!/bin/sh
# 给节点的流量角色 armadra_relay_traffic 授权（迁移之后才有这两张表）；可重复执行，恢复备份后也要重跑一次。
# 角色不存在时（没走 compose 的初始化，例如端到端用的一次性库）用 RELAY_TRAFFIC_PASSWORD 建出来；
# 已经存在就不动它的口令。环境：PG* 连接变量（口令用 PGPASSWORD 或 PGPASSWORD_FILE），
# 角色要新建时再给 RELAY_TRAFFIC_PASSWORD（或 RELAY_TRAFFIC_PASSWORD_FILE），连接的用户要有 CREATEROLE。
set -eu
if [ -n "${PGPASSWORD_FILE:-}" ]; then PGPASSWORD="$(cat "$PGPASSWORD_FILE")"; export PGPASSWORD; fi
if [ -n "${RELAY_TRAFFIC_PASSWORD_FILE:-}" ]; then RELAY_TRAFFIC_PASSWORD="$(cat "$RELAY_TRAFFIC_PASSWORD_FILE")"; fi
psql -v ON_ERROR_STOP=1 -v pw="${RELAY_TRAFFIC_PASSWORD:-}" <<'SQL'
SELECT format('CREATE ROLE armadra_relay_traffic LOGIN PASSWORD %L', :'pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'armadra_relay_traffic') \gexec
GRANT CONNECT ON DATABASE armadra_cloud TO armadra_relay_traffic;
GRANT USAGE ON SCHEMA public TO armadra_relay_traffic;
GRANT SELECT, INSERT, UPDATE ON relay_traffic_buckets TO armadra_relay_traffic;
GRANT SELECT ON cloud_migrations TO armadra_relay_traffic;
SQL
echo "armadra_relay_traffic 已就绪"
