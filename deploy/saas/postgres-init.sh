#!/bin/sh
# postgres 首次初始化时执行（docker-entrypoint-initdb.d），用超级用户建两个非超级用户角色和库：
#   armadra_cloud          库属主；控制面的 migrate / serve 与 db-roles 以它连
#   armadra_relay_traffic  节点的流量角色（权限由 db-roles 在迁移之后授，这里只建角色）
# 之后服务不再用超级用户。改口令：以超级用户 `ALTER ROLE … PASSWORD …`，同时改 secrets/ 里对应的文件。
set -eu
cloud_pw="$(cat /run/secrets/cloud_db_password)"
traffic_pw="$(cat /run/secrets/traffic_db_password)"
psql -v ON_ERROR_STOP=1 -U postgres -v cloud_pw="$cloud_pw" -v traffic_pw="$traffic_pw" <<'SQL'
SELECT format('CREATE ROLE armadra_cloud LOGIN PASSWORD %L', :'cloud_pw') \gexec
SELECT format('CREATE ROLE armadra_relay_traffic LOGIN PASSWORD %L', :'traffic_pw') \gexec
CREATE DATABASE armadra_cloud OWNER armadra_cloud;
SQL
