#!/bin/sh
# 为 Linux 容器（OrbStack / Docker）交叉编译压测用的二进制到 bench/bin/（不入库）。
# 用法: build.sh [arm64|amd64]   缺省取 docker 服务端的架构
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ARCH=${1:-$(docker version --format '{{.Server.Arch}}')}
case $ARCH in aarch64) ARCH=arm64 ;; x86_64) ARCH=amd64 ;; esac
mkdir -p "$HERE/bin"
cd "$HERE/.."
for target in cmd/armadra-relay:armadra-relay bench/loadgen:loadgen bench/calib:calib bench/mint:mint; do
  pkg=${target%%:*} out=${target##*:}
  CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -o "$HERE/bin/$out-linux" "./$pkg"
done
echo "已编译 linux/$ARCH → $HERE/bin"
