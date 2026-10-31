# armadra-cloud 开发约定

## 代码边界

- 中继与 SaaS 控制面的运行时实现是 Go（`go/`，镜像由 `go/Dockerfile`（中继）与 `go/Dockerfile.cloud`（控制面 `armadra-cloud`）构建）；TypeScript 只剩 Cloudflare Workers 中继 `apps/relay-workers`（TS 版 Node 中继 `apps/relay` 与 `packages/relay-runtime-node` 已随 #59 的 G4.1 删除，TS 控制面 `apps/cloud` 已随 #177 删除）。`apps/*` 不得 import Armadra 仓的源码；与 Armadra 的全部往来只经 `@armadra/platform-protocol` 与容器镜像。
- 共享内核两个包（Workers 中继用；Go 中继另有自己的实现，行为以协议包夹具对齐）：`packages/relay-kernel`（隧道帧流与信用窗口、握手、心跳、隧道表、边缘判定、限流、媒体票、令牌校验、`ControlPlane` 接口与内存实现；HTTP/1.1 与 WebSocket 帧的纯编解码在协议包的 `http1` / `ws-frames`）与 `packages/control-core`（存储端口、账号 / 会话 / 签发 / 源 / 链接 / me-stream 服务、`createCloudRouter`）。`relay-kernel` 运行时无关：源码只许 `node:crypto` / `node:buffer`，不 import `ws`、`node:http` / `net` / `stream` / `fs` 与 `control-core`，不带 PostgreSQL / Redis；Cloudflare Workers 部分（Worker 入口与闸、`SourceDO` / `ControlDO`、Hibernation 粘合、DO 内的 HTTP / WS 转发、DO SQLite 适配）在 `packages/relay-runtime-workers`，源码只许 `node:crypto` / `node:buffer`，复用 `control-store-sqlite` 的可移植模块（不碰它的 `open` / `migrate`），wrangler 项目是 `apps/relay-workers`（只 re-export 运行时）。`control-core` 只经端口读写状态，不 import `node:fs`、存储客户端、HTTP / WS 与中继内核；端口的存储实现是 `packages/control-store-sqlite`（`node:sqlite`，Workers 的 DO SQLite 适配复用它的可移植部分）与 `packages/control-store-postgres`（PostgreSQL 迁移的唯一源；运行代码现在只给 `go/testdata/compat` 的夹具生成与读回、以及端口合同的集成测试用，同一套端口合同与服务合同）；包不 import app。边界由各包的 `boundaries.test.ts`、ESLint 与 `repo.rules.json` 的 `packageDependencies` 守。
- `packages/e2e-fake-core` 只给 `scripts/e2e` 的 saas / workers 模式当假 core（Node `http` + `ws` 的隧道客户端），不发布，也不进任何镜像。
- `/v1/*` 的路由表只有 `control-core` 的 `createCloudRouter` 一份：personal 给处理器、其余答 404；cloud 未实现的统一答 501。
- `@orpc/*` 只能出现在 `packages/platform-protocol/src/cloud-api/`、`packages/control-core/src/router.ts`、`tools/contract/`（ESLint `no-restricted-imports` 守）。
- 协议包只含 zod schema、类型、纯编解码与常量，不 import `node:*`（`src/fixtures/` 例外，只在测试里用）。
- 错误码只从 `@armadra/platform-protocol/errors` 取；线上形状 `{ code, message, requestId?, details? }`，JSON camelCase。
- Go 中继只在 `go/internal/store`（SQLite 缺省、`--store postgres` 显式选）与 `go/internal/control/{redis,postgres}`（多节点控制面）碰存储；数据目录缺省只有 `relay.db`（显式选 `--store postgres` 时在库里，数据目录留 `postgres.json` 标记，不含连接串）。Workers 中继的状态只在 `ControlDO` 的 DO SQLite（同一份迁移与账本，签名私钥用 `RELAY_MASTER_KEY` 包裹）与 `SourceDO` 的 WS 附件 / 同步 KV（媒体票只存派生哈希与用票本身派生的密钥封起来的密文）/ DO SQLite（只有流量统计表 `relay_traffic_buckets`，同一份迁移）。relay 与 cloud 都不得持久化或记录任何流正文、终端输出、画布内容、口令、令牌明文。
- `personal` 的缺省装配（sqlite 存储 + 内存控制面）不得静态依赖 PostgreSQL / Redis；`--store postgres` 与 `--control redis|postgres`（多节点）是显式选项，Go 的 `go/internal/personal` 只在选了它们时才建立连接。

## 范围与优先级

- 个人中转与 SaaS 按 #59（中继 Go 重写）、#60（SaaS 能力）推进；Workers 版冻结功能，只修缺陷。
- SaaS 服务端（控制面的账号、团队、授予、计费等）由 Go 的 `armadra-cloud` 实现；`control-core` 路由表里 saas 模式未实现的业务路由统一答 501 `not_implemented`（测试守住），实现时逐条拿掉；`/v1` 的新增 procedure 同样先预留再实现。
- 个人中转的缺省入口（`pnpm relay:personal`、`deploy/personal/compose.yml`）不得依赖 PostgreSQL / Redis；多节点是显式选项（`deploy/compose.yml` 的 `relay-personal` profile）。

## Go 代码

- Go 模块在 `go/`（`module github.com/AMA-Link/armadra-cloud/go`），随 #59 分批落地；设计见 `docs/design/go-relay-design.md`（PR #61 合并后）。
- 协议包是源：`go/internal/protocol/gen/` 是 `tools/schemagen` 从 `packages/platform-protocol/schema/` 生成的，文件头标「生成物，勿手改」。改了 schema、夹具或身份常量，同一个 PR 里 `go generate ./...` 并提交；`pnpm go:check` 的最后一步（`go generate` 后 `git diff` 必须为空）守住。
- 错误码只从 `gen` 取（`gen.Err*`），不在业务代码里写错误码字符串；线上形状与 TS 一致：`{ code, message, requestId?, details? }`。
- 日志字段走白名单（`internal/obs`）：字段名固定，不记流正文、终端输出、画布内容、口令、令牌明文；日志 `msg` 是简体中文短句，变量进字段。
- `internal/core` 不 import `internal/tunnel`、`internal/edge`（延续「控制核不碰中继内核」），由 `repo.rules.json` 的 `goModules.internalBoundaries` 守。
- `TeamScope` 只能在 `internal/cloud/teams` 的 `Require` / `Lock` 里构造：`internal/store/saas/teamscope` 的生产代码只许 `cloud/teams` 与 `store/saas`（类型别名）import，由 `goModules.onlyImportedBy`（白名单写法，`_test.go` 不受限）守；团队范围的仓储方法只收 `TeamScope`，不收裸 teamId。
- 依赖写精确版本，`go/` 只许 import 标准库、本模块与 `repo.rules.json` 的 `goModules.allow` 登记过的模块（`go.mod` 不用 `replace`）；测试专用的模块（JSON Schema 校验库）只许出现在 `_test.go` 与 `testutil/`。staticcheck 固定版本，经 `go run` 调用，不进 `go.mod`。改 `go.mod` 后 `pnpm notices` 并提交 `THIRD_PARTY_NOTICES.md`。
- 代码注释与日志、CLI 文案是简体中文短句，标识符用英文；夹具（`packages/platform-protocol/fixtures/`）作表驱动测试，测试不迁就实现去改夹具。
- 存储与兼容：迁移 SQL 以 TS 包里的为准（`packages/control-store-sql/migrations/`、`packages/control-store-postgres/migrations/`），`go generate` 拷到 `go/internal/store/{sqlite,postgres}/migrations/` 供 `go:embed`，`repo-check` 按 `migrations.lock` 校验两份逐字节一致；新迁移只在 TS 包里加。`go/testdata/compat/` 是 TS 存储写出来的固定夹具（SQLite 库、PostgreSQL 转储、`state.json`）和生成脚本（`state-schema.ts`、`snapshot.ts` 是它自带的最小结构，TS 版个人中转已删除），`internal/store/compat` 的测试让 Go 读这些夹具、再把 Go 写过的库交回 TS 的存储（`control-store-*`）读；合同或记录形状变了，同 PR 重新生成夹具（`node go/testdata/compat/make-fixtures.mjs`，用法见脚本头）。
- Redis 的测试读 `REDIS_URL`（`pnpm dev:up` 的 Redis 是 `redis://127.0.0.1:6390`，CI 的 `go` 作业带 Redis 服务容器），每个测试一个随机前缀、用完只删这个前缀的键；没有这个变量就跳过，CI 里（`CI` 非空）一律失败。多节点控制面（`internal/control/{redis,postgres}`）的键、频道、表与 JSON 形状以 Go 实现为准（线上形状与已删除的 Node 版相同，滚动切换时两种节点可共用同一个 Redis / 库）；合同测试在 `internal/control/controltest`。
- 邮件的集成测试读 `MAILPIT_SMTP_ADDR`（`pnpm dev:up` 的 mailpit 是 `127.0.0.1:1026`）与 `MAILPIT_API_URL`（`http://127.0.0.1:8026`），CI 的 `go` 作业带 mailpit 服务容器；mailpit 是共用的，每个测试用随机收件地址、只按 `to:` 查自己的信，不清空信箱；没有这两个变量就跳过，CI 里（`CI` 非空）一律失败。
- PostgreSQL 的测试读 `ARMADRA_CLOUD_TEST_DATABASE_URL`（`pnpm dev:up` 的库或 CI 的服务容器），每个测试一个随机名字的 schema、用完只删它；没有这个变量就跳过并说明。CI 的 `go` 作业带 PostgreSQL 服务容器并装 TS 依赖，所以两边都会跑。
- 验证：`pnpm go:check`（gofmt、`go vet`、staticcheck、`go test -race`、生成物新鲜度）。

## 界面与文案统一

- 所有平台的前后端遵循同一套设计逻辑、样式、语言格式与多语言形式。任何页面遵循用户级 `better-*` 规则与 Armadra 的 `docs/design/design-system.md`（token、组件与形态，只用现有 shadcn 组件与 ui 原语）；极简界面，不加说明性文字，不中英混排，文案进 i18n、中英同步、术语统一。
- 个人中转托管的页面直接托管 Armadra `apps/web` 的构建产物，不在本仓另写前端；本仓若必须有页面（中继首页、错误页），复用同一份 token 与文案风格，接入同样的语言切换。
- 错误一律 `{ code, message }`，`code` 稳定且登记在协议包的注册表；界面按 `code` 取 i18n 文案，不直接展示后端 `message`。
- CLI 与日志的语言风格统一：用法写成「用法: <命令> …」加两格缩进的子命令表；提示与错误是简体中文短句，取值放在全角冒号之后；日志 `msg` 是简体中文短句，变量进字段。

## 数据库

- 迁移两个目录，各自从 0001 连续：PostgreSQL 在 `packages/control-store-postgres/migrations/`（cloud 的 `armadra-cloud migrate` 与 Go 中继的 `--store postgres` / `--control postgres` 共用，账本 `cloud_migrations`），SQLite（Go 中继 `--store sqlite` 与 Workers 中继的 DO SQLite 共用；新增迁移同时登记进 `relay-runtime-workers` 的 `store-do-sqlite/migrations.ts`）在 `packages/control-store-sql/migrations/`；字节记进 `migrations.lock`（`pnpm repo:check`）；已发布迁移不得修改；未知 / 损坏的库或账本拒绝启动，禁止自动清库或重建。
- 每个迁移同 PR 带测试。

## 按需阅读与验证

- 提交信息、分支、PR 与 issue 规范见 [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md)；接到需求先建或找到对应 Issue，按其中「需求流程」推进（Issue → 设计回填 → PR 关联 → 合并后收尾）。

- 怎么跑见 [README](README.md)；文档索引在 [docs/README.md](docs/README.md)，新增文档要登记。
- 契约：`docs/contracts/cloud-api.md`（§N 不改号，只追加）；形状表与 `cloud-openapi.json` 由 `pnpm contract:generate` 从协议包生成，改形状改协议包。
- 协议包的黄金夹具、schema 导出（`schema/`，JSON Schema、错误码表、常量与 OpenAPI）与身份向量是生成物：改了帧、声明、契约或身份常量，同一个 PR 里重新生成（`pnpm --filter @armadra/platform-protocol fixtures`、`pnpm --filter @armadra/platform-protocol schema`、`pnpm contract:generate`、`node scripts/gen-vectors.mjs`），`pnpm protocol:check` 守住。
- 验证：`pnpm check`、`pnpm repo:test`、`pnpm go:check`、`pnpm test`、`pnpm test:integration`（需 `pnpm dev:up`）、`pnpm images:build`、`pnpm e2e --mode personal`（Go 镜像 + Armadra 服务器壳）；Workers 中继另有 `pnpm e2e --mode personal-workers`（本地 workerd）；SaaS 另有 `pnpm e2e --mode saas`（真实控制面 + 两个节点 + 一次性 PostgreSQL / Redis / mailpit 容器，设计 §8.4 的全部场景）与 `pnpm e2e --mode saas-nodes`（三个节点 + 一次性 Redis 容器，测试签发者代替控制面）。
- 发布（npm、GHCR）、域名、证书、密钥都是对外操作，由维护者执行；本地绝不手动 `npm publish` / `docker push`。Workers 中继同理：`wrangler deploy` / `rollback` / `secret put|delete`、改 DNS 与面板设置都由维护者执行，测试与端到端只用本地 workerd（vitest 的 Workers 插件、`wrangler dev --local`）。
- 依赖写精确版本（无 `^`）；改生产依赖后 `pnpm notices` 并提交 `THIRD_PARTY_NOTICES.md`（含 Go 模块）。
- 不出现任何第三方参考项目的名字。

## Review guidelines

- 简体中文，先结论，每条写文件与行号、会出什么错、怎么修。
- app import Armadra 源码、`@orpc/*` 出现在门面之外：P1；Go 的 `internal/core` import `internal/tunnel` / `internal/edge`、`go/` import 未登记的模块或 Armadra 源码：P1。
- 修改已发布迁移、编号不连续、lock 未同步、任何自动清库：P0。
- 正文 / 令牌 / 口令进入日志、持久化或响应：P0；Go 日志字段不在白名单、把这些放进字段：P0。
- 令牌校验少了 `aud` / `exp` / `jti` 任一项、中继把流转给非令牌 `src` 的源：P0；Go 的 `internal/jose` 少了验证步骤、改了顺序（typ → kid → 签名 → schema → aud → iss → 时间 → jti）、口令或令牌哈希不用定时安全比较：P0。
- 接口形状改了而协议包 / `cloud-api.md` 没同步：P1；手改 `go/internal/protocol/gen/`、协议包改了而 Go 生成物没重新生成、Go 业务代码里写错误码字符串字面量：P1。
- `personal` 的缺省装配静态建立 PostgreSQL / Redis 连接：P1；依赖没写精确版本（含 `go.mod`、staticcheck 版本）或 `THIRD_PARTY_NOTICES.md` 未同步：P2。
- 界面 / CLI 文案没进 i18n 或中英不同步：P2。
- 新增行为没有对应测试时指出缺口；测试只改断言或重生成夹具去迁就实现时要说明理由是否成立。
