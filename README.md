# armadra-cloud

Armadra 平台的服务端：控制面 `apps/cloud`、中继 `apps/relay`，以及两仓共用的协议包 `@armadra/platform-protocol`。MIT 许可。

中继有两种运行模式：

- **saas**：多租户，配合控制面、PostgreSQL 与 Redis。
- **personal**：单人中转，一个进程、不带数据库。

## 目录

| 路径                         | 内容                                                       |
| ---------------------------- | ---------------------------------------------------------- |
| `apps/cloud`                 | 控制面（`armadra-cloud`）                                  |
| `apps/relay`                 | 中继（`armadra-relay`）                                    |
| `packages/platform-protocol` | 协议包：隧道帧、断言、`/v1` 契约、错误码、身份参数、夹具   |
| `packages/cloud-shared`      | 内部共享：配置、日志、健康检查、HTTP、PostgreSQL 与 Redis  |
| `deploy/`                    | compose 与部署说明（[deploy/README.md](deploy/README.md)） |
| `docs/`                      | 契约与运维文档（[索引](docs/README.md)）                   |
| `tools/`                     | 仓库校验、契约生成、工作流校验、第三方声明                 |

## 准备

- Node ≥ 22.18（原生类型剥离，开发态直接跑 `.ts`）
- pnpm 11.18.0（`corepack enable`）
- Docker（只用于本地 PostgreSQL / Redis 与镜像构建）

```sh
pnpm install
```

## 个人中转

不需要 PostgreSQL / Redis：

```sh
pnpm relay:personal            # 从源码起个人中转：http://127.0.0.1:8102，数据目录 .data/personal
curl http://127.0.0.1:8102/health
```

容器：`docker build -f apps/relay/Dockerfile -t ghcr.io/owlbay/armadra-relay:local .` 之后按 [deploy/personal/compose.yml](deploy/personal/compose.yml) 的注释起。现阶段有中继内核（隧道终端 `/t/v1`、`/s/<sourceId>/` 边缘、`/health`、可选 `--metrics-listen` 上的 `/metrics`）与 `--tls plain`；账号、签名钥、源注册与 TLS 在后续包落地，在那之前没有可用的令牌，隧道与边缘一律拒绝。

## SaaS 本地开发

```sh
pnpm dev:up      # 首次生成 .env.dev；起 PostgreSQL 17(127.0.0.1:5440)、Redis 8(127.0.0.1:6390)、mailpit；等健康后执行迁移
pnpm dev         # 从源码起 cloud(http://127.0.0.1:8100) 与 relay saas(http://127.0.0.1:8101)，改源码自动重启；/v1 业务路由预留，答 501
curl http://127.0.0.1:8100/health
curl http://127.0.0.1:8101/health
curl http://127.0.0.1:8100/ready   # 依赖就绪：PostgreSQL 与 Redis
pnpm dev:down    # 停掉依赖；pnpm dev:down --volumes 连数据一起删
```

也可以把 cloud 与 relay 一起放进容器：`sh scripts/dev.sh up --stack`，端口是 `127.0.0.1:8110`（cloud）与 `127.0.0.1:8111`（relay）。

## 检查与测试

```sh
pnpm check             # 构建协议包 + prettier + eslint + typecheck + repo:check + contract:check + 工作流校验 + 第三方声明
pnpm repo:test         # tools/ 的自检
pnpm test              # 全部单测
pnpm test:integration  # 集成测试，需要 pnpm dev:up；见下
pnpm images:build      # 本地构建两个镜像（不推送）
pnpm db:migrate        # 执行数据库迁移（读 .env.dev）
pnpm protocol:pack     # 协议包打成 tgz（.data/pack/），核对内容，不发布
```

## 协议包

`packages/platform-protocol`（[说明](packages/platform-protocol/README.md)）是与 Armadra 共用的 schema 与编解码。改形状的顺序：

1. 改 `src/` 里的 schema 或常量；
2. `pnpm --filter @armadra/platform-protocol fixtures` 重新生成黄金夹具，改了身份常量再 `node scripts/gen-vectors.mjs`；
3. `pnpm contract:generate` 更新 `docs/contracts/cloud-api.md` 的形状表与 `cloud-openapi.json`；
4. `pnpm check`（含 `protocol:check` 与 `contract:check`）。

发布前 Armadra 用 `pnpm link ../armadra-cloud/packages/platform-protocol` 联调（不提交）。

集成测试读 `ARMADRA_CLOUD_TEST_DATABASE_URL` 与 `REDIS_URL`（没有时退回 `.env.dev` 里的 `ARMADRA_CLOUD_DATABASE_URL` 与 `ARMADRA_CLOUD_REDIS_URL`）：

```sh
set -a && . ./.env.dev && set +a && pnpm test:integration
```

## 约定

开发约定见 [AGENTS.md](AGENTS.md)；契约见 [docs/contracts/cloud-api.md](docs/contracts/cloud-api.md)。
