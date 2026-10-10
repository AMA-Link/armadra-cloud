# armadra-cloud 开发约定

## 代码边界

- `apps/cloud` 是控制面，`apps/relay` 是中继；`apps/relay` 不得 import `apps/cloud`，两者只共享 `packages/*`。两个 app 都不得 import Armadra 仓的源码；与 Armadra 的全部往来只经 `@armadra/platform-protocol` 与容器镜像。
- 共享内核两个包：`packages/relay-kernel`（隧道终端、多路复用与信用窗口、心跳、边缘转发、限流、clientIp、媒体票、令牌校验、`ControlPlane` 接口与内存实现）与 `packages/control-core`（存储端口、账号 / 会话 / 签发 / 源 / 链接 / me-stream 服务、`createCloudRouter`）。`relay-kernel` 不 import `control-core`、不带 PostgreSQL / Redis；`control-core` 只经端口读写状态，不 import `node:fs`、存储客户端、HTTP / WS 与 `relay-kernel`；包不 import app。边界由各包的 `boundaries.test.ts`、ESLint 与 `repo.rules.json` 的 `packageDependencies` 守。
- `/v1/*` 的路由表只有 `control-core` 的 `createCloudRouter` 一份：personal 给处理器、其余答 404；cloud 未实现的统一答 501。
- `@orpc/*` 只能出现在 `packages/platform-protocol/src/cloud-api/`、`packages/control-core/src/router.ts`、`tools/contract/`（ESLint `no-restricted-imports` 守）。
- 协议包只含 zod schema、类型、纯编解码与常量，不 import `node:*`（`src/fixtures/` 例外，只在测试里用）。
- 错误码只从 `@armadra/platform-protocol/errors` 取；线上形状 `{ code, message, requestId?, details? }`，JSON camelCase。
- relay 不碰 PostgreSQL；personal 的状态只在数据目录（`state.json` 或 `node:sqlite` 的 `relay.db`，经 `packages/control-store-sqlite`）。relay 与 cloud 都不得持久化或记录任何流正文、终端输出、画布内容、口令、令牌明文。
- `personal` 模式不得引入对 PostgreSQL / Redis 的运行时依赖（`apps/relay/src/boundaries.test.ts` 连同它装配的内核与控制核一起扫）。

## 范围与优先级

- 先交付个人中转（`armadra-relay personal`）；SaaS 服务端（控制面的账号、组织、源目录、计费，中继的 Redis 实现）只预留接口：协议包的 `cloud-api` schema 保留，`apps/cloud` 的业务路由在路由表里统一答 501 `not_implemented`（测试守住），实现时逐条拿掉。
- 个人中转的入口（`pnpm relay:personal`、`deploy/personal/compose.yml`）不得依赖 PostgreSQL / Redis。

## 界面与文案统一

- 所有平台的前后端遵循同一套设计逻辑、样式、语言格式与多语言形式。任何页面遵循用户级 `better-*` 规则与 Armadra 的 `docs/design/design-system.md`（token、组件与形态，只用现有 shadcn 组件与 ui 原语）；极简界面，不加说明性文字，不中英混排，文案进 i18n、中英同步、术语统一。
- 个人中转托管的页面直接托管 Armadra `apps/web` 的构建产物，不在本仓另写前端；本仓若必须有页面（中继首页、错误页），复用同一份 token 与文案风格，接入同样的语言切换。
- 错误一律 `{ code, message }`，`code` 稳定且登记在协议包的注册表；界面按 `code` 取 i18n 文案，不直接展示后端 `message`。
- CLI 与日志的语言风格统一：用法写成「用法: <命令> …」加两格缩进的子命令表；提示与错误是简体中文短句，取值放在全角冒号之后；日志 `msg` 是简体中文短句，变量进字段。

## 数据库

- 迁移两个目录，各自从 0001 连续：PostgreSQL 在 `apps/cloud/src/db/migrations/`，SQLite（Node 中继 `--store sqlite` 与 DO SQLite 共用）在 `packages/control-store-sql/migrations/`；字节记进 `migrations.lock`（`pnpm repo:check`）；已发布迁移不得修改；未知 / 损坏的库或账本拒绝启动，禁止自动清库或重建。
- 每个迁移同 PR 带测试。

## 按需阅读与验证

- 提交信息、分支、PR 与 issue 规范见 [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md)；接到需求先建或找到对应 Issue，按其中「需求流程」推进（Issue → 设计回填 → PR 关联 → 合并后收尾）。

- 怎么跑见 [README](README.md)；文档索引在 [docs/README.md](docs/README.md)，新增文档要登记。
- 契约：`docs/contracts/cloud-api.md`（§N 不改号，只追加）；形状表与 `cloud-openapi.json` 由 `pnpm contract:generate` 从协议包生成，改形状改协议包。
- 协议包的黄金夹具、schema 导出（`schema/`，JSON Schema、错误码表、常量与 OpenAPI）与身份向量是生成物：改了帧、声明、契约或身份常量，同一个 PR 里重新生成（`pnpm --filter @armadra/platform-protocol fixtures`、`pnpm --filter @armadra/platform-protocol schema`、`pnpm contract:generate`、`node scripts/gen-vectors.mjs`），`pnpm protocol:check` 守住。
- 验证：`pnpm check`、`pnpm repo:test`、`pnpm test`、`pnpm test:integration`（需 `pnpm dev:up`）、`pnpm images:build`。
- 发布（npm、GHCR）、域名、证书、密钥都是对外操作，由维护者执行；本地绝不手动 `npm publish` / `docker push`。
- 依赖写精确版本（无 `^`）；改生产依赖后 `pnpm notices` 并提交 `THIRD_PARTY_NOTICES.md`。
- 不出现任何第三方参考项目的名字。

## Review guidelines

- 简体中文，先结论，每条写文件与行号、会出什么错、怎么修。
- relay import cloud、app import Armadra 源码、`@orpc/*` 出现在门面之外：P1。
- 修改已发布迁移、编号不连续、lock 未同步、任何自动清库：P0。
- 正文 / 令牌 / 口令进入日志、持久化或响应：P0。
- 令牌校验少了 `aud` / `exp` / `jti` 任一项、中继把流转给非令牌 `src` 的源：P0。
- 接口形状改了而协议包 / `cloud-api.md` 没同步：P1。
- `personal` 模式引入 PostgreSQL / Redis 运行时依赖：P1；依赖没写精确版本或 `THIRD_PARTY_NOTICES.md` 未同步：P2。
- 界面 / CLI 文案没进 i18n 或中英不同步：P2。
- 新增行为没有对应测试时指出缺口；测试只改断言或重生成夹具去迁就实现时要说明理由是否成立。
