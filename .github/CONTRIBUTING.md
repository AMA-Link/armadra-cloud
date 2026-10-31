# 贡献指南 / Contributing

开发环境、检查命令与协议包的改动顺序见 [README](../README.md)；目录边界与红线见 [AGENTS.md](../AGENTS.md)；文档索引在 [docs/README.md](../docs/README.md)。

For the development setup, check commands and the order for changing the protocol package, see the [README](../README.md); for boundaries and hard rules, see [AGENTS.md](../AGENTS.md); the documentation index is [docs/README.md](../docs/README.md).

## 需求流程 / Workflow

每个需求都按同一条路径推进，结论留在 Issue 里，而不只在聊天或提交信息里。

Every requirement follows the same path, and its conclusions live in the Issue rather than only in chat or commit messages.

1. **建 Issue / Open an Issue**：简单梳理后用对应表单建 Issue，写清背景、目标与范围，标签 `needs-triage`。 / Sort the requirement out briefly and open an Issue with the matching form: background, goals and scope, labelled `needs-triage`.
2. **完善 Issue 与设计 / Refine the Issue and design**：调研结论与设计方案回填到同一个 Issue（方案、批次、验收标准）；方案较大时写成仓库里的设计文档，Issue 里放链接。方案未定时标 `needs-design`，定下后改为 `ready`。 / Put research findings and the design (approach, batches, acceptance criteria) back into the same Issue; larger designs become a design document in the repository, linked from the Issue. Label `needs-design` while the approach is open and `ready` once it is settled.
3. **开 PR 实现 / Implement in PRs**：按批次开 PR，正文按模板填写并关联 Issue——中间批次写 `Refs #N`，最后一个写 `Closes #N`；Issue 里的批次清单随 PR 勾选。 / Open one PR per batch, fill in the template and link the Issue — `Refs #N` for intermediate batches, `Closes #N` for the last one; tick the Issue's batch checklist as PRs land.
4. **合并与收尾 / Merge and close**：CI 全部通过后用 merge commit 合并；全部批次合入后关闭 Issue，并在 Issue 里写一段结果（做了什么、实测数据、遗留项，遗留项另开 Issue）。 / Merge with a merge commit once CI is green; when every batch has landed, close the Issue with a short result (what was done, measurements, leftovers — leftovers get their own Issues).

小修小补（拼写、单行修复）可以直接开 PR，但仍在 PR 里说明来源。 / Trivial fixes (typos, one-line fixes) may go straight to a PR, still stating where they came from.

## 提交 issue / Opening issues

- 先搜索已有 issue，一个 issue 只写一个问题。 / Search existing issues first; one problem per issue.
- 使用对应的表单：缺陷报告、功能建议、中继 / 部署问题、文档问题。 / Use the matching form: bug report, feature request, relay or deployment issue, or documentation.
- 写清模式（personal / saas）、部署方式（compose、镜像、源码）、版本、协议版本与 Armadra 客户端版本。 / Include the mode (personal / saas), deployment (compose, image, source), version, protocol version and Armadra client version.
- **不要贴凭据**（口令、令牌、签名钥、`state.json`、`relay.db`、`.env`）、流正文与终端输出，日志只摘相关行并把域名、IP 打码。 / **Never paste credentials** (passwords, tokens, signing keys, `state.json`, `relay.db`, `.env`), stream bodies or terminal output; quote only relevant log lines with domains and IPs redacted.
- 安全问题按 [SECURITY.md](SECURITY.md) 私下报告。 / Report security problems privately as described in [SECURITY.md](SECURITY.md).

## 提交信息 / Commit messages

格式为 `类型(范围): 描述`，描述用中文，说清行为变化而不只是改了哪个文件：

The format is `type(scope): description`. Descriptions are written in Chinese and state the behavior change, not just which file was touched:

```text
fix(relay): 边缘缺省额度按真实使用放宽，契约 §7 与部署说明同步
feat(platform-protocol): 加 links.update，夹具、契约生成块与 OpenAPI 同步
docs(contract): cloud-api §12 媒体票
```

- 类型 / Types：`feat`、`fix`、`docs`、`test`、`refactor`、`perf`、`build`、`chore`、`ci`、`release`、`revert`。
- 范围 / Scopes：模块或目录名，例如 `relay`、`cloud`、`relay-kernel`、`relay-runtime-workers`、`relay-workers`、`control-core`、`platform-protocol`、`cloud-shared`、`contract`、`deploy`、`e2e`、`changelog`；跨两处用逗号，例如 `test(relay,e2e)`。 / A module or directory name; join two with a comma.
- 一个模块连同它的测试一个提交；不要在提交信息里加生成说明或自动署名。 / One commit per module together with its tests; no generated notes or automatic trailers.

## 提交 PR / Opening pull requests

- 只改 Markdown 与 `docs/`（契约等生成物除外）的 PR，CI 只跑轻量的 `docs` 作业；代码审查不自动触发，需要时由维护者评论 `@codex review`。 / Docs-only PRs run only the light `docs` CI job; code review is not automatic — a maintainer comments `@codex review` when wanted.
- 从 `main` 拉出按用途命名的分支，例如 `fix/relay-keepalive-limits`、`docs/github-community-files`。 / Branch from `main` with a purpose-based name.
- PR 标题沿用提交信息格式；正文按模板填写，不适用的小节写「无 / None」。 / The PR title follows the commit format; fill in the template and write "无 / None" for sections that do not apply.
- 用 `Closes #N` 关联 issue。 / Link issues with `Closes #N`.
- 合并前 `pnpm check`、`pnpm repo:test` 与 CI 全部通过；合并使用 merge commit，保留逐个提交。 / `pnpm check`, `pnpm repo:test` and CI must pass before merging; merges use a merge commit and keep individual commits.

## 本仓库的约束 / Repository rules

- 各 app 都不 import Armadra 源码，往来只经 `@armadra/platform-protocol` 与容器镜像。 / No app imports Armadra sources, and everything shared goes through `@armadra/platform-protocol` and container images.
- 中继与控制面都不持久化、不记录流正文、终端输出、画布内容、口令与令牌明文。 / Neither the relay nor the control plane persists or logs stream bodies, terminal output, canvas content, passwords or plaintext tokens.
- `personal` 模式与其入口（`pnpm relay:personal`、`deploy/personal/compose.yml`）不依赖 PostgreSQL / Redis。SaaS 业务路由只预留，答 501 `not_implemented`。 / `personal` mode and its entry points do not depend on PostgreSQL / Redis. SaaS business routes are reserved and answer 501 `not_implemented`.
- 改接口形状先改协议包，再 `pnpm contract:generate`；[契约](../docs/contracts/cloud-api.md)已有 §N 不改号，新增节往后加。 / Change shapes in the protocol package first, then run `pnpm contract:generate`; keep existing §N numbers in the [contract](../docs/contracts/cloud-api.md) and append new sections.
- 协议包的黄金夹具与身份向量是生成物，同一个 PR 里重新生成（`pnpm --filter @armadra/platform-protocol fixtures`、`node scripts/gen-vectors.mjs`）。 / Golden fixtures and identity vectors are generated files; regenerate them in the same PR.
- 数据库迁移只在 `packages/control-store-postgres/migrations/`（PostgreSQL）与 `packages/control-store-sql/migrations/`（SQLite）新增编号迁移并同步 `migrations.lock`，每个迁移带测试，已发布迁移不得修改。 / Only add numbered migrations under `packages/control-store-postgres/migrations/` (PostgreSQL) and `packages/control-store-sql/migrations/` (SQLite), update `migrations.lock` and test every migration; never modify published migrations.
- 依赖写精确版本（无 `^`）；改了生产依赖跑 `pnpm notices` 并提交 `THIRD_PARTY_NOTICES.md`。 / Pin exact dependency versions (no `^`); after changing production dependencies, run `pnpm notices` and commit `THIRD_PARTY_NOTICES.md`.
- CLI 提示、错误与日志 `msg` 用简体中文短句，变量进字段；错误码登记在协议包的注册表。 / CLI prompts, errors and log `msg` values are short Simplified Chinese sentences with variables in fields; error codes are registered in the protocol package.
- 用户可见的变化记进 [CHANGELOG](../CHANGELOG.md) 的 Unreleased。 / Record user-visible changes under Unreleased in the [CHANGELOG](../CHANGELOG.md).
- 发布（npm、GHCR、`v*` 标签）、域名、证书与密钥由维护者执行；不要在本地 `npm publish` 或 `docker push`。 / Publishing (npm, GHCR, `v*` tags), domains, certificates and keys are handled by maintainers; never run `npm publish` or `docker push` locally.
