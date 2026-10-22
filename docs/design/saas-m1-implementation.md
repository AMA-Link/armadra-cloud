# SaaS 控制面 M1 实施设计（G6-M1）

> 状态：待审（2026-10-23）。实施跟踪 AMA-Link/armadra-cloud#60（SaaS 能力）与 #59（Go 重写的 G6）。
>
> - 能力、角色、分期与规范细节以 [SaaS 能力设计](saas-design.md)（下称「能力设计」）为准；架构、目录、节点与部署拓扑以 [中继 Go 重写](go-relay-design.md)（下称「Go 设计」）为准；线上形状以契约 `cloud-api.md` §1–§32（协议 1.4）与协议包为准。
> - 本文只定**怎么落地**：进程与部署、迁移 DDL、Go 包与端口、HTTP 层、邮件与作业、跨节点约定、测试、分批。与上面三份冲突时以它们为准，并在 §10 记成待定问题。
> - 控制台（Armadra `apps/web`）的页面与批次见 Armadra `docs/design/cloud-console.md` §6、§7，本文 §9.3 给对应关系。

## §0 摘要

- **一个新二进制** `armadra-cloud`（`go/cmd/armadra-cloud`），业务在 `go/internal/cloud/*`，复用 `internal/core`（会话、签名、源、链接、me-stream）与 `internal/store/postgres`；SaaS 专有的表经新端口 `internal/store/saas`，只有 PostgreSQL 实现。
- **一个租户**：SaaS 在共享表里用租户 `cloud`（issuer = `APP_ORIGIN`）；个人中转仍是 `default`，两者从不同库或同库不同 schema 读写，互不可见。
- **控制核不知道团队**：`internal/core` 只加接口（可见性与授权策略、源地址、设备种类、错误详情、事件受众、撤销钩子），personal 用现有行为实现，SaaS 由 `internal/cloud` 实现。
- **与中继节点只经 Redis 约定**（§2.4）：路由、在线事件、撤销、节点命令、节点表、团队并发额度、事故开关。G5 的 `armadra-relay saas serve` 实现节点一侧。
- **迁移一次落齐**：M1 要的表在第一批 S0b 里以 6 个连续迁移落库（编号合并时取 main 最大号 +1 起），后续各批只写代码，不再争号。
- **十一批**（§9）：S0a、S0b 并行打底，S1 起服务与账号，之后按「邮件 / 设备码 / 团队 → 主机与授予 → 分享与信任 → 审计、配额与运营 → 跨节点 → 部署与端到端」推进，每批 ≤ 约 1500 行（含测试）。

## §1 范围

### 1.1 M1 做（能力设计 §9 M1 的逐项落点）

| 能力                                                   | 过程（契约）                                                                                                                                                                                                                                                                        | 批        |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------- |
| 平台信息与 JWKS                                        | `platform.info`（`mode: "saas"`、`relay.addressing: "host"`、`registrationsOpen`、`challenge`）、`platform.jwks`、`/health`、`/ready`                                                                                                                                               | S1        |
| 注册、登录、刷新、登出、改口令、提权                   | `auth.register`、`auth.login`、`auth.refresh`、`auth.logout`、`auth.changePassword`、`auth.stepUp`（只认 `password`，运营账号另认 `totpCode`）                                                                                                                                      | S1        |
| 邮箱验证、找回口令、改邮箱、新设备提醒                 | `auth.emailVerify`、`auth.emailVerifyResend`、`auth.passwordResetStart/Finish`、`me.emailChange`                                                                                                                                                                                    | S2        |
| 设备码                                                 | `auth.deviceStart`、`auth.devicePoll`、`auth.deviceApprove`、`auth.deviceGet`、`auth.deviceDeny`                                                                                                                                                                                    | S3        |
| 账号与设备                                             | `me.get`（`teams`、`scopes`、`csrfToken`）、`me.update`、`me.devices`、`me.deviceRevoke`、`me.spaces`                                                                                                                                                                               | S1、S4    |
| 团队                                                   | `teams.*` 全部 14 个                                                                                                                                                                                                                                                                | S4        |
| 团队邀请                                               | `invitations.*`（`kind = team`）                                                                                                                                                                                                                                                    | S4        |
| 主机                                                   | `sources.registrationToken`（`space`）、`sources.register`、`sources.heartbeatHint`、`sources.list`（筛选、分页）、`sources.get`、`sources.update`、`sources.revoke`、`sources.assertion`、`sources.accessList`、`sources.accessRevoke`、`me.sources`                               | S5        |
| 授予                                                   | `grants.list`、`grants.create`、`grants.update`、`grants.revoke`、`grants.effective`（主体 `team` / `member` / `guest`）                                                                                                                                                            | S5        |
| 分享                                                   | `links.*`（加 `space`）、`invitations.*`（`kind = host`）                                                                                                                                                                                                                           | S6        |
| 新设备信任（云端只转通知、记拒绝）                     | `trust.list`、`trust.reject`；请求的来路见 §10 Q1                                                                                                                                                                                                                                   | S6        |
| 审计与安全日志                                         | 写入贯穿各批；`audit.list`、`me.securityLog`                                                                                                                                                                                                                                        | S2 起、S7 |
| Free 配额与用量页                                      | `limit_reached` 检查贯穿各批；`teams.usage`、`billing.plan`、`billing.usage`（含 `granularity = day`）                                                                                                                                                                              | S4 起、S7 |
| 运营后台最小集                                         | `auth.login { scope: "operator" }`、`auth.mfaVerify`（只给运营账号）、`admin.tenants`、`admin.accounts`、`admin.account`、`admin.accountStatus`、`admin.teams`、`admin.team`、`admin.teamStatus`、`admin.relayNodes`、`admin.relayNodeDrain`、`admin.incident`、`admin.incidentSet` | S7        |
| 多节点中继与按团队计量、撤销即生效、me-stream 站内事件 | `me.streamTicket` + `GET /v1/me/stream`、§32 的事件（`accessRequested` 除外）、Redis 约定（§2.4）                                                                                                                                                                                   | S8        |
| 部署与端到端                                           | `deploy/saas/compose.yml`、`pnpm e2e --mode saas`                                                                                                                                                                                                                                   | S9        |

### 1.2 M1 明确不做（路由表里照旧答 `501 not_implemented`）

| 不做                                                                                                                 | 归属                                |
| -------------------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| 两步验证（用户侧）、passkey、OAuth、恢复码：`auth.totp*`、`auth.passkey*`、`auth.oauth*`、`auth.recoveryCodesRotate` | M2（运营账号的 TOTP 例外，见 §4.9） |
| 分组接口 `groups.*`、授予主体 `group`（答 `403 plan_feature_unavailable`）、按标签授予                               | M2（**表 M1 建好**）                |
| 访问申请 `accessRequests.*`、主机转移 `sources.transfer*`                                                            | M2（表 M2 再建）                    |
| 注销与导出 `me.delete`、`me.deleteCancel`、`me.export`；通知偏好 `me.notificationPrefs*`                             | M2                                  |
| 审计导出 `audit.export`；付费 `billing.checkout`、`billing.portal`、`billing.invoices`、`billing.webhook`            | M2                                  |
| API 令牌 `apiTokens.*`、`admin.statusPublish`、`admin.abuseList`、`admin.linkFreeze`、`admin.flags*`、`status.get`   | M2                                  |
| `relay.nodePresence`（只给不连 Redis 的 Workers 节点）                                                               | 不实现（Go 节点直写 Redis）         |
| 流量超额的降级与宽限、个人空间的并发上限                                                                             | M2（§10 Q5）                        |
| 组织层、单点登录、域名、区域、Webhook、托管主机                                                                      | M3                                  |

`auth.stepUp` M1 实现但不强制任何过程提权（控制台 M1 用输入确认，见 cloud-console D5）；从 M2 起 `teams.delete`、`teams.transferOwnership` 要求提权（§10 Q7）。

## §2 进程与部署

### 2.1 命令行（`armadra-cloud`）

```text
用法: armadra-cloud <命令> [选项]

命令:
  serve            起控制面（HTTP、作业、Redis 订阅）
  migrate          执行数据库迁移（只加不改）
  keys rotate      轮换签名钥（新钥立即生效，旧钥 1 小时内仍在 JWKS 里）
  nodes token      给中继节点签节点令牌（--node <id> --days <n>，缺省 30）
  operators add    给已有账号加运营范围（--email <地址>）
  operators totp   为运营账号登记两步验证（在终端打印一次 otpauth 地址）
  health           探活（容器健康检查用）
  version          打印版本
```

| 规则     | 内容                                                                                                                                                                                                            |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 退出码   | 0 成功、64 用法错误、1 运行出错；参数优先于环境变量，环境变量优先于缺省（与 `armadra-relay` 相同）                                                                                                              |
| 迁移     | `serve` 不自动迁移：账本落后于二进制答「数据库版本落后，先运行 armadra-cloud migrate」并退出 1；账本比二进制新、未知或损坏同样退出 1（Go 设计 §3.4 的拒绝规则）                                                 |
| 租户引导 | `serve` 启动时在 `pg_advisory_xact_lock` 下检查租户 `cloud`：不存在就建（issuer = 配置）并生成第一把签名钥（主密钥包裹）；存在而 issuer 与配置不同 → 「issuer 与库里的不一致」退出 1；绝不改写已有租户的 issuer |
| 关闭顺序 | 停收新请求 → me-stream 发关闭（1001）→ 等在途请求（10 秒）→ 停作业领取、等当前作业（30 秒）→ 刷 `lastSeen` → 关 Redis → 关 PostgreSQL                                                                           |
| 秘密     | 口令、令牌、主密钥、SMTP 口令都只从 `*_FILE` 或环境变量读，不进日志；CLI 打印的 otpauth 地址与节点令牌只出现在运维终端                                                                                          |

### 2.2 配置（环境变量，前缀 `ARMADRA_CLOUD_`）

| 变量                                                                                          | 缺省                                                                      | 说明                                                                  |
| --------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `ISSUER`                                                                                      | 必填                                                                      | 断言与令牌的 `iss`，`https` 来源                                      |
| `APP_ORIGIN`                                                                                  | 必填                                                                      | 控制台来源；M1 必须等于 `ISSUER`，不等即退出 1（能力设计 §11.6）      |
| `LISTEN`                                                                                      | `0.0.0.0:8100`                                                            | 明文监听，TLS 由前面的反向代理终止                                    |
| `DATABASE_URL`                                                                                | 必填                                                                      | 非回环地址必须 `sslmode=verify-full` 或 `require`，否则退出 1         |
| `REDIS_URL`                                                                                   | 必填                                                                      | 与中继节点共用的那一个 Redis（§2.4），要求 Redis ≥ 7.4（`HPEXPIRE`）  |
| `MASTER_KEY` / `MASTER_KEY_FILE`                                                              | 必填                                                                      | 32 字节 hex；包裹签名私钥、TOTP 种子；HKDF 派生 CSRF 钥（§5.2）       |
| `RELAY_DOMAIN`                                                                                | 必填                                                                      | 源地址 `<sourceId>.src.<RELAY_DOMAIN>`                                |
| `EDGE_URL`                                                                                    | `wss://edge.<RELAY_DOMAIN>/t/v1`                                          | 节点表为空时 `heartbeatHint` 的兜底地址                               |
| `MIN_TEAM_CORE_VERSION`                                                                       | 代码常量 `0.10.0`                                                         | 能签带 `team` 断言的最低 core 版本（契约 §30），§4.6                  |
| `REQUIRE_EMAIL_VERIFIED`                                                                      | `1`                                                                       | 未验证邮箱的门（§4.3）；只在开发环境设 `0`                            |
| `NAME`                                                                                        | 空                                                                        | `platform.info.name`                                                  |
| `WEB_ROOT`                                                                                    | 空                                                                        | Armadra `apps/web` 构建产物目录；空 = 不托管页面（`/` 答 404）        |
| `SMTP_HOST` / `SMTP_PORT` / `SMTP_USERNAME` / `SMTP_PASSWORD_FILE` / `SMTP_FROM` / `SMTP_TLS` | —, 587, —, —, 必填, `starttls`                                            | `SMTP_TLS` 取 `starttls` / `tls` / `none`；`none` 只许回环（mailpit） |
| `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_FILE` / `CHALLENGE_SCOPE`                            | 空 / 空 / `auth.login,auth.register,auth.passwordResetStart,links.accept` | 两者都配才要求挑战；只配一个是配错，退出 1                            |
| `TRUSTED_PROXY` / `TRUSTED_PROXY_PREFER`                                                      | 空 / `forwarded-for`                                                      | CIDR 列表；规则同 Go 设计 §3.5                                        |
| `HIBP_URL`                                                                                    | 空                                                                        | 可选的泄露口令查询（能力设计 §11.8）                                  |
| `JOB_WORKERS`                                                                                 | `2`                                                                       | 作业并发                                                              |
| `METRICS_LISTEN` / `METRICS_TOKEN_FILE`                                                       | 空                                                                        | 同中继                                                                |
| `LOG_LEVEL`                                                                                   | `info`                                                                    | —                                                                     |

`OAUTH_<ID>_*` 等 M2 变量 M1 不解析；出现未知的 `ARMADRA_CLOUD_*` 变量只记一条警告。

### 2.3 单机 M1 拓扑（`deploy/saas/compose.yml`，S9）

| 服务                 | 镜像 / 命令                                        | 说明                                                                |
| -------------------- | -------------------------------------------------- | ------------------------------------------------------------------- |
| `proxy`              | 反向代理                                           | 终止 TLS：`app.`、`edge.`、`*.src.`；WebSocket 设置按 Go 设计 §3.10 |
| `cloud`              | `armadra-cloud serve`（先跑一次 `migrate` 的 job） | `app.<域>` → `cloud:8100`                                           |
| `relay-a`、`relay-b` | `armadra-relay saas serve`（G5）                   | `edge.<域>`、`*.src.<域>` → 两个节点轮询；节点 id 固定为 `a`、`b`   |
| `postgres`           | `postgres:17`                                      | 一个库，cloud 用 schema `public`；节点只写流量表（§10 Q4）          |
| `redis`              | `redis:8`                                          | `maxmemory 128mb`、`maxmemory-policy noeviction`、不持久化          |
| `push-relay`         | Armadra `apps/push-relay`                          | 能力设计 §11.11                                                     |

现有 `deploy/compose.yml` 的 `cloud` 服务（TS 镜像、`SIGNING_KEY_FILE`）在 S9 换成 Go 镜像与本表的变量；签名钥改为库里主密钥包裹（能力设计 §4），不再挂文件。

### 2.4 控制面与中继节点的约定（Redis，规范）

前缀沿用中继的 `relay:`（节点与控制面共读写）；只有控制面自己用的键用 `cloud:`。所有值是 JSON（camelCase），不含任何令牌明文。

| 键 / 频道                       | 写方                   | 读方         | 形状与规则                                                                                                                                                                          |
| ------------------------------- | ---------------------- | ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `relay:route:src:<id>`          | 节点                   | 节点、控制面 | 现有路由（`{node, tunnelId, sinceMs, coreVersion}`，`SET PX`）；控制面只读：在线判定、`coreVersion`、把命令发给持有节点                                                             |
| `relay:presence`（频道）        | 节点                   | 节点、控制面 | 现有 `{kind:"presence", event:{type, sourceId, node, tunnelId, coreVersion?, atMs}}`；控制面据此更新 `sources.last_seen_at_ms` 并发 me-stream 的上下线                              |
| `relay:revoked:<jti>`           | 控制面                 | 节点         | 现有：`SET … PXAT <exp>`，随后 `PUBLISH relay:revoked <jti>`                                                                                                                        |
| `relay:ctl`（频道，广播）       | 控制面                 | 节点         | `{kind:"command", command:{type, sourceId?, subject?, subjectPrefix?, deviceId?, reason}}`；`type` ∈ `kick`、`closeTunnel`、`drain`；`kick` 的过滤字段取「且」，全缺 = 该源全部连接 |
| `relay:node:<id>`（频道）       | 控制面                 | 该节点       | 与 `relay:ctl` 同形；路由已知时发到持有节点，未知时退回广播                                                                                                                         |
| `relay:nodes`（哈希）           | 节点（每 20 秒）       | 控制面       | 字段 = 节点 id，值 `{url, weight, version, draining, connections, tunnels}`；每个字段 `HPEXPIRE 60000`，60 秒没续自然消失                                                           |
| `relay:quota:team:<teamId>`     | 控制面                 | 节点         | `{concurrent}`：`edge.concurrent.team` 的上限；计划变化或团队新建时写，不设 TTL，团队清除时删；缺键时节点用自己的缺省                                                               |
| `relay:incident`                | 控制面                 | 节点         | `{rejectNewConnections, atMs}`；节点据此拒绝新的隧道与 e2e 升级（4429 `limit_reached`），在线连接保留                                                                               |
| `relay:jwks:<sha256(issuer)>`   | 控制面（轮换后）、节点 | 节点         | 共享 JWKS 缓存（Go 设计 §3.6）；`keys rotate` 后控制面直接覆盖                                                                                                                      |
| `cloud:issued:acct:<accountId>` | 控制面                 | 控制面       | 有序集合，成员 `<accountId>\|<sourceId>\|<deviceId>\|<jti>`，分数 = `exp` 毫秒；每次写 `PEXPIRE 3600000`；写前 `ZREMRANGEBYSCORE -inf <now>`（能力设计 §11.7）                      |
| `cloud:issued:src:<sourceId>`   | 控制面                 | 控制面       | 同上；隧道令牌也记在这里（`accountId = -`）                                                                                                                                         |
| `cloud:events`（频道）          | 控制面各实例           | 控制面各实例 | 领域事件 `{event, audience}`，见 §7.1                                                                                                                                               |
| `cloud:ticket:<sha256>`         | 控制面                 | 控制面       | me-stream 票（30 秒、`GETDEL` 一次性），值 `{accountId, deviceId}`                                                                                                                  |
| `cloud:limit:<种类>:<sha256>`   | 控制面                 | 控制面       | 速率限流桶，复用 `internal/control/redis` 的 `Limiter`，只换前缀                                                                                                                    |

- **节点令牌**：`armadra-cloud nodes token --node a` 用租户签名钥签 `{iss, sub:"a", aud:"relay-node", iat, exp, jti, ver}`（缺省 30 天），节点之间 `/x/v1` 升级时出示，持有节点按 JWKS 验签与 `aud`（Go 设计 §3.6）；M1 控制面自己不收节点令牌。
- **节点令牌的撤销**：`jti` 形如 `n.<nodeId>.<随机>`；运营方要作废时写 `relay:revoked:<jti>`（CLI `nodes revoke --jti`，S8）。
- **兼容**：`kick` 的 `subject` / `deviceId` 过滤与 `drain` 命令是 G5 新增；不认识这些字段的节点会按 `sourceId` 踢掉该源全部连接（多踢不漏踢，未撤销的人凭有效令牌重连即可）。

## §3 数据模型

### 3.1 租户与空间

| 项       | 规则                                                                                                                                                |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| 租户     | SaaS 恒为 `tenant_id = 'cloud'`；`tenants.issuer` = `ISSUER`。所有 SaaS 表都带 `tenant_id` 并参与外键与主键，存储层的每条 SQL 都写 `tenant_id = $1` |
| 个人中转 | 仍是 `default`；`--store postgres` 的个人中转会跑到同一套迁移，SaaS 表在它的 schema 里建出来但始终为空                                              |
| 账号 id  | SaaS 账号 `account_id` = UUIDv4 小写；`accounts.name` 也写成这个 uuid（满足现有 `NOT NULL` 与唯一索引，登录按邮箱，不按 `name`）                    |
| 其余 id  | 团队、邀请、授予、信任请求、审计事件：UUIDv4 小写文本；`sourceId` 沿用 32 位 hex                                                                    |
| 空间     | 个人空间 = `(personal, account_id)`，团队空间 = `(team, team_id)`；主机的空间记在 `source_spaces.team_id`（NULL = 个人空间，归 `owner_account_id`） |
| 现有源   | 个人中转的源没有 `source_spaces` 行，语义就是「个人空间、租户唯一账号所有」，不做数据迁移（能力设计 §5 0006 的「迁成 personal」由此满足）           |
| 隔离     | `internal/store/saas` 的每个方法签名里有 `tenantID` 与空间或调用者；跨团队读取只能经 `teamID` 参数，测试覆盖「跨团队读不到」（§8.5）                |

### 3.2 为什么用扩展表而不给 `sources` 加列

现有端口 `SourceStore.Put` 在 PostgreSQL 里是「删了再插」（保持创建顺序，`go/internal/store/postgres/ports.go`），给 `sources` 加的列会在每次重新登记时被清掉，挂在 `sources` 上的 `ON DELETE CASCADE` 外键也会连带删掉授予。所以：

- 主机的空间、所有者、标签、设置放在 1:1 的 `source_spaces`，**不对 `sources` 建外键**；`source_grants` 同理。
- SaaS 的登记在一个事务里写 `sources`（复用 `rows.go` 的插入）与 `source_spaces`（§4.5）。
- `accounts`、`devices` 的端口是原地 `UPDATE` 列名单，新增列不会被核心写坏，可以直接加列。

### 3.3 迁移清单

编号规则：M1 的六个迁移在 S0b 一次落库，合并时取 main 最大号 +1 起连续编号（下表按 main 现在的 0003 写）；进 `migrations.lock`；每个迁移同 PR 带测试；已发布的不改，M1 内要改就再加一个迁移。SQLite 侧没有 SaaS 迁移。

| 编号（暂定） | 文件                     | 内容                                                                                     |
| ------------ | ------------------------ | ---------------------------------------------------------------------------------------- |
| 0004         | `0004_saas_accounts.sql` | `accounts`、`devices` 加列；`account_credentials`、`email_tokens`、`device_codes`        |
| 0005         | `0005_saas_teams.sql`    | `teams`、`team_members`、至少一个 owner 的约束触发器、`groups`、`group_members`（M2 用） |
| 0006         | `0006_saas_hosts.sql`    | `source_spaces`、`registration_tokens.team_id`、`entitlements`                           |
| 0007         | `0007_saas_grants.sql`   | `source_grants`、`invitations`（团队与主机两类）、`trust_requests`                       |
| 0008         | `0008_saas_audit.sql`    | `audit_log`（按月分区）                                                                  |
| 0009         | `0009_saas_jobs.sql`     | `jobs`、`outbox_emails`、`platform_settings`；`relay_traffic_buckets.team_id`            |

M1 不建（M2 起各自加迁移）：`account_recovery_codes`、`access_requests`、`source_transfers`、`plans`、`subscriptions`、`usage_monthly`、`invoices`、`api_tokens`、`feature_flags`、`abuse_reports`。能力设计 §11.8 的 `account_lockouts` 不另建：沿用 `accounts.failures / locked_until_ms / last_failure_at_ms`，与个人中转同一套锁定规则。

### 3.4 DDL（规范）

迁移文件不写事务语句（迁移器整体包事务）；注释用简体中文。

```sql
-- 0004_saas_accounts.sql：SaaS 账号（租户 cloud）。个人中转的行这些列取缺省、不受影响。
ALTER TABLE accounts
  ADD COLUMN email text,
  ADD COLUMN email_verified_at_ms bigint,
  ADD COLUMN display_name text NOT NULL DEFAULT '',
  ADD COLUMN locale text NOT NULL DEFAULT 'zh-CN' CHECK (locale IN ('zh-CN', 'en')),
  ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  ADD COLUMN status_reason text,
  -- 特殊范围（如运营方的 operator）；me.get.scopes。
  ADD COLUMN scopes text[] NOT NULL DEFAULT '{}';

CREATE UNIQUE INDEX accounts_email ON accounts (tenant_id, lower(email)) WHERE email IS NOT NULL;

-- 会话种类：bearer（原生、core、CLI）/ cookie（控制台）/ operator（运营 Cookie，1 小时绝对寿命）。
ALTER TABLE devices
  ADD COLUMN kind text NOT NULL DEFAULT 'bearer' CHECK (kind IN ('bearer', 'cookie', 'operator')),
  ADD COLUMN elevated_until_ms bigint;

CREATE TABLE account_credentials (
  tenant_id text NOT NULL,
  credential_id text NOT NULL,
  account_id text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('totp', 'passkey', 'oauth')),
  label text NOT NULL DEFAULT '',
  -- 秘密（TOTP 种子等）用主密钥包裹，形如 aes256gcm-v1:<base64>；明文从不落库。
  secret_wrap text,
  public_json jsonb,
  created_at_ms bigint NOT NULL,
  last_used_at_ms bigint,
  PRIMARY KEY (tenant_id, credential_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX account_credentials_account ON account_credentials (tenant_id, account_id, kind);

-- 邮件里的一次性令牌：只存哈希；verify 24 小时，reset 30 分钟，change 24 小时。
CREATE TABLE email_tokens (
  tenant_id text NOT NULL,
  token_hash text NOT NULL,
  account_id text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('verify', 'reset', 'change')),
  -- 发往的地址；change 是新地址，确认时据此改邮箱。
  email text NOT NULL,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  consumed_at_ms bigint,
  PRIMARY KEY (tenant_id, token_hash),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX email_tokens_account ON email_tokens (tenant_id, account_id, purpose);

-- 设备码（能力设计 §11.5）：pending → approved → consumed，或 denied / expired。
CREATE TABLE device_codes (
  tenant_id text NOT NULL,
  device_code_hash text NOT NULL,
  user_code text NOT NULL CHECK (user_code ~ '^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$'),
  status text NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'consumed', 'expired')),
  platform text NOT NULL,
  name text NOT NULL,
  push_public_key text,
  account_id text,
  ip_prefix text,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  last_poll_at_ms bigint,
  decided_at_ms bigint,
  PRIMARY KEY (tenant_id, device_code_hash),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX device_codes_user_code ON device_codes (tenant_id, user_code)
  WHERE status IN ('pending', 'approved');
CREATE INDEX device_codes_expires ON device_codes (expires_at_ms);
```

```sql
-- 0005_saas_teams.sql：团队是租户（能力设计 §1）。分组的表 M1 建好、M2 起使用。
CREATE TABLE teams (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  team_id text NOT NULL,
  slug text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
  avatar_url text,
  -- 企业组织层预留（能力设计 §1.4），M1 恒为 NULL。
  organization_id text,
  -- teamSettings 的全部字段（缺省见 §4.4），按协议包形状存。
  settings jsonb NOT NULL,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
  status_reason text,
  created_by text NOT NULL,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  deleted_at_ms bigint,
  purge_at_ms bigint,
  PRIMARY KEY (tenant_id, team_id),
  CHECK ((deleted_at_ms IS NULL) = (purge_at_ms IS NULL))
);

-- slug 在恢复期内仍占用，清除后释放。
CREATE UNIQUE INDEX teams_slug ON teams (tenant_id, slug);
CREATE INDEX teams_purge ON teams (purge_at_ms) WHERE purge_at_ms IS NOT NULL;

CREATE TABLE team_members (
  tenant_id text NOT NULL,
  team_id text NOT NULL,
  account_id text NOT NULL,
  role text NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'billing', 'guest')),
  joined_at_ms bigint NOT NULL,
  invited_by text,
  PRIMARY KEY (tenant_id, team_id, account_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX team_members_account ON team_members (tenant_id, account_id);

-- 每个团队至少一个 owner：事务提交时检查（转移所有权可以在一个事务里先升后降）。
-- 服务层先查并答 409 team_last_owner；这里是兜底，违反时 SQLSTATE 23514、约束名 team_has_owner。
CREATE FUNCTION team_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  t_tenant text;
  t_team text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    t_tenant := OLD.tenant_id;
    t_team := OLD.team_id;
  ELSE
    t_tenant := NEW.tenant_id;
    t_team := NEW.team_id;
  END IF;
  IF EXISTS (SELECT 1 FROM teams WHERE tenant_id = t_tenant AND team_id = t_team)
     AND NOT EXISTS (SELECT 1 FROM team_members
                     WHERE tenant_id = t_tenant AND team_id = t_team AND role = 'owner') THEN
    RAISE EXCEPTION 'team has no owner' USING ERRCODE = '23514', CONSTRAINT = 'team_has_owner';
  END IF;
  RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER team_members_owner AFTER UPDATE OR DELETE ON team_members
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION team_owner_guard();
CREATE CONSTRAINT TRIGGER teams_owner AFTER INSERT ON teams
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION team_owner_guard();

CREATE TABLE groups (
  tenant_id text NOT NULL,
  group_id text NOT NULL,
  team_id text NOT NULL,
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
  created_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, group_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX groups_name ON groups (tenant_id, team_id, name);

-- 组员必须是团队成员（guest 不行，由服务层判）；成员离队即出组。
CREATE TABLE group_members (
  tenant_id text NOT NULL,
  group_id text NOT NULL,
  team_id text NOT NULL,
  account_id text NOT NULL,
  is_lead boolean NOT NULL DEFAULT false,
  added_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, group_id, account_id),
  FOREIGN KEY (tenant_id, group_id) REFERENCES groups (tenant_id, group_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, team_id, account_id)
    REFERENCES team_members (tenant_id, team_id, account_id) ON DELETE CASCADE
);
```

```sql
-- 0006_saas_hosts.sql：主机的空间与配额。不对 sources 建外键（Put 是删了再插，见设计 §3.2）。
CREATE TABLE source_spaces (
  tenant_id text NOT NULL,
  source_id text NOT NULL,
  -- NULL = 个人空间（归 owner_account_id）。
  team_id text,
  -- 登记它的账号；离队后改成团队 owner（能力设计 §1.2）。
  owner_account_id text NOT NULL,
  tags text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 16),
  -- hostSettings；NULL = 缺省（§4.5）。
  settings jsonb,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, source_id),
  FOREIGN KEY (tenant_id, team_id) REFERENCES teams (tenant_id, team_id),
  FOREIGN KEY (tenant_id, owner_account_id) REFERENCES accounts (tenant_id, account_id)
);

CREATE INDEX source_spaces_team ON source_spaces (tenant_id, team_id) WHERE team_id IS NOT NULL;
CREATE INDEX source_spaces_owner ON source_spaces (tenant_id, owner_account_id);
CREATE INDEX source_spaces_tags ON source_spaces USING gin (tags);

-- 注册令牌带空间：NULL = 个人空间（个人中转的令牌照旧）。
ALTER TABLE registration_tokens ADD COLUMN team_id text;

-- 空间的计划（M1 只有 Free；没有行 = free / active）。额度表在代码里（§4.10），overrides 给运营方单独放宽。
CREATE TABLE entitlements (
  tenant_id text NOT NULL,
  space_kind text NOT NULL CHECK (space_kind IN ('personal', 'team')),
  -- 个人空间是 account_id，团队空间是 team_id。
  space_id text NOT NULL,
  plan text NOT NULL DEFAULT 'free',
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'trialing', 'past_due', 'canceled')),
  overrides jsonb NOT NULL DEFAULT '{}',
  period_start_ms bigint,
  updated_at_ms bigint NOT NULL,
  updated_by text,
  PRIMARY KEY (tenant_id, space_kind, space_id)
);
```

```sql
-- 0007_saas_grants.sql：授予、邀请（团队与主机两类）、新设备待信任。有效权限按查询合成，不落库。
CREATE TABLE source_grants (
  tenant_id text NOT NULL,
  grant_id text NOT NULL,
  source_id text NOT NULL,
  -- 主机属于团队时是该团队；个人空间的主机为 NULL。
  team_id text,
  subject_kind text NOT NULL CHECK (subject_kind IN ('team', 'member', 'group', 'guest')),
  -- team → team_id；member / guest → account_id；group → group_id。
  subject_id text NOT NULL,
  role text NOT NULL CHECK (role IN ('viewer', 'editor', 'operator', 'driver')),
  -- 空 = 整台主机。
  workspace_ids text[] NOT NULL DEFAULT '{}' CHECK (cardinality(workspace_ids) <= 64),
  expires_at_ms bigint,
  granted_by text NOT NULL,
  -- 来路：grant（grants.create）或 invitation（主机邀请被接受）。
  origin text NOT NULL DEFAULT 'grant' CHECK (origin IN ('grant', 'invitation')),
  invitation_id text,
  created_at_ms bigint NOT NULL,
  updated_at_ms bigint NOT NULL,
  revoked_at_ms bigint,
  revoked_by text,
  PRIMARY KEY (tenant_id, grant_id)
);

-- 同一主机对同一主体只有一条有效授予（重复建答 409 conflict）。
CREATE UNIQUE INDEX source_grants_active ON source_grants (tenant_id, source_id, subject_kind, subject_id)
  WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_subject ON source_grants (tenant_id, subject_kind, subject_id)
  WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_team ON source_grants (tenant_id, team_id) WHERE revoked_at_ms IS NULL;
CREATE INDEX source_grants_expiry ON source_grants (expires_at_ms)
  WHERE revoked_at_ms IS NULL AND expires_at_ms IS NOT NULL;

CREATE TABLE invitations (
  tenant_id text NOT NULL,
  invitation_id text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('team', 'host')),
  -- 规范化为小写。
  email text NOT NULL,
  -- team：必有；host：主机属于团队时有。
  team_id text,
  team_role text CHECK (team_role IN ('admin', 'member', 'billing', 'guest')),
  source_id text,
  host_role text CHECK (host_role IN ('viewer', 'editor', 'operator', 'driver')),
  workspace_ids text[] NOT NULL DEFAULT '{}' CHECK (cardinality(workspace_ids) <= 64),
  -- 邮件作业发信时生成令牌、只存哈希；NULL = 还没发出。resend 换新哈希。
  token_hash text,
  state text NOT NULL CHECK (state IN ('pending', 'accepted', 'declined', 'revoked', 'expired')),
  invited_by text NOT NULL,
  decided_by text,
  created_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  decided_at_ms bigint,
  sent_at_ms bigint,
  resend_count integer NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, invitation_id),
  CHECK (kind <> 'team' OR (team_id IS NOT NULL AND team_role IS NOT NULL AND source_id IS NULL)),
  CHECK (kind <> 'host' OR (source_id IS NOT NULL AND host_role IS NOT NULL AND team_role IS NULL))
);

CREATE UNIQUE INDEX invitations_pending_team ON invitations (tenant_id, team_id, email)
  WHERE kind = 'team' AND state = 'pending';
CREATE UNIQUE INDEX invitations_pending_host ON invitations (tenant_id, source_id, email)
  WHERE kind = 'host' AND state = 'pending';
CREATE INDEX invitations_received ON invitations (tenant_id, email) WHERE state = 'pending';
CREATE INDEX invitations_team ON invitations (tenant_id, team_id, created_at_ms DESC);
CREATE UNIQUE INDEX invitations_token ON invitations (tenant_id, token_hash) WHERE token_hash IS NOT NULL;

-- 新设备待信任（能力设计 §11.4）：云端只存通知所需的元数据，没有校验码。
CREATE TABLE trust_requests (
  tenant_id text NOT NULL,
  request_id text NOT NULL,
  source_id text NOT NULL,
  team_id text,
  account_id text NOT NULL,
  -- 断言里的 device.deviceId（云端设备）。
  device_id text NOT NULL,
  device_name text NOT NULL,
  platform text NOT NULL,
  state text NOT NULL CHECK (state IN ('pending', 'trusted', 'rejected', 'expired')),
  requested_at_ms bigint NOT NULL,
  expires_at_ms bigint NOT NULL,
  resolved_at_ms bigint,
  resolved_by text,
  PRIMARY KEY (tenant_id, request_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX trust_requests_pending ON trust_requests (tenant_id, source_id, device_id)
  WHERE state = 'pending';
CREATE INDEX trust_requests_team ON trust_requests (tenant_id, team_id, state);
```

```sql
-- 0008_saas_audit.sql：团队审计与账号安全日志（一张表）。只记主体、动作、对象、时间、jti、IP 前缀。
-- 按月分区：分区由作业 audit.partitions 提前两个月建（CREATE TABLE IF NOT EXISTS … PARTITION OF），
-- 保留期按计划逐团队删；超过最长保留期（13 个月）的整月分区直接 DROP。
CREATE TABLE audit_log (
  at_ms bigint NOT NULL,
  event_id text NOT NULL,
  tenant_id text NOT NULL,
  team_id text,
  -- 安全日志的本人（action 以 security. 开头）。
  account_id text,
  actor_kind text NOT NULL CHECK (actor_kind IN ('account', 'api_token', 'system', 'operator')),
  actor_id text,
  action text NOT NULL CHECK (char_length(action) BETWEEN 1 AND 64),
  target_kind text,
  target_id text,
  jti text,
  ip_prefix text,
  PRIMARY KEY (at_ms, event_id)
) PARTITION BY RANGE (at_ms);

-- 兜底分区：作业漏建时也能写；作业发现它非空就记错误日志。
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;

CREATE INDEX audit_log_team ON audit_log (tenant_id, team_id, at_ms DESC) WHERE team_id IS NOT NULL;
CREATE INDEX audit_log_account ON audit_log (tenant_id, account_id, at_ms DESC) WHERE account_id IS NOT NULL;
CREATE INDEX audit_log_target ON audit_log (tenant_id, target_kind, target_id, at_ms DESC);
```

```sql
-- 0009_saas_jobs.sql：作业队列（SKIP LOCKED）、邮件发件箱、平台设置；流量桶加团队维度。
CREATE TABLE jobs (
  job_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id text NOT NULL,
  kind text NOT NULL,
  -- 只有标识与参数，不含令牌、口令与内容。
  payload jsonb NOT NULL,
  -- 同一 kind 下未完成的作业按它去重。
  dedupe_key text,
  run_at_ms bigint NOT NULL,
  attempts integer NOT NULL DEFAULT 0,
  max_attempts integer NOT NULL DEFAULT 8,
  locked_by text,
  locked_until_ms bigint,
  -- 只记错误类名或错误码。
  last_error text,
  created_at_ms bigint NOT NULL,
  done_at_ms bigint,
  failed_at_ms bigint
);

CREATE INDEX jobs_due ON jobs (run_at_ms) WHERE done_at_ms IS NULL AND failed_at_ms IS NULL;
CREATE UNIQUE INDEX jobs_dedupe ON jobs (tenant_id, kind, dedupe_key)
  WHERE dedupe_key IS NOT NULL AND done_at_ms IS NULL AND failed_at_ms IS NULL;

-- 发件箱：业务事务里插一行 + 一个 mail.send 作业；发信成功记 sent_at_ms，重试前先看它（幂等）。
CREATE TABLE outbox_emails (
  tenant_id text NOT NULL,
  email_id text NOT NULL,
  job_id bigint REFERENCES jobs (job_id),
  template text NOT NULL,
  locale text NOT NULL CHECK (locale IN ('zh-CN', 'en')),
  to_email text NOT NULL,
  -- 渲染参数：标识与显示名；需要一次性令牌的模板在发信时现生成（§6.3），这里没有令牌。
  params jsonb NOT NULL,
  message_id text NOT NULL,
  created_at_ms bigint NOT NULL,
  sent_at_ms bigint,
  PRIMARY KEY (tenant_id, email_id)
);

CREATE INDEX outbox_emails_unsent ON outbox_emails (created_at_ms) WHERE sent_at_ms IS NULL;

-- 平台级开关（事故开关等），运营后台写；键见设计 §4.11。
CREATE TABLE platform_settings (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  key text NOT NULL,
  value jsonb NOT NULL,
  updated_at_ms bigint NOT NULL,
  updated_by text,
  PRIMARY KEY (tenant_id, key)
);

-- 流量按团队计（能力设计 §11.14）：入口节点写中继令牌的 team；个人空间为空串。
-- 不进主键：同一小时同一主机换团队只在转移（M2）时发生，按首次写入的团队计。
ALTER TABLE relay_traffic_buckets ADD COLUMN team_id text NOT NULL DEFAULT '';
CREATE INDEX relay_traffic_team ON relay_traffic_buckets (tenant_id, team_id, bucket_start_ms)
  WHERE team_id <> '';
```

### 3.5 迁移测试（S0b 必带）

| 测试                                                                                                                       | 断言                                    |
| -------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| 空库连续跑 0001–0009，再对 0003 的库（`go/testdata/compat` 的 PG 转储）跑 0004–0009                                        | 账本连续；老行取缺省；TS 版存储仍能读写 |
| `team_owner_guard`：删唯一 owner、把唯一 owner 降级、建团队不插 owner 都在提交时失败；同一事务先升后降成功；删团队级联成功 | SQLSTATE 23514、约束名 `team_has_owner` |
| 各 CHECK 与部分唯一索引（待处理邀请、有效授予、`user_code`）                                                               | 违反即拒                                |
| `audit_log` 建月分区后写入落到分区、默认分区为空；`DROP` 旧分区不影响其他月                                                | —                                       |
| `devices.kind` / `accounts.*` 新列不被现有端口的 `UPDATE` 改写（端口合同 15 例照旧过）                                     | —                                       |

## §4 Go 包设计

### 4.1 目录与边界

```text
go/cmd/armadra-cloud/        命令行（§2.1）
go/internal/store/saas/      SaaS 端口与记录（无驱动）
go/internal/store/postgres/  现有实现 + saas_*.go（实现 saas.Store，复用连接池、事务与 rows.go）
go/internal/cloud/
  config/      环境变量解析（§2.2）
  server/      装配：存储、Redis、core.Context、各服务、HTTP、作业；serve 的生命周期
  httpx/       Cookie 会话、CSRF、Origin、CORS、限流与挑战、客户端 IP、错误映射（§5）
  accounts/    注册、登录、会话种类、邮箱验证、找回、改邮箱、提权、设备码、me.*
  teams/       团队、成员、设置、删除与恢复、所有权转移
  invitations/ 团队与主机邀请
  hosts/       登记到空间、目录、改名与标签、设置、撤销、断言合成（policy 的 SaaS 实现）
  grants/      授予与有效权限
  links/       空间维度的链接（包一层 core.Links）
  trust/       待信任请求
  audit/       写审计、安全日志、查询、保留期
  quota/       计划额度、用量统计、limit_reached
  mail/        SMTP 适配器、模板、发件
  jobs/        作业队列、调度、各类作业的注册表
  admin/       运营后台、事故开关、运营 TOTP
  events/      领域事件、受众、Redis 广播、me-stream 过滤
  relayctl/    与中继节点的约定（§2.4）：签发索引、撤销、踢人、关隧道、节点表、额度、在线事件
```

- `repo.rules.json` 的 `goModules.internalBoundaries` 加：`internal/cloud/**` 不 import `internal/tunnel`、`internal/edge`、`internal/netpoll`、`internal/dataplane`、`internal/personal`；`internal/core`、`internal/store/**` 不 import `internal/cloud/**`。
- 新依赖只有 G3a 已登记的 `github.com/redis/go-redis/v9`；SMTP 用标准库 `net/smtp` + `crypto/tls`，TOTP 用标准库 HMAC，模板用 `text/template` / `html/template`。

### 4.2 `internal/core` 的扩展（只加接口，personal 行为不变）

| 扩展           | 形状                                                                                                                                                                                                              | personal 实现                       | SaaS 实现                               |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- | --------------------------------------- |
| 访问策略       | `Context.Policy core.AccessPolicy`（下方代码）；`Sources.List/Get/Assertion`、`Links.Create/List/Update/Revoke`、`MeStream.sees` 都改为经它                                                                       | 现有逻辑原样搬进 `personalPolicy`   | `hosts.Policy`（§4.6）                  |
| 源地址         | `Context.RelayOrigin func(sourceID) string`、`Context.RelayBaseURL func(sourceID) string`；`TunnelURL` 改成 `TunnelNodes func(ctx, sourceID) []TunnelNode`                                                        | issuer、`issuer/s/<id>`、单节点     | `https://<id>.src.<域>`、节点表（§7.3） |
| 会话种类与传输 | `store.DeviceRecord` 加 `Kind`、`ElevatedUntilMs`（`omitempty`，PG 读写，SQLite 恒空）；`Authenticator.AuthenticateToken(ctx, token, Transport)`；`Caller` 加 `Kind`、`Transport`、`AccountID`、`ElevatedUntilMs` | Kind 恒为 bearer                    | §5.1 的绑定规则                         |
| 刷新寿命       | `Refresh` 对 `Kind = operator` 不延长 `ExpiresAtMs`（1 小时绝对寿命）                                                                                                                                             | 不涉及                              | —                                       |
| 登录拆分       | 导出 `core.VerifyPassword(ctx, c, account *store.AccountRecord, password, nowMs)`：锁定、失败计数、未知账号等时派生，供按邮箱登录复用                                                                             | `Accounts.Login` 改为调用它         | `accounts.Login`                        |
| 设备生成       | 导出 `core.NewDevice(subject, input, kind, nowMs, ttl) (record, refreshToken)`（纯函数），持久化由调用方                                                                                                          | `CreateDevice` 改为调用它           | 在事务里插入                            |
| 错误详情       | `core.Error.Details map[string]any`；`FailWith(code, details)`；`WriteError` 输出 `details`（与 `retryAfterMs` 合并）                                                                                             | 不变                                | `limit_reached`、`team_policy_denied`   |
| 事件受众       | `core.Audience` 加 `Accounts []string`、`Team *TeamAudience{TeamID, Roles}`；`EventBus` 接口化，SaaS 换成 Redis 广播                                                                                              | 只用 `LinkID`                       | §7.1                                    |
| 撤销钩子       | `Context.Revocations core.RevocationSink`：设备撤销、登出、改口令、链接撤销、源撤销后回调（参数是受影响的账号 / 设备 / 源 / 链接）                                                                                | nil（personal 靠 jti 形状即时判定） | `relayctl`（§7.2）                      |
| 断言附加       | `GrantExtra` 加 `Team *gen.AssertionClaimsTeam`、`Role string`、`Email`、`EmailVerified`；中继令牌与隧道令牌加 `team`；`SignOptions.JTIPrefix` 给断言用 `s.<deviceId>.<sourceId>`                                 | 都不带                              | §4.6                                    |
| 登记拆分       | 导出纯函数 `core.BuildSourceRecord(existing, input, nowMs)`（`source_taken`、协议版本与字段合并），`Sources.Register` 改为调用它                                                                                  | 原逻辑                              | `hosts` 在自己的事务里写                |
| 签名钥包裹     | `store.SigningKeyRecord` 的 `private_key_wrap = aes256gcm-v1` 由 `KeyStore` 在读时解包（主密钥经 `postgres.Open` 的选项传入）                                                                                     | `none`                              | 主密钥                                  |

```go
// AccessPolicy 决定谁能看到、管理、访问哪台主机（core 不知道团队）。
type AccessPolicy interface {
	// Visible 答调用方能看到的主机与来路（owner / team / member / guest / link），按 filter 分页。
	Visible(ctx context.Context, caller Caller, filter SourceFilter) (page []VisibleSource, next string, err error)
	// Intent 答签断言要带的意图；没有访问权答 source_access_denied。
	Intent(ctx context.Context, caller Caller, source store.SourceRecord) (GrantExtra, error)
	// CanManage 判定 sources.update/revoke、links.*、grants.* 的管理权（owner 或团队 admin）。
	CanManage(ctx context.Context, caller Caller, sourceID string, action ManageAction) error
}
```

### 4.3 账号（`cloud/accounts`）

| 过程                                 | 规则                                                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auth.register`                      | 次序：按 IP 限流 → 事故开关 `registrationsPaused`（`503 registrations_paused`）→ 挑战 → 口令策略（四条 + 可选 HIBP，`names` = 邮箱本地部分与显示名）→ 事务：邮箱唯一（`409 email_taken`，按 `lower(email)`）、插账号、建 `cookie` 或 `bearer` 设备（按传输，§5.1）、插 `mail.verify` 发件、写安全日志 `security.register` → 答会话（Cookie 传输时下发 Cookie 与 `csrfToken`） |
| `auth.login`                         | `account` 字段是邮箱；不存在走等时派生；`status = disabled` 答 `403 account_disabled`（在口令比对之后，避免枚举）；`scope = operator` 见 §4.9；新设备（同平台同名 30 天内没出现过）插 `mail.newDevice`                                                                                                                                                                        |
| `auth.refresh` / `auth.logout`       | 核心逻辑原样；Cookie 传输的凭据取自刷新 Cookie（§5.1）；`scope = operator` 只作用于运营会话                                                                                                                                                                                                                                                                                   |
| `auth.changePassword`                | 核心逻辑原样（撤销本人其他设备，经撤销钩子写 Redis 撤销）+ 安全日志 + `mail.passwordChanged`                                                                                                                                                                                                                                                                                  |
| `auth.stepUp`                        | 按设备限流（10 分钟 10 次）→ 口令（运营会话另认 `totpCode`）→ `devices.elevated_until_ms = now + 10 分钟`；M1 不要求任何过程提权                                                                                                                                                                                                                                              |
| `auth.emailVerify`                   | 令牌哈希命中、未消耗、未过期（过期 `410 email_token_expired`，其余 `400 email_token_invalid`）；`verify` 置 `email_verified_at_ms`；`change` 改 `email`（新地址再查唯一）并置已验证、给旧地址发 `mail.emailChanged`；同一账号同目的的其余令牌一并作废                                                                                                                         |
| `auth.emailVerifyResend`             | 已验证答 `409 conflict`；按账号每小时 3 次                                                                                                                                                                                                                                                                                                                                    |
| `auth.passwordResetStart` / `Finish` | Start 总答成功（不泄露是否注册），存在且未停用才插 `mail.passwordReset`；Finish 过口令策略 → 换口令、清锁、撤销全部设备（经撤销钩子）→ 不答会话                                                                                                                                                                                                                               |
| `me.get`                             | `account`、`teams`（`team_members` ⋈ `teams`，排除已删除）、`scopes`、Cookie 传输时 `csrfToken`                                                                                                                                                                                                                                                                               |
| `me.update`                          | `displayName`、`locale`、`avatarUrl`（按协议包形状）                                                                                                                                                                                                                                                                                                                          |
| `me.emailChange`                     | 口令比对（计入锁定）→ 新地址未被占用 → 插 `change` 令牌的发件；验证前旧邮箱照常生效                                                                                                                                                                                                                                                                                           |
| `me.devices` / `me.deviceRevoke`     | 核心逻辑原样 + 撤销钩子                                                                                                                                                                                                                                                                                                                                                       |
| `me.spaces`                          | 个人空间（计划取 `entitlements`）+ 所属团队与角色、计划                                                                                                                                                                                                                                                                                                                       |

**未验证邮箱的门**（`REQUIRE_EMAIL_VERIFIED=1`）答 `403 email_unverified`：`teams.create`、`sources.registrationToken`、`links.create`、`invitations.create`、`invitations.accept`、`grants.create`（主体账号未验证）、`trust.reject`。登录、`me.*`、`sources.list` 不受限。

**设备码**（S3，能力设计 §11.5）：

| 步骤                           | 规则                                                                                                                                                                                                                                         |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `deviceStart`                  | 按 IP 每小时 30 次；生成 `deviceCode`（32 字节随机，只存哈希）与 `userCode`（8 位，字母表 `BCDFGHJKLMNPQRSTVWXZ`，`XXXX-XXXX`，与待处理的冲突就重抽）；10 分钟；`intervalMs = 5000`；`verificationUrl = <APP_ORIGIN>/device?code=<userCode>` |
| `devicePoll`                   | 距上次轮询不足 `intervalMs` 答 `429 rate_limited`；`approved` 时在事务里建 `bearer` 设备、状态改 `consumed`、答会话（只此一次）；`denied` / `expired` 原样答                                                                                 |
| `deviceGet`                    | 要会话；按 IP 每分钟 30 次（防猜码）；答发起设备的平台、名字与时间                                                                                                                                                                           |
| `deviceApprove` / `deviceDeny` | 要会话；只对 `pending` 生效（否则 `410 device_code_expired` / `404 not_found`）；写安全日志 `security.deviceApprove`                                                                                                                         |

### 4.4 团队与邀请（`cloud/teams`、`cloud/invitations`）

**团队设置缺省**（`teams.create` 写入）：`membersCanRegisterHosts: true`、`defaultHostRole: "viewer"`、`allowGuests: true`、`allowAnonymousLinks: false`（能力设计 Q2）、`deviceEndorsers: "owner_admin"`、`memberSelfEndorse: true`、`requireMfa: false`、`sessionMaxAgeMs: null`、`allowedEmailDomains: []`。M2 的三项 M1 可读可写、不生效。

| 过程                           | 规则（全部在一个事务里，先 `SELECT … FROM teams … FOR UPDATE` 锁团队行）                                                                                                                                                  |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 角色检查                       | 公共函数 `teams.Require(ctx, tx, caller, teamID, min Role)`：不是成员答 `404 not_found`（不泄露存在），角色不够 `403 team_role_insufficient`，已删除 `410 team_deleted`（`teams.restore` 例外），停用只挡签断言与登记主机 |
| `teams.create`                 | 邮箱已验证 → 配额「可建团队数」（锁账号行）→ slug 唯一（`409 team_slug_taken`）→ 插团队、owner 成员、`entitlements`（free）→ 写 `relay:quota:team:<id>`（提交后）→ 审计 `team.create`                                     |
| `teams.memberRole`             | admin 不能改 owner、不能把人升成 owner（只经转移）；降最后一个 owner 答 `409 team_last_owner`；降为 `billing` / `guest` 或降 admin 都走撤销流程（§4.8）                                                                   |
| `teams.memberRemove` / `leave` | 最后一个 owner 不能；删成员（分组成员级联）、撤销以此人为主体的授予、此人登记的主机 owner 改成团队 owner（取 `joined_at_ms` 最早的 owner）、撤销流程                                                                      |
| `teams.transferOwnership`      | 目标必须是 admin（否则 `400 bad_request`）；同一事务里目标升 owner、调用者降 admin                                                                                                                                        |
| `teams.delete`                 | 有在线主机（`relay:route:src:*` 命中任一团队主机）答 `409 conflict`；置 `deleted_at_ms`、`purge_at_ms = +30 天`；撤销团队全部已签令牌；插 `team.purge` 作业（`run_at = purge_at_ms`，`dedupe_key = teamId`）              |
| `teams.restore`                | 只在恢复期内、只 owner；清两个时间，删 `team.purge` 作业                                                                                                                                                                  |
| 清除作业 `team.purge`          | 撤销团队主机（`sources.revoked_at_ms`）、删授予、邀请、链接、待信任、`source_spaces`、`entitlements`，最后删团队行（成员级联）；审计行保留（无外键）                                                                      |
| `teams.settingsUpdate`         | 按协议包 schema；关 `allowGuests` 不自动移除已有 guest（M1；§10 Q8）                                                                                                                                                      |

**邀请**（两类共用一套过程，契约 §22）：

| 步骤     | 规则                                                                                                                                                                                                                                                              |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `create` | team：`team:admin`；角色 `guest` 而团队关了 guest → `403 guests_disabled`；已是成员 `409 member_exists`；同邮箱有待处理的 `409 conflict`；配额（成员数或 guest 数，**待处理邀请也计入**）。host：主机管理权（§4.6）+ 主机设置 `allowGuests`；按邀请人每小时 50 次 |
| 发信     | 事务里插邀请（`token_hash = NULL`）与 `mail.invitation` 发件；作业发信时生成 32 字节令牌、写哈希与 `sent_at_ms`、渲染链接 `<APP_ORIGIN>/invite/<invitationId>?token=<令牌>`                                                                                       |
| `get`    | 匿名；按 IP 每分钟 60 次；令牌哈希必须对上（否则 `404 not_found`）；答遮掩后的摘要                                                                                                                                                                                |
| `accept` | 要会话、邮箱已验证、`lower(账号邮箱) = 邀请邮箱`（`403 invitation_email_mismatch`）；过期 `410 invitation_expired`、撤销 `410 invitation_revoked`；已接受的再接受答原结果。team：插成员（再查配额与 `member_exists`）。host：§4.7                                 |
| `resend` | 换新令牌（作业里生成）、`expires_at = now + 7 天`、`resend_count + 1`；按邀请每小时 5 次                                                                                                                                                                          |
| 过期     | 读时把 `pending` 且过期的按 `expired` 答；`invitations.expire` 作业每小时落库并发 `invitationChanged`                                                                                                                                                             |

### 4.5 主机（`cloud/hosts`）

| 过程                        | 规则                                                                                                                                                                                                                                                                                                 |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sources.registrationToken` | 邮箱已验证；`space = team` 时：成员、团队未停用（`403 team_suspended`）未删除（`410 team_deleted`）、`member` 角色看 `membersCanRegisterHosts`（`403 host_registration_disabled`）、`billing` / `guest` 答 `403 team_role_insufficient`；配额（空间主机数）；令牌写 `team_id`                        |
| `sources.register`          | 一个事务：`Consume` 注册令牌（带出账号与 `team_id`）→ 团队主机且 `coreVersion < MIN_TEAM_CORE_VERSION` 答 `426 protocol_unsupported`（不让老 core 进团队，§4.6）→ 锁空间行再查配额 → 写 `sources`（复用 `core.BuildSourceRecord` 纯函数与 `rows.go`）→ upsert `source_spaces` → 审计 `host.register` |
| `sources.heartbeatHint`     | 核心逻辑；节点取 §7.3；隧道令牌带 `team`；隧道令牌 `jti` 记进 `cloud:issued:src`                                                                                                                                                                                                                     |
| `sources.list`              | 经 `AccessPolicy.Visible`：`space` 缺省 = 全部可达空间；`status`、`tag`（多个取「且」）、`q`（名称 `ILIKE`，转义 `%_`）、`cursor`（`(registered_at_ms, source_id)` 的不透明编码）、`limit` 1–200；摘要多 `ownerName`                                                                                 |
| `sources.update`            | 名称、标签（协议包 `hostTagsSchema`）、主机设置；管理权                                                                                                                                                                                                                                              |
| `sources.revoke`            | 管理权；置 `revoked_at_ms`、撤销该源全部已签令牌、`closeTunnel` + `kick`、`sourceRevoked` 事件、审计（能力设计 §11.7）                                                                                                                                                                               |
| `sources.accessList`        | 主机「谁能访问」：owner + 每条有效授予展开到人（team 授予展开成当前成员），行带 `grantId`、`role`、`workspaceIds`、`expiresAtMs`                                                                                                                                                                     |
| `sources.accessRevoke`      | 撤销以此账号为主体（member / guest）的授予；只来自 team 授予的人答 `409 conflict`（要改团队授予或移除成员）                                                                                                                                                                                          |

**主机设置缺省**：`linkCreators: "owner_admin"`、`allowGuests: true`、`defaultRole: "viewer"`。

### 4.6 授予、有效权限与断言合成（`cloud/grants`、`cloud/hosts`）

**谁能管**：主机 owner；团队主机另加团队 owner / admin。member 只能管自己是 owner 的主机。

**主体的合法性**：

| 主机空间 | `team`            | `member`                                  | `guest`                                                                  | `group`                            |
| -------- | ----------------- | ----------------------------------------- | ------------------------------------------------------------------------ | ---------------------------------- |
| 团队     | 只能是本团队      | 本团队成员，角色 ∈ owner / admin / member | 本团队 `guest` 角色的成员；团队 `allowGuests` 关时 `403 guests_disabled` | M2：`403 plan_feature_unavailable` |
| 个人     | `400 bad_request` | `400 bad_request`                         | 任何已验证邮箱的账号（主机设置 `allowGuests`）                           | 同上                               |

**有效权限**（`grants.Effective(ctx, tx, accountID, sourceID, nowMs)`，纯查询 + 纯函数合成，不落库）：

1. 账号 `status = disabled` 或主机已撤销 → 无。
2. 是 `owner_account_id` → 来源 `owner`，角色 `driver`，整台。
3. 团队主机：必须是该团队成员，团队未删除；`team` 授予只给角色 owner / admin / member（billing、guest 不受团队全员授予）；`member` / `guest` 授予给主体本人。
4. 忽略已撤销与 `expires_at_ms ≤ now` 的授予。
5. 合成：角色取最高（`viewer < editor < operator < driver`）；范围取并集，任一来源为整台则整台（能力设计 §3.5 的规范语义：高角色窄范围 + 低角色整台 = 高角色整台）；`expiresAtMs` 取各来源中最早的非空到期。
6. `grants.effective` 答合成结果与 `from[]`。

**断言合成**（`sources.assertion`，`hosts.Policy.Intent`）：

| 步骤 | 规则                                                                                                                                                                                                                                                                                              |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1    | 事故开关 `assertionsPaused` → `503 unavailable`；限流 `assert.acct` 每账号每分钟 60 次（能力设计 §11.7）                                                                                                                                                                                          |
| 2    | 主机已撤销 `410 source_revoked`；团队停用 `403 team_suspended`；团队已删除 `403 source_access_denied`                                                                                                                                                                                             |
| 3    | 链接访客（`guest:<linkId>:…` 主体）走现有链接路径，范围取链接的 `scope`                                                                                                                                                                                                                           |
| 4    | 账号：有效权限为空 → `403 source_access_denied`                                                                                                                                                                                                                                                   |
| 5    | **版本门（契约 §30）**：主机属于团队时，`source.coreVersion` 按语义化版本比较不低于 `MIN_TEAM_CORE_VERSION` 才签；低于或解析不了 → `403 source_access_denied`（不签不带 `team` 的降级断言）。个人空间主机不受此门                                                                                 |
| 6    | 断言声明：`sub = accountId`、`aud = sourceId`、`name = displayName`、`email`、`emailVerified`、`team = { teamId, role: 团队角色 }`（只团队主机）、`role = 有效角色`（owner 来源且个人空间时不带，与个人中转同形）、`scp = ws:<id>…`（整台时不带）、`device`；`jti` 前缀 `s.<deviceId>.<sourceId>` |
| 7    | 中继令牌：`sub`、`aud = relay`、`src`、`team`（团队主机）、`dev`；`ro` 不带（读写由 core 判）；`jti` 前缀 `r.<deviceId>.<sourceId>`                                                                                                                                                               |
| 8    | 提交后把两个 `jti` 写进 `cloud:issued:acct:<id>` 与 `cloud:issued:src:<id>`；审计 `assertion.issue`（`jti` = 断言的 jti，`target = host`，`team_id`）                                                                                                                                             |
| 9    | 答 `relayOrigin = relayBaseUrl = https://<sourceId>.src.<RELAY_DOMAIN>`、`online`（读路由）                                                                                                                                                                                                       |

### 4.7 分享（`cloud/links`、主机邀请）

- **链接**包一层 `core.Links`：`links.create` 先查管理权与主机设置 `linkCreators`（`driver` 档 = 有效角色为 driver 的人也能签）、团队主机的 `allowGuests`；`links.list` / `update` / `revoke` 加 `space` 过滤；`links.accept` 对团队主机且 `allowAnonymousLinks = false` 时要求带会话（`403 anonymous_links_disabled`）。core 的条件加一与 `max_uses` 语义不变（能力设计 §11.3）；撤销链接经撤销钩子踢 `guest:<linkId>:` 前缀。
- **主机邀请被接受**（一个事务）：
  - 团队主机：接受者不是成员 → 先按团队 `allowGuests` 与 guest 配额加成 `guest` 成员；再建授予（主体 `guest`，若已是 owner / admin / member 则主体 `member`），`origin = invitation`，`role` / `workspace_ids` / `expires_at_ms` 取自邀请。
  - 个人空间主机：建主体 `guest` 的授予。core 能否只凭 `role` 准入个人空间的访客见 §10 Q2。
  - 已有同主体的有效授予 → 按「角色取高、范围并集」更新那一条，不建第二条。

### 4.8 撤销即生效（两道门，能力设计 §11.2、§11.7）

第一道门在云端，按下表；第二道门在 core：下次 `cloud/login` 时整份替换 `origin = cloud:<iss>` 的授予，或 30 天租约到期（Armadra A1 已实现）。

| 触发                                                   | 库                              | 撤销哪些已签令牌（`cloud:issued:*` 过滤）                            | 节点命令                                       | 事件                            |
| ------------------------------------------------------ | ------------------------------- | -------------------------------------------------------------------- | ---------------------------------------------- | ------------------------------- |
| 授予撤销 / 降级 / 缩范围 / 到期（`grant.expire` 作业） | 授予行                          | 对受影响的每个账号重算有效权限，**任何收窄**都撤销 acct ∩ src 的令牌 | `kick {sourceId, subject: accountId}`          | `grantChanged`                  |
| 成员移除 / 离开 / 降为 billing 或 guest                | 成员行、授予                    | 对团队每台主机同上                                                   | 每台主机 `kick {sourceId, subject}`            | `memberChanged`、`grantChanged` |
| 设备撤销、登出、改口令、找回口令                       | 设备行                          | acct 集合里 `deviceId` 相符的（全部设备时全部）                      | `kick {subject, deviceId?}`（不带 `sourceId`） | —                               |
| 账号停用（运营）                                       | `accounts.status`、全部设备撤销 | acct 集合全部                                                        | `kick {subject}`                               | —                               |
| 主机撤销                                               | `sources.revoked_at_ms`         | src 集合全部（含隧道令牌）                                           | `closeTunnel {sourceId}` + `kick {sourceId}`   | `sourceRevoked`                 |
| 团队停用 / 删除                                        | 团队行                          | 团队每台主机的 src 集合全部                                          | 每台主机 `kick {sourceId}`（隧道保留）         | `memberChanged`（删除时）       |
| 链接撤销                                               | 链接与访客设备（core）          | src 集合里 `subject` 以 `guest:<linkId>:` 开头的                     | `kick {sourceId, subjectPrefix}`               | `accessRevoked`（core 现有）    |

- 写撤销：`SET relay:revoked:<jti> 1 PXAT <exp>`，随后 `PUBLISH relay:revoked <jti>`；批量用 pipeline。
- 时序：库事务提交 → 撤销令牌 → 发节点命令 → 发事件。Redis 失败时重试 3 次后插 `relay.revoke` 作业兜底（作业重放幂等），并记错误日志；此时第二道门仍兜底。
- `kick` 只断连接；被撤销的人重连时拿不到新断言（库已改），旧令牌已在撤销表里。

### 4.9 运营后台（`cloud/admin`）

| 项                             | 规则                                                                                                                                                                                                                                                                                 |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 运营账号                       | `accounts.scopes` 含 `operator`；只经 CLI `operators add` 设置，接口不能改                                                                                                                                                                                                           |
| 两步验证                       | M1 只给运营账号：`operators totp` 生成 20 字节种子（主密钥包裹进 `account_credentials`，kind `totp`），终端打印一次 otpauth；校验 RFC 6238（SHA-1、6 位、30 秒、±1 步、同一步不重用）                                                                                                |
| 登录                           | `auth.login { scope: "operator" }`：非运营账号 `403 forbidden`；没有 TOTP `403 operator_mfa_required`；口令对了答 MFA 挑战（`challengeId` 5 分钟、存 Redis `cloud:mfa:<哈希>`，每个挑战最多 5 次）→ `auth.mfaVerify` 建 `operator` 设备（1 小时绝对寿命）并下发 `__Host-armadra-ops` |
| 准入                           | `admin.*` 只认 `Kind = operator` 的会话（Cookie 或 Bearer）；运营会话对 `admin.*` 与自己的 `auth.refresh` / `auth.logout` 以外的过程一律 `401 unauthenticated`                                                                                                                       |
| 写操作                         | 都必须带 `reason`（schema 已要求），审计 `actor.kind = "operator"`、`team_id` 为对象团队                                                                                                                                                                                             |
| `accountStatus` / `teamStatus` | 停用走 §4.8；恢复只改状态                                                                                                                                                                                                                                                            |
| `relayNodes`                   | 读 `relay:nodes`；`relayNodeDrain` 发 `relay:node:<id>` 的 `drain`，节点自己把 `draining` 写回节点表（G5）                                                                                                                                                                           |
| 事故开关                       | `platform_settings` 键 `incident = {assertionsPaused, registrationsPaused, rejectNewConnections}`；写库后镜像到 `relay:incident`；控制面各实例每 5 秒读一次库（不依赖 Redis 保活）                                                                                                   |
| 只读                           | 看不到任何内容与令牌；租户搜索按邮箱 / 显示名 / slug 前缀与包含（`ILIKE`，最多 50 行）                                                                                                                                                                                               |

### 4.10 配额（`cloud/quota`）

M1 的额度表写在代码里（`quota.Plans`，数值取能力设计 §3.10 初稿；改数只改代码），`entitlements.overrides` 可逐项放宽。

| 额度（`details.quota`） | 空间 | Free      | 检查点（锁谁）                                                                         |
| ----------------------- | ---- | --------- | -------------------------------------------------------------------------------------- |
| `hosts`                 | 个人 | 2         | `sources.registrationToken`（快速失败）、`sources.register`（锁账号行，权威）          |
| `teams`                 | 个人 | 1         | `teams.create`（锁账号行；计调用者为 owner 且未删除的团队）                            |
| `members`               | 团队 | 3         | 邀请创建与接受（锁团队行；计非 guest 成员 + 待处理的非 guest 邀请）                    |
| `hosts`                 | 团队 | 3         | 同个人主机（锁团队行）                                                                 |
| `guests`                | 团队 | 3         | 团队邀请（`guest`）与主机邀请的接受（计 guest 成员 + 待处理的 guest 邀请）             |
| `connections`           | 团队 | 10        | 中继节点的 `edge.concurrent.team`（`relay:quota:team:<id>`）；控制面只展示             |
| `relay.bytes`           | 两者 | 5 GB / 月 | M1 只展示（`relay_traffic_buckets` 按 `team_id` 或个人空间主机汇总，入口节点的 `e2e`） |

- 超限答 `429 limit_reached`，`details = { quota, used, limit }`（`quota` 与 `usageEntry.metric` 同名）。
- `teams.usage`、`billing.usage` 用同一组统计函数；`billing.usage` 的 `granularity = day` 按 UTC 天汇总当前计费周期（M1 周期 = 自然月）。
- 分组、访问申请等「计划不含」的功能答 `403 plan_feature_unavailable`。

### 4.11 存储端口（`internal/store/saas`）

```go
// Store 是 SaaS 的存储端口，只有 PostgreSQL 实现（postgres.Backend.SaaS()）。
type Store interface {
	// Tx 在一个读已提交事务里执行 fn；fn 返回错误即回滚。提交后按序执行 tx.AfterCommit 登记的回调
	// （发 Redis 撤销、节点命令、事件）。死锁与序列化失败自动重试 3 次。
	Tx(ctx context.Context, fn func(tx Tx) error) error
	Read() Reader // 不开事务的只读查询
}

type Tx interface {
	Reader
	Accounts() AccountRepo      // 邮箱、状态、范围、凭据、邮件令牌、设备码
	Teams() TeamRepo            // 团队、成员、设置；LockTeam / LockAccount
	Invitations() InvitationRepo
	Hosts() HostRepo            // source_spaces、注册令牌（带空间）、登记事务
	Grants() GrantRepo
	Trust() TrustRepo
	Audit() AuditRepo           // 只追加
	Jobs() JobRepo              // Enqueue（含 dedupe）、发件箱
	Settings() SettingsRepo     // platform_settings、entitlements
	AfterCommit(fn func(ctx context.Context))
}
```

- 每个 Repo 方法的第一个参数是 `tenantID`；团队范围的方法第二个参数是 `teamID`，查询必写 `team_id = $2`。
- 核心端口（`store.Ports`）与 SaaS 端口共用同一个 `pgxpool`；需要同事务写核心表（登记主机、建设备）时由 `postgres` 包提供 `TxPorts(tx)` 把核心端口绑到同一个事务。
- 错误：唯一冲突（23505）按约束名映射成 `team_slug_taken`、`email_taken`、`conflict`；`team_has_owner`（23514）映射成 `team_last_owner`。

## §5 HTTP 层

### 5.1 认证与会话（`cloud/httpx`）

| 凭据                   | 携带                                                                                                                                                                                                               | 能用于                                                      |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------- |
| 控制台会话（`cookie`） | `__Host-armadra-cloud=<访问令牌>; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=900` + `__Secure-armadra-cloud-refresh=<刷新令牌>; Path=/v1/auth/refresh; Secure; HttpOnly; SameSite=Strict; Max-Age=2592000` | 除 `admin.*` 外的会话类过程                                 |
| 运营会话（`operator`） | `__Host-armadra-ops=<访问令牌>`（同上属性）+ `__Secure-armadra-ops-refresh`（`Path=/v1/auth/refresh`，`Max-Age` = 会话剩余寿命）                                                                                   | `admin.*` 与自己的 `auth.refresh` / `auth.logout`           |
| Bearer（`bearer`）     | `Authorization: Bearer <访问令牌>`，刷新令牌在请求体                                                                                                                                                               | 原生 App、core、CLI；运营设备的 Bearer 令牌也可调 `admin.*` |
| 源                     | `Authorization: Source <jws>`                                                                                                                                                                                      | `sources.heartbeatHint`                                     |

- **传输绑定**：`cookie` / `operator` 设备的访问令牌只认从 Cookie 来；`bearer` 设备的只认从头来；对不上答 `401 unauthenticated`。登录类过程按请求定传输：带 `Origin = APP_ORIGIN` 且不带 `Authorization` → Cookie 传输（响应 `Set-Cookie`、体里不含 `refreshToken`、含 `csrfToken`）；否则 Bearer 传输。
- **不混用**：同一请求既有任一会话 Cookie 又有 `Authorization` → `400 bad_request`（能力设计 §11.6）；两枚会话 Cookie 同时存在是允许的，按过程挑：`admin.*` 与带 `scope: "operator"` 的刷新 / 登出用 `__Host-armadra-ops`，其余用 `__Host-armadra-cloud`。
- **刷新**：`auth.refresh` 不带 `refreshToken` 时取对应的刷新 Cookie；成功后两枚 Cookie 一起换新。`auth.logout` 撤销设备并用 `Max-Age=0` 清掉该会话的两枚 Cookie（刷新 Cookie 按它自己的 `Path` 清）。
- **契约措辞**：契约 §1 写「刷新凭据在 Cookie 里（`__Host-armadra-cloud` / `__Host-armadra-ops`）」且「刷新 Cookie 路径限定在 `/v1/auth/refresh`」；`__Host-` 前缀要求 `Path=/`，两者不能是同一枚 Cookie，所以拆成上表的访问 Cookie 与 `__Secure-` 刷新 Cookie，见 §10 Q3。

### 5.2 CSRF 与来源

| 规则        | 内容                                                                                                                                                                                  |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 值          | `csrfToken = base64url(HMAC-SHA256(K_csrf, "armadra/csrf\0" + deviceId))`，`K_csrf = HKDF-SHA256(主密钥, info "armadra-cloud/csrf")`；无状态，同一设备不变，`auth.refresh` 照常返回它 |
| 校验范围    | Cookie 认证的每个非 GET / HEAD 请求（含 `auth.logout`，**不含 `auth.refresh`**）：`X-Armadra-CSRF` 与该 Cookie 对应设备的值常量时间比较，不符 `403 forbidden`                         |
| Origin      | 同一批请求还要求 `Origin` 头恰好出现一次且等于 `APP_ORIGIN`（`auth.refresh` 也要），否则 `403 forbidden`；这挡住同站的兄弟子域（`*.src.<域>` 与 `app.<域>` 同站，`SameSite` 挡不住）  |
| 运营 Cookie | 用运营设备自己的值                                                                                                                                                                    |

### 5.3 CORS 与页面

- `/v1/*` 与 `/.well-known/*`：对任意来源答 `Access-Control-Allow-Origin: *`、`Allow-Headers: authorization, content-type`，**从不**发 `Allow-Credentials`——浏览器跨源请求带不上 Cookie，跨源只能用 Bearer。控制台同源，不走 CORS。
- 托管页面（`WEB_ROOT`，S9）：复用 `internal/web`；`/`、`/login`、`/t/*`、`/h/*`、`/j/*`、`/device`、`/invite/*`、`/ops/*` 回落 `index.html`；HTML 的 CSP：`default-src 'self'; connect-src 'self' wss://*.src.<域> https://challenges.cloudflare.com; script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; frame-ancestors 'none'; base-uri 'none'`。

### 5.4 限流与挑战

速率桶在 Redis（`cloud:limit:`），窗口固定；`429` 带 `details.retryAfterMs` 与 `Retry-After`。

| 种类                                | 键            | 额度                 | 过程                                                              |
| ----------------------------------- | ------------- | -------------------- | ----------------------------------------------------------------- |
| `login.ip`                          | IP            | 20 / 分钟            | `auth.login`、`auth.changePassword`、`sources.register`（沿用）   |
| `login.acct`                        | 邮箱的哈希    | 20 / 分钟            | `auth.login`（另有逐账号锁定）                                    |
| `register.ip`                       | IP            | 10 / 小时            | `auth.register`                                                   |
| `reset.ip` / `reset.email`          | IP / 邮箱哈希 | 10 / 小时、3 / 小时  | `auth.passwordResetStart`                                         |
| `verify.acct`                       | 账号          | 3 / 小时             | `auth.emailVerifyResend`、`me.emailChange`                        |
| `token.ip`                          | IP            | 30 / 分钟            | `auth.emailVerify`、`auth.passwordResetFinish`、`invitations.get` |
| `device.start.ip` / `device.get.ip` | IP            | 30 / 小时、30 / 分钟 | `auth.deviceStart`、`auth.deviceGet`                              |
| `stepup.dev`                        | 设备          | 10 / 10 分钟         | `auth.stepUp`、`auth.mfaVerify`（每挑战另限 5 次）                |
| `assert.acct`                       | 账号          | 60 / 分钟            | `sources.assertion`                                               |
| `invite.acct` / `invite.resend`     | 账号 / 邀请   | 50 / 小时、5 / 小时  | `invitations.create` / `resend`                                   |
| `link.accept.ip`                    | IP            | 20 / 分钟            | `links.accept`（沿用）                                            |

挑战（Turnstile，契约 §16）缺省范围：`auth.login`、`auth.register`、`auth.passwordResetStart`、`links.accept`；校验沿用 `core.ChallengeGate`，先于口令与锁定、晚于按 IP 限流。原生 App 与 core 经同源挑战页取令牌，规则不变。

### 5.5 路由、入参与错误

- **路由**：`api.New(Options{Mode: ModeCloud, Handlers: cloud.Handlers(...)})`；每批把实现的过程加进 `Handlers`，其余照旧 `501 not_implemented`（测试守住「启用集合 = 本文 §1.1 该批之前的累计」）。`platform.health` 由外层答（`Exclude`）。
- **认证中间件**：在 `api.Call` 上加 `Transport` 与 `Cookies`，`Call.Caller` 先按 §5.1 选凭据再走 `AuthenticateToken`；CSRF 与 Origin 在处理器之前、入参校验之后。
- **错误**：处理器只返回 `core.Fail` / `core.FailWith`；存储错误按 §4.11 映射；其余一律 `500 internal`，日志只记错误类名与 `requestId`。
- **入参解释器的缺口（S0a，第一包）**：`gen.UnsupportedInputs` 现有 13 个过程因 `format`、`minItems`、`propertyNames` 没有入参 schema，`NewRouter` 拒绝给它们装处理器。M1 用到其中 5 个（`auth.register`、`auth.stepUp`、`auth.passwordResetStart`、`me.emailChange`、`invitations.create`）。S0a 在 `internal/protocol/schema` 与 `tools/schemagen` 补：

  | 关键字                                       | 语义（与 zod 一致）                                                         |
  | -------------------------------------------- | --------------------------------------------------------------------------- |
  | `minItems`                                   | 数组长度下界                                                                |
  | `format: email`                              | 与 zod 4 `z.email()` 同一个正则（从协议包常量导出，避免两边各写一份）       |
  | `format: uri`                                | 与 `z.url()` 一致：`url.Parse` 成功、有 scheme 与 host                      |
  | `propertyNames` + `additionalProperties: {}` | 键是字符串、值不限（`z.record(z.string(), z.unknown())`）；剪枝时保留全部键 |

  另补 query 数组：schema 为 `array` 的参数收全部同名值（`sources.list` 的 `tag`），不再只取最后一个。补完 `UnsupportedInputs` 为空，`internal/api` 的夹具测试与路由测试相应收紧（「每个过程都有入参 schema」）。

## §6 邮件与作业

### 6.1 作业（`cloud/jobs`）

| 规则 | 内容                                                                                                                                                                                                                                                                                                                                                                         |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 入队 | 业务事务里 `Enqueue(kind, payload, runAtMs, dedupeKey?)`；同 kind 同 `dedupe_key` 的未完成作业已在 → 不再插（`ON CONFLICT DO NOTHING`）                                                                                                                                                                                                                                      |
| 领取 | `UPDATE jobs SET locked_by=$1, locked_until_ms=$now+60000, attempts=attempts+1 WHERE job_id IN (SELECT job_id FROM jobs WHERE done_at_ms IS NULL AND failed_at_ms IS NULL AND run_at_ms <= $now AND (locked_until_ms IS NULL OR locked_until_ms < $now) ORDER BY run_at_ms LIMIT $n FOR UPDATE SKIP LOCKED) RETURNING …`；空闲时 1 秒一轮，有 `LISTEN cloud_jobs` 时立即唤醒 |
| 完成 | 成功置 `done_at_ms`；失败 `run_at_ms = now + min(30 秒 × 2^(attempts-1), 1 小时)`，`last_error` 只记类名或错误码；到 `max_attempts` 置 `failed_at_ms` 并记错误日志                                                                                                                                                                                                           |
| 幂等 | 每个作业处理器必须可重放：先读状态再动作（例如发信前看 `outbox_emails.sent_at_ms`，清除前看团队是否已清）                                                                                                                                                                                                                                                                    |
| 清理 | 完成超过 7 天、失败超过 30 天的作业由 `jobs.gc` 删                                                                                                                                                                                                                                                                                                                           |

| 作业                                                                     | 触发                                              | 动作                                                     |
| ------------------------------------------------------------------------ | ------------------------------------------------- | -------------------------------------------------------- |
| `mail.send`                                                              | 发件箱                                            | §6.3                                                     |
| `grant.expire`                                                           | 授予有到期时间时排到那一刻                        | 撤销流程（§4.8）                                         |
| `team.purge`                                                             | 团队删除                                          | §4.4                                                     |
| `relay.revoke`                                                           | Redis 写撤销失败的兜底                            | 重写撤销与命令                                           |
| `invitations.expire`、`trust.expire`、`deviceCodes.gc`、`emailTokens.gc` | 每小时（调度器插带 `dedupe_key = 时间窗` 的作业） | 落库过期并发事件；删过期 24 小时以上的行                 |
| `audit.partitions`、`audit.retention`                                    | 每天                                              | 建未来两个月分区；按计划删超期行、`DROP` 13 个月前的分区 |

调度器在每个实例里跑，靠 `dedupe_key` 保证同一时间窗只排一次。

### 6.2 SMTP 适配器（`cloud/mail`）

- 接口 `Sender.Send(ctx, Message) error`；实现 `smtpSender`（`net/smtp`，`starttls` / `tls` / `none`，连接与整封 30 秒超时）与测试用的 `memorySender`。
- `Message-ID = <email_id@APP_ORIGIN 的主机>`：重试用同一个，提供方可去重；`List-Unsubscribe` 只给非安全类（M2 的通知偏好用）。
- 开发：`pnpm dev:up` 已有 mailpit（`SMTP_HOST=127.0.0.1`、`SMTP_PORT=1025`、`SMTP_TLS=none`），集成测试经 mailpit 的 HTTP API 取信。
- 日志只记模板名、`email_id`、结果与错误类名；不记收件人、主题与正文。

### 6.3 模板（中英双语，按收件账号的 `locale`；没有账号的收件人按发起人的 `locale`）

| 模板               | 触发                         | 带一次性令牌                    |
| ------------------ | ---------------------------- | ------------------------------- |
| `verify_email`     | 注册、重发                   | `email_tokens(verify)`，24 小时 |
| `email_change`     | `me.emailChange`（发新地址） | `email_tokens(change)`，24 小时 |
| `email_changed`    | 改邮箱完成（发旧地址）       | —                               |
| `password_reset`   | `auth.passwordResetStart`    | `email_tokens(reset)`，30 分钟  |
| `password_changed` | 改口令、找回完成             | —                               |
| `new_device`       | 新设备登录                   | —                               |
| `team_invitation`  | 团队邀请创建、重发           | 邀请令牌，7 天                  |
| `host_invitation`  | 主机邀请创建、重发           | 邀请令牌，缺省 7 天、最长 30 天 |

- 模板在 `cloud/mail/templates/<locale>/<name>.{subject,txt,html}`（`go:embed`）；两种语言的模板集合必须相同（测试守住）；文案遵守 AGENTS.md 的界面与文案规则，链接一律指向 `APP_ORIGIN` 的页面（`/verify`、`/reset`、`/invite/<id>`）。
- **令牌在发信时现生成**：发件箱与作业载荷只有账号或邀请 id；`mail.send` 在自己的事务里生成令牌、写哈希（同目的的旧令牌作废）、提交后发信。重试会换新令牌，旧的随之作废——用户只会拿到最后一封有效。明文令牌只存在于内存与发出的邮件里。

## §7 跨节点

### 7.1 me-stream 与领域事件

- `me.streamTicket` 把票写进 Redis（§2.4），`GET /v1/me/stream` 在任一控制面实例上 `GETDEL` 兑换；每 30 秒复核设备（核心现有逻辑）。
- 事件源：领域服务在事务提交后 `events.Publish(event, audience)` → `PUBLISH cloud:events` → 每个实例按受众投递给本机连接。在线事件不经 `cloud:events`：每个实例都订阅 `relay:presence`。
- 受众与过滤：

| 事件                                               | 受众                                                                                         |
| -------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `sourceOnline` / `sourceOffline` / `sourceRevoked` | 对该主机有有效权限的账号（连接上缓存可见集合，收到 `grantChanged` / `memberChanged` 时重算） |
| `accessRevoked`、`linkUsed`                        | 现有（链接访客 / 主机管理者）                                                                |
| `memberChanged`                                    | 该团队全部成员                                                                               |
| `invitationChanged`                                | 邀请人、团队 admin 以上、被邀请邮箱对应的账号                                                |
| `grantChanged`                                     | 主机 owner、团队 admin 以上、授予主体涉及的账号                                              |
| `deviceTrustRequested` / `deviceTrustResolved`     | 按团队设置 `deviceEndorsers` / `memberSelfEndorse` 算出的背书人 + 请求者本人                 |
| `planChanged`                                      | 空间的 owner / admin / billing（个人空间是本人）                                             |

### 7.2 撤销、踢人、关隧道（`cloud/relayctl`）

- 接口 `relayctl.Control`：`IndexIssued(ctx, accountID, sourceID, deviceID, jti, exp)`、`RevokeWhere(ctx, Filter)`、`Kick(ctx, KickFilter, reason)`、`CloseTunnel(ctx, sourceID)`、`Route(ctx, sourceID)`、`Nodes(ctx)`、`Drain(ctx, node)`、`SetTeamQuota(ctx, teamID, Quota)`、`SetIncident(ctx, Incident)`；Redis 实现在 S8，S1–S7 用内存实现（行为相同，测试可断言发出的命令）。
- 实现 `core.RelayPort`（`Route`、`CloseTunnel`、`Disconnect`）与 `core.RevocationSink`，所以核心的撤销路径（登出、链接撤销等）自动走到 Redis。
- 命令发给持有节点（读 `relay:route:src:<id>` 的 `node`），没有路由时退回 `relay:ctl` 广播；`kick {subject}`（跨源）总是广播。

### 7.3 节点表与 `heartbeatHint`

- `Nodes()` 读 `relay:nodes`，去掉 `draining` 的节点；空表退回 `EDGE_URL` 单节点。
- 排序按 sourceId 做 rendezvous 哈希（`sha256(sourceId + "\0" + node)` 取前 8 字节为 64 位整数，加权：`-ln(u) / weight`），答前三个，`weight` 递减为 3、2、1（Go 设计 §3.6）；同一函数放 `internal/control`，G5 节点侧共用。

## §8 测试

### 8.1 合同与夹具

- `packages/platform-protocol/fixtures/cloud-api/<procedure>.json` 的入参全部过生成的 schema（S0a 后没有例外）。
- 每个启用的 cloud 过程有一张表驱动测试：在 PG 上跑处理器，答案用 `testutil/schema` 按 `cloud-openapi.json` 校验；错误码必须在该过程契约的错误列表里（测试从 `gen.Routes` 的错误表取）。
- 核心的 15 + 19 例端口与服务合同在 `AccessPolicy` 抽出后照旧对 personal 跑全；另加 SaaS 策略下的同名服务合同（可见性、断言、链接）。

### 8.2 PostgreSQL 集成

- 读 `ARMADRA_CLOUD_TEST_DATABASE_URL`（`pnpm dev:up`），每个测试一个随机 schema，跑全量迁移；没有变量就跳过并说明。
- Redis 用 `ARMADRA_CLOUD_TEST_REDIS_URL`，每个测试一个随机前缀（不用 `FLUSHDB`、不扫别人的键）。
- 并发：10 个同时接受 `max_uses = 5` 的链接恰好 5 个成功（能力设计 §11.3）；10 个同时 `teams.create` / 主机登记不越过 Free 额度；10 个同时降级最后两个 owner 恰好 1 个成功。

### 8.3 邮件

- mailpit：注册 → 取信 → 解析链接 → `auth.emailVerify`；重发后旧链接 `email_token_invalid`；双语模板渲染快照（不含令牌）。

### 8.4 端到端 `pnpm e2e --mode saas`（S9）

拓扑：`armadra-cloud` + 两个 `armadra-relay saas serve`（a、b）+ PG + Redis + mailpit + 两个假 core（复用 `go/internal/testutil/relayenv` 的假 core，报 `e2e` 与 1.4 的 `coreVersion`）。客户端走 Bearer（原生形态）与 Cookie（控制台形态）各一遍。

| #   | 场景                                                      | 断言                                                                                   |
| --- | --------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| 1   | A 注册 → 未验证时建团队 `403 email_unverified` → 取信验证 | 门生效                                                                                 |
| 2   | A 建团队 T → 邀请 B（member）→ B 注册、验证、接受         | `memberChanged` 送达 A；`me.spaces` 含 T                                               |
| 3   | A 设备码登录假 core H1 → 选空间 T 登记                    | `ownerAccountId = A`；`sources.list(space=T)` 含 H1                                    |
| 4   | A 授予 T 全员 `editor`                                    | `grants.effective(B, H1)` = editor，来源 team                                          |
| 5   | H1 隧道在节点 a，B 的客户端连节点 b（强制）               | 断言 `team = {T, member}`、`role = editor`；节点间转发通；流量只在 b 计、`team_id = T` |
| 6   | A 撤销授予                                                | B 的通道 1 秒内被关；B 的旧中继令牌重连 4401；B 再要断言 `403 source_access_denied`    |
| 7   | A 把 B 移出团队（重新授予后）                             | 同 6；`memberChanged`                                                                  |
| 8   | A 撤销 H1                                                 | 隧道关、`sourceRevoked`、心跳 `410 source_revoked`                                     |
| 9   | 老版本 core H2 登记进 T                                   | `426 protocol_unsupported`；登记到个人空间成功、断言不带 `team`                        |
| 10  | 第 4 台主机登记进 Free 团队                               | `429 limit_reached`，`details.quota = hosts`                                           |
| 11  | 运营登录（口令 + TOTP）→ 停用团队 T                       | 在线连接被踢、断言 `403 team_suspended`；审计 `actor.kind = operator`                  |
| 12  | 节点 a 排空                                               | H1 收 GOAWAY rebalance 转到 b；`heartbeatHint` 不再含 a                                |
| 13  | 泄密扫描（沿用 personal 的 secret scan）                  | 日志、库转储、Redis 转储里没有任何令牌、口令、邮件令牌明文                             |

### 8.5 安全测试（各批随功能带上）

| 类别          | 用例                                                                                                                                                                                                                |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CSRF          | Cookie 写请求缺头 / 错值 → 403；`auth.refresh` 不要求；`auth.logout` 要求；运营 Cookie 用普通会话的值 → 403                                                                                                         |
| 来源          | Origin 缺失、重复、为 `https://x.src.<域>` → 403；跨源请求带 Cookie 拿不到 `Allow-Credentials`                                                                                                                      |
| Cookie 作用域 | `__Host-` 属性逐项断言；刷新 Cookie 只在 `/v1/auth/refresh` 下发；登出两枚都清；Cookie + Bearer 同带 → 400；Cookie 设备的令牌放进 `Authorization` → 401                                                             |
| 提权          | `elevatedUntilMs` 10 分钟后失效；提权失败计入限流                                                                                                                                                                   |
| 运营隔离      | 普通会话调 `admin.*` → 401；运营会话调 `teams.list` → 401；运营会话 1 小时后刷新失败；非运营账号 `scope: operator` → 403、没 TOTP → `operator_mfa_required`                                                         |
| 租户隔离      | 「跨团队读不到」：对每个带 `teamId` 的过程，用 T2 的成员调 T1 → `404 not_found`；存储层每个团队范围方法的 SQL 都含 `team_id`（静态检查：扫描 `saas_*.go` 的查询串）；`tenant_id = default` 的行在 SaaS 接口里不可见 |
| 撤销          | §4.8 每一行：撤销后 1 秒内节点拒绝旧令牌；Redis 不可用时作业兜底                                                                                                                                                    |
| 枚举          | 登录、找回、`invitations.get` 对存在与不存在的邮箱 / 邀请响应同形同时（误差 < 20%）                                                                                                                                 |
| 泄密          | 每个模板、日志字段、审计行不含令牌（单测断言 + e2e 扫描）                                                                                                                                                           |

## §9 实施批次

规则：每批一个 worktree（`worktrees/armadra-cloud/g6-s<N>`）、一个 PR（`Refs #60, #59`），每个模块一个提交，CI 绿后由维护者合并；Sonnet 执行，账号、会话、授予与撤销由 Opus 复核（Go 设计 §7）。行数含测试。

### 9.1 批次表

| 批  | 内容                                                                                                                                                                                                                                                                                                                                        | 依赖                         | 验收                                                                                      | 规模                                  |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------- | ----------------------------------------------------------------------------------------- | ------------------------------------- |
| S0a | 入参解释器补 `minItems`、`format`（email / uri）、`propertyNames`；`schemagen` 生成；query 数组；`UnsupportedInputs` 清空；`core.Error.Details` 与 `WriteError` 输出 `details`                                                                                                                                                              | —                            | `pnpm go:check`；全部 `cloud-api` 夹具都有入参 schema；email 正则与协议包同源的一致性测试 | ~800                                  |
| S0b | §3.4 的六个迁移 + `migrations.lock` + 迁移测试（§3.5）；`internal/store/saas` 端口骨架、`postgres.Backend.SaaS()`、`Tx` / `AfterCommit` / 错误映射                                                                                                                                                                                          | —（与 S0a 并行）             | 迁移测试全过；端口合同 15 例照旧过；编号合并时取 main 最大号 +1                           | ~1400                                 |
| S1  | `cmd/armadra-cloud`（`serve`、`migrate`、`version`、`keys rotate`、`health`）、配置、租户引导与签名钥包裹；`core` 扩展（§4.2 除事件外全部）；`httpx`（会话、CSRF、Origin、CORS、限流、挑战）；`register`、`login`、`refresh`、`logout`、`changePassword`、`stepUp`、`me.get/update/devices/deviceRevoke`、`platform.*`；`relayctl` 内存实现 | S0a、S0b                     | §8.5 的 CSRF / 来源 / Cookie 用例；personal 的 e2e 不回归（`pnpm e2e --mode personal`）   | ~1500（超出再拆 S1a 外壳 / S1b 账号） |
| S2  | `jobs`、`mail`（SMTP、模板、发件箱）、`audit` 写入与安全日志写入；`emailVerify`、`emailVerifyResend`、`passwordResetStart/Finish`、`me.emailChange`、新设备提醒                                                                                                                                                                             | S1                           | mailpit 集成；令牌只在内存（扫描库与日志）；作业重放幂等测试                              | ~1300                                 |
| S3  | 设备码五个过程                                                                                                                                                                                                                                                                                                                              | S1（与 S2 并行）             | 状态机全分支；轮询过快 429；猜码限流                                                      | ~600                                  |
| S4  | `teams`（14 个过程）、团队邀请、`quota`（团队、成员、guest）、`me.spaces`、`me.get.teams`、`relay:quota:team` 写入（经 `relayctl`）                                                                                                                                                                                                         | S2                           | 角色矩阵（5 角色 × 每过程）；最后一个 owner 并发；删除 / 恢复 / 清除作业；跨团队读不到    | ~1500                                 |
| S5  | `hosts`（登记到空间、目录筛选分页、改名标签设置、撤销、访问清单）、`grants`（五个过程、有效权限）、断言合成与版本门、签发索引、撤销流程（§4.8 前三行）；`AccessPolicy` 的 SaaS 实现                                                                                                                                                         | S4                           | 有效权限表驱动（主体 × 角色 × 范围 × 到期）；版本门；撤销后令牌进撤销表（内存实现断言）   | ~1500                                 |
| S6  | 链接加 `space` 与团队设置、主机邀请（两种空间）、`trust.list` / `trust.reject`（+ §10 Q1 定下后的请求来路）                                                                                                                                                                                                                                 | S5                           | 链接并发 `max_uses`；匿名链接开关；主机邀请成 guest；信任列表按背书人收窄、不含校验码     | ~1200                                 |
| S7  | `audit.list`、`me.securityLog`、保留期与分区作业；`teams.usage`、`billing.plan`、`billing.usage`（含按天）；运营：`operators add/totp`、`auth.login{operator}`、`auth.mfaVerify`、`admin.*` M1、事故开关                                                                                                                                    | S4（审计与用量需 S5 的主机） | 运营隔离用例；停用即踢；事故开关三项；保留期删除只删本团队                                | ~1500                                 |
| S8  | `relayctl` 的 Redis 实现（§2.4 全部键）、`relay:presence` 订阅与 `last_seen`、`cloud:events` 与 me-stream（票、受众、可见集合缓存）、节点表与 rendezvous、`nodes token` / `revoke`                                                                                                                                                          | S5（与 S6、S7 并行）         | 两个控制面实例 + Redis 的事件互通；撤销 1 秒内到达假节点；Redis 断线重连后撤销表全量重读  | ~1300                                 |
| S9  | `deploy/saas/compose.yml`、`armadra-cloud` 镜像（distroless）、`WEB_ROOT` 托管与 CSP、`scripts/e2e.mjs --mode saas`、`deploy/README.md` 与 runbook；`apps/cloud` TS 的去留（§10 Q9）                                                                                                                                                        | S6、S7、S8、**G5**           | §8.4 全部场景；`pnpm images:build` 冒烟                                                   | ~1200                                 |

并行：S0a ∥ S0b；S2 ∥ S3；S6 ∥ S7 ∥ S8。关键路径：S0 → S1 → S2 → S4 → S5 → S8 → S9。

### 9.2 G5 依赖（`armadra-relay saas serve` 要提供的）

| #   | 项                                                                                                        | 用在      |
| --- | --------------------------------------------------------------------------------------------------------- | --------- |
| N1  | 按 §2.4 读写 `relay:route`、`relay:presence`、`relay:revoked`（现有 G3a 实现）                            | S5、S8    |
| N2  | `relay:ctl` / `relay:node:<id>` 的 `kick` 过滤字段 `subject`、`subjectPrefix`、`deviceId` 与 `drain` 命令 | S5、S7    |
| N3  | `relay:nodes` 心跳（`HPEXPIRE`）与 `draining`、`connections`、`tunnels`                                   | S7、S8    |
| N4  | `edge.concurrent.team` 读 `relay:quota:team:<id>`；`relay:incident` 的 `rejectNewConnections`             | S4、S7    |
| N5  | 流量桶写 `team_id`（中继令牌的 `team`），只在入口节点计；SaaS 节点写 PG 的方式见 §10 Q4                   | S7        |
| N6  | 节点令牌验签（`aud = relay-node`）与 `n.` 前缀的撤销                                                      | S8        |
| N7  | `/x/v1` 节点间转发、子域寻址、GOAWAY 再平衡                                                               | S9（e2e） |

### 9.3 Armadra 侧对应（cloud-console §7）

| 控制台批 | 用到的 G6 批        | 说明                                                              |
| -------- | ------------------- | ----------------------------------------------------------------- |
| U1b      | S1                  | 数据层先接 mock；联调要 Cookie 会话与 `csrfToken`                 |
| U2a      | S1、S2              | 登录、注册（含暂停注册态）、验证邮箱、找回                        |
| U2b      | S3、S4、S6          | `/device`、`/invite/`、`/j/` 的 SaaS 分支                         |
| U3       | S1、S5、S8          | 个人空间、主机目录（筛选分页、`ownerName`）、设备、安全日志（S7） |
| U4       | S4                  | 团队、成员与邀请、设置、危险操作                                  |
| U5       | S5                  | 团队主机、授予、有效权限                                          |
| U6       | S6、S7              | 链接、设备信任、审计、用量与计划                                  |
| U7a–c    | S3、S5（+ A1 后续） | core 侧设备码、登记到空间、侧栏按团队分组                         |
| U8       | S7                  | 运营后台                                                          |

Armadra 侧还要：§10 Q1 定下后 core 上报待信任请求；§10 Q2 定下后 core 准入个人空间访客。两者各开一个 Armadra Issue。

## §10 待定问题（附建议缺省）

| #   | 问题                                                                                                                                                | 建议                                                                                                                                                                                                                                                                                                                                                                                                                            |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Q1  | 待信任请求怎么进云端：契约 §31 只有 `trust.list` / `trust.reject`，没有「创建」与「已信任」的来路                                                   | 由**主机**上报（权威、成员伪造不了）：协议加两个 `source` 范围的过程 `sources.trustRequest`（`POST /v1/sources/me/trust-requests`，`{accountId, deviceId, deviceName, platform, expiresAtMs}`，云端核对 `deviceId` 属于该账号且账号对主机有有效权限、按 `(source, device)` 去重）与 `sources.trustResolve`（`{requestId, outcome: trusted \| expired}`）；只做加法，进协议补缺 PR；core 侧随 Armadra 后续 PR。S6 先做列表与拒绝 |
| Q2  | 个人空间主机的访客（主机邀请、guest 授予）：断言有 `role` 没有 `team`，A1 之后的 core 只认 `team` + `role` 免邀请令牌准入                           | core 扩展：来自已登记 SaaS issuer、`sub` 有云端授予意图（带 `role`）的断言即准入，按 `role` / `scp` 建授予并同样整份同步；在此之前云端对这类访客的断言答 `403 source_access_denied`（同 §4.6 版本门，用同一个最低版本常量）                                                                                                                                                                                                     |
| Q3  | 契约 §1 说刷新凭据在 `__Host-armadra-cloud` / `__Host-armadra-ops` 里且刷新 Cookie 路径限定在 `/v1/auth/refresh`——`__Host-` 必须 `Path=/`，自相矛盾 | 按 §5.1 拆成访问 Cookie（`__Host-…`，`Path=/`）与刷新 Cookie（`__Secure-…-refresh`，`Path=/v1/auth/refresh`）；协议补缺 PR 里改契约 §1 的这句措辞，形状不变                                                                                                                                                                                                                                                                     |
| Q4  | SaaS 节点的流量桶怎么进 PG（节点设计上只连 Redis）                                                                                                  | 节点直写 PG 的 `relay_traffic_buckets`，用只对这张表有 `INSERT` / `UPDATE` 的独立数据库角色（`armadra_relay_traffic`）；不走 Redis 中转                                                                                                                                                                                                                                                                                         |
| Q5  | 个人空间的「同时连接」额度：中继令牌不带空间，个人空间主机的连接无法按空间计                                                                        | M1 不限个人空间的并发（节点照旧按账号 256 防滥用）；M2 计费时给中继令牌加可选声明 `space`（协议加法）再按空间计                                                                                                                                                                                                                                                                                                                 |
| Q6  | `limit_reached` 的 `details` 形状契约里只有文字                                                                                                     | 定为 `{ quota, used, limit }`（`quota` 与 `usageEntry.metric` 同名），协议补缺 PR 里写进契约 §21                                                                                                                                                                                                                                                                                                                                |
| Q7  | 哪些过程要求提权                                                                                                                                    | M1 不要求（控制台 M1 用输入确认）；M2 起 `teams.delete`、`teams.transferOwnership`、`me.delete` 要求，未提权答 `401 mfa_required`                                                                                                                                                                                                                                                                                               |
| Q8  | 关掉团队的 `allowGuests` 时已有 guest 怎么办                                                                                                        | M1 不动已有 guest（只挡新增）；控制台在设置项旁提示现有 guest 数；M2 再议自动移除                                                                                                                                                                                                                                                                                                                                               |
| Q9  | `apps/cloud`（TS，全 501）在 Go 控制面上线后的去留                                                                                                  | S9 把 `armadra-cloud` 镜像换成 Go；TS `apps/cloud` 在下一个版本删除（同 G4.1 的节奏），`control-store-postgres` 的迁移目录保留为唯一的源                                                                                                                                                                                                                                                                                        |
| Q10 | 团队删除的前提：契约写「没有在线主机」，能力设计写「先撤销全部主机或转走」                                                                          | 按契约：只要求没有在线主机；主机在恢复期内保留，清除时撤销；控制台在删除确认里列出将被撤销的主机                                                                                                                                                                                                                                                                                                                                |
| Q11 | 授予的有效到期没有带进断言，core 只靠 30 天租约                                                                                                     | 云端按到期时刻跑 `grant.expire`（撤销 + 踢人），足以做到准时失效；不为此改断言                                                                                                                                                                                                                                                                                                                                                  |
| Q12 | 审计按月分区由作业在运行时建分区，不经迁移                                                                                                          | 接受：分区是数据而不是结构；`audit_log_default` 兜底，非空即告警；`armadra-cloud migrate` 之后的第一次 `serve` 立即建当月与后两个月                                                                                                                                                                                                                                                                                             |
