#!/bin/sh
# Linux 容器内（OrbStack 2 vCPU）：中继容器钉在 CPU 0，压测容器钉在 CPU 1，共用中继的网络与 PID 命名空间
# （回环直连，不经宿主端口转发；压测端直接读中继的 /proc/1）。
# 用法: run.sh <源数> <每源通道数> <标签> [loadgen 额外参数…]
# 环境变量: RELAY_CPUS、LOAD_CPUS（钉核）、BENCH_IMAGE、RELAY_DOCKER_ARGS（给中继容器加参数，如 "-e GOGC=50"）
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
SOURCES=$1 PER=$2 LABEL=$3
shift 3
mkdir -p "$HERE/results" "$HERE/logs" "$HERE/work"
[ -x "$HERE/bin/armadra-relay-linux" ] || "$HERE/build.sh"
PORT=18443
NAME=rb-$LABEL
IMAGE=${BENCH_IMAGE:-node:24.21.0-bookworm-slim}
RELAY_CPUS=${RELAY_CPUS:-0}
LOAD_CPUS=${LOAD_CPUS:-1}
CREDS=work/creds-$SOURCES.json JWKS=work/jwks-$SOURCES.json
[ -f "$HERE/$CREDS" ] || "$HERE/bin/mint-linux" "$SOURCES" "$HERE/$CREDS" "$HERE/$JWKS" 2>/dev/null \
  || docker run --rm -v "$ROOT:/w" -w /w/go/bench "$IMAGE" ./bin/mint-linux "$SOURCES" "$CREDS" "$JWKS"
ISSUER=https://cloud.armadra.test
docker run --rm -d --name "$NAME" --cpuset-cpus "$RELAY_CPUS" --ulimit nofile=1048576:1048576 ${RELAY_DOCKER_ARGS:-} \
  -v "$ROOT:/w:ro" -w /w/go/bench "$IMAGE" \
  ./bin/armadra-relay-linux dev --listen "127.0.0.1:$PORT" --jwks "$JWKS" --issuer "$ISSUER" --debug >/dev/null
trap 'docker rm -f $NAME >/dev/null 2>&1 || true' EXIT
i=0
until docker logs "$NAME" 2>&1 | grep -q '"ready":true'; do
  i=$((i + 1)); [ $i -gt 150 ] && { echo "中继没起来" >&2; docker logs "$NAME"; exit 1; }
  sleep 0.2
done
docker run --rm --name "$NAME-load" --cpuset-cpus "$LOAD_CPUS" --ulimit nofile=1048576:1048576 \
  --network "container:$NAME" --pid "container:$NAME" \
  -v "$ROOT:/w" -w /w/go/bench "$IMAGE" \
  ./bin/loadgen-linux -url "ws://127.0.0.1:$PORT" -creds "$CREDS" \
  -sources "$SOURCES" -per "$PER" -docker proc:1 -label "$LABEL" \
  -out "results/$LABEL.json" -stats "http://127.0.0.1:$PORT/debug/stats" "$@" 2>&1 | tee "$HERE/logs/$LABEL.loadgen.log"
docker logs "$NAME" 2>&1 | grep -v '^{"time"' >"$HERE/logs/$LABEL.relay.log" || true
