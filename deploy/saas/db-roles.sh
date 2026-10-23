#!/bin/sh
# 建（或更新口令）节点的流量角色并授权；可重复执行。环境：PG* 连接变量与 RELAY_TRAFFIC_PASSWORD。
set -eu
psql -v ON_ERROR_STOP=1 -v pw="$RELAY_TRAFFIC_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE armadra_relay_traffic LOGIN PASSWORD %L', :'pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'armadra_relay_traffic') \gexec
SELECT format('ALTER ROLE armadra_relay_traffic LOGIN PASSWORD %L', :'pw') \gexec
GRANT CONNECT ON DATABASE armadra_cloud TO armadra_relay_traffic;
GRANT USAGE ON SCHEMA public TO armadra_relay_traffic;
GRANT SELECT, INSERT, UPDATE ON relay_traffic_buckets TO armadra_relay_traffic;
GRANT SELECT ON cloud_migrations TO armadra_relay_traffic;
SQL
echo "armadra_relay_traffic 已就绪"
