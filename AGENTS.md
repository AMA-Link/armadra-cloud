# armadra-cloud 开发约定

## 代码边界

- `apps/cloud` 是控制面，`apps/relay` 是中继；`apps/relay` 不得 import `apps/cloud`，两者只共享 `packages/*`。两个 app 都不得 import Armadra 仓的源码；与 Armadra 的全部往来只经 `@armadra/platform-protocol` 与容器镜像。
- `@orpc/*` 只能出现在 `packages/platform-protocol/src/cloud-api/`、`apps/cloud/src/http/rpc.ts`、`apps/relay/src/personal/api.ts`、`tools/contract/`（ESLint `no-restricted-imports` 守）。
- 协议包只含 zod schema、类型、纯编解码与常量，不 import `node:*`（`src/fixtures/` 例外，只在测试里用）。
- 错误码只从 `@armadra/platform-protocol/errors` 取；线上形状 `{ code, message, requestId?, details? }`，JSON camelCase。
- relay 不碰数据库；relay 与 cloud 都不得持久化或记录任何流正文、终端输出、画布内容、口令、令牌明文。
- `personal` 模式不得引入对 PostgreSQL / Redis 的运行时依赖（`control/memory.ts` 的 import 扫描测试守）。

## 数据库

- 迁移只有一个目录 `apps/cloud/src/db/migrations/`，编号从 0001 连续；字节记进 `migrations.lock`（`pnpm repo:check`）；已发布迁移不得修改；未知 / 损坏的账本拒绝启动，禁止自动清库或重建。
- 每个迁移同 PR 带测试。

## 按需阅读与验证

- 怎么跑见 [README](README.md)；文档索引在 [docs/README.md](docs/README.md)，新增文档要登记。
- 契约：`docs/contracts/cloud-api.md`（§N 不改号，只追加）；形状表由 `pnpm contract:generate` 从协议包生成，改形状改协议包。
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
- 新增行为没有对应测试时指出缺口。
