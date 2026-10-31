# armadra-cloud

Armadra 平台的服务端：控制面 `apps/cloud`、中继 `go/`（Go，`armadra-relay`）与 `apps/relay-workers`（Cloudflare Workers），以及两仓共用的协议包 `@armadra/platform-protocol`。MIT 许可。

中继有两种运行模式：

- **saas**：多租户，配合控制面、PostgreSQL 与 Redis。
- **personal**：单人中转，一个进程、不带外部数据库（状态在数据目录里的 SQLite）。

## 目录

| 路径                             | 内容                                                                                                          |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `apps/cloud`                     | 控制面（`armadra-cloud`）                                                                                     |
| `packages/platform-protocol`     | 协议包：隧道帧、断言、`/v1` 契约、错误码、身份参数、夹具                                                      |
| `packages/relay-kernel`          | 中继内核：隧道、边缘判定、限流、令牌校验（运行时无关）                                                        |
| `packages/e2e-fake-core`         | 端到端的假 core（Node `http` + `ws` 的隧道客户端），给 `pnpm e2e` 的 saas / workers 模式用                    |
| `packages/relay-runtime-workers` | 中继的 Workers 运行时：Worker 入口、`SourceDO`、`ControlDO`、DO SQLite                                        |
| `apps/relay-workers`             | Workers 中继的 wrangler 项目（`relay.armadra.com`）                                                           |
| `packages/control-store-sql`     | SQLite 迁移与账本判定（Go 中继与 DO SQLite 共用）                                                             |
| `packages/control-store-sqlite`  | 控制核端口的 SQLite 实现（`node:sqlite`，可移植部分 Workers 复用）                                            |
| `packages/control-core`          | 控制核：存储端口、账号 / 会话 / 源 / 链接服务、`/v1` 路由                                                     |
| `packages/cloud-shared`          | 内部共享：日志、JWS、中继页面、PostgreSQL 客户端                                                              |
| `deploy/`                        | compose 与部署说明（[deploy/README.md](deploy/README.md)）                                                    |
| `docs/`                          | 契约与运维文档（[索引](docs/README.md)）                                                                      |
| `tools/`                         | 仓库校验、契约生成、工作流校验、第三方声明                                                                    |
| `go/`                            | 中继（`armadra-relay`，#59 的 Go 重写）：协议、JWS 与身份算法、存储、控制核服务、`/v1` 路由、数据面、个人中转 |

## 准备

- Node ≥ 24（原生类型剥离，开发态直接跑 `.ts`）
- pnpm 12.8.1（`corepack enable`）
- Go 1.27.1（中继在 `go/`：`relay:personal`、`dev`、`go:check`、`notices` 与镜像之外的本地运行都要）
- Docker（只用于本地 PostgreSQL / Redis 与镜像构建）

```sh
pnpm install
```

## 个人中转

不需要 PostgreSQL / Redis：

```sh
pnpm relay:personal            # go build 到 go/bin/armadra-relay 再起个人中转：https://127.0.0.1:8102，自签 TLS，数据目录 .data/personal
                               # 首次运行建账号 dev，口令写在 .data/personal/dev.env；启动日志打印 CA 指纹
curl -k https://127.0.0.1:8102/health
go/bin/armadra-relay personal status --data-dir .data/personal
```

Armadra 的页面产物（`apps/web/dist`）在 `ARMADRA_WEB_DIST` 或 `../Armadra/apps/web/dist` 时托管在 `/app` 与 `/j/<linkId>`。

子命令：`personal init | serve | status | passwd | rotate-key | links list|revoke | traffic | migrate | backup | doctor`（`armadra-relay personal --help`）。一个账号（地址 + 账号 + 口令），状态只有 `<data-dir>/relay.db`（新装缺省，`--store sqlite`）（0.3.x 之前的 `<data-dir>/state.json` 先用 `personal migrate --to sqlite` 迁过去）：账号、口令哈希、签名钥、源 / 设备 / 链接登记，不存任何消息内容，见[运维手册](docs/runbooks/personal-storage.md)。TLS 缺省自签 CA，可选 ACME 或文件证书。接口见[契约](docs/contracts/cloud-api.md) §10–§11。

中继是 Go 实现（`go/cmd/armadra-relay`；0.10.0 之前的 Node 版已删除）。容器：`docker build -f go/Dockerfile -t ghcr.io/ama-link/armadra-relay:local .`，之后 `docker run --rm ghcr.io/ama-link/armadra-relay:local personal --help`；部署按 [deploy/personal/compose.yml](deploy/personal/compose.yml) 的注释起。

## Cloudflare Workers 上的个人中转

同一套协议跑在 Workers + Durable Objects 上（`relay.armadra.com`，一源一个 `SourceDO`、一租户一个 `ControlDO`，状态在 DO SQLite）。本地只用 workerd，不连 Cloudflare 账号：

```sh
pnpm --filter @armadra/relay-runtime-workers test   # vitest 的 Workers 插件：握手、转发、休眠后唤醒、判死、撤销、闸门
pnpm --filter @armadra/relay-workers dev            # wrangler dev --local
pnpm e2e --mode personal-workers                    # 本地 workerd 上的端到端
```

部署（维护者执行）见 [deploy/cloudflare/README.md](deploy/cloudflare/README.md)，运维见[运维手册](docs/runbooks/relay-on-cloudflare.md)。

## SaaS 本地开发

```sh
pnpm dev:up      # 首次生成 .env.dev；起 PostgreSQL 17(127.0.0.1:5440)、Redis 8(127.0.0.1:6390)、mailpit；等健康后执行迁移
pnpm dev         # 从源码起 cloud(http://127.0.0.1:8100) 与 relay saas(http://127.0.0.1:8101)，都是 Go 二进制（首次自动 go build 到 go/bin/，改 Go 代码要重启）；/.well-known/armadra-platform 报 platform.info
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
pnpm go:check          # Go：gofmt + vet + staticcheck + race 测试 + 生成物新鲜度；PostgreSQL、Redis、mailpit 的测试读 ARMADRA_CLOUD_TEST_DATABASE_URL、REDIS_URL、MAILPIT_SMTP_ADDR + MAILPIT_API_URL（没有就跳过）
pnpm test              # 全部单测
pnpm test:integration  # 集成测试，需要 pnpm dev:up；见下
pnpm images:build      # 本地构建两个 Go 镜像：armadra-cloud（go/Dockerfile.cloud）与 armadra-relay（go/Dockerfile），不推送
pnpm e2e --mode personal  # 跨仓端到端：个人中转 + Armadra 服务器壳 + Node 客户端，需要 Docker，见下
pnpm e2e --mode personal-workers  # 本地 workerd 上的 Workers 中继对假 core 与 Node 客户端，不需要 Docker
pnpm e2e --mode saas      # 真实控制面 + 两个 SaaS 节点 + 一次性 PostgreSQL / Redis / mailpit 容器：注册与邮件、团队、授予、断言、转发、撤销、停用、再平衡（设计 §8.4）
pnpm e2e --mode saas-nodes  # 三个 SaaS 中继节点 + 一次性 Redis 容器对假 core（测试签发者）：节点间转发、慢客户端、撤销、下线再平衡
pnpm db:migrate        # 执行数据库迁移（读 .env.dev）
pnpm protocol:pack     # 协议包打成 tgz（.data/pack/），核对内容，不发布
```

## 跨仓端到端

`pnpm e2e --mode personal`：用 `deploy/e2e/compose.yml` 起个人中转（本仓镜像）与 Armadra 服务器壳（不发布端口，只能经中继到达），脚本里的 Node 客户端走完登录、断言换会话、经中继的 HTTP / 事件流 / 终端 / 协作板、分享链接访客、撤销链接、源离线与撤销源，最后检查全部日志里没有秘密；跑完 `down -v`，结果写在 `.data/e2e/result.json`。

服务器壳镜像取自 Armadra 主仓：`ARMADRA_REPO`（缺省 `../ai-coding-canvas`）与 `ARMADRA_REF`（缺省 `origin/main`，用 `git archive` 导出构建，不动主仓检出；`worktree` 则用目录现状）。`--skip-build` 复用已构建的 `armadra-relay:e2e` 与 `armadra-server:e2e`，`--server-image` 指定现成镜像，`--keep` 保留现场。夜间 `nightly.yml` 对 `deploy/compat.json` 的 min 镜像与 main 现构建各跑一次。

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
