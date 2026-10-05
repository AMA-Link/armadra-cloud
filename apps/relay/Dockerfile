# armadra-relay 中继镜像（saas 与 personal 同一镜像）。构建上下文是仓库根：
#   docker build -f apps/relay/Dockerfile -t armadra-relay:local .
#
# personal 模式托管的页面产物来自 Armadra 服务器镜像，见 apps/cloud/Dockerfile 的说明。
ARG NODE_IMAGE=node:22.23.3-bookworm-slim
ARG WEB_IMAGE=web-empty

FROM ${NODE_IMAGE} AS web-empty
RUN mkdir -p /app/web

FROM ${WEB_IMAGE} AS web

FROM ${NODE_IMAGE} AS build
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable
WORKDIR /src
COPY . .
RUN pnpm install --frozen-lockfile && pnpm libs:build && pnpm --filter @armadra/relay build
RUN mkdir -p /app && cp -r apps/relay/out /app/out

FROM ${NODE_IMAGE} AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tini && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 10002 armadra && useradd --system --uid 10002 --gid armadra --home-dir /data armadra && mkdir -p /data && chown armadra:armadra /data
COPY --from=build /app /app
COPY --from=web /app/web /app/web
ENV NODE_ENV=production RELAY_WEB_ROOT=/app/web RELAY_DATA_DIR=/data RELAY_LISTEN=0.0.0.0:8443
USER armadra
VOLUME ["/data"]
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["node","-e","fetch('http://127.0.0.1:8443/health').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
ENTRYPOINT ["/usr/bin/tini","--","node","/app/out/cli.js"]
CMD ["saas","serve"]
