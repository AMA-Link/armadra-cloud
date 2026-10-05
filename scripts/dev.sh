#!/bin/sh
# 本地开发依赖的起停（pnpm dev:up / pnpm dev:down）。
#
#   sh scripts/dev.sh up [--stack]     起 postgres + redis + mailpit（--stack 另起 cloud 与 relay 容器），
#                                      等健康检查通过，然后执行数据库迁移
#   sh scripts/dev.sh down [--volumes] 停掉；--volumes 连数据卷一起删（只删本项目的卷）
#
# 首次 up 生成 .env.dev（随机口令与 master key，不提交）。只用 Docker，不改系统配置。
set -eu

cd "$(dirname "$0")/.."
COMPOSE_FILE=deploy/dev/compose.yml
ENV_FILE=.env.dev

ensure_env() {
  if [ -f "$ENV_FILE" ]; then
    return
  fi
  node -e '
const { randomBytes } = require("node:crypto");
const { writeFileSync } = require("node:fs");
const pg = randomBytes(18).toString("hex");
const lines = [
  "# 由 scripts/dev.sh 生成的本地开发配置；只用于本机，不要提交。",
  `DEV_PG_PASSWORD=${pg}`,
  "ARMADRA_CLOUD_ISSUER=http://127.0.0.1:8100",
  "ARMADRA_CLOUD_APP_ORIGIN=http://127.0.0.1:8100",
  "ARMADRA_CLOUD_LISTEN=127.0.0.1:8100",
  `ARMADRA_CLOUD_DATABASE_URL=postgres://armadra:${pg}@127.0.0.1:5440/armadra_cloud`,
  "ARMADRA_CLOUD_REDIS_URL=redis://127.0.0.1:6390",
  `ARMADRA_CLOUD_MASTER_KEY=${randomBytes(32).toString("hex")}`,
  "ARMADRA_CLOUD_RELAY_DOMAIN=src.localhost",
  "ARMADRA_CLOUD_SMTP_URL=smtp://127.0.0.1:1026",
  "RELAY_LISTEN=127.0.0.1:8101",
  "RELAY_NODE_ID=dev-1",
  "RELAY_DOMAIN=src.localhost",
  "RELAY_CLOUD_ISSUER=http://127.0.0.1:8100",
  "RELAY_REDIS_URL=redis://127.0.0.1:6390",
  "RELAY_ALLOW_INSECURE_TUNNEL=1",
];
writeFileSync(".env.dev", lines.join("\n") + "\n", { mode: 0o600 });
'
  echo "已生成 $ENV_FILE"
}

compose() {
  docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" "$@"
}

command=${1:-}
[ $# -gt 0 ] && shift

case "$command" in
  up)
    ensure_env
    profiles=""
    for arg in "$@"; do
      case "$arg" in
        --stack) profiles="--profile stack" ;;
        *) echo "未知参数：$arg" >&2; exit 64 ;;
      esac
    done
    # shellcheck disable=SC2086
    compose $profiles up -d --wait ${profiles:+--build}
    node --env-file="$ENV_FILE" apps/cloud/src/cli.ts migrate
    echo "PostgreSQL 127.0.0.1:5440、Redis 127.0.0.1:6390、mailpit http://127.0.0.1:8026 已就绪"
    ;;
  down)
    if [ ! -f "$ENV_FILE" ]; then
      echo "没有 $ENV_FILE，没有要停的东西"
      exit 0
    fi
    volumes=""
    for arg in "$@"; do
      case "$arg" in
        --volumes) volumes="--volumes" ;;
        *) echo "未知参数：$arg" >&2; exit 64 ;;
      esac
    done
    # shellcheck disable=SC2086
    compose --profile stack --profile acme down $volumes
    ;;
  *)
    echo "用法：sh scripts/dev.sh up [--stack] | down [--volumes]" >&2
    exit 64
    ;;
esac
