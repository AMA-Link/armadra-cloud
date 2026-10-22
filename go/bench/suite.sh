#!/bin/sh
# 三档（t1 250 隧道 / 1000 通道，t2 1500 / 6000，t3 3000 / 12000）× N 轮；最后在 t1 跑同隧道队头阻塞
# （探测与大流量共用隧道）。性能门槛见设计 §3.3：每连接内存、15000 连接小消息 p99、单核双向转发。
# 用法: suite.sh [轮数]
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROUNDS=${1:-3}
mkdir -p "$HERE/logs"
LOG=$HERE/logs/suite.log
[ -x "$HERE/bin/armadra-relay-linux" ] || "$HERE/build.sh"
for r in $(seq 1 "$ROUNDS"); do
  echo "=== 第 $r 轮 $(date '+%H:%M:%S')" | tee -a "$LOG"
  docker run --rm --cpuset-cpus 1 -v "$HERE:/b" "${BENCH_IMAGE:-node:24.21.0-bookworm-slim}" /b/bin/calib-linux | tee -a "$LOG"
  for tier in "250 4 t1" "1500 4 t2" "3000 4 t3"; do
    set -- $tier
    "$HERE/run.sh" "$1" "$2" "$3-r$r" >>"$LOG" 2>&1 || echo "失败：$3-r$r" | tee -a "$LOG"
    sleep 5
  done
done
for r in 1 2; do
  "$HERE/run.sh" 250 4 "hol-r$r" -probe-same -idle 3s -rtt 5s >>"$LOG" 2>&1 || echo "失败：hol-r$r" | tee -a "$LOG"
  sleep 5
done
echo "=== 结束 $(date '+%H:%M:%S')" | tee -a "$LOG"
