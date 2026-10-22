# armadra-relay 的 Go 镜像（个人中转；saas 随 G5）。构建上下文是仓库根：
#   docker build -f go/Dockerfile -t armadra-relay:go .
#
# 运行约定与 Node 镜像相同：uid 10002、/data 卷、端口 8443、页面产物在 /app/web
# （发布构建传 --build-arg WEB_IMAGE=ghcr.io/ama-link/armadra-server:<deploy/compat.json 的 web>）。
# 缺省是空目录：/app 答「页面不可用」错误页，其余功能不受影响。
ARG GO_IMAGE=golang:1.27.1-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
ARG WEB_IMAGE=web-empty

FROM ${RUNTIME_IMAGE} AS web-empty
RUN mkdir -p /app/web

FROM ${WEB_IMAGE} AS web

FROM ${GO_IMAGE} AS build
ARG VERSION=dev
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/armadra-relay ./cmd/armadra-relay

FROM ${RUNTIME_IMAGE} AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tini && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 10002 armadra && useradd --system --uid 10002 --gid armadra --home-dir /data armadra && mkdir -p /data && chown armadra:armadra /data
COPY --from=build /out/armadra-relay /usr/local/bin/armadra-relay
COPY --from=web /app/web /app/web
ENV RELAY_WEB_ROOT=/app/web RELAY_DATA_DIR=/data RELAY_LISTEN=0.0.0.0:8443
USER armadra
VOLUME ["/data"]
EXPOSE 8443
# 先 HTTPS（personal 缺省自签）再明文（在反向代理之后）。
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["armadra-relay","health"]
ENTRYPOINT ["/usr/bin/tini","--","armadra-relay"]
CMD ["help"]
