# armadra-relay 中继镜像：Go 实现（go/cmd/armadra-relay），saas 与 personal 同一镜像。构建上下文是仓库根：
#   docker build -f go/Dockerfile -t armadra-relay:local .
#
# 运行镜像是 distroless static（没有 shell、包管理器与 tini；Go 二进制自己当 PID 1，不起子进程，
# 无需回收僵尸进程，SIGTERM 由它自己的优雅关闭处理）。其余约定不变：入口 armadra-relay、缺省命令
# saas serve、uid / gid 10002、/data 卷、端口 8443、RELAY_WEB_ROOT / RELAY_DATA_DIR / RELAY_LISTEN、
# HEALTHCHECK 用 health 子命令。容器里执行子命令：docker compose exec relay armadra-relay personal <子命令> …
# 排障没有 shell：用 docker run --rm -it --network container:<名> --pid container:<名> busybox，或
# docker debug；评估记录见 docs/design/go-relay-design.md §9 Q6。
#
# personal 模式托管的页面（/app、/j）是 Armadra apps/web 的构建产物，放在 /app/web：
#   发布构建传 --build-arg WEB_IMAGE=ghcr.io/ama-link/armadra-server:<deploy/compat.json 的 web>；
#   或运行时挂载一份构建产物并设 RELAY_WEB_ROOT（--web-root）。
# 缺省是空目录：/app 答「页面不可用」错误页，其余功能不受影响。
#
# 版本号缺省取根 package.json（/health 与 version 子命令报它），--build-arg VERSION=<v> 可覆盖。
ARG GO_IMAGE=golang:1.27.1-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12
ARG WEB_IMAGE=web-empty

FROM ${GO_IMAGE} AS prep
RUN mkdir -p /rootfs/data /rootfs/app/web && chown 10002:10002 /rootfs/data

FROM scratch AS web-empty
COPY --from=prep /rootfs/app/web /app/web

FROM ${WEB_IMAGE} AS web

FROM ${GO_IMAGE} AS build
ARG VERSION=
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY package.json /src/package.json
COPY go/ ./
RUN v="${VERSION:-$(sed -n 's/^  "version": "\([^"]*\)",$/\1/p' /src/package.json)}" \
 && test -n "$v" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${v}" -o /out/armadra-relay ./cmd/armadra-relay

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /out/armadra-relay /usr/local/bin/armadra-relay
COPY --from=prep --chown=10002:10002 /rootfs/data /data
COPY --from=web /app/web /app/web
ENV RELAY_WEB_ROOT=/app/web RELAY_DATA_DIR=/data RELAY_LISTEN=0.0.0.0:8443 PATH=/usr/local/bin
USER 10002:10002
VOLUME ["/data"]
EXPOSE 8443
# 先 HTTPS（personal 缺省自签）再明文（saas 在反向代理之后）。
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["armadra-relay","health"]
ENTRYPOINT ["armadra-relay"]
CMD ["saas","serve"]
