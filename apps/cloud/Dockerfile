# armadra-cloud 控制面镜像。构建上下文是仓库根：
#   docker build -f apps/cloud/Dockerfile -t armadra-cloud:local .
#
# 页面产物（/app/web）来自 Armadra 服务器镜像：发布构建传
#   --build-arg WEB_IMAGE=ghcr.io/owlbay/armadra-server:<deploy/compat.json 的 web>
# 缺省用空目录，本地与 CI 构建不依赖外部镜像。
ARG NODE_IMAGE=node:24.21.0-bookworm-slim
ARG WEB_IMAGE=web-empty

FROM ${NODE_IMAGE} AS web-empty
RUN mkdir -p /app/web

FROM ${WEB_IMAGE} AS web

FROM ${NODE_IMAGE} AS build
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable
WORKDIR /src
COPY . .
RUN pnpm install --frozen-lockfile && pnpm libs:build && pnpm --filter @armadra/cloud build
RUN mkdir -p /app && cp -r apps/cloud/out /app/out && cp -r packages/control-store-postgres/migrations /app/migrations

FROM ${NODE_IMAGE} AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tini && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 10002 armadra && useradd --system --uid 10002 --gid armadra --home-dir /data armadra && mkdir -p /data && chown armadra:armadra /data
COPY --from=build /app /app
COPY --from=web /app/web /app/web
ENV NODE_ENV=production ARMADRA_CLOUD_WEB_ROOT=/app/web ARMADRA_CLOUD_MIGRATIONS_DIR=/app/migrations
USER armadra
VOLUME ["/data"]
EXPOSE 8100
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["node","-e","fetch('http://127.0.0.1:8100/health').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
ENTRYPOINT ["/usr/bin/tini","--","node","/app/out/main.js"]
CMD ["serve"]
