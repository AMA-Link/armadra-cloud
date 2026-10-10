<!--
标题沿用提交信息格式：`类型(范围): 中文描述`，例如 `fix(relay): 闲置连接按 keep-alive 上限关闭`。
Title follows the commit format: `type(scope): description`, for example `fix(relay): …`.
规范见 .github/CONTRIBUTING.md。不需要的小节写「无 / None」，不要删掉。
See .github/CONTRIBUTING.md. Write "无 / None" for sections that do not apply instead of deleting them.
-->

## 变更摘要 / Summary

<!-- 做了什么、为什么。 / What changed and why. -->

## 关联 issue / Related issues

<!-- Closes #123 / Refs #456 -->

## 改动范围 / Scope

- [ ] relay（`apps/relay`）
- [ ] cloud（`apps/cloud`）
- [ ] 协议包 / protocol package（`packages/platform-protocol`）
- [ ] 中继内核 / relay kernel（`packages/relay-kernel`）
- [ ] 中继 Node 运行时 / relay Node runtime（`packages/relay-runtime-node`）
- [ ] 中继 Workers 运行时 / relay Workers runtime（`packages/relay-runtime-workers`、`apps/relay-workers`）
- [ ] 控制核 / control core（`packages/control-core`）
- [ ] cloud-shared（`packages/cloud-shared`）
- [ ] deploy / 镜像 / images（`deploy/`、`apps/*/Dockerfile`）
- [ ] docs
- [ ] tools / scripts / CI

## 契约与协议变化 / Contract and protocol changes

<!--
`docs/contracts/cloud-api.md` 的哪几节（§N）；新增节往后加，不改已有节号。形状改在协议包里，再 `pnpm contract:generate`。
协议包版本与 `PROTOCOL_VERSION`（major.minor）是否变了；`deploy/compat.json` 是否同步；Armadra 客户端是否要跟着改。
黄金夹具与身份向量是否在本 PR 里重新生成。
Which sections (§N) of docs/contracts/cloud-api.md; new sections are appended, existing numbers are kept. Shapes change in the protocol package, then `pnpm contract:generate`.
Whether the protocol package version or `PROTOCOL_VERSION` (major.minor) changed, whether `deploy/compat.json` is updated, and whether the Armadra client must follow.
Whether golden fixtures and identity vectors were regenerated in this PR.
-->

- 契约节号 / Sections:
- 协议版本 / Protocol version:
- 生成物 / Generated files:

## 数据库迁移 / Database migrations

<!--
新迁移的编号（main 上最大号 +1）与 `migrations.lock` 是否同步；已发布的迁移不得修改；每个迁移带测试。
Number of the new migration (max on main + 1) and whether `migrations.lock` is updated; published migrations must not change; every migration ships with a test.
-->

- 编号 / Number:
- `migrations.lock`:

## 测试 / Testing

<!-- 跑了哪些命令、加了哪些用例。 / Commands run and tests added. -->

- [ ] 单元测试 / Unit tests（`pnpm test`）:
- [ ] 集成测试 / Integration tests（`pnpm test:integration`，需 `pnpm dev:up`）:
- [ ] 跨仓端到端 / Cross-repo e2e（`pnpm e2e --mode personal`）:
- [ ] 镜像 / Images（`pnpm images:build`）:
- [ ] `pnpm check` 与 / and `pnpm repo:test`

## 检查清单 / Checklist

<!-- 见 AGENTS.md。 / See AGENTS.md. -->

- [ ] relay 不 import cloud，app 不 import Armadra 源码，与 Armadra 只经 `@armadra/platform-protocol` 与镜像往来 / relay does not import cloud, apps do not import Armadra sources, and everything shared with Armadra goes through `@armadra/platform-protocol` and images
- [ ] `@orpc/*` 只出现在允许的门面文件里；协议包不 import `node:*`（夹具除外） / `@orpc/*` appears only in the allowed facade files; the protocol package does not import `node:*` (fixtures excepted)
- [ ] 流正文、终端输出、画布内容、口令与令牌明文不进入日志、持久化或响应 / Stream bodies, terminal output, canvas content, passwords and plaintext tokens stay out of logs, persistence and responses
- [ ] 令牌校验保留 `aud`、`exp`、`jti`；中继只把流转给令牌 `src` 指定的源 / Token checks keep `aud`, `exp` and `jti`; the relay only forwards to the source named by the token's `src`
- [ ] `personal` 模式与其入口不依赖 PostgreSQL / Redis / `personal` mode and its entry points do not depend on PostgreSQL / Redis
- [ ] 迁移只新增，编号连续，`migrations.lock` 已同步；没有自动清库、重建或跳过损坏账本的逻辑 / Migrations are append-only and contiguous, `migrations.lock` updated; no auto-wipe, rebuild or skipping of a corrupt ledger
- [ ] 错误码取自协议包注册表，线上形状为 `{ code, message, requestId?, details? }`，JSON 为 camelCase / Error codes come from the protocol package registry, the wire shape is `{ code, message, requestId?, details? }`, JSON is camelCase
- [ ] 改了形状的同时改了协议包、`cloud-api.md`、OpenAPI、夹具与身份向量（`pnpm protocol:check`、`pnpm contract:check` 通过） / Shape changes update the protocol package, `cloud-api.md`, OpenAPI, fixtures and identity vectors (`pnpm protocol:check` and `pnpm contract:check` pass)
- [ ] 依赖写精确版本；改了生产依赖已 `pnpm notices` 并提交 `THIRD_PARTY_NOTICES.md` / Dependencies use exact versions; production dependency changes include a regenerated `THIRD_PARTY_NOTICES.md`
- [ ] CLI 与日志用简体中文短句，变量进字段；新文档已登记到 `docs/README.md` / CLI and log messages are short Simplified Chinese sentences with variables in fields; new docs are registered in `docs/README.md`
- [ ] 用户可见的变化已记进 `CHANGELOG.md` 的 Unreleased / User-visible changes are recorded under Unreleased in `CHANGELOG.md`

## 需要维护者提供的外部条件 / External prerequisites

<!-- npm、GHCR 发布，域名、证书、密钥、真实部署等无法在 CI 里验证的部分，以及用本地或 mock 验证到了哪一步。发布由维护者执行。 / npm and GHCR publishing, domains, certificates, keys, real deployments, etc. that CI cannot verify, and how far local runs or mocks cover them. Publishing is done by maintainers. -->

## 已知限制 / Known limitations
