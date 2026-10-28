#!/bin/sh
# 生成 secrets/ 下的随机口令与连接串文件（已有的不覆盖），并建 tls/（证书放这里）。用法：sh init-secrets.sh
# 目录 0700、文件 0444：容器里的 uid 10002（控制面与节点）、70（postgres）、999（redis）要能读，
# 宿主上的其他用户进不了目录。节点令牌（node_token_*）要等控制面起来后用 nodes token 签，这里先放占位文件，
# 内容留空（0644，方便 nodes token 的输出直接覆盖），签完再覆盖。主密钥 master_key 必须另存一份。托管数据库 / Redis 时，直接把
# cloud_database_url、relay_database_url、redis_url 写成托管服务的连接串即可。
set -eu
cd "$(dirname "$0")"
umask 077
mkdir -p secrets tls
chmod 700 secrets
# 两个目录都只放秘密（tls/ 放证书与私钥 fullchain.pem、privkey.pem），各写一个忽略全部的 .gitignore，免得在检出里误提交。
for dir in secrets tls; do
  if [ ! -e "$dir/.gitignore" ]; then
    printf '*\n' > "$dir/.gitignore"
    chmod 644 "$dir/.gitignore"
  fi
done
rand() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }
put() { # put <名字> <内容> [模式]
  if [ -e "secrets/$1" ]; then echo "已有，跳过：secrets/$1"; return; fi
  printf '%s' "$2" > "secrets/$1"
  chmod "${3:-444}" "secrets/$1"
  echo "已生成：secrets/$1"
}
cloud_pw="$(rand)"; traffic_pw="$(rand)"; redis_pw="$(rand)"
put master_key "$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
put smtp_password ""
put postgres_password "$(rand)"
put cloud_db_password "$cloud_pw"
put traffic_db_password "$traffic_pw"
put redis_password "$redis_pw"
put cloud_database_url "postgres://armadra_cloud:${cloud_pw}@postgres:5432/armadra_cloud?sslmode=require"
put relay_database_url "postgres://armadra_relay_traffic:${traffic_pw}@postgres:5432/armadra_cloud?sslmode=require"
put redis_url "redis://:${redis_pw}@redis:6379"
put node_token_a "" 644
put node_token_b "" 644
