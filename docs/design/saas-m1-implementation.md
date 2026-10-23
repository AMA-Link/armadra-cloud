# SaaS 控制面 M1 实施设计（G6-M1）

> 状态：已审（2026-10-23，PR #71 设计审查的决定已并入，§10 全部已定）。实施跟踪 AMA-Link/armadra-cloud#60（SaaS 能力）与 #59（Go 重写的 G6）。
>
> - 能力、角色、分期与规范细节以 [SaaS 能力设计](saas-design.md)（下称「能力设计」）为准；架构、目录、节点与部署拓扑以 [中继 Go 重写](go-relay-design.md)（下称「Go 设计」）为准；线上形状以契约 `cloud-api.md` §1–§32（协议 1.4）与协议包为准。
> - 本文只定**怎么落地**：进程与部署、迁移 DDL、Go 包与端口、HTTP 层、邮件与作业、跨节点约定、测试、分批。与上面三份冲突的地方已在 §10 定下，需要改协议与契约的项由协议补缺 PR「P1d」（AMA-Link/armadra-cloud#72）完成，本文引用的名字都以它为准。
> - 控制台（Armadra `apps/web`）的页面与批次见 Armadra `docs/design/cloud-console.md` §6、§7，本文 §9.3 给对应关系，§9.4 列 Armadra 要跟着做的事。

## §0 摘要

- **一个新二进制** `armadra-cloud`（`go/cmd/armadra-cloud`），业务在 `go/internal/cloud/*`，复用 `internal/core`（会话、签名、源、链接、me-stream）、`internal/store/postgres` 与 `internal/control/redis`；SaaS 专有的表经新端口 `internal/store/saas`，只有 PostgreSQL 实现。
- **业务上的租户是团队**（能力设计 §1）。**存储上**，SaaS 的全部行都在 `tenant_id = 'cloud'` 这一个存储租户里（issuer = `APP_ORIGIN`），团队之间的隔离靠 `team_id` 与编译期的 `TeamScope` 类型（§4.11）；个人中转的存储租户仍是 `default`。两种存储租户从不同库或同库不同 schema 读写，互不可见。
- **控制核不知道团队**：`internal/core` 只加接口（可见性与授权策略、源地址、会话的传输与范围、错误详情、事件受众、撤销钩子）。personal 用现有行为实现，这一步单独成批（S1a），先证明 personal 行为不变；SaaS 由 `internal/cloud` 实现。
- **与中继节点只经 Redis**，而且不手写线上格式：`cloud/relayctl` 包装 `control/redis.Plane`（控制面用自己的 `origin`），广播外壳与命令形状一律取 `control/cluster/wire.go`（§2.4）。撤销与签发索引另在 PostgreSQL 落盘，`serve` 启动与 Redis 重连时重放（§7.2）。
- **迁移一次落齐**：M1 要的表在 S0b 里以 6 个连续迁移落库（编号合并时取 main 最大号 +1 起），合并前由 Opus 做一次 DDL 冻结审查；后续各批只写代码，不再争号。
- **十三批**（§9）：S0a、S0b 并行打底；S1a 抽出访问策略、S1b 起服务与账号；之后按「邮件 / 设备码 / 团队 → 主机 → 授予与断言 → 分享与信任 / 审计、配额与运营 / 跨节点 → 部署与端到端」推进，每批 ≤ 约 1500 行（含测试）。S9 还要等 G3b → G4 → G5。

## §1 范围

### 1.1 M1 做（能力设计 §9 M1 的逐项落点）

| 能力                                                   | 过程（契约）                                                                                                                                                                                                                                                                        | 批        |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------- |
| 平台信息与 JWKS                                        | `platform.info`（`mode: "saas"`、`relay.addressing: "subdomain"`、`registrationsOpen`、`challenge`、`capabilities` 见 §5.3）、`platform.jwks`、`/health`、`/ready`                                                                                                                  | S1b       |
| 注册、登录、刷新、登出、改口令、提权                   | `auth.register`、`auth.login`、`auth.refresh`、`auth.logout`、`auth.changePassword`、`auth.stepUp`（只认 `password`，运营账号另认 `totpCode`）                                                                                                                                      | S1b       |
| 邮箱验证、找回口令、改邮箱、新设备提醒                 | `auth.emailVerify`、`auth.emailVerifyResend`、`auth.passwordResetStart/Finish`、`me.emailChange`                                                                                                                                                                                    | S2        |
| 设备码                                                 | `auth.deviceStart`、`auth.devicePoll`、`auth.deviceApprove`、`auth.deviceGet`、`auth.deviceDeny`                                                                                                                                                                                    | S3        |
| 账号与设备                                             | `me.get`（`teams`、`scopes`、`csrfToken`）、`me.update`、`me.devices`、`me.deviceRevoke`、`me.spaces`                                                                                                                                                                               | S1b、S4   |
| 团队                                                   | `teams.*` 全部 14 个                                                                                                                                                                                                                                                                | S4        |
| 团队邀请                                               | `invitations.*`（`kind = team`）                                                                                                                                                                                                                                                    | S4        |
| 主机                                                   | `sources.registrationToken`（`space`）、`sources.register`、`sources.heartbeatHint`、`sources.list`（筛选、分页）、`sources.get`、`sources.update`、`sources.revoke`、`me.sources`                                                                                                  | S5a       |
| 授予与访问清单                                         | `grants.list`、`grants.create`、`grants.update`、`grants.revoke`、`grants.effective`（主体 `team` / `member` / `guest`）、`sources.assertion`、`sources.accessList`、`sources.accessRevoke`                                                                                         | S5b       |
| 分享                                                   | `links.*`（加 `space`）、`invitations.*`（`kind = host`）                                                                                                                                                                                                                           | S6        |
| 新设备信任（云端只转通知、记拒绝）                     | `trust.list`、`trust.reject`；主机上报 `sources.trustRequest` / `sources.trustResolve`（契约 §31，#72；§4.7）                                                                                                                                                                       | S6        |
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

`auth.stepUp` M1 实现但不强制任何过程提权（控制台 M1 用输入确认，见 cloud-console D5）；M2 起的要求见 §10 Q7。

**新设备背书不在网页控制台做**：控制台由运营方下发，页面可以被篡改（能力设计 §11.13），背书只在桌面与原生 App 里做。控制台只列待信任请求、允许拒绝（拒绝只是云端记录），并引导到 App（§4.7、§9.4）。

## §2 进程与部署

### 2.1 命令行（`armadra-cloud`）

```text
用法: armadra-cloud <命令> [选项]

命令:
  serve            起控制面（HTTP、作业、Redis 订阅）
  migrate          执行数据库迁移（只加不改）
  keys rotate      轮换签名钥（新钥立即生效，旧钥 1 小时内仍在 JWKS 里）
  nodes token      给中继节点签节点令牌（--node <id> --days <n>，缺省 30）
  nodes revoke     作废一枚节点令牌（--jti <jti>）
  operators add    给已有账号加运营范围（--email <地址>）
  operators totp   为运营账号登记两步验证（在终端打印一次 otpauth 地址）
  health           探活（容器健康检查用）
  version          打印版本
```

| 规则     | 内容                                                                                                                                                                                                  |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 退出码   | 0 成功、64 用法错误、1 运行出错；参数优先于环境变量，环境变量优先于缺省（与 `armadra-relay` 相同）                                                                                                    |
| 迁移     | `serve` 不自动迁移：账本落后于二进制答「数据库版本落后，先运行 armadra-cloud migrate」并退出 1；账本比二进制新、未知或损坏同样退出 1（Go 设计 §3.4 的拒绝规则）                                       |
| 启动检查 | 依次：账本 → 租户引导 → 审计分区（§3.4 的 0008，同步建当月与后两个月，失败退出 1）→ Redis 连通 → 重放撤销与签发索引（§7.2）→ 开监听                                                                   |
| 租户引导 | 在 `pg_advisory_xact_lock` 下检查存储租户 `cloud`：不存在就建（issuer = 配置）并生成第一把签名钥（主密钥包裹）；存在而 issuer 与配置不同 → 「issuer 与库里的不一致」退出 1；绝不改写已有租户的 issuer |
| 关闭顺序 | 停收新请求 → me-stream 发关闭（1001）→ 等在途请求（10 秒）→ 停作业领取、等当前作业（30 秒）→ 刷 `lastSeen` → 关 Redis → 关 PostgreSQL                                                                 |
| 秘密     | 口令、令牌、主密钥、SMTP 口令都只从 `*_FILE` 或环境变量读，不进日志；CLI 打印的 otpauth 地址与节点令牌只出现在运维终端                                                                                |

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
| `REQUIRE_EMAIL_VERIFIED`                                                                      | `1`                                                                       | 未验证邮箱的门（§4.3）；只在开发环境设 `0`                            |
| `NAME`                                                                                        | 空                                                                        | `platform.info.name`                                                  |
| `WEB_ROOT`                                                                                    | 空                                                                        | Armadra `apps/web` 构建产物目录；空 = 不托管页面（`/` 答 404）        |
| `SMTP_HOST` / `SMTP_PORT` / `SMTP_USERNAME` / `SMTP_PASSWORD_FILE` / `SMTP_FROM` / `SMTP_TLS` | —, 587, —, —, 必填, `starttls`                                            | `SMTP_TLS` 取 `starttls` / `tls` / `none`；`none` 只许回环（mailpit） |
| `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_FILE` / `CHALLENGE_SCOPE`                            | 空 / 空 / `auth.login,auth.register,auth.passwordResetStart,links.accept` | 两者都配才要求挑战；只配一个是配错，退出 1                            |
| `TRUSTED_PROXY` / `TRUSTED_PROXY_PREFER`                                                      | 空 / `forwarded-for`                                                      | CIDR 列表；规则同 Go 设计 §3.5                                        |
| `HIBP_URL`                                                                                    | 空                                                                        | 可选的泄露口令查询（能力设计 §11.8）                                  |
| `JOB_WORKERS`                                                                                 | `2`                                                                       | 作业并发                                                              |
| `TRAFFIC_RETENTION_DAYS`                                                                      | `400`                                                                     | 流量桶保留期，由控制面的作业清理（§6.1）；节点不再自己清              |
| `METRICS_LISTEN` / `METRICS_TOKEN_FILE`                                                       | 空                                                                        | 同中继                                                                |
| `LOG_LEVEL`                                                                                   | `info`                                                                    | —                                                                     |

- 两个最低 core 版本是**协议常量**（#72），不是环境变量：`MIN_CORE_VERSION_TEAM_ASSERTION`（带 `team` 的断言）与 `MIN_CORE_VERSION_ROLE_ONLY_ASSERTION`（只带 `role` / `scp`、不带 `team` 的断言），在协议包 `version.ts` 与 `schema/constants.json` 的 `minCoreVersion`，Go 里是 `gen.MinCoreVersionTeamAssertion` / `gen.MinCoreVersionRoleOnlyAssertion`；现在都是占位 `0.10.0`，发版时填定。
- `OAUTH_<ID>_*` 等 M2 变量 M1 不解析；出现未知的 `ARMADRA_CLOUD_*` 变量只记一条警告。

### 2.3 单机 M1 拓扑（`deploy/saas/compose.yml`，S9）

| 服务                 | 镜像 / 命令                                        | 说明                                                                                                                                                        |
| -------------------- | -------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `proxy`              | 反向代理                                           | 终止 TLS：`app.<域>`、`a.edge.<域>`、`b.edge.<域>`、`*.src.<域>`；WebSocket 设置按 Go 设计 §3.10；访问日志**不记 query 串**（`$uri` 而不是 `$request_uri`） |
| `cloud`              | `armadra-cloud serve`（先跑一次 `migrate` 的 job） | `app.<域>` → `cloud:8100`                                                                                                                                   |
| `relay-a`、`relay-b` | `armadra-relay saas serve`（G5）                   | **每个节点有自己的隧道地址** `wss://<node>.edge.<域>/t/v1`，写进节点表（§2.4），不在一个共用的 `edge.` 后面轮询；`*.src.<域>` 可以落任一节点（节点间转发）  |
| `postgres`           | `postgres:17`                                      | 一个库：cloud 用 schema `public`；节点用独立角色 `armadra_relay_traffic`（§10 Q4）                                                                          |
| `redis`              | `redis:8`                                          | `appendonly yes`、`appendfsync everysec`、`maxmemory 128mb`、`maxmemory-policy noeviction`                                                                  |
| `push-relay`         | Armadra `apps/push-relay`                          | 能力设计 §11.11                                                                                                                                             |

- 现有 `deploy/compose.yml` 的 `cloud` 服务（TS 镜像、`SIGNING_KEY_FILE`）在 S9 换成 Go 镜像与本表的变量；签名钥改为库里主密钥包裹（能力设计 §4），不再挂文件。
- `armadra_relay_traffic` 角色由 compose 的初始化脚本与 runbook 建（不进迁移）：`relay_traffic_buckets` 的 `SELECT` / `INSERT` / `UPDATE`，`cloud_migrations` 的 `SELECT`（节点启动时核对账本），别的表没有任何权限。
- **长期**：源地址 `*.src.` 与控制台同在一个可注册域下，Cookie 层面同站；以后把源地址迁到独立的可注册域（如 `armadra-src.net`），届时 §5.2 的 Origin 检查仍保留。

### 2.4 控制面与中继节点的约定（Redis，规范）

**不手写线上格式。** `cloud/relayctl` 持有一个 `control/redis.Plane`（前缀 `relay:`，`Origin = cloud/<实例 id>`），路由读取、撤销（`Tokens().Revoke`）、命令（`Command`）、在线事件订阅都经它；广播外壳 `{origin, kind, event|command|jti+expMs}` 与命令形状以 `control/cluster/wire.go` 为准。下表新增的字段与种类先在 `wire.go` 里加（S8 与 G5 共用同一份），老节点按现有解析规则忽略不认识的字段与种类。

| 键 / 频道                                       | 写方                   | 读方         | 形状与规则                                                                                                                                                                                                                                                                     |
| ----------------------------------------------- | ---------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `relay:route:src:<id>`                          | 节点                   | 节点、控制面 | 现有路由（`cluster.EncodeRoute`）；控制面只读：在线判定、**隧道上报的 `coreVersion`**、持有节点                                                                                                                                                                                |
| `relay:presence`                                | 节点                   | 节点、控制面 | 现有 `kind: presence` 广播；控制面据此更新 `sources.last_seen_at_ms` 并发 me-stream 的上下线                                                                                                                                                                                   |
| `relay:revoked:<jti>` + `relay:revoked`         | 控制面                 | 节点         | 现有：`Plane.Tokens().Revoke(jti, exp)`（`SET … PXAT` + `kind: revoked` 广播）；同时落 PostgreSQL（§7.2）                                                                                                                                                                      |
| `relay:ctl`                                     | 控制面                 | 节点         | 现有 `kind: command`，`commandWire = {type, sourceId, reason}`，**`sourceId` 必填**（现有解析器拒绝没有它的命令）。G5（N2）给 `commandWire` 加可选的 `subject`、`subjectPrefix`、`deviceId` 与新种类 `drain`；老节点忽略这些字段、按 `sourceId` 踢掉该源全部连接（多踢不漏踢） |
| `relay:node:<id>`                               | 控制面                 | 该节点       | **G5 之后才用**，与 `relay:ctl` 同形；发到持有节点时**同时**照样广播，不替代广播                                                                                                                                                                                               |
| `relay:ctl` 的 `kind: quota` / `kind: incident` | 控制面                 | 节点         | G5（N4）新增的广播种类，载荷放在外壳的 `event` 里：`{teamId, concurrent}` 与 `{rejectNewConnections, atMs}`（`cluster.EncodeQuota` / `EncodeIncident`）；与下两行的键一起写                                                                                                    |
| `relay:quota:team:<teamId>`                     | 控制面                 | 节点         | `{concurrent}`：`edge.concurrent.team` 的上限。节点在订阅就绪时 `SCAN` 全量读、之后按 `kind: quota` 广播增量更新；缺键或坏了用节点缺省（最严格的套餐，§7.2）                                                                                                                   |
| `relay:incident`                                | 控制面                 | 节点         | `{rejectNewConnections, atMs}`；节点在订阅就绪时读、之后按 `kind: incident` 更新；开着时拒绝新的隧道与 e2e 升级（4429 `limit_reached`），在线连接保留                                                                                                                          |
| `relay:nodes`（哈希）                           | 节点（每 20 秒）       | 控制面       | 字段 = 节点 id，值 `{url, weight, version, draining, connections, tunnels, internodeUrl?}`，`url` 是该节点自己的隧道地址，`internodeUrl` 是节点间转发连的 `/x/v1`（可以缺）；每个字段 `HPEXPIRE 60000`；编解码 `cluster.EncodeNode` / `DecodeNode`（Go 设计 §3.6）             |
| `relay:jwks:<sha256(issuer)>`                   | 控制面（轮换后）、节点 | 节点         | 共享 JWKS 缓存（Go 设计 §3.6）；`keys rotate` 后控制面直接覆盖                                                                                                                                                                                                                 |
| `relay:conc:team:<teamId>`                      | 节点                   | 节点         | G5（N4）：`edge.concurrent.team` 的跨节点计数，有序集合，成员 `<节点>:<随机>`、分数是到期毫秒（Go 设计 §3.6）；控制面不读不写                                                                                                                                                  |
| `cloud:issued:acct:<accountId>`                 | 控制面                 | 控制面       | 有序集合，成员 `<accountId>\|<sourceId>\|<deviceId>\|<jti>`，分数 = `exp` 毫秒；写前 `ZREMRANGEBYSCORE -inf <now>`，`PEXPIRE 3600000`（能力设计 §11.7）；PostgreSQL 里有同一份（§7.2）                                                                                         |
| `cloud:issued:src:<sourceId>`                   | 控制面                 | 控制面       | 同上；隧道令牌也记在这里（`accountId = -`）                                                                                                                                                                                                                                    |
| `cloud:issued:jti:<jti>`                        | 控制面                 | 控制面       | S8 新增：`SET … PXAT exp`，值是上面的成员串；只知道 jti 的撤销（签发方复核）按它找回条目（§7.4）                                                                                                                                                                               |
| `cloud:events`                                  | 控制面各实例           | 控制面各实例 | 领域事件 `{origin, event, audience}`，见 §7.1                                                                                                                                                                                                                                  |
| `cloud:ticket:<sha256>`                         | 控制面                 | 控制面       | me-stream 票（30 秒、`GETDEL` 一次性），值 `{accountId, deviceId}`                                                                                                                                                                                                             |
| `cloud:mfa:<sha256>`                            | 控制面                 | 控制面       | 运营登录的 MFA 挑战（5 分钟），值 `{accountId, attempts}`；`auth.mfaVerify` 每次失败 `HINCRBY`，到 5 次删键                                                                                                                                                                    |
| `cloud:limit:<种类>:<sha256>`                   | 控制面                 | 控制面       | 速率限流桶。把 `control/redis` 里的限流脚本与 `remoteHit` 抽成可单独构造的 `redis.NewLimiter(client, prefix, rules)`（S1b），控制面用前缀 `cloud:limit:` 自己建一个，不借第二个 `Plane`                                                                                        |

- **跨主机的踢人**（账号停用、设备撤销等，§4.8）：不发不带 `sourceId` 的 `kick`。控制面从 `cloud:issued:acct:<id>` 取出这个账号有令牌的全部主机，逐个发 `kick {sourceId, subject[, deviceId]}`——N2 起的节点按人精确踢，老节点也能执行（退化为踢该源全部连接）。
- **节点令牌**（契约 §30，#72）：`armadra-cloud nodes token --node a` 用租户签名钥签头 `typ = armadra-node`（`gen.TokenTypeNode`）、声明 `{iss, sub: "a", aud: "relay-node", iat, exp, jti: "n.<nodeId>.<随机>", ver}` 的 JWS（`nodeTokenClaimsSchema`，`aud` 即 `gen.TokenAudienceRelayNode`；`sub` 是节点名，`^[a-z0-9][a-z0-9-]{0,62}$`），缺省 30 天。节点之间 `/x/v1` 升级时出示，持有节点按 JWKS 验签、`typ`、`aud` 与撤销表（Go 设计 §3.6）；M1 控制面自己不收节点令牌。`nodes revoke` 走普通撤销（§7.2）。

## §3 数据模型

### 3.1 存储租户与空间

| 项       | 规则                                                                                                                                                                                    |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 存储租户 | 业务租户是团队（能力设计 §1）；存储上 SaaS 的行都在 `tenant_id = 'cloud'`（`tenants.issuer` = `ISSUER`）。所有 SaaS 表都带 `tenant_id` 并参与外键与主键，每条 SQL 都写 `tenant_id = $1` |
| 团队隔离 | 团队范围的仓储方法只收 `saas.TeamScope`（§4.11）：这个值只能由 `teams.Require` 在核过成员与角色之后构造，编译期就排除了「拿调用方给的 teamId 直接查」                                   |
| 个人中转 | 存储租户仍是 `default`；`--store postgres` 的个人中转会跑到同一套迁移，SaaS 表在它的 schema 里建出来但始终为空（S0b 的 CHANGELOG 写明）                                                 |
| 账号 id  | SaaS 账号 `account_id` = UUIDv4 小写；`accounts.name` 也写成这个 uuid（满足现有 `NOT NULL` 与唯一索引，登录按邮箱，不按 `name`）                                                        |
| 其余 id  | 团队、邀请、授予、信任请求、审计事件：UUIDv4 小写文本；`sourceId` 沿用 32 位 hex                                                                                                        |
| 空间     | 个人空间 = `(personal, account_id)`，团队空间 = `(team, team_id)`；主机的空间记在 `source_spaces.team_id`（NULL = 个人空间，归 `owner_account_id`）                                     |
| 现有源   | 个人中转的源没有 `source_spaces` 行，语义就是「个人空间、租户唯一账号所有」，不做数据迁移（能力设计 §5 0006 的「迁成 personal」由此满足）                                               |
| 邮箱     | 一律存小写（表上有 `CHECK (email = lower(email))`），比较与唯一索引都按原值                                                                                                             |

### 3.2 为什么用扩展表、为什么外键要延迟检查

现有端口 `SourceStore.Put` 在 PostgreSQL 里是同一事务内「删了再插」（保持创建顺序，`go/internal/store/postgres/ports.go`），所以：

- 给 `sources` 加列会在每次重新登记时被清掉 → 主机的空间、所有者、标签、设置放在 1:1 的 `source_spaces`。
- 引用 `sources` 的外键（`source_spaces`、`source_grants`、`trust_requests`、`invitations`）一律 `DEFERRABLE INITIALLY DEFERRED`、动作 `NO ACTION`（**不用 `ON DELETE CASCADE`**，级联不随延迟推迟）：同一事务里删了再插，提交时行已经回来，检查通过；真删源的路径（团队清除）先删子行。
- `accounts`、`devices` 的端口是原地 `UPDATE` 列名单，新增列不会被核心写坏，可以直接加列。

### 3.3 迁移清单

编号规则：M1 的六个迁移在 S0b 一次落库，合并时取 main 最大号 +1 起连续编号（下表按 main 现在的 0003 写）；进 `migrations.lock`；每个迁移同 PR 带测试；S0b 合并前由 Opus 做 DDL 冻结审查；已发布的不改，M1 内要改就再加一个迁移。SQLite 侧没有 SaaS 迁移。

| 编号（暂定） | 文件                     | 内容                                                                                                                                            |
| ------------ | ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| 0004         | `0004_saas_accounts.sql` | `accounts`、`devices` 加列；`account_credentials`、`email_tokens`、`device_codes`                                                               |
| 0005         | `0005_saas_teams.sql`    | `teams`、`team_members`、至少一个 owner 的约束触发器、`groups`、`group_members`（M2 用）                                                        |
| 0006         | `0006_saas_hosts.sql`    | `source_spaces`、`registration_tokens.team_id`、`entitlements`                                                                                  |
| 0007         | `0007_saas_grants.sql`   | `source_grants`、`invitations`（团队与主机两类）、`trust_requests`                                                                              |
| 0008         | `0008_saas_audit.sql`    | `audit_log`（按月分区）与分区维护函数                                                                                                           |
| 0009         | `0009_saas_ops.sql`      | `jobs`、`outbox_emails`、`platform_settings`、`cloud_issued_tokens`；`relay_traffic_buckets.team_id`（撤销沿用 0002 的 `relay_revoked_tokens`） |

M1 不建（M2 起各自加迁移）：`account_recovery_codes`、`access_requests`、`source_transfers`、`plans`、`subscriptions`、`usage_monthly`、`invoices`、`api_tokens`、`feature_flags`、`abuse_reports`。能力设计 §11.8 的 `account_lockouts` 不另建：沿用 `accounts.failures / locked_until_ms / last_failure_at_ms`，与个人中转同一套锁定规则。

### 3.4 DDL（规范）

迁移文件不写事务语句（迁移器整体包事务）；注释用简体中文。

```sql
-- 0004_saas_accounts.sql：SaaS 账号（存储租户 cloud）。个人中转的行这些列取缺省、不受影响。
ALTER TABLE accounts
  ADD COLUMN email text CHECK (email = lower(email)),
  ADD COLUMN email_verified_at_ms bigint,
  ADD COLUMN display_name text NOT NULL DEFAULT '',
  ADD COLUMN locale text NOT NULL DEFAULT 'zh-CN' CHECK (locale IN ('zh-CN', 'en')),
  ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  ADD COLUMN status_reason text,
  -- 特殊范围（如运营方的 operator）；me.get.scopes。
  ADD COLUMN scopes text[] NOT NULL DEFAULT '{}';

CREATE UNIQUE INDEX accounts_email ON accounts (tenant_id, email) WHERE email IS NOT NULL;

-- 会话的传输（bearer：原生、core、CLI；cookie：控制台）与范围（session：普通；operator：运营，
-- 1 小时绝对寿命）。两者正交：Bearer 的运营会话照样能调 admin.*（契约 §28）。
ALTER TABLE devices
  ADD COLUMN transport text NOT NULL DEFAULT 'bearer' CHECK (transport IN ('bearer', 'cookie')),
  ADD COLUMN scope text NOT NULL DEFAULT 'session' CHECK (scope IN ('session', 'operator')),
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
  -- TOTP：最后一次用过的时间步，同一步不重用。
  last_step bigint,
  last_used_at_ms bigint,
  PRIMARY KEY (tenant_id, credential_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX account_credentials_account ON account_credentials (tenant_id, account_id, kind);
-- 一个账号至多一个 TOTP。
CREATE UNIQUE INDEX account_credentials_totp ON account_credentials (tenant_id, account_id) WHERE kind = 'totp';

-- 邮件里的一次性令牌：只存哈希；verify 24 小时，reset 30 分钟，change 24 小时。
CREATE TABLE email_tokens (
  tenant_id text NOT NULL,
  token_hash text NOT NULL,
  account_id text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('verify', 'reset', 'change')),
  -- 发往的地址；change 是新地址，确认时据此改邮箱。
  email text NOT NULL CHECK (email = lower(email)),
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
-- 0005_saas_teams.sql：团队是业务租户（能力设计 §1）。分组的表 M1 建好、M2 起使用。
CREATE TABLE teams (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  team_id text NOT NULL,
  slug text NOT NULL CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$'),
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
-- 读已提交下两个事务各降一个 owner 会写偏斜：触发器先锁团队行，再用新快照查，后提交的那个必然看到
-- 前一个的结果而失败。服务层另外在改成员前先 SELECT … FOR UPDATE 团队行，并先查后答 409
-- team_last_owner；这里是兜底，违反时 SQLSTATE 23514、约束名 team_has_owner。
-- UPDATE 时新旧两个团队都查（team_id 本不该被改，改了也守得住）。
CREATE FUNCTION team_owner_check(t_tenant text, t_team text) RETURNS void LANGUAGE plpgsql
SET search_path FROM CURRENT AS $$
BEGIN
  PERFORM 1 FROM teams WHERE tenant_id = t_tenant AND team_id = t_team FOR UPDATE;
  IF FOUND AND NOT EXISTS (SELECT 1 FROM team_members
                           WHERE tenant_id = t_tenant AND team_id = t_team AND role = 'owner') THEN
    RAISE EXCEPTION 'team has no owner' USING ERRCODE = '23514', CONSTRAINT = 'team_has_owner';
  END IF;
END
$$;

CREATE FUNCTION team_owner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path FROM CURRENT AS $$
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') AND TG_TABLE_NAME = 'team_members' THEN
    PERFORM team_owner_check(OLD.tenant_id, OLD.team_id);
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') THEN
    PERFORM team_owner_check(NEW.tenant_id, NEW.team_id);
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
  -- 给 group_members 的复合外键用：组员的 team_id 必须等于组所属的团队。
  UNIQUE (tenant_id, team_id, group_id),
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
  FOREIGN KEY (tenant_id, team_id, group_id) REFERENCES groups (tenant_id, team_id, group_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, team_id, account_id)
    REFERENCES team_members (tenant_id, team_id, account_id) ON DELETE CASCADE
);
```

```sql
-- 0006_saas_hosts.sql：主机的空间与配额。引用 sources 的外键延迟到提交时检查（Put 是同事务删了再插，设计 §3.2）。
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
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
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
  -- 撤销与到期都写这里（到期由 grant.expire 写，revoked_by = 'system'）。
  revoked_at_ms bigint,
  revoked_by text,
  PRIMARY KEY (tenant_id, grant_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  -- team 主体就是主机所属的团队；team / member 主体只出现在团队主机上。
  CHECK (subject_kind <> 'team' OR subject_id = team_id),
  CHECK (subject_kind NOT IN ('team', 'member') OR team_id IS NOT NULL)
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
  email text NOT NULL CHECK (email = lower(email)),
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
  -- 主机邀请被接受后授予的到期；NULL = 不过期。与邀请自身的有效期（expires_at_ms）无关。
  grant_expires_at_ms bigint,
  decided_at_ms bigint,
  sent_at_ms bigint,
  resend_count integer NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, invitation_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  CHECK (kind <> 'team' OR (team_id IS NOT NULL AND team_role IS NOT NULL AND source_id IS NULL
                            AND host_role IS NULL AND cardinality(workspace_ids) = 0)),
  CHECK (kind <> 'host' OR (source_id IS NOT NULL AND host_role IS NOT NULL AND team_role IS NULL))
);

CREATE UNIQUE INDEX invitations_pending_team ON invitations (tenant_id, team_id, email)
  WHERE kind = 'team' AND state = 'pending';
CREATE UNIQUE INDEX invitations_pending_host ON invitations (tenant_id, source_id, email)
  WHERE kind = 'host' AND state = 'pending';
CREATE INDEX invitations_received ON invitations (tenant_id, email) WHERE state = 'pending';
CREATE INDEX invitations_team ON invitations (tenant_id, team_id, created_at_ms DESC);
CREATE UNIQUE INDEX invitations_token ON invitations (tenant_id, token_hash) WHERE token_hash IS NOT NULL;

-- 新设备待信任（能力设计 §11.4）：主机上报（sources.trustRequest），云端只存通知所需的元数据，没有校验码。
CREATE TABLE trust_requests (
  tenant_id text NOT NULL,
  request_id text NOT NULL,
  source_id text NOT NULL,
  -- 主机侧的待信任 id，作幂等键：同一主机同一 pending_id 重报答原请求。
  pending_id text NOT NULL,
  team_id text,
  account_id text NOT NULL,
  -- 断言里的 device.deviceId（云端设备）；设备名与平台由云端从 devices 表填，不信主机报的。
  device_id text NOT NULL,
  device_name text NOT NULL,
  platform text NOT NULL,
  state text NOT NULL CHECK (state IN ('pending', 'trusted', 'rejected', 'expired')),
  requested_at_ms bigint NOT NULL,
  -- 不超过请求时刻 + 24 小时。
  expires_at_ms bigint NOT NULL CHECK (expires_at_ms <= requested_at_ms + 86400000),
  resolved_at_ms bigint,
  resolved_by text,
  PRIMARY KEY (tenant_id, request_id),
  FOREIGN KEY (tenant_id, source_id) REFERENCES sources (tenant_id, source_id) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY (tenant_id, account_id) REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX trust_requests_idem ON trust_requests (tenant_id, source_id, pending_id);
CREATE UNIQUE INDEX trust_requests_pending ON trust_requests (tenant_id, source_id, device_id)
  WHERE state = 'pending';
-- 拒绝后的冷却期按 (source, device) 查最近一次 rejected。
CREATE INDEX trust_requests_device ON trust_requests (tenant_id, source_id, device_id, resolved_at_ms DESC);
CREATE INDEX trust_requests_team ON trust_requests (tenant_id, team_id, state);
```

```sql
-- 0008_saas_audit.sql：团队审计与账号安全日志（一张表）。只记主体、动作、对象、时间、jti、IP 前缀与运营理由。
-- 按月分区：serve 启动时同步建当月与后两个月（失败拒绝启动），每天的作业再往后补；
-- 保留期按计划逐团队删，账号安全日志固定 90 天；超过 13 个月的整月分区直接 DROP。
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
  -- 涉及的主机（audit.list 的 sourceId 筛选用）。
  source_id text,
  jti text,
  ip_prefix text,
  -- 设备名快照（me.securityLog 的 deviceName；设备改名或删除后日志里仍是当时的名字）。
  device_name text CHECK (char_length(device_name) <= 128),
  -- 运营写操作的理由（契约 §28 必填）；其余为 NULL。
  reason text CHECK (char_length(reason) <= 500),
  PRIMARY KEY (at_ms, event_id)
) PARTITION BY RANGE (at_ms);

-- 兜底分区：正常情况下恒为空（见 audit_ensure_partition）。
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;

CREATE INDEX audit_log_team ON audit_log (tenant_id, team_id, at_ms DESC) WHERE team_id IS NOT NULL;
CREATE INDEX audit_log_account ON audit_log (tenant_id, account_id, at_ms DESC) WHERE account_id IS NOT NULL;
CREATE INDEX audit_log_actor ON audit_log (tenant_id, actor_kind, actor_id, at_ms DESC);
CREATE INDEX audit_log_target ON audit_log (tenant_id, target_kind, target_id, at_ms DESC);
CREATE INDEX audit_log_source ON audit_log (tenant_id, team_id, source_id, at_ms DESC) WHERE source_id IS NOT NULL;

-- 建一个月的分区（幂等）。分区名必须是 audit_log_YYYY_MM，边界必须是该 UTC 月的月初与下月月初，否则拒绝（22023）。
-- 兜底分区里若已有这个月的行：在同一事务里先把兜底分区摘下，建新分区，把那些行搬过去，再把兜底分区挂回。
-- 调用方持 pg_advisory_xact_lock，并先 SET LOCAL lock_timeout（摘 / 挂分区要父表的排他锁，见 §6）。
CREATE FUNCTION audit_ensure_partition(month_start_ms bigint, month_end_ms bigint, part_name text)
RETURNS void LANGUAGE plpgsql
SET search_path FROM CURRENT AS $$
DECLARE
  m timestamp := date_trunc('month', to_timestamp(month_start_ms / 1000.0) AT TIME ZONE 'UTC');
BEGIN
  IF month_start_ms <> (extract(epoch FROM m) * 1000)::bigint
     OR month_end_ms <> (extract(epoch FROM m + interval '1 month') * 1000)::bigint
     OR part_name <> 'audit_log_' || to_char(m, 'YYYY_MM') THEN
    RAISE EXCEPTION 'invalid audit partition' USING ERRCODE = '22023';
  END IF;
  IF to_regclass(part_name) IS NOT NULL THEN
    RETURN;
  END IF;
  IF EXISTS (SELECT 1 FROM audit_log_default WHERE at_ms >= month_start_ms AND at_ms < month_end_ms) THEN
    ALTER TABLE audit_log DETACH PARTITION audit_log_default;
    EXECUTE format('CREATE TABLE %I PARTITION OF audit_log FOR VALUES FROM (%s) TO (%s)',
                   part_name, month_start_ms, month_end_ms);
    EXECUTE format('INSERT INTO %I SELECT * FROM audit_log_default WHERE at_ms >= %s AND at_ms < %s',
                   part_name, month_start_ms, month_end_ms);
    DELETE FROM audit_log_default WHERE at_ms >= month_start_ms AND at_ms < month_end_ms;
    ALTER TABLE audit_log ATTACH PARTITION audit_log_default DEFAULT;
  ELSE
    EXECUTE format('CREATE TABLE %I PARTITION OF audit_log FOR VALUES FROM (%s) TO (%s)',
                   part_name, month_start_ms, month_end_ms);
  END IF;
END
$$;
```

```sql
-- 0009_saas_ops.sql：作业队列（SKIP LOCKED）、邮件发件箱、平台设置、签发索引；流量桶加团队维度。
-- 撤销表沿用 0002 的 relay_revoked_tokens（只有 jti 与到期时间）。
CREATE TABLE jobs (
  job_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id text NOT NULL,
  kind text NOT NULL,
  -- 只有标识与参数，不含令牌、口令与内容。
  payload jsonb NOT NULL,
  -- 同一 kind 下未完成的作业按它去重（例如 team.purge 按团队）。
  dedupe_key text,
  -- 定时作业的时间槽（例如 audit.retention:2026-10-23）：不论完成与否都唯一，同一槽只跑一次。
  slot_key text,
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
-- 已完成 / 已失败作业的清理（jobs.gc）按时间扫这两个索引。
CREATE INDEX jobs_done ON jobs (done_at_ms) WHERE done_at_ms IS NOT NULL;
CREATE INDEX jobs_failed ON jobs (failed_at_ms) WHERE failed_at_ms IS NOT NULL;
CREATE UNIQUE INDEX jobs_dedupe ON jobs (tenant_id, kind, dedupe_key)
  WHERE dedupe_key IS NOT NULL AND done_at_ms IS NULL AND failed_at_ms IS NULL;
CREATE UNIQUE INDEX jobs_slot ON jobs (tenant_id, kind, slot_key) WHERE slot_key IS NOT NULL;

-- 发件箱：业务事务里插一行 + 一个 mail.send 作业；发信成功记 sent_at_ms，重试前先看它（幂等）。
-- 含收件地址（个人信息）：发出或失败满 30 天由 outbox.gc 删除。
CREATE TABLE outbox_emails (
  tenant_id text NOT NULL,
  email_id text NOT NULL,
  job_id bigint REFERENCES jobs (job_id) ON DELETE SET NULL,
  template text NOT NULL,
  locale text NOT NULL CHECK (locale IN ('zh-CN', 'en')),
  to_email text NOT NULL CHECK (to_email = lower(to_email)),
  -- 渲染参数：标识与显示名；需要一次性令牌的模板在发信时现生成（§6.3），这里没有令牌。
  params jsonb NOT NULL,
  message_id text NOT NULL,
  created_at_ms bigint NOT NULL,
  sent_at_ms bigint,
  failed_at_ms bigint,
  PRIMARY KEY (tenant_id, email_id)
);

CREATE INDEX outbox_emails_gc ON outbox_emails (created_at_ms);
-- 删作业时 ON DELETE SET NULL 要按 job_id 找发件箱行。
CREATE INDEX outbox_emails_job ON outbox_emails (job_id) WHERE job_id IS NOT NULL;

-- 平台级开关（事故开关等），运营后台写；键见设计 §4.9。
CREATE TABLE platform_settings (
  tenant_id text NOT NULL REFERENCES tenants (tenant_id),
  key text NOT NULL,
  value jsonb NOT NULL,
  updated_at_ms bigint NOT NULL,
  updated_by text,
  PRIMARY KEY (tenant_id, key)
);

-- 已签令牌的索引（Redis cloud:issued:* 的落盘副本，§7.2）：只有标识，没有令牌本身。
CREATE TABLE cloud_issued_tokens (
  tenant_id text NOT NULL,
  jti text NOT NULL,
  account_id text NOT NULL,
  source_id text NOT NULL,
  device_id text NOT NULL,
  exp_ms bigint NOT NULL,
  PRIMARY KEY (tenant_id, jti)
);

CREATE INDEX cloud_issued_tokens_account ON cloud_issued_tokens (tenant_id, account_id, exp_ms);
CREATE INDEX cloud_issued_tokens_source ON cloud_issued_tokens (tenant_id, source_id, exp_ms);
CREATE INDEX cloud_issued_tokens_exp ON cloud_issued_tokens (tenant_id, exp_ms);

-- 流量按团队计（能力设计 §11.14）：入口节点写中继令牌的 team；个人空间为空串。
-- 不进主键：同一小时同一主机换团队只在转移（M2）时发生，按首次写入的团队计。
ALTER TABLE relay_traffic_buckets ADD COLUMN team_id text NOT NULL DEFAULT '';
CREATE INDEX relay_traffic_team ON relay_traffic_buckets (tenant_id, team_id, bucket_start_ms)
  WHERE team_id <> '';
```

### 3.5 迁移测试（S0b 必带）

| 测试                                                                                                                                                | 断言                                    |
| --------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| 空库连续跑 0001–0009，再对 0003 的库（`go/testdata/compat` 的 PG 转储）跑 0004–0009                                                                 | 账本连续；老行取缺省；TS 版存储仍能读写 |
| `team_owner_guard`：删唯一 owner、把唯一 owner 降级、建团队不插 owner 都在提交时失败；同一事务先升后降成功；两个并发事务各降一个 owner 恰好一个失败 | SQLSTATE 23514、约束名 `team_has_owner` |
| 延迟外键：端口的 `Sources.Put`（删了再插）在有 `source_spaces` / 授予时照常成功；真删源而子行还在时提交失败                                         | —                                       |
| 各 CHECK 与部分唯一索引（待处理邀请、有效授予、`user_code`、邮箱小写、信任请求 24 小时）                                                            | 违反即拒                                |
| `audit_ensure_partition`：幂等；兜底分区里已有该月的行时搬进新分区、兜底分区随后为空且仍挂着                                                        | —                                       |
| `jobs_slot`：同一槽在作业完成后再插仍冲突；`outbox_emails.job_id` 在作业被删后置空                                                                  | —                                       |
| `devices.transport / scope`、`accounts.*` 新列不被现有端口的 `UPDATE` 改写（端口合同 15 例照旧过）                                                  | —                                       |
| `group_members` 跨团队：把别团队的成员或别团队的组塞进来被拒；组员随成员 / 组 / 团队删除级联                                                        | SQLSTATE 23503                          |
| 授予 / 邀请的新 CHECK（team 主体 = 团队、team / member 主体必有 `team_id`、团队邀请不带主机角色与工作区）；`account_credentials_totp` 每账号一个    | 违反即拒                                |
| `audit_ensure_partition` 拒绝错误的分区名与月边界；函数带固定 `search_path`；`audit_log.device_name` 长度上限、`source_id` 索引                     | SQLSTATE 22023 / 23514                  |
| `outbox_emails_job`、`jobs_done`、`jobs_failed`、`audit_log_source` 存在；`cloud_issued_tokens` 主键含 `tenant_id`                                  | —                                       |

## §4 Go 包设计

### 4.1 目录与边界

```text
go/cmd/armadra-cloud/        命令行（§2.1）
go/internal/store/saas/      SaaS 端口、记录与 TeamScope（无驱动）
go/internal/store/postgres/  现有实现 + saas_*.go（实现 saas.Store，复用连接池、事务与 rows.go）
go/internal/cloud/
  config/      环境变量解析（§2.2）
  server/      装配：存储、Redis、core.Context、各服务、HTTP、作业；serve 的生命周期
  httpx/       Cookie 会话、CSRF、Origin、CORS、限流与挑战、客户端 IP、错误映射（§5）
  accounts/    注册、登录、会话、邮箱验证、找回、改邮箱、提权、设备码、me.*
  teams/       团队、成员、设置、删除与恢复、所有权转移；TeamScope 的唯一构造点
  invitations/ 团队与主机邀请
  hosts/       登记到空间、目录、改名与标签、设置、撤销
  grants/      授予、有效权限、断言合成（AccessPolicy 的 SaaS 实现）
  links/       空间维度的链接（包一层 core.Links）
  trust/       待信任请求
  audit/       写审计、安全日志、查询、保留期、分区
  quota/       计划额度、用量统计、limit_reached
  mail/        SMTP 适配器、模板、发件
  jobs/        作业队列、调度、各类作业的注册表
  admin/       运营后台、事故开关、运营 TOTP
  events/      领域事件、受众、Redis 广播、me-stream 过滤
  relayctl/    与中继节点的约定（§2.4）：包装 control/redis.Plane；签发索引、撤销（含落盘与重放）、踢人、关隧道、节点表、额度
```

- `repo.rules.json` 的 `goModules.internalBoundaries` 加：`internal/cloud/**` 不 import `internal/tunnel`、`internal/edge`、`internal/netpoll`、`internal/dataplane`、`internal/personal`；`internal/core`、`internal/store/**`、`internal/control/**` 不 import `internal/cloud/**`。
- 新依赖只有 G3a 已登记的 `github.com/redis/go-redis/v9`；SMTP 用标准库 `net/smtp` + `crypto/tls`，TOTP 用标准库 HMAC，模板用 `text/template` / `html/template`。

### 4.2 `internal/core` 的扩展（只加接口，personal 行为不变）

`AccessPolicy` 的抽出单独成批（S1a），只动 personal：把现有可见性与管理权判断原样搬进 `personalPolicy`，15 + 19 例合同、`pnpm e2e --mode personal` 全过才算完成。其余各项在 S1b 加。

| 扩展             | 形状                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | personal 实现                       | SaaS 实现                               | 批  |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- | --------------------------------------- | --- |
| 访问策略         | `Context.Policy core.AccessPolicy`（下方代码）；`Sources.List/Get/Assertion`、`Links.Create/List/Update/Revoke`、`MeStream.sees` 都改为经它                                                                                                                                                                                                                                                                                                                                                                      | 现有逻辑原样搬进 `personalPolicy`   | `grants.Policy`（§4.6）                 | S1a |
| 源地址           | `Context.RelayOrigin func(sourceID) string`、`Context.RelayBaseURL func(sourceID) string`；`TunnelURL` 改成 `TunnelNodes func(ctx, sourceID) ([]TunnelNode, error)`                                                                                                                                                                                                                                                                                                                                              | issuer、`issuer/s/<id>`、单节点     | `https://<id>.src.<域>`、节点表（§7.3） | S1a |
| 登记拆分         | 导出纯函数 `core.BuildSourceRecord(existing, input, nowMs)`（`source_taken`、协议版本与字段合并），`Sources.Register` 改为调用它                                                                                                                                                                                                                                                                                                                                                                                 | 原逻辑                              | `hosts` 在自己的事务里写                | S1a |
| 会话的传输与范围 | `store.DeviceRecord` 加 `Transport`、`Scope`、`ElevatedUntilMs`（`omitempty`，PG 读写，SQLite 恒空 = bearer / session）；`Authenticator.AuthenticateToken(ctx, token, transport)`；`Caller` 加 `Transport`、`Scope`、`AccountID`、`ElevatedUntilMs`                                                                                                                                                                                                                                                              | 恒为 bearer / session               | §5.1 的绑定规则                         | S1b |
| 刷新寿命         | `Refresh` 对 `Scope = operator` 不延长 `ExpiresAtMs`（1 小时绝对寿命）                                                                                                                                                                                                                                                                                                                                                                                                                                           | 不涉及                              | —                                       | S1b |
| 登录拆分         | 导出 `core.VerifyPassword(ctx, c, account *store.AccountRecord, password, nowMs)`：锁定、失败计数、未知账号等时派生，供按邮箱登录复用                                                                                                                                                                                                                                                                                                                                                                            | `Accounts.Login` 改为调用它         | `accounts.Login`                        | S1b |
| 设备生成         | 导出 `core.NewDevice(subject, input, transport, scope, nowMs, ttl) (record, refreshToken)`（纯函数），持久化由调用方                                                                                                                                                                                                                                                                                                                                                                                             | `CreateDevice` 改为调用它           | 在事务里插入                            | S1b |
| 事件受众         | `core.Audience` 加 `AccountIDs []string`（S8 定：受众由发事件的一方在提交后算成账号列表，不带团队与角色的描述）；`EventBus` 加 publisher 钩子与只在本机分发的 `Deliver`，SaaS 的 publisher 是 `cloud/events.Hub`（Redis 广播）                                                                                                                                                                                                                                                                                   | 只用 `LinkID`                       | §7.1                                    | S8  |
| 撤销钩子         | `Context.Revocations core.RevocationSink`：设备撤销、登出、改口令、**刷新令牌重放（撤销整台设备时）**、链接撤销、源撤销之后回调（参数是受影响的账号 / 设备 / 源 / 链接）。**在 SaaS 的绑定事务里调 core 的撤销函数（`RevokeDevice`、`RevokeLink`、`SetPasswordNames` 等，钩子都在这些低层函数里调）时，`Context.Revocations` 必须换成经 `tx.AfterCommit` 投递的版本（`core.Context.Bound(tx.Ports(), relayctl.Deferred(tx, sink))`）**：钩子在库改完、事务提交之前就被调到，直接投递会让令牌先被撤、回滚后又白撤 | nil（personal 靠 jti 形状即时判定） | `relayctl`（§7.2）                      | S1b |
| 断言附加         | `GrantExtra` 加 `Team *gen.AssertionClaimsTeam`、`Role string`、`Email`、`EmailVerified`；中继令牌与隧道令牌加 `team`；`SignOptions.JTIPrefix` 给断言用 `s.<deviceId>.<sourceId>`                                                                                                                                                                                                                                                                                                                                | 都不带                              | §4.6                                    | S1b |
| 签名钥包裹       | `private_key_wrap = aes256gcm-v1` 由 `KeyStore` 在读时解包（主密钥经 `postgres.Open` 的选项传入）                                                                                                                                                                                                                                                                                                                                                                                                                | `none`                              | 主密钥                                  | S1b |

错误详情（`core.Error.Details`、`FailWith`、`WriteError` 输出 `details`）只在 S0a 做，S1 不重复。

```go
// AccessPolicy 决定谁能看到、管理、访问哪台主机（core 不知道团队）。
type AccessPolicy interface {
	// Visible 答调用方能看到的主机与来路（owner / team / member / guest / link），按 filter 分页；
	// 调用方（Sources.List）循环取页直到 next 为空。
	Visible(ctx context.Context, caller Caller, filter SourceFilter) (page []VisibleSource, next string, err error)
	// Access 答单台主机能否访问（Get / Assertion）；不能用 Visible 的第一页代替。
	Access(ctx context.Context, caller Caller, sourceID string) (visible VisibleSource, ok bool, err error)
	// Intent 答签断言要带的意图；没有访问权答 source_access_denied。
	Intent(ctx context.Context, caller Caller, source store.SourceRecord) (GrantExtra, error)
	// CanManage 判定 sources.update/revoke、links.*、grants.* 的管理权（owner 或团队 admin）。
	// target 只填 LinkID 时由实现从链接解析所属的源；target 全空只来自不带过滤的 links.list，
	// SaaS 实现必须拒绝或收窄到调用方有管理权的主机，不得放行为全租户。
	CanManage(ctx context.Context, caller Caller, target ManageTarget, action ManageAction) error
	// Watch 在 me-stream 连接建立时解析事件可见性。
	Watch(ctx context.Context, caller Caller) (Watcher, error)
}

type ManageTarget struct{ SourceID, LinkID string }

// Watcher 是一个连接的事件可见性，三个方法都不做 I/O：Sees 判可见，Refresh 在每个事件分发前
// 收到它（授予 / 撤销时更新可见集合），Close 在连接关闭时释放订阅。
type Watcher interface {
	Sees(sourceID string) bool
	Refresh(event Event)
	Close()
}
```

### 4.3 账号（`cloud/accounts`）

| 过程                                 | 规则                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auth.register`                      | 次序：按 IP 限流 → 事故开关 `registrationsPaused`（`503 registrations_paused`）→ 挑战 → 口令策略（四条 + 可选 HIBP，`names` = 邮箱本地部分与显示名）→ 事务：邮箱转小写后唯一（`409 email_taken`）、插账号、按传输建设备（§5.1）、插 `verify_email` 发件、写安全日志 `security.register` → 答会话（Cookie 传输时下发 Cookie 与 `csrfToken`）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `auth.login`                         | `account` 字段是邮箱（转小写）；不存在走等时派生；`status = disabled` 答 `403 account_disabled`（在口令比对之后，避免枚举）；**未注册邮箱按规范化邮箱同样计失败次数并套同一个 `NextLockout`（计数放 Redis，多实例共享），达阈值后同样答 `429 account_locked` 与 `retryAfterMs`，已锁路径也做一次等价的口令派生**，所以锁定不暴露邮箱是否注册。**Redis 故障时失败关闭（S8 定，取代 S1b 的「回落进程内计数」）**：已注册账号的计数在账号行（PostgreSQL），未注册邮箱的计数在 Redis，一旦未注册邮箱回落到各实例进程内计数，N 个实例下两类邮箱达到锁定的次数就不同，攻击者可借此区分。所以不回落：`auth.login` 在按邮箱找账号之前先对 Redis 做写探活（`httpx.UnknownLockouts.Guard`，`SET cloud:lockout:probe … PX`，500 毫秒超时；不用 `PING`——Redis 拒写（OOM、READONLY、MISCONF）而 `PING` 照常答时，只有未注册路径的记账写会失败，两类邮箱就能被区分，出错后 1 秒内快速失败，不去打一个正在出问题的 Redis），不可用时对**已注册与未注册的邮箱一律**答 `503 unavailable`（`retryAfterMs = 5000`），不比对口令、不计数；分叉之后的最终答复前再复核一次（已注册的路径在口令比对失败后探活，未注册的路径以 `Fail` 的那次 Redis 记账收尾，出错同样答 503），所以探活与记账之间 Redis 掉线也不会让两类邮箱的答案不同。代价：Redis 故障期间密码登录整体不可用（Redis 本来是 `/ready` 的硬依赖，实例会被摘出轮换；已登录的会话、刷新与其他过程不受影响）。每次拒绝记警告日志并计入指标 `armadra_cloud_lockout_unavailable_total`，运维据此告警；已锁的状态在 Redis 恢复后照旧（故障期间不改任何计数，已注册账号的 `failures` 也不动）。测试：两个实例、故障前各锁一对、故障期间 4 类邮箱 × 2 实例 × 对错口令答复逐条一致。`scope = operator` 见 §4.9；新设备插 `new_device` 发件并写 `security.newDevice`（同平台同名的设备 30 天内没出现过，**或**该账号 30 天内没有从同一个 IP 前缀用同名设备登录过——设备名谁都能起；设备码兑换同样写 `security.login` 并按这条规则提醒） |
| `auth.refresh` / `auth.logout`       | 核心逻辑原样；Cookie 传输的凭据取自刷新 Cookie（§5.1）；`scope = operator` 只作用于运营会话；刷新令牌重放撤销整台设备并经撤销钩子撤销已签令牌                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `auth.changePassword`                | 核心逻辑原样（撤销本人其他设备，经撤销钩子写撤销；整个改动在一个 SaaS 事务里）+ 作废本人未用的 `change` 令牌并删掉排队未发的 `email_change` 发件 + 安全日志 + `password_changed` 发件                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `auth.stepUp`                        | 按设备限流（10 分钟 10 次）→ 口令（运营账号另认 `totpCode`）→ `devices.elevated_until_ms = now + 10 分钟`；M1 不要求任何过程提权                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `auth.emailVerify`                   | 令牌哈希命中、未消耗、未过期（过期 `410 email_token_expired`，其余 `400 email_token_invalid`）；`verify` 置 `email_verified_at_ms`；`change` 改 `email`（新地址再查唯一）并置已验证、给旧地址发 `email_changed`，并作废该账号所有 `reset` 令牌与排队未发的找回邮件（旧地址不再是账号的邮箱）；同一账号同目的的其余令牌一并作废                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `auth.emailVerifyResend`             | 已验证答 `409 conflict`；按账号每小时 3 次                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `auth.passwordResetStart` / `Finish` | Start 总答成功（不泄露是否注册），存在且未停用才插 `password_reset` 发件；Finish 核对令牌地址仍是账号当前邮箱 → 过口令策略 → 换口令、清锁、撤销全部设备（经撤销钩子）、作废该账号所有未用的 `change` 令牌与排队未发的 `email_change` 发件 → 不答会话；`reset` 类发件发信时收件地址必须仍是账号当前邮箱，否则丢掉                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `me.get`                             | `account`、`teams`（`team_members` ⋈ `teams`，排除已删除）、`scopes`、Cookie 传输时 `csrfToken`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `me.update`                          | `displayName`、`locale`、`avatarUrl`（按协议包形状）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `me.emailChange`                     | 口令比对（计入锁定）→ 新地址未被占用 → 插 `change` 令牌的发件；验证前旧邮箱照常生效                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `me.devices` / `me.deviceRevoke`     | 核心逻辑原样 + 撤销钩子                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `me.spaces`                          | 个人空间（计划取 `entitlements`）+ 所属团队与角色、计划                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |

**未验证邮箱的门**（`REQUIRE_EMAIL_VERIFIED=1`）答 `403 email_unverified`：`teams.create`、`sources.registrationToken`、`links.create`、`invitations.create`、`invitations.accept`、`grants.create`（主体账号未验证）、`trust.reject`。登录、`me.*`、`sources.list` 不受限。

**设备码**（S3，能力设计 §11.5）：

| 步骤                           | 规则                                                                                                                                                                                                                                         |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `deviceStart`                  | 按 IP 每小时 30 次；生成 `deviceCode`（32 字节随机，只存哈希）与 `userCode`（8 位，字母表 `BCDFGHJKLMNPQRSTVWXZ`，`XXXX-XXXX`，与待处理的冲突就重抽）；10 分钟；`intervalMs = 5000`；`verificationUrl = <APP_ORIGIN>/device?code=<userCode>` |
| `devicePoll`                   | 距上次轮询不足 `intervalMs` 答 `429 rate_limited`；`approved` 时在事务里建 `bearer` 设备、状态改 `consumed`、答会话（只此一次）；`denied` / `expired` 原样答                                                                                 |
| `deviceGet`                    | 要会话；按 IP 每分钟 30 次（防猜码）；答发起设备的平台、名字与时间                                                                                                                                                                           |
| `deviceApprove` / `deviceDeny` | 要会话；只对 `pending` 生效（否则 `410 device_code_expired` / `404 not_found`）；写安全日志 `security.deviceApprove`                                                                                                                         |

**设备码的补充规则**（S3 审查定下）：

- **批准要什么会话**：`deviceApprove(approve: true)` 只认 Cookie 会话（页面里的人点的，Origin 与 CSRF 由认证闸核过），或提权窗口内（`elevated_until_ms` 未过）的 Bearer 会话；其余答 `401 mfa_required`（同 Q7 的「未提权」码，客户端调 `auth.stepUp` 后重试）。拒绝（`deviceDeny`、`approve: false`）不要求。一个被盗的 Bearer 令牌因此不能静默地把别的设备登进来。
- **猜码**：`deviceGet` / `deviceApprove` / `deviceDeny` 先判账号桶 `device.guess.acct`（只统计查无此码的 `404` 失败，每账号每小时 10 次，满了直接 `429`，换 IP 也逃不掉），再计 IP 桶 `device.get.ip`（三者共用）；`410` 与成功不计入账号桶。
- **轮询**：`devicePoll` 先过 IP 桶 `device.poll.ip`（每分钟 120 次），再进事务；会话的访问令牌在兑换事务提交之前签好，提交失败则丢弃。
- **加锁**：兑换先不加锁读 `account_id` → `LockAccount` → `FOR UPDATE` 重读设备码行并复核状态；批准 / 拒绝只锁设备码行。

### 4.4 团队与邀请（`cloud/teams`、`cloud/invitations`）

**团队设置缺省**（`teams.create` 写入）：`membersCanRegisterHosts: true`、`defaultHostRole: "viewer"`、`allowGuests: true`、`allowAnonymousLinks: false`（能力设计 Q2）、`deviceEndorsers: "owner_admin"`、`memberSelfEndorse: true`、`requireMfa: false`、`sessionMaxAgeMs: null`、`allowedEmailDomains: []`。M2 的三项 M1 可读可写、不生效。

| 过程                           | 规则（全部在一个事务里，先 `SELECT … FROM teams … FOR UPDATE` 锁团队行）                                                                                                                                                                                                               |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 角色检查                       | `teams.Require(ctx, tx, caller, teamID, min Role) (saas.TeamScope, error)`：不是成员答 `404 not_found`（不泄露存在），角色不够 `403 team_role_insufficient`，已删除 `410 team_deleted`（`teams.restore` 例外），停用只挡签断言与登记主机；后续团队范围的仓储调用都用它答的 `TeamScope` |
| `teams.create`                 | 邮箱已验证 → 配额 `teams`（锁账号行）→ slug 唯一（`409 team_slug_taken`）→ 插团队、owner 成员、`entitlements`（free）→ 提交后经 `relayctl` 写团队并发额度 → 审计 `team.create`                                                                                                         |
| `teams.memberRole`             | admin 不能改 owner、不能把人升成 owner（只经转移）；降最后一个 owner 答 `409 team_last_owner`；降为 `billing` / `guest` 或降 admin 都走撤销流程（§4.8）                                                                                                                                |
| `teams.memberRemove` / `leave` | 最后一个 owner 不能；删成员（分组成员级联）、撤销以此人为主体的授予、此人登记的主机 owner 改成团队 owner（取 `joined_at_ms` 最早的 owner）、撤销流程                                                                                                                                   |
| `teams.transferOwnership`      | 目标必须是 admin（否则 `400 bad_request`）；同一事务里目标升 owner、调用者降 admin                                                                                                                                                                                                     |
| `teams.delete`                 | 有在线主机（任一团队主机有路由）答 `409 conflict`；置 `deleted_at_ms`、`purge_at_ms = +30 天`；撤销团队全部已签令牌；插 `team.purge` 作业（`run_at = purge_at_ms`，`dedupe_key = teamId`）                                                                                             |
| `teams.restore`                | 只在恢复期内、只 owner；清两个时间，删 `team.purge` 作业                                                                                                                                                                                                                               |
| 清除作业 `team.purge`          | 撤销团队主机（`sources.revoked_at_ms`）、删授予、邀请、链接、待信任、`source_spaces`、`entitlements`，最后删团队行（成员级联）；审计行保留（无外键）                                                                                                                                   |
| `teams.settingsUpdate`         | 按协议包 schema；关 `allowGuests` 不自动移除已有 guest（§10 Q8）                                                                                                                                                                                                                       |

**邀请**（两类共用一套过程，契约 §22）：

| 步骤     | 规则                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `create` | team：`team:admin`；角色 `guest` 而团队关了 guest → `403 guests_disabled`；已是成员 `409 member_exists`；同邮箱有待处理的 `409 conflict`；配额（成员数或 guest 数，**待处理邀请也计入**）。host：主机管理权（§4.6）+ 主机设置 `allowGuests`；按邀请人每小时 50 次                                                                                                                                                                                 |
| 发信     | 事务里插邀请（`token_hash = NULL`）与发件；作业发信时生成 32 字节令牌、写哈希与 `sent_at_ms`，链接是 `<APP_ORIGIN>/invite/<invitationId>#<令牌>`——**令牌在片段里**，不进服务器与代理日志；页面另发 `Referrer-Policy: no-referrer`                                                                                                                                                                                                                 |
| `get`    | 匿名；按 IP 每分钟 60 次；令牌哈希必须对上（否则 `404 not_found`）；答遮掩后的摘要                                                                                                                                                                                                                                                                                                                                                                |
| `accept` | 要会话、邮箱已验证、账号邮箱 = 邀请邮箱（`403 invitation_email_mismatch`）；过期 `410 invitation_expired`、撤销 `410 invitation_revoked`；已接受的再接受答原结果。**插入新的 pending 邀请前（`invitations.create`、`resend` 之外的重新邀请同理），同一事务先把同一 `(团队, 邮箱)` 或 `(主机, 邮箱)` 下已过期的 pending 改成 `expired`**，否则过期未处理的行会占着部分唯一索引挡住重新邀请。team：插成员（再查配额与 `member_exists`）。host：§4.7 |
| `resend` | 换新令牌（作业里生成）、`expires_at = now + 7 天`、`resend_count + 1`；按邀请每小时 5 次                                                                                                                                                                                                                                                                                                                                                          |
| 过期     | 读时把 `pending` 且过期的按 `expired` 答；`invitations.expire` 作业每小时落库并发 `invitationChanged`                                                                                                                                                                                                                                                                                                                                             |

- **收件箱规则**：`invitations.list` 的 `box = received` 一律要求邮箱已验证（**不看** `REQUIRE_EMAIL_VERIFIED` 开关），未验证答空列表——未验证的地址可能是别人注册时随手填的，不能借它读到发给那个地址的邀请。
- **邀请人失效**：邀请人被移除、离开，或被降到 admin 以下时，同事务把他发出的待处理团队邀请改 `revoked`；`teams.Join` 另外核对邀请人此刻仍是 admin 以上（兜底，否则 `409 conflict`）。
- **`teams.Join(ctx, tx, tenantID, nowMs, invitationID, account)`**：`accept` 的后半段自己 `LockTeam`、锁邀请行，核对 `kind = team`、仍待处理（撤销 / 过期 / 已拒绝各有各的码）、账号邮箱等于邀请邮箱，再核团队状态、重复成员、邀请人身份、guest 设置与额度，插成员并置 `accepted`；令牌由 `accept` 先核。`TeamScope` 因此不出 `cloud/teams`。
- **预览**：`invitations.get` 的 `?token=` 会进代理与访问日志，已弃用（保留兼容）；客户端用 `invitations.preview`（`POST …/preview`，令牌在体里，协议包 0.10.1），两者共用限流与全部规则。
- **标签清理**：团队名与邀请人显示名进邮件前经 `SanitizeLabel`：去网址、邮箱，以及任何「域名形态」（字母数字串 + `.` + 两个以上字母，点两侧的空白与全角点先归一），宁可多删。

### 4.5 主机（`cloud/hosts`，S5a）

| 过程                        | 规则                                                                                                                                                                                                                                                                                                          |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sources.registrationToken` | 邮箱已验证；`space = team` 时：成员、团队未停用（`403 team_suspended`）未删除（`410 team_deleted`）、`member` 角色看 `membersCanRegisterHosts`（`403 host_registration_disabled`）、`billing` / `guest` 答 `403 team_role_insufficient`；配额（空间主机数）；令牌写 `team_id`                                 |
| `sources.register`          | 一个事务：`Consume` 注册令牌（带出账号与 `team_id`）→ 团队主机且上报的 `coreVersion` 低于 `gen.MinCoreVersionTeamAssertion` 答 `426 protocol_unsupported`（不让老 core 进团队，§4.6）→ 锁空间行再查配额 → 写 `sources`（`core.BuildSourceRecord` + `rows.go`）→ upsert `source_spaces` → 审计 `host.register` |
| `sources.heartbeatHint`     | 核心逻辑；节点取 §7.3；隧道令牌带 `team`；隧道令牌 `jti` 记进签发索引                                                                                                                                                                                                                                         |
| `sources.list`              | 经 `AccessPolicy.Visible`：`space` 缺省 = 全部可达空间；`status`、`tag`（多个取「且」）、`q`（名称 `ILIKE`，转义 `%_`）、`cursor`（`(registered_at_ms, source_id)` 的不透明编码）、`limit` 1–200；摘要多 `ownerName`                                                                                          |
| `sources.update`            | 名称、标签（协议包 `hostTagsSchema`）、主机设置；管理权                                                                                                                                                                                                                                                       |
| `sources.revoke`            | 管理权；置 `revoked_at_ms`、撤销该源全部已签令牌、`closeTunnel` + `kick`、`sourceRevoked` 事件、审计（能力设计 §11.7）                                                                                                                                                                                        |

**主机设置缺省**：`linkCreators: "owner_admin"`、`allowGuests: true`、`defaultRole: "viewer"`。

**S5a 实现注记**（与上表的差别与补充）：

- **可见性在目录查询里，不经 `AccessPolicy`**：`grants.Policy` 随 S5b，S5a 的 `sources.list` / `get` / `me.sources` 直接用存储层的目录查询（`saas.HostFilter.ViewerID`）。可见 = 个人空间的所有者；团队主机给团队 owner / admin，以及登记它的成员（member 角色）；团队进入恢复期则团队主机全部不可见；billing / guest / 外人什么都看不到。S5b 把授予带来的可见性并进同一条查询，`core.AccessPolicy` 的 SaaS 实现 `grants.Policy` 包它（见 §4.6 的 S5b 实现注记）。
- **管理权**：主机所有者，或团队主机的团队 owner / admin；看不到的一律 `404 not_found`，看得到但管不了（S5b 起才会出现）答 `403 forbidden`。
- **撤销后的令牌**：S5a 的签发索引只有隧道令牌（`heartbeatHint` 签出时记 jti，`RelayInfo.TunnelJTI`），撤销时 `RevokeWhere` 把它们写进撤销表；断言与中继令牌随 S5b 进索引（已完成，见 §4.6 的 S5b 实现注记）。`cloud_issued_tokens` 的落盘在 S5b（`relayctl.Durable`），重放仍是 S8。
- **注册令牌按账号计**：核心的 `Registrations.Add` 的上限（20）按整个租户计，SaaS 不能用；`HostRepo.AddRegistration` 按账号计，并只清这个账号过期的令牌。`team_id` 带在令牌上，`PeekRegistration` 先不加锁地读出空间，再按「团队 → 账号 → 源」加锁后才 `ConsumeRegistration`（先删令牌再等团队锁会和清除作业互等）。
- **登记事务回滚不消耗令牌**：核心的个人中转「先消耗，拒绝也不能再用」；SaaS 按设计 §4.5 的单事务，失败整个回滚，令牌留到自己过期。
- **同一 `sourceId` 的登记**：`LockSource`（事务级咨询锁）串行化；已登记未撤销的源只允许同一账号在同一空间用同一把钥重登记，其余 `409 source_taken`；撤销后任何持有同一把钥的账号都可以登记到自己的空间（空间行重置标签与设置）。
- **签发索引的顺序（§4.8 同）**：任何会被撤销流程撤到的令牌，一律「签令牌 → 登记签发索引 → 再读库复核」，复核看到已撤销就对该令牌（或该源）`RevokeWhere` 并答 `source_revoked`。撤销在库提交之后才撤索引里的令牌，所以与并发的签发交错也不会漏；`heartbeatHint` 已按此做，S5b 给断言与中继令牌建索引时照做。
- **撤销后换钥**：撤销后同一个 `sourceId` 也只许同一把钥重登记，换钥一律 `409 source_taken`（SaaS 侧在 `registerIn` 判，不改个人中转共用的 `BuildSourceRecord`）。
- **S8 之前不部署**（S8 已解除）：S8 起生产装配用包装 `control/redis.Plane` 的 Redis 实现，`heartbeatHint` 读 `relay:nodes`（§7.3）；内存实现只在测试里用（`Deps.Relay`）。
- **主机设置只存写过的项**：缺省走 `defaultRole` ← 团队 `defaultHostRole` ← 主机自己写过的项；`trustedOrigins` 只收 `http(s)://主机[:端口]`，去重、至多 16 个。

**`defaultRole` / `defaultHostRole` 在 M1 的语义**：只是控制台与客户端分享弹层里「角色」的**预选值**（主机设置优先，没设时取团队的 `defaultHostRole`）；`grants.create` 与 `invitations.create` 的 `role` 是必填，服务端不替调用方填角色，也不据它改已有授予。

### 4.6 授予、有效权限与断言合成（`cloud/grants`，S5b）

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
4. 忽略已撤销（含到期后由 `grant.expire` 写了 `revoked_at_ms` 的）与 `expires_at_ms ≤ now` 的授予。
5. 合成（#111，不放大）：范围 S 取所有有效来路范围的并集，任一来路整台则 S = 整台；角色 R 取最大的角色，使 S 中每一处都被某个角色 ≥ R 的来路覆盖——整台时 R = 整台来路中的最高角色，只有限工作空间来路时 R = min over w∈S 的（覆盖 w 的来路中的最高角色）。结果永不超出任一范围上实际授予的角色：限工作空间的更高角色若无法与更宽范围同时表达则舍弃（`driver@w1` + 团队 `viewer` 整台 = `viewer` 整台，不是 `driver` 整台；`driver@w1` + `viewer@w2` = `viewer` {w1, w2}；`editor@w1` + `driver@w1` = `driver` {w1}；`driver` 整台 + `viewer@w1` = `driver` 整台）。`expiresAtMs` 取对 S 与 R 有贡献的来路中最早的非空到期（整台时是角色恰为 R 的整台来路；限范围时是角色 ≥ R 的来路），被舍弃的高角色来路的到期不拖短结果。断言的 `role` / `scp`、`Narrowed`（新合成结果角色更低、范围更小或到期更早即收窄）、访问清单、`grant.expire` 前后快照都用这同一个合成结果。将来如需「按工作空间分角色」，属协议扩展（断言 `scp` 带角色），M1 不做。
6. `grants.effective` 答合成结果与 `from[]`。

**断言合成**（`sources.assertion`，`grants.Policy.Intent`）：

| 步骤 | 规则                                                                                                                                                                                                                                                                                                                                                                       |
| ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1    | 事故开关 `assertionsPaused` → `503 unavailable`；限流 `assert.acct` 每账号每分钟 60 次（能力设计 §11.7）                                                                                                                                                                                                                                                                   |
| 2    | 主机已撤销 `410 source_revoked`；团队停用 `403 team_suspended`；团队已删除 `403 source_access_denied`                                                                                                                                                                                                                                                                      |
| 3    | 链接访客（`guest:<linkId>:…` 主体）走现有链接路径，范围取链接的 `scope`                                                                                                                                                                                                                                                                                                    |
| 4    | 账号：有效权限为空 → `403 source_access_denied`                                                                                                                                                                                                                                                                                                                            |
| 5    | **版本门（契约 §30）**：取 core 版本时**优先用在线隧道上报的 `coreVersion`**（路由里的），不在线才用登记时的。团队主机要求不低于 `gen.MinCoreVersionTeamAssertion`；个人空间主机上的访客（来源只有 `guest` 授予）要求不低于 `gen.MinCoreVersionRoleOnlyAssertion`（§10 Q2）；低于或解析不了 → `403 source_access_denied`，不签降级断言。owner 本人访问个人空间主机不受门限 |
| 6    | 断言声明：`sub = accountId`、`aud = sourceId`、`name = displayName`、`email`、`emailVerified`、`team = { teamId, role: 团队角色 }`（只团队主机）、`role = 有效角色`（owner 来源且个人空间时不带，与个人中转同形）、`scp = ws:<id>…`（整台时不带）、`device`；`jti` 前缀 `s.<deviceId>.<sourceId>`                                                                          |
| 7    | 中继令牌：`sub`、`aud = relay`、`src`、`team`（团队主机）、`dev`；`ro` 不带（读写由 core 判）；`jti` 前缀 `r.<deviceId>.<sourceId>`                                                                                                                                                                                                                                        |
| 8    | 提交后把两个 `jti` 记进签发索引（PostgreSQL 与 Redis，§7.2）；审计 `assertion.issue`（`jti` = 断言的 jti，`target = host`，`team_id`）                                                                                                                                                                                                                                     |
| 9    | 答 `relayOrigin = relayBaseUrl = https://<sourceId>.src.<RELAY_DOMAIN>`、`online`（读路由）                                                                                                                                                                                                                                                                                |

**S5b 实现注记（第一部分：访问策略与签发索引）**：

- **`grants.Policy` 就是 `core.AccessPolicy`**：装配时设为 `core.Context.Policy`，也是 `hosts.Directory` 的实现——S5a 的 `sources.list` / `get` / `update` / `revoke` / `me.sources` 不再直接问存储，一律经 `Policy.Hosts`（填好时钟再调 `ListHosts`）。链接访客（`guest:<linkId>:…`）转给 core 的 `personalPolicy`（S6 已接上，见 §4.7 的 S6 实现注记）。
- **可见性 ≠ 可达**：目录看得到 = 个人空间的所有者与 `guest` 授予；团队主机（团队未进恢复期）给团队 owner / admin 看全部，member 看自己登记的以及 `member` / `team` 授予到的，guest 角色看 `guest` 授予到的，billing 与外人什么都看不到。团队 owner / admin 看得到全部却不一定拿得到断言（有效权限空 → `403 source_access_denied`，这是 M2 访问申请要解决的）。可见性 SQL（`visibleSQL`）与 `grants.Compose` 逐个主体对照（`TestDirectoryVisibilityMatchesEffectivePermissions`）。
- **团队主机的所有者要仍是 owner / admin / member**：降为 billing / guest 就既看不到、管不了，也拿不到断言（与 §4.8「降为 billing 或 guest」撤令牌一致，否则撤了立刻又能重签）。
- **来路枚举**：契约 `sourceSummary.via` 只有 `owner | link | team | org`，个人空间里被授予的访客按 `link` 答，团队主机上的非所有者按 `team`。
- **断言的 `scp`**：授予可以限在多个工作空间，`GrantExtra` 加 `Scp`（非 nil 时取代链接范围换出的 `scp`）；整台主机不带 `scp`。
- **签发索引落盘（`relayctl.Durable`）**：包住内层（内存 / S8 的 Redis）：`IndexIssued` 先 `INSERT cloud_issued_tokens` 再进内层；`RevokeWhere` 以 PostgreSQL 为准选出未到期的令牌、写 `relay_revoked_tokens`、从索引删，再让内层撤并踢人（内层可能有 PostgreSQL 已删的条目——签发方与撤销交错时——所以两边都撤）；core 的撤销钩子（登出、撤设备、改口令、撤链接与源）同样先落盘；PostgreSQL 命中的令牌补登记进内层后按 `Filter{JTI}` 逐个撤（内层索引缺了也撤到），设备撤销的踢人用两边命中的并集、按 (源, subject, 设备) 去重发 `Kick`（`Control.Revoke` 答命中的条目）。索引过滤带 `tenant_id`，前缀里的 LIKE 通配符按字面。重放（启动与 Redis 订阅重新就绪时把撤销表与索引写回 Redis）S8 已做（§7.4）。
- **`Filter.JTI`**：签发方复核发现自己签的令牌不该有效时，只撤自己签的那一枚。
- **`Sources.List` 的游标防护**：策略重复给同一个游标（或绕回已取过的）答 `internal`，不再死循环。
- **`Watcher`**：`Sees` / `Refresh` / `Close` 可并发调用，`Close` 之后 `Sees` 答 false、`Refresh` 什么都不做；`AccessChanged` 事件（S8 把 `grantChanged` / `memberChanged` 转成它）更新可见集合，`SourceRevoked` 移除；`AccessChanged` 是 `core.InternalEvent`，`MeStream.offer` 只交给 `Watcher.Refresh`、不进客户端队列，空 `AccountID` 只允许移除（`Visible = false`），放宽必须点名账号。

**S5b 实现注记（第二部分：授予、断言与撤销）**：

- **`grants.*` 的管理权与错误**：看不到主机 `404 not_found`；看得到但管不了，团队主机答 `403 team_role_insufficient`，个人空间答 `403 source_access_denied`；团队主机先锁团队行并核成员（owner / admin / member，guest 与 billing 答 `team_role_insufficient`），再锁主机、授予（团队 → 主机 → 授予的顺序，与 S4 的团队 → 账号 → 其余一致）。
- **主体校验**：`group` 一律 `403 plan_feature_unavailable`（M2）；`team` 只能是主机所属团队；`member` 的主体不是团队成员 `404`、角色不是 owner / admin / member `400`；`guest`：团队主机要求 guest 角色的成员（不是成员 `404`，角色不对 `400`），团队 `allowGuests` 关或主机设置 `allowGuests` 关答 `403 guests_disabled`，个人空间要账号存在且邮箱已验证（`403 email_unverified`）；以主机所有者为主体 `400`；`expiresAtMs` 必须在将来；同主机同主体已有有效授予 `409 conflict`（先查，唯一索引兜底）。授予不设数量上限，`limit_reached` 不会由 `grants.create` 给出（guest 额度在成为团队 guest 成员时已计）。
- **收窄即撤销（§4.8 第 1 行）**：在锁内先对受影响的账号（账号主体就是他，`team` 主体是团队全体成员）取一份有效权限快照，改完再取一份，`Narrowed` 为真的账号，提交后 `RevokeWhere{Subject, SourceID}` 撤断言与中继令牌，再 `Kick{SourceID, Subject}` 按人踢；放宽、不变不撤。`grantChanged` / `memberChanged` 事件随 S8 的事件总线（本批不发）。成员移除 / 离开 / 降级沿用 S4 的撤销流程，经同一个 `relayctl.Durable`，所以同样落盘。
- **`grant.expire`**：授予有到期时排一个 `run_at = expires_at_ms`、`dedupe = grantId` 的作业，改到期时间先撤旧再排新，清掉到期或撤销授予就撤掉作业；处理器锁团队 → 主机 → 授予后重读，已撤销或到期被改到以后就结束，否则写 `revoked_by = 'system'`、取「到期前一毫秒」与「此刻」两份有效权限快照（复用 `narrowedAccounts`），只对真正收窄的账号撤令牌并踢人——团队授予到期不牵连另有来路而权限没变的人；令牌寿命本来就不超过授予的到期，所以这一步是兜底、审计 `grant.expire`（actor = system）。每小时的 `grant.expireDue` 给到期却没撤销的授予补插作业（作业丢了或被去重挡掉的兜底）；`issued.gc` 每小时删已到期的签发索引与撤销行。
- **范围校验**：`workspaceIds` 每项按协议包 `scopeEntrySchema`（`[A-Za-z0-9_.:-]{1,128}`，至多 64 项）校验，不合格 `400 bad_request`；团队 `guest` 主体的授予必须带非空 `workspaceIds`（只能给具体的工作空间，不能整台主机），建与改都校验。
- **`sources.assertion`**：事故开关 `assertionsPaused` → `503 unavailable`，限流 `assert.acct`（每账号每分钟 60 次）→ 主机已撤销 `410 source_revoked` → 看不到 / 没有有效权限 `403 source_access_denied`（团队停用先答 `403 team_suspended`，只对看得到这台主机的人）→ 版本门 → 签断言与中继令牌（断言 `jti` 前缀 `s.<deviceId>.<sourceId>`，中继令牌 `r.…`）→ **登记签发索引（先 PostgreSQL 后内层）→ 再读库复核**：主机已撤销则撤自己签的并答 `410`；没有有效权限则撤并答 `403`；有效权限比签发时窄（含团队角色变了）则撤自己签的并按新权限重签（至多 3 次，仍被打断答 `503 unavailable`）。撤销在库提交之后才撤索引里的令牌，所以复核看到未收窄，之后的撤销一定撤得到；与授予收窄、成员降级、主机撤销在「签出 / 登记」之间交错都不漏撤（`Options.Hook` 在 `signed` / `indexed` 两点停住请求的交错用例，加 6 路断言对 4 轮授予建撤的压力用例）。复核还会重读设备（签发中途登出、改口令、撤设备 → 撤自己签的并答 `401 unauthenticated`）并看团队是否停用（`team_suspended`，与 `Intent` 共用一个判断）。令牌寿命 = min(缺省, 有效权限的最早到期 − 现在)，不足 1 秒答 `403 source_access_denied`；合成后的 `scp` 超过 64 项答 `403 source_access_denied` 并记日志。成功后在一个事务里写审计 `assertion.issue`（`jti` = 断言的 jti，`target = host`，团队主机带 `team_id`）。链接访客（`guest:<linkId>:…`）的断言在 S6 接上（§4.7 的 S6 实现注记），S5b 时一律答 `403 source_access_denied`。
- **`sources.accessList`**：所有者与每个有有效权限的账号（`team` 授予展开到 owner / admin / member 成员）一行，角色 / 范围 / 到期是合成后的结果，`grantId` 取贡献最高角色的那条授予（所有者行不挂授予），最多 500 行；`sources.accessRevoke` 撤销一个账号**本人**的 `member` / `guest` 授予（`team` 授予撤不掉单个人，要走 `grants.revoke`），没撤到任何授予 `404`，之后按收窄撤令牌。

### 4.7 分享与新设备信任（`cloud/links`、主机邀请、`cloud/trust`，S6）

- **链接**包一层 `core.Links`：`links.create` 先查管理权与主机设置 `linkCreators`（`driver` 档 = 有效角色为 driver 的人也能签）、团队主机的 `allowGuests`；`links.list` / `update` / `revoke` 加 `space` 过滤；`links.accept` 对团队主机且 `allowAnonymousLinks = false` 时要求带会话（`403 anonymous_links_disabled`）。core 的条件加一与 `max_uses` 语义不变（能力设计 §11.3）；撤销链接经撤销钩子踢 `guest:<linkId>:` 前缀。
- **主机邀请被接受**落成**云端授予**（契约 §22，#72：主机邀请 = 云端授予 + guest 成员关系，不在 core 里落一张邀请；分享链接仍落成 core 的邀请），一个事务：
  - 团队主机：接受者不是成员 → 先按团队 `allowGuests` 与 guest 配额加成 `guest` 成员；再建授予（主体 `guest`，若已是 owner / admin / member 则主体 `member`），`origin = invitation`，`role` / `workspace_ids` 取自邀请，授予的 `expires_at_ms` 取邀请的 `grant_expires_at_ms`（NULL = 不过期），**不取邀请自身的有效期 `expires_at_ms`**（那只管邀请多久内可接受）。同样，`sources.trustRequest` 插新 pending 前，同一事务先把同 `(源, 设备)` 已过期的 pending 改成 `expired`，否则 `trust_requests_pending` 会挡住重新上报。
  - 个人空间主机：建主体 `guest` 的授予；core 按 §10 Q2 准入。
  - 已有同主体的有效授予 → 把新旧两条按 §4.6 的合成规则合成后更新那一条，不建第二条（不是各取最高角色与范围并集）。
- **新设备信任**（契约 §31，§10 Q1）：

| 步骤                   | 规则                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sources.trustRequest` | `POST /v1/sources/me/trust-requests`（契约 §31，#72），`Authorization: Source <jws>`；入参 `{pendingId, accountId, deviceId, expiresAtMs}`，`expiresAtMs` 至多比现在晚 24 小时（超出 `400 bad_request`）。按源限流（每分钟 30 次，`429 rate_limited`）；源已不在团队或账号不是该团队成员 → `403 source_access_denied`；`deviceId` 不是该账号未撤销的设备 → `404 not_found`；同 `(源, pendingId)` 重报答同一个 `requestId`，对已解决的再登记 `409 conflict`；设备名与平台由云端从 `devices` 表填。答 `{requestId, status}`：`pending` = 已登记并通知背书人；同 `(源, 设备)` 最近一次被拒在 1 小时冷却期内答 `rejected`，不建新请求、不再通知 |
| `sources.trustResolve` | `POST /v1/sources/me/trust-requests/{pendingId}/resolve`，Source 认证；入参 `{pendingId, result: "trusted" \| "expired"}`；按 `(源, pendingId)` 找请求（没有 `404 not_found`，已解决 `409 conflict`），改状态、撤掉通知、发 `deviceTrustResolved`                                                                                                                                                                                                                                                                                                                                                                                           |
| `trust.list`           | 按团队设置 `deviceEndorsers` / `memberSelfEndorse` 收窄；不含校验码                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `trust.reject`         | 背书人才行；只记拒绝（请求作废、开始冷却），不通知主机，设备继续停在 `device_pending`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| 背书                   | **只在桌面与原生 App 里**经端到端通道发到主机（契约 §77 的「已受信设备背书」），云端看不到也改不了；网页控制台不渲染「信任」，只引导到 App（§1.2）                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| 过期                   | `trust.expire` 作业每小时把过期的 `pending` 改 `expired` 并发事件                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |

**S6 实现注记**（与上文的差别与补充）：

- **装配**：`cloud/links`（`links.*`）与 `cloud/trust`（`sources.trustRequest` / `trustResolve`、`trust.list` / `reject`、`trust.expire`）是新包；主机邀请在 `cloud/invitations/host.go`；落成授予的合并在 `cloud/grants/invite.go`（`ApplyInvitation`），团队加 guest 成员在 `cloud/teams/join.go`（`EnsureGuestMember`，`TeamScope` 仍只在 `cloud/teams` 构造）；`grants.ManageHost` / `FindHost` / `DenyManage` / `CleanWorkspaces` 导出，授予、主机邀请与链接共用同一套管理权与范围校验。core 只加了不影响 personal 的接口：`core.LinkHooks`（`Admit` 在消耗次数前、`Issued` 在签出令牌后）、`Links.CreateAuthorized`（调用方已核管理权）、`LinkSummaryOf`、`AcceptInput.AccountID`；personal 不设钩子，行为字节不变。
- **主机邀请的两个有效期**：邀请自身恒为 7 天（`TTL`，只管多久内可接受，`resend` 重新计）；`create` 入参的 `expiresAtMs` 是**授予的到期**（存 `invitations.grant_expires_at_ms`，必须在将来、至多 30 天；缺省 = `NULL` = 不过期）。协议包注释里的「缺省 7 天」按邀请自身的有效期理解。接受时授予的到期已过，按 `410 invitation_expired` 答（接受了也得不到访问）。
- **主机邀请的创建**：`ManageHost`（看不到 `404`、看得到管不了 `403 team_role_insufficient` / `source_access_denied`）→ 主机设置与团队设置的 `allowGuests`（`403 guests_disabled`）→ 受邀者是主机所有者 `409 member_exists`、是团队 `billing` `409 conflict` → 团队主机上受邀者不是 owner / admin / member（接受后会是 guest）时 `workspaceIds` 不能为空（`400 bad_request`；团队 guest 不给整台，接受时再核一次）→ 先把同 `(主机, 邮箱)` 过期的 pending 改 `expired` 再查 pending（`409 conflict`）。团队主机的邀请行写 `team_id`（清除作业按它删）。待处理的主机邀请不占团队席位（接受时才占 guest 额度）。
- **主机邀请的接受**（一个事务，锁序「团队行 → 主机 → 邀请行」）：令牌 → 邮箱一致 → 已接受的原样答 `{ sourceId[, teamId] }` → 撤销 `410` / 过期 `410` / 已拒绝 `409` → 主机已撤销 `404` → 邀请人此刻仍有管理权、接受者不是主机所有者（否则 `409 conflict`，邀请人被降级或移除时邀请不必逐条撤，接受时兜底）→ 主机设置 `allowGuests` → 团队主机：`EnsureGuestMember`（不是成员先加成 guest：团队恢复期 `410 team_deleted`、团队关了 guest `403 team_policy_denied`、guest 额度 `429 limit_reached`，失败整个回滚）；接受者是 owner / admin / member 时授予主体为 `member`，是 guest 时为 `guest`（要有 `workspaceIds` 且团队仍开着 guest），billing `409`；个人空间主机要求账号邮箱已验证（`403 email_unverified`，与 `grants.create` 一致）→ `ApplyInvitation` → 置 `accepted` → 审计 `invitation.accept`（带 `source_id`）。`ApplyInvitation`：已有同主体的有效授予就和邀请合成一条（范围取并集（任一整台即整台）、角色取「使范围内每一处都被角色 ≥ 它的来路覆盖」的最高角色（不放大：driver@w1 加 viewer 整台是 viewer 整台）、到期取对结果有贡献的来路里最早的非空到期（直接用 `composeSources`，与 `Compose` 同一规则）；接受前后他的有效权限收窄时（含新增一条受限授予压低了别的来路）撤掉已签的令牌并踢人（reason `invitation_merged`））；合成会降低他已有的角色（更宽但角色更低的邀请，如 driver@w1 + viewer 整台）时答 `409 conflict`、不静默降级，由管理者用 `grants.update` 明确处理；已到期却还没被 `grant.expire` 撤销的授予当作不存在（以 `system` 撤销再建新的）；新授予 `origin = invitation`、`invitation_id` 指向邀请、`granted_by` 取邀请人、按到期排 `grant.expire`。
- **待处理主机邀请随访问变化作废**：团队成员被移除 / 离开 / 降为 billing 或 guest 时（`teams` 的 `revokeMember`），发给其邮箱、在该团队主机上的待处理主机邀请同事务改 `revoked`；`grants.revoke` 撤销 member / guest 授予、`grants.update` 收窄（`Narrowed` 的账号）时，发给其邮箱、在该主机上的待处理主机邀请同样作废（`RevokeHostByEmail`），否则旧邀请接受时会按「角色取高、到期取晚」放宽甚至清掉新加的到期。作废的邀请都发 `invitationChanged`（与 `RevokeByInviter` 一致）；账号改邮箱（`me.emailChange` 确认）同事务作废发给旧邮箱的全部待处理邀请（团队与主机）。`grant.expire` 不再作废邀请：到期的授予由 `ApplyInvitation` 当作不存在撤销再建。`resend` 主机邀请时授予到期已过答 `400 bad_request`；`ApplyInvitation` 撤销到期授予没成功时回滚整个接受（`409`）。
- **其余邀请过程**：`list` 的 `sourceId` 要主机管理权，缺省列团队邀请（admin 的团队）加调用者管得了的全部主机的主机邀请（上限 500）；`teamId` 过滤只列团队邀请；`revoke` / `resend` 对主机邀请要主机管理权（`lockForManage` 按「团队 → 主机 → 邀请」加锁），`resend` 的过期重核含主机与团队的 `allowGuests`；`decline` 两类共用；`get` / `preview` 的主机邀请摘要带 `sourceName` / `hostRole`（主机已撤销或团队在恢复期内 `404`）；`received` 收件箱不列已撤销主机的主机邀请。邮件模板 `host_invitation`（中英双语，主机名与邀请人显示名经 `SanitizeLabel`，角色名 viewer / editor / operator / driver 按收件语言取，链接同团队邀请的 `<APP_ORIGIN>/invite/<邀请 id>#<令牌>`）。
- **信任 `sources.trustRequest`**（Source 认证；`trust.src` 每源每分钟 30 次，与 `trustResolve` 共用）：`expiresAtMs` 必须在将来且至多 24 小时（`400`；库里的 CHECK 同样守）→ 源在团队里且账号是团队成员、对这台主机有有效访问权（`Compose` 非空）否则 `403 source_access_denied`（个人空间的主机没有团队背书）→ `deviceId` 是该账号未撤销的设备否则 `404`，设备名与平台取自 `devices` 表 → 同 `(源, pendingId)` 重报答同一个 `requestId`（账号与设备必须一致，否则 `409`；`pending` 未过期答 `pending`；`rejected` 且仍在 1 小时冷却内答 `rejected`，冷却结束后**同一个条目 id 重新开成 pending（新的 `requestId`）**；批准 / 过期的 `409`）→ 同 `(源, 设备)` 最近一次被拒在冷却期内答 `rejected`（带那条被拒请求的 id，不建新请求）→ 插新 pending 前同事务先把同 `(源, 设备)` 过期的 pending 改 `expired`，仍待处理的（主机换了条目 id）作废（`resolved_by = superseded`）。冷却检查在最前，旧条目 id 的重报也受它约束（冷却期内答 `rejected` 和最近被拒的 `requestId`）；重开时团队、设备名与平台按此刻的值刷新。整个过程在 `LockSource` 的咨询锁里，并发上报不会撞 `trust_requests_pending`。
- **信任 `trustResolve` / `trust.list` / `trust.reject`**：`trustResolve` 在 `LockSource` 下按 `(源, pendingId)` 找（没有 `404`、已解决 `409`），改 `trusted` / `expired`（`resolved_by = source`）；`trust.list`（背书人条件写进 SQL 再 `LIMIT 500`，不会先截断再过滤；**非背书人得到过滤后的空列表，不答 `team_role_insufficient`**）只列所在团队里、主机未撤销、请求者仍是成员、未过期的 pending，再按背书人收窄：主机所有者（仍是 owner / admin / member）总是；团队 owner / admin 在 `deviceEndorsers = owner_admin` 时是；请求者本人在 `memberSelfEndorse` 开着时是；**guest 的请求只给主机所有者**（core 的 `authorizeEndorsement` 只认 owner / admin / member，后两条对 guest 请求不成立）；`space` 是个人空间答空、不是成员的团队 `404`；`trust.reject` 先读视图（不是成员或请求不存在 `404`）、`teams.Lock` 后按最新角色与设置判背书人（否则 `403 team_role_insufficient`）、`FOR UPDATE` 请求：已拒绝的再拒绝是空操作，已批准 / 已过期（含 pending 但已过期）`409`，其余置 `rejected`（`resolved_by` 是拒绝的人）并记审计 `trust.reject`，不通知主机。 `trust.reject` 不实现 `device_untrusted`（云端无从判断调用设备是否受信）。Armadra 侧 core A1 要把 `trustRequest` 的 `rejected`、`404`、`409` 当作正常情况处理（冷却中、设备已撤销、条目已解决）。`trust.expire` 每小时把过期的 pending 落库成 `expired`，并清走解决超过 7 天的行。云端不存也不传校验码。
- **链接 `create`**：只有 `source_invite`（`team_join` 是 M2，答 `400 bad_request`）；一个事务，团队主机先 `teams.Lock(MinStaff)` 再锁主机（团队 guest / billing 答 `403 team_role_insufficient`）；谁能签看主机设置 `linkCreators`：`owner_admin`（缺省）= 主机管理者（所有者，或团队 owner / admin），`owner` = 仅主机所有者（团队 admin 看得到也管得了但答 `403 team_policy_denied`，`details.setting = linkCreators`），`driver` = 再加有效角色为 `driver` 的成员——driver 档**逐来路**判断（不看合成结果）：要有某个 `driver` 来路覆盖整台或链接的 `scope.workspaceId`，且链接的 `expiresAtMs` 不超过该来路的授予到期（否则 `400 bad_request`），移除成员 / 撤授予不连带撤销其签的链接，需管理者另撤；其余看不到 `404`、管不了 `403 team_role_insufficient` / `source_access_denied`；主机设置与团队设置的 `allowGuests` 关了 `403 guests_disabled`；角色必须是四档之一；之后由 `Links.CreateAuthorized` 建（期限 ≤ 30 天、范围规范化），审计 `link.create` 同事务。答复仍带 `secret`（片段里的秘密要由客户端拼，契约 §10 的写法）。
- **链接 `get` / `accept`**：`get` 的 `requiresAccount` 在团队主机且 `allowAnonymousLinks = false` 时为真；团队在恢复期内、主机已撤销都是 `link_invalid`。`accept` 带会话与匿名走同一条核心路径（会话不可用一律当匿名，只会让匿名开关更严）；`Admit` 在核过秘密之后、消耗次数之前：团队恢复期 `link_invalid`、停用 `403 team_suspended`、团队或主机 `allowGuests` 关了当链接失效（`link_invalid`，与 core 的租户级 guest 开关一致）、团队主机且匿名关着而没有会话 `403 anonymous_links_disabled`（被拒的不消耗次数）。次数的条件加一仍在 core 的行锁里（10 个并发、`max_uses = 5` 恰好 5 个成功，其余 `410 link_exhausted`）。`Issued` 在访客设备建好、令牌签出之后：断言 `jti` 带 `s.<deviceId>.<sourceId>` 前缀（`TrackJTI`）→ 登记签发索引（先 PostgreSQL 后内层，`Subject` 是访客主体）→ 读库复核链接未撤销、主机在、团队没停用、访客设备活着；任何一项不成立撤自己签的两枚、撤销刚建的访客设备并答错误——这同时堵住了「撤销链接与接受交错留下一台活着的访客设备」。`LinkUsed` 事件带账号时记 `accountId`，匿名记 `guest`。
- **链接 `list` / `update` / `revoke`**：`list` 的 `sourceId` 要管理权（看不到 `404`、管不了 `403`），`space` 过滤（团队空间调用者不是成员 `404`；个人空间 = 团队 id 为空的主机），不带过滤是调用者管得了的全部主机名下的链接（`LinksForSources` 一次查，主机 ≤ 2000、链接 ≤ 2000）；`update` 只改备注，管理权由 `Policy.CanManage` 按链接所属主机判；`revoke` 一个事务：`ManageHost` → `core.RevokeLink`（绑定到事务，撤销钩子经 `AfterCommit`）→ 审计 `link.revoke`，提交后撤访客令牌（钩子）、按 `guest:<linkId>:` 前缀踢人、发 `accessRevoked`。签链接的 driver 没有管理权，所以撤不了也改不了自己签的链接（要主机管理者来）。
- **链接访客的 `sources.assertion`**（`grants/guest.go`）：事故开关与 `assert.acct` 限流（键是访客主体）→ 主机已撤销 `410` → 链接有效且指向这台主机（`personalPolicy.Access`）否则 `403 source_access_denied` → 团队主机的团队在恢复期内 `403 source_access_denied`、停用 `403 team_suspended` → 断言取 `Intent` 的名字 / `link` 声明 / 范围（没有 `team` 与 `role`；链接路径是旧路径，不过版本门）→ 签 → 登记签发索引 → 复核（设备、主机、链接、团队）→ 审计 `assertion.issue`（行动者记 `system`，`actor_id` 是访客主体）。交错用例用 `Options.Hook` 在 `signed` / `indexed` 两点停住，撤销链接之后接受答 `link_invalid`、自己签的令牌都已撤、没有留下活着的访客设备。
- **事件**：`deviceTrustRequested` / `deviceTrustResolved`、`grantChanged` 等团队事件的发送随 S8 的事件总线（`MeStream` 要先有跨节点受众），本批客户端靠 `trust.list` 拉取；`AccessChanged`（Watcher 可见集合）同样随 S8。
- **没做 / 已知差异**：邀请自身的有效期不暴露授予到期（协议包 `invitationSchema` 没有该字段，UI 想显示要加字段，另开）；`links.create` 的 `team_join` 与 `links.accept` 的 `link_frozen` 分别随 M2 与 S7；撤销主机时不逐条撤待处理的主机邀请，预览 / 接受 / 收件箱各自核主机状态。

### 4.8 撤销即生效（两道门，能力设计 §11.2、§11.7）

**第一道门在云端**：库里改掉 → 撤销已签令牌（中继拒门）→ 踢掉在线连接 → 发事件。**第二道门在 core**：下次 `cloud/login` 时整份替换 `origin = cloud:<iss>` 的授予，或 30 天租约到期（Armadra A1 已实现）。

**收窄在下次 `cloud/login` 时才在 core 生效**：降级、缩范围这类「仍可达、但权限变小」的改动，第一道门同样撤销令牌并踢人，但客户端重连时可能还带着 core 侧已建立的会话，旧授予要等它下一次 `cloud/login` 才被替换。为此 Armadra 侧要做：**经 SaaS 断言建立的 core 会话寿命限在约 1 小时，到期必须带新断言重新 `cloud/login`**（§9.4 A3，协调方另开 Issue）。完全失去访问权（撤销、移除、停用）则在第一道门就挡住：拿不到新断言与中继令牌，旧令牌已在撤销表里。

| 触发                                                                 | 库                              | 撤销哪些已签令牌（签发索引过滤）                                     | 节点命令（全部带 `sourceId`，§2.4）                               | 事件                            |
| -------------------------------------------------------------------- | ------------------------------- | -------------------------------------------------------------------- | ----------------------------------------------------------------- | ------------------------------- |
| 授予撤销 / 降级 / 缩范围 / 到期（`grant.expire` 写 `revoked_at_ms`） | 授予行                          | 对受影响的每个账号重算有效权限，**任何收窄**都撤销 acct ∩ src 的令牌 | `kick {sourceId, subject: accountId}`                             | `grantChanged`                  |
| 成员移除 / 离开 / 降为 billing 或 guest                              | 成员行、授予                    | 对团队每台主机同上                                                   | 每台主机 `kick {sourceId, subject}`                               | `memberChanged`、`grantChanged` |
| 设备撤销、登出、改口令、找回口令、刷新令牌重放                       | 设备行                          | acct 集合里 `deviceId` 相符的（全部设备时全部）                      | 按 acct 集合展开到每台主机：`kick {sourceId, subject, deviceId?}` | —                               |
| 账号停用（运营）                                                     | `accounts.status`、全部设备撤销 | acct 集合全部                                                        | 按 acct 集合展开：`kick {sourceId, subject}`                      | —                               |
| 主机撤销                                                             | `sources.revoked_at_ms`         | src 集合全部（含隧道令牌）                                           | `closeTunnel {sourceId}` + `kick {sourceId}`                      | `sourceRevoked`                 |
| 团队停用 / 删除                                                      | 团队行                          | 团队每台主机的 src 集合全部                                          | 每台主机 `kick {sourceId}`（隧道保留）                            | `memberChanged`（删除时）       |
| 链接撤销                                                             | 链接与访客设备（core）          | src 集合里 `subject` 以 `guest:<linkId>:` 开头的                     | `kick {sourceId, subjectPrefix}`                                  | `accessRevoked`（core 现有）    |

- 带 `subject` / `deviceId` / `subjectPrefix` 的 `kick` 在 G5（N2）起的节点上按人精确踢；只有不认识这些字段的老节点才退化为踢该源全部连接（多踢不漏踢）。
- 签发与撤销的竞态：签发方一律「签令牌 → 登记签发索引 → 再读库复核，已撤销则自己撤并答错误」（见 §4.5 的 S5a 注记）。
- 时序：库事务提交 → 撤销令牌（先 PostgreSQL 后 Redis，§7.2）→ 发节点命令 → 发事件。Redis 失败时重试 3 次后插 `relay.revoke` 作业兜底（重放幂等），并记错误日志；重连时的全量重放也会补上。

### 4.9 运营后台（`cloud/admin`，S7）

| 项                             | 规则                                                                                                                                                                                                                                                                                                                 |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 运营账号                       | `accounts.scopes` 含 `operator`；只经 CLI `operators add` 设置，接口不能改                                                                                                                                                                                                                                           |
| 两步验证                       | M1 只给运营账号：`operators totp` 生成 20 字节种子（主密钥包裹进 `account_credentials`，kind `totp`），终端打印一次 otpauth；校验 RFC 6238（SHA-1、6 位、30 秒、±1 步，`last_step` 防同一步重用）                                                                                                                    |
| 登录                           | `auth.login { scope: "operator" }`：非运营账号 `403 forbidden`；没有 TOTP `403 operator_mfa_required`；口令对了答 MFA 挑战（`cloud:mfa:*`，5 分钟、每挑战 5 次）→ `auth.mfaVerify` 按**账号**限流（`mfa.acct`，10 分钟 10 次）→ 建 `scope = operator` 的设备（1 小时绝对寿命），Cookie 传输下发 `__Host-armadra-ops` |
| 准入                           | `admin.*` 只认 `Scope = operator` 的会话——Cookie 与 Bearer 都行（契约 §28 的 Bearer 运营令牌就是 Bearer 传输的运营会话）；运营会话对 `admin.*` 与自己的 `auth.refresh` / `auth.logout` 以外的过程一律 `401 unauthenticated`                                                                                          |
| 写操作                         | 都必须带 `reason`（schema 已要求），审计 `actor.kind = "operator"`、`reason` 列、`team_id` 为对象团队                                                                                                                                                                                                                |
| `accountStatus` / `teamStatus` | 停用走 §4.8；恢复只改状态                                                                                                                                                                                                                                                                                            |
| `relayNodes`                   | 读 `relay:nodes`；`relayNodeDrain` 发 `drain`（G5 N2），节点自己把 `draining` 写回节点表                                                                                                                                                                                                                             |
| 事故开关                       | `platform_settings` 键 `incident = {assertionsPaused, registrationsPaused, rejectNewConnections}`；写库后经 `relayctl` 写 `relay:incident` 并广播；控制面各实例每 5 秒读一次库（不依赖 Redis 保活）                                                                                                                  |
| 只读                           | 看不到任何内容与令牌；租户搜索按邮箱 / 显示名 / slug 前缀与包含（`ILIKE`，最多 50 行）                                                                                                                                                                                                                               |

**S7 实现注记（第二部分：运营后台）**：

- **命令行**：`operators add --email <地址>` 给账号加 `operator` 范围（幂等，邮箱不分大小写）；`operators totp --email <地址> [--reset]` 生成 20 字节种子，用主密钥包裹（附加数据绑定「用途、租户、账号」）后落 `account_credentials`，终端只打印一次 `otpauth://` 地址；账号还不是运营账号、已登记过（没带 `--reset`）、没有这个账号都是退出码 1，缺参数 64。两条命令各写一条系统审计（`operator.add`、`operator.totp`），种子不进日志。
- **登录**：`auth.login { scope: "operator" }` 口令验过之后才看范围（口令错仍是 `credentials_invalid`，不借此枚举运营账号）：非运营 `403 forbidden`、没有 TOTP `403 operator_mfa_required`，否则把挑战（账号、设备入参、过期时刻）存进 Redis `cloud:mfa:<sha256(id)>` 的哈希，答 `{ mfa: { challengeId, methods: ["totp"] } }`，不建会话。
- **`auth.mfaVerify`**：挑战不存在 / 过期（5 分钟，按控制面时钟判，Redis 的 TTL 只清理）/ 账号被停用或摘掉运营范围 → `410 mfa_expired`；按账号限流 `mfa.acct`（10 分钟 10 次，跨挑战累计，超限 `429`，连对的码也不认）；验证码 `401 mfa_invalid`（每个挑战错满 5 次即删）；TOTP 校验当前步和前后各 1 步，**命中的时间步必须大于账号已用过的最后一步**才接受（`last_step` 的原子 `UPDATE … WHERE last_step IS NULL OR last_step < $step`），所以同一个码、更早的码都不能再用，并发提交同一个码只会成功一次；通过后取走挑战（一次性），再建运营设备（传输按本次请求：带控制台 `Origin` 且不带 `Authorization` 走 Cookie，否则 Bearer；绝对寿命 1 小时，刷新不延长）并写本人安全日志 `security.operatorLogin`。运营账号的普通会话也可以用 `totpCode` 做 `auth.stepUp`（同样防重放，并另按账号计 `mfa.acct`，与 `auth.mfaVerify` 共用一个桶，换设备猜码也逃不掉）。`operators totp --reset` 在同一事务里撤销该账号的全部运营会话设备（有撤销钩子时提交后通知它；命令行没有中继连接，只撤库里的设备）。MFA 挑战的 `Fail` 是 Lua（键不存在就不写，免得造出没有 TTL 的残键），`Get` 遇到缺字段的键当作不存在。事故开关写节点失败不记「已写」，下一轮（5 秒）照库重写。
- **准入的第二道门**：HTTP 闸保证 `admin.*` 只有运营会话进得来；处理器再重读账号——账号被停用 `401`、摘掉运营范围 `403 forbidden`，会话没到期也立刻失效。
- **列表**：账号 / 团队 / 租户搜索按 `created_at_ms, id` 倒序的游标分页，一页最多 50 行（`limit` 超过按 50）；搜索词是邮箱 / 显示名 / 团队名 / slug 的 `ILIKE` 包含（`% _ \` 转义成字面量）。账号详情多 `seats`（个人空间恒为 1）、本 UTC 月 e2e 字节与最近 5 条运营动作（`audit_log` 里 `actor_kind = operator` 且对象相符的行，走 `audit_log_target` 索引），团队详情的席位是非 guest 成员数；理由只在审计表的 `reason` 列，不进事件形状。
- **停用账号**：锁账号行 → 状态与原因 → 撤销全部设备 → 提交后经撤销钩子撤已签令牌并按签发索引逐主机 `kick {sourceId, subject}`（§4.8）→ 审计 `account.disable`（`team_id` 空）；恢复只改状态（`account.enable`）。不许停用自己。**停用团队**：锁团队行 → 状态与原因 → 提交后撤销每台主机的全部已签令牌并 `kick {sourceId}`，不关隧道 → 审计 `team.suspend`（`team_id` = 对象团队，团队的 owner / admin 在 `audit.list` 里看得到，看不到理由）；恢复 `team.resume`。
- **节点**：`relayctl.Control` 加 `AllNodes`（含排空中的节点；`Nodes` 仍只答没排空的，给隧道地址选择用）。节点表里的节点都算在线（字段 60 秒过期），`lastSeenAtMs` 取读表的此刻。`relayNodeDrain` 的 `drain = true` 发 `drain` 命令并写审计 `relay.drain`；节点一旦排空只能重启回来，控制面没有取消命令，`drain = false` 是空操作、答复如实报节点此刻的状态（契约给这个过程的错误码只有 `not_found` / `forbidden`，不另答 `conflict`）。
- **事故开关**：协议补一项 `rejectNewConnections`（见 CHANGELOG 与契约 §28，只加不改）。`incidentSet` 在 `platform_settings` 行锁下读-改-写，只改给出的项，提交后写 `relay:incident` 并广播（`relayctl.SetIncident`）。`pauseAssertions` 与 `pauseRegistrations` 本来就每次读库（`sources.assertion` 答 `503 unavailable`，`auth.register` 答 `503 registrations_paused`，`platform.info.registrationsOpen` 为 false）。各控制面实例另有 `admin.Service.Start`：启动时与之后每 5 秒读库，开关或时刻变了立刻写给节点，没变也每分钟重写一遍（Redis 里的键丢了能自愈），不依赖 Redis 保活。

### 4.10 配额（`cloud/quota`）

M1 的额度表写在代码里（`quota.Plans`，数值取能力设计 §3.10 初稿；改数只改代码），`entitlements.overrides` 可逐项放宽。

| 额度（`details.quota`） | 空间 | Free      | 检查点（锁谁）                                                                                   |
| ----------------------- | ---- | --------- | ------------------------------------------------------------------------------------------------ |
| `hosts`                 | 个人 | 2         | `sources.registrationToken`（快速失败）、`sources.register`（锁账号行，权威）                    |
| `teams`                 | 个人 | 1         | `teams.create`（锁账号行；计调用者为 owner 且未删除的团队）                                      |
| `members`               | 团队 | 3         | 邀请创建与接受（锁团队行；计非 guest 成员 + 待处理的非 guest 邀请）                              |
| `hosts`                 | 团队 | 3         | 同个人主机（锁团队行）                                                                           |
| `guests`                | 团队 | 3         | 团队邀请（`guest`）与主机邀请的接受（计 guest 成员 + 待处理的 guest 邀请）                       |
| `concurrent`            | 团队 | 10        | 中继节点的 `edge.concurrent.team`（跨节点计数，G5 N4）；控制面只写额度、只展示                   |
| `traffic`               | 两者 | 5 GB / 月 | M1 只展示（`relay_traffic_buckets` 按 `team_id` 或个人空间主机汇总，入口节点的 `e2e`，单位字节） |

- 超限答 `429 limit_reached`，`details = { quota, used, limit }`，按协议包 `./errors` 的 `limitReachedDetailsSchema`；`quota` 取 `QUOTAS`（`teams`、`hosts`、`members`、`guests`、`concurrent`、`traffic`，契约 §21，#72），`teams.usage` / `billing.usage` 的 `metric` 用同一张表。
- `teams.usage`、`billing.usage` 用同一组统计函数；`billing.usage` 的 `granularity = day` 按 UTC 天汇总当前计费周期（M1 周期 = 自然月）。
- 分组、访问申请等「计划不含」的功能答 `403 plan_feature_unavailable`。

**S7 实现注记（第一部分：审计、用量与保留期）**：

- **审计读取**：`audit.list` 的包 `cloud/audit` 不 import `cloud/teams`（teams 已经 import 它来写日志），团队范围由装配方注入一个 `Authorizer`（`teams.Require` + `MinAdmin`）；`TeamScope` 的唯一构造点不变。游标是 `(at_ms, event_id)` 倒序的不透明编码，坏游标 `400 bad_request`；`actor` 精确匹配 `actor_id`，`action` 按前缀（`% _ \` 转义）。`me.securityLog` 只读本人 `account_id` 且 `action LIKE 'security.%'`、`team_id IS NULL` 的行。
- **保留期**（`audit.retention`，每天一次，时间槽 `audit.retention:YYYY-MM-DD`）：按最短保留期（Free 7 天）找出有旧行的团队（按团队 id 升序翻页），再按各团队**此刻**的计划算界线（`quota.AuditRetention`：free 7 / pro 30 / team 90 / enterprise 365 天，未知计划按 free），一个团队一个团队地分批（5000 行）`DELETE … WHERE team_id = $2`；账号安全日志固定 90 天；整月都早于「现在往前 13 个月」的分区 `DROP`（`DB.DropAuditPartitionsBefore`，同样先 `pg_advisory_xact_lock` 再 `SET LOCAL lock_timeout = '5s'`）。运营动作（没有团队的非 `security.` 行）只随整月分区 DROP。`audit.partitions` 每天补建当月与后两个月。
- **已清除团队的审计行保留**：保留期扫描从 `teams` 表出发（每个团队一次 `EXISTS` 走 `audit_log_team`），团队清除后 `entitlements` 与团队行都没了，它的审计行不按 free 的 7 天删，只随 13 个月的整月分区 `DROP`（§3.4：清除时审计行保留）。账号安全日志的清理有部分索引 `audit_log_security`（迁移 0010）。
- **`traffic.retention`**：删早于 `TRAFFIC_RETENTION_DAYS` 的流量桶（走 `(tenant_id, bucket_start_ms, …)` 主键前缀，`ctid` 分批）。
- **用量**：`teams.usage` 与 `billing.usage` 共用 `teams.TeamUsage`；个人空间用量是个人主机数（`CountPersonal`）、名下团队数、个人空间主机的 e2e 流量（`source_spaces.team_id IS NULL` 且 `relay_traffic_buckets.team_id = ''`）。`granularity = day` 的 `series` 补零到本周期每一天（UTC 零点）。`billing.plan` 的 `periodEndsAtMs` 是本 UTC 月底，`seats` 个人为 1、团队为非 guest 成员数；团队空间要 owner 或 billing（`teams.MinBilling`），个人空间永远是调用者自己的。

### 4.11 存储端口（`internal/store/saas`）

```go
// TeamScope 是「已核过成员与角色」的团队范围。字段不导出，只有 teams.Require 能构造；
// 团队范围的仓储方法只收它，不收裸 teamID——跨团队读取在编译期就写不出来。
type TeamScope struct {
	tenantID string
	teamID   string
	role     TeamRole
}

// Store 是 SaaS 的存储端口，只有 PostgreSQL 实现（(*postgres.DB).SaaS()）。
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
	Jobs() JobRepo              // Enqueue（含 dedupe / slot、NOTIFY）、发件箱
	Settings() SettingsRepo     // platform_settings、entitlements
	Issued() IssuedRepo         // cloud_issued_tokens、relay_revoked_tokens
	AfterCommit(fn func(ctx context.Context))
}
```

- 每个 Repo 方法的第一个参数是 `tenantID`（或已含它的 `TeamScope`）；团队范围的方法收 `TeamScope`，查询必写 `team_id = $2`。`teams.Require` 是 `TeamScope` 的唯一构造点（同包内的构造函数不导出，由 `store/saas` 暴露给 `cloud/teams` 的受限入口；`repo.rules.json` 守住只有 `cloud/teams` 能 import 它）。
- 核心端口（`store.Ports`）与 SaaS 端口共用同一个 `pgxpool`；需要同事务写核心表（登记主机、建设备）时由 `postgres` 包提供 `TxPorts(tx)` 把核心端口绑到同一个事务。**`TxPorts` 同时定下统一的加锁顺序：团队行 → 账号行 → 其余（`LockTeam`、`LockAccount` 按此序调用，避免两个事务反序互等）；同一类的多行按 id 升序加锁（批量用 `LockTeams` / `LockAccounts`，它们自己排序，单个调用反序报 `ErrLockOrder`），加锁失败不改变顺序状态。绑定事务里读核心行（读-改-写）一律 `FOR UPDATE`，并发的读-改-写按行排队、不丢更新；`registrations.Consume` 核对 `RowsAffected() == 1`。**禁止在 `Tx` 闭包里调用用独立 Ports 的 core 函数**（独立端口走别的连接，会等本事务持有的行锁而挂死），闭包里一律用 `tx.Ports()`；SaaS 事务里绑定的核心端口免掉核心写路径的 `tenants` 行 `FOR UPDATE`（那把租户级锁会把所有 SaaS 写串行化），`devices.Insert` 清理失效设备改为按账号（`subject`）清理，不扫整个租户。**
- 错误：唯一冲突（23505）按约束名映射成 `team_slug_taken`、`email_taken`、`conflict`；`team_has_owner`（23514）映射成 `team_last_owner`。

## §5 HTTP 层

### 5.1 认证与会话（`cloud/httpx`）

| 凭据                                   | 携带                                                                                                                                                                 | 能用于                                                                  |
| -------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| 控制台会话（cookie / session）         | `__Host-armadra-cloud=<访问令牌>`（`Max-Age=900`）+ `__Host-armadra-cloud-refresh=<刷新令牌>`（`Max-Age=2592000`）；都是 `Path=/; Secure; HttpOnly; SameSite=Strict` | 除 `admin.*` 外的会话类过程                                             |
| 运营会话（cookie / operator）          | `__Host-armadra-ops` + `__Host-armadra-ops-refresh`（属性同上，`Max-Age` 不超过会话剩余寿命）                                                                        | `admin.*` 与自己的 `auth.refresh` / `auth.logout`                       |
| Bearer（bearer / session 或 operator） | `Authorization: Bearer <访问令牌>`，刷新令牌在请求体                                                                                                                 | 原生 App、core、CLI；`scope = operator` 的可调 `admin.*`                |
| 源                                     | `Authorization: Source <jws>`                                                                                                                                        | `sources.heartbeatHint`、`sources.trustRequest`、`sources.trustResolve` |

- **全部是 `__Host-` 前缀、`Path=/`**（契约 §1，#72；§10 Q3）：子域写不进、路径改不了。刷新 Cookie 每个请求都会带上，服务端只在 `auth.refresh` / `auth.logout` 读它。
- **同名 Cookie 重复**（`Cookie` 头里同一个名字出现两次以上，常见于 Cookie 投放攻击）→ `401 unauthenticated` 并在答复里清掉这些 Cookie（契约 §1 的最终写法），不挑其中一个用。
- **传输绑定**：`transport = cookie` 的设备的访问令牌只认从 Cookie 来；`bearer` 的只认从头来；对不上答 `401 unauthenticated`。登录类过程按请求定传输：带 `Origin = APP_ORIGIN` 且不带 `Authorization` → Cookie 传输（响应 `Set-Cookie`、体里不含 `refreshToken`、含 `csrfToken`）；否则 Bearer 传输。
- **不混用**：同一请求既有任一会话 Cookie 又有 `Authorization` → `400 bad_request`（能力设计 §11.6）；两套会话 Cookie 同时存在是允许的，按过程挑：`admin.*` 与带 `scope: "operator"` 的刷新 / 登出用运营那一套，其余用普通那一套。
- **刷新与登出**：`auth.refresh` 不带 `refreshToken` 时取对应的刷新 Cookie；成功后两枚一起换新。`auth.logout` 撤销设备并用 `Max-Age=0` 清掉该会话的两枚 Cookie。无论认证成败都清；访问 Cookie 缺失或过期时，用同范围的刷新 Cookie 找到设备再撤销（仍要求 `Origin` 等于控制台来源、`X-Armadra-CSRF` 用该设备的值）。

### 5.2 CSRF 与来源

| 规则        | 内容                                                                                                                                                                                                         |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 值          | `csrfToken = base64url(HMAC-SHA256(K_csrf, "armadra/csrf\0" + deviceId))`，`K_csrf = HKDF-SHA256(主密钥, info "armadra-cloud/csrf")`；无状态，同一设备不变，`auth.refresh` 照常返回它                        |
| 校验范围    | Cookie 认证的每个非 GET / HEAD 请求（含 `auth.logout`，**不含 `auth.refresh`**）：`X-Armadra-CSRF` 与该 Cookie 对应设备的值常量时间比较，不符 `403 forbidden`                                                |
| Origin      | 同一批请求还要求 `Origin` 头恰好出现一次且等于 `APP_ORIGIN`（`auth.refresh` 也要），否则 `403 forbidden`；这挡住同站的兄弟子域（`*.src.<域>` 与 `app.<域>` 同站，`SameSite` 挡不住；长期见 §2.3 的独立域名） |
| 运营 Cookie | 用运营设备自己的值                                                                                                                                                                                           |

### 5.3 CORS、页面与能力

- `/v1/*` 与 `/.well-known/*`：对任意来源答 `Access-Control-Allow-Origin: *`、`Allow-Headers: authorization, content-type`，**从不**发 `Allow-Credentials`——浏览器跨源请求带不上 Cookie，跨源只能用 Bearer。控制台同源，不走 CORS。
- 托管页面（`WEB_ROOT`，S9）：复用 `internal/web`；`/`、`/login`、`/verify`、`/reset`、`/t/*`、`/h/*`、`/j/*`、`/device`、`/invite/*`、`/ops/*` 回落 `index.html`；所有 HTML 带 `Referrer-Policy: no-referrer` 与 CSP：`default-src 'self'; connect-src 'self' wss://*.src.<域> https://challenges.cloudflare.com; script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; frame-ancestors 'none'; base-uri 'none'`。
- `platform.info.capabilities`（SaaS M1）：`auth.password`、`auth.password-change`、`auth.device-code`、`links.source-invite`、`links.update`、`links.scope`、`me.stream`、`teams`、`invitations`、`grants`、`audit`、`billing`（只有 `plan` / `usage`，其余过程 501），配了挑战时加 `challenge.turnstile`。不报 `auth.passkey`、`auth.totp`、`auth.oauth`、`groups`、`access-requests`、`api-tokens`、`links.team-join`、`me.traffic`、`sources.hosted`、`orgs`。

### 5.4 限流与挑战

速率桶在 Redis（`cloud:limit:`，§2.4 的独立限流器），窗口固定；`429` 带 `details.retryAfterMs` 与 `Retry-After`。键以 `.ip` 结尾的种类按客户端 IP 限流，**IPv6 截到所在的 /64**（只影响限流键）。

| 种类                                | 键            | 额度                         | 过程                                                                    |
| ----------------------------------- | ------------- | ---------------------------- | ----------------------------------------------------------------------- |
| `login.ip`                          | IP            | 20 / 分钟                    | `auth.login`、`auth.changePassword`、`sources.register`（沿用）         |
| `login.acct`                        | 邮箱的哈希    | 20 / 分钟                    | `auth.login`（另有逐账号锁定）                                          |
| `register.ip`                       | IP            | 10 / 小时                    | `auth.register`                                                         |
| `reset.ip` / `reset.email`          | IP / 邮箱哈希 | 10 / 小时、3 / 小时          | `auth.passwordResetStart`                                               |
| `verify.acct`                       | 账号          | 3 / 小时                     | `auth.emailVerifyResend`、`me.emailChange`                              |
| `token.ip`                          | IP            | 30 / 分钟                    | `auth.emailVerify`、`auth.passwordResetFinish`、`invitations.get`       |
| `device.start.ip` / `device.get.ip` | IP            | 30 / 小时、30 / 分钟         | `auth.deviceStart`、`auth.deviceGet`（批准 / 拒绝共用 `device.get.ip`） |
| `device.poll.ip`                    | IP            | 120 / 分钟                   | `auth.devicePoll`                                                       |
| `device.guess.acct`                 | 账号          | 10 / 小时（只计 `404` 失败） | `auth.deviceGet`、`auth.deviceApprove`、`auth.deviceDeny`               |
| `stepup.dev`                        | 设备          | 10 / 10 分钟                 | `auth.stepUp`                                                           |
| `mfa.acct`                          | 账号          | 10 / 10 分钟                 | `auth.mfaVerify`（每挑战另限 5 次）                                     |
| `assert.acct`                       | 账号          | 60 / 分钟                    | `sources.assertion`                                                     |
| `trust.src`                         | 源            | 30 / 分钟                    | `sources.trustRequest`                                                  |
| `invite.acct` / `invite.resend`     | 账号 / 邀请   | 50 / 小时、5 / 小时          | `invitations.create` / `resend`                                         |
| `link.accept.ip`                    | IP            | 20 / 分钟                    | `links.accept`（沿用）                                                  |

挑战（Turnstile，契约 §16）缺省范围：`auth.login`、`auth.register`、`auth.passwordResetStart`、`links.accept`；校验沿用 `core.ChallengeGate`，先于口令与锁定、晚于按 IP 限流。原生 App 与 core 经同源挑战页取令牌，规则不变。

### 5.5 路由、入参与错误

- **路由**：`api.New(Options{Mode: ModeCloud, Handlers: cloud.Handlers(...)})`；每批把实现的过程加进 `Handlers`，其余照旧 `501 not_implemented`（测试守住「启用集合 = 本文 §1.1 该批之前的累计」）。`platform.health` 由外层答（`Exclude`）。
- **认证中间件**：在 `api.Call` 上加 `Transport` 与 `Cookies`，`Call.Caller` 先按 §5.1 选凭据再走 `AuthenticateToken`；重复 Cookie、CSRF 与 Origin 在处理器之前、入参校验之后。
- **错误**：处理器只返回 `core.Fail` / `core.FailWith`；存储错误按 §4.11 映射；其余一律 `500 internal`，日志只记错误类名与 `requestId`。
- **入参解释器的缺口（S0a，第一包）**：`gen.UnsupportedInputs` 现有 13 个过程因 `format`、`minItems`、`propertyNames` 没有入参 schema，`NewRouter` 拒绝给它们装处理器。M1 用到其中 5 个（`auth.register`、`auth.stepUp`、`auth.passwordResetStart`、`me.emailChange`、`invitations.create`）。S0a 在 `internal/protocol/schema` 与 `tools/schemagen` 补：

  | 关键字                                       | 语义（与 zod 一致）                                                         |
  | -------------------------------------------- | --------------------------------------------------------------------------- |
  | `minItems`                                   | 数组长度下界                                                                |
  | `format: email`                              | 与 zod 4 `z.email()` 同一个正则（从协议包常量导出，避免两边各写一份）       |
  | `format: uri`                                | 与 `z.url()` 一致：`url.Parse` 成功、有 scheme 与 host                      |
  | `propertyNames` + `additionalProperties: {}` | 键是字符串、值不限（`z.record(z.string(), z.unknown())`）；剪枝时保留全部键 |

  另补 query 数组：schema 为 `array` 的参数收全部值，认三种写法——重复 `tag=a&tag=b`、`tag[]=a`、`tag[0]=a&tag[1]=b`（官方 oRPC 客户端的写法，下标必须是规范十进制且恰为 0..n−1，否则 400）；非数组参数重复出现、同一父键既平铺又括号（`tag` 与 `tag[x]`、`space` 与 `space[kind]`）都答 400 `bad_request`，不再取最后一个；不成形的括号键（`space[kind][x]`）前缀若是已声明的属性同样答 400，不剪枝放过。补完 `UnsupportedInputs` 为空，`internal/api` 的夹具测试与路由测试相应收紧（「每个过程都有入参 schema」）。

  **解释器表达不了、处理器必须自己补的校验**：
  - 带 `.refine` 的过程（例如 `auth.stepUp` 的「口令与 passkey 断言至少给一项」）：JSON Schema 导出里没有这类约束，解释器不会拒绝两项都缺的输入，处理器自己判并答 `bad_request`。
  - `format: uri` 只保证形状（有 scheme、非空主机名、端口 0–65535、主机字符合法；非 ASCII 主机名先经 `golang.org/x/net/idna` 转 punycode 再校验，与 z.url() 对 IDN 的接受一致，另套双向文本规则 `idna.BidiRule()`；已知差异：`127.0.0.999` 这类数字结尾的非法 IPv4 主机，Node 拒绝而 Go 放过，不在本批修），`javascript://x` 之类 TS 与 Go 两边都接受。`redirectUri` / `returnUrl` 这类会被服务端跳转或写进邮件的地址，处理器必须按来源（团队或租户登记的域名）与协议（只许 `https`，开发环境 `http://localhost`）白名单校验，不能只信 `format`。

## §6 邮件与作业

### 6.1 作业（`cloud/jobs`）

| 规则 | 内容                                                                                                                                                                                                                                                                                                                                                                            |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 入队 | 业务事务里 `Enqueue(kind, payload, runAtMs, dedupeKey?, slotKey?)`（`ON CONFLICT DO NOTHING`），同一事务里 `SELECT pg_notify('cloud_jobs', kind)`——随提交送达，回滚即不发                                                                                                                                                                                                       |
| 去重 | `dedupe_key`：同 kind 只防**未完成**的重复（如 `team.purge` 按团队）；`slot_key`：定时作业按时间槽唯一，**完成后也不能再插**（如 `audit.retention:2026-10-23`），多实例的调度器各插一次只成功一个                                                                                                                                                                               |
| 领取 | `UPDATE jobs SET locked_by=$1, locked_until_ms=$now+60000, attempts=attempts+1 WHERE job_id IN (SELECT job_id FROM jobs WHERE done_at_ms IS NULL AND failed_at_ms IS NULL AND run_at_ms <= $now AND (locked_until_ms IS NULL OR locked_until_ms < $now) ORDER BY run_at_ms LIMIT $n FOR UPDATE SKIP LOCKED) RETURNING …`；每个实例 `LISTEN cloud_jobs`，收到即领，否则 1 秒一轮 |
| 续租 | 跑得久的作业（清除、保留期、批量撤销）每 20 秒把 `locked_until_ms` 往后推 60 秒（`WHERE locked_by = 自己`，推不动说明租约已丢，立即停手）                                                                                                                                                                                                                                       |
| 完成 | 成功置 `done_at_ms`；失败 `run_at_ms = now + min(30 秒 × 2^(attempts-1), 1 小时)`，`last_error` 只记类名或错误码；到 `max_attempts` 置 `failed_at_ms` 并记错误日志                                                                                                                                                                                                              |
| 幂等 | 每个作业处理器必须可重放：先读状态再动作（例如发信前看 `outbox_emails.sent_at_ms`，清除前看团队是否已清）                                                                                                                                                                                                                                                                       |
| 清理 | 完成超过 7 天、失败超过 30 天的作业由 `jobs.gc` 删（发件箱的 `job_id` 随之置空）                                                                                                                                                                                                                                                                                                |
| 分区 | `audit.partitions` 与启动检查调 `audit_ensure_partition` 前，同一事务先 `pg_advisory_xact_lock` 再 `SET LOCAL lock_timeout = '5s'`：摘 / 挂兜底分区要父表的排他锁，拿不到就失败重试，不能无限排在业务写入前面堵住全库                                                                                                                                                           |

| 作业                                                                                  | 触发                        | 动作                                                                                                                                                       |
| ------------------------------------------------------------------------------------- | --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mail.send`                                                                           | 发件箱                      | §6.3                                                                                                                                                       |
| `grant.expire`                                                                        | 授予有到期时间时排到那一刻  | 写 `revoked_at_ms`（`revoked_by = 'system'`）+ 撤销流程（§4.8）                                                                                            |
| `team.purge`                                                                          | 团队删除                    | §4.4                                                                                                                                                       |
| `relay.revoke`                                                                        | Redis 写撤销失败的兜底      | 重写撤销与命令                                                                                                                                             |
| `invitations.expire`、`trust.expire`、`deviceCodes.gc`、`emailTokens.gc`、`issued.gc` | 每小时（`slot_key` = 小时） | 落库过期并发事件；删过期 24 小时以上的行；删已过期的签发索引（`cloud_issued_tokens`，`tenant_id = $1 AND exp_ms < $2`，走 `tenant_id` 打头的索引）与撤销行 |
| `outbox.gc`                                                                           | 每天                        | 删发出或失败满 30 天的发件箱行（收件地址等个人信息最多留 30 天）                                                                                           |
| `audit.partitions`、`audit.retention`                                                 | 每天                        | 补建未来两个月分区；按计划删团队审计超期行、账号安全日志满 90 天的行、`DROP` 13 个月前的分区                                                               |
| `traffic.retention`                                                                   | 每天                        | 删早于 `TRAFFIC_RETENTION_DAYS` 的流量桶（节点侧的清理关掉，G5 N5）                                                                                        |

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

- 模板在 `cloud/mail/templates/<locale>/<name>.{subject,txt,html}`（`go:embed`）；两种语言的模板集合必须相同（测试守住）；文案遵守 AGENTS.md 的界面与文案规则。
- **令牌一律放在链接的 `#` 片段里**：`<APP_ORIGIN>/verify#<令牌>`、`/reset#<令牌>`、`/invite/<id>#<令牌>`；页面读片段后用 POST 交给接口，令牌不进服务器与代理的访问日志，也不经 `Referer` 外泄（页面 `Referrer-Policy: no-referrer`）。
- **令牌在发信时现生成**：发件箱与作业载荷只有账号或邀请 id；`mail.send` 在自己的事务里生成令牌、写哈希（同目的的旧令牌作废）、提交后发信。重试会换新令牌，旧的随之作废——用户只会拿到最后一封有效。明文令牌只存在于内存与发出的邮件里。

## §7 跨节点

### 7.1 me-stream 与领域事件

- `me.streamTicket` 把票写进 Redis（§2.4），`GET /v1/me/stream` 在任一控制面实例上 `GETDEL` 兑换；每 30 秒复核设备（核心现有逻辑）。
- 事件源：领域服务在事务提交后 `events.Publish(event, audience)` → `PUBLISH cloud:events` → 每个实例按受众投递给本机连接（自己 `origin` 的消息本机已投递，不再投一次）。在线事件不经 `cloud:events`：每个实例都经 `relayctl` 的 `Plane.Presence().Subscribe` 收 `relay:presence`。
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

- `invitationChanged` 推给被邀请邮箱对应的账号时，**只推邮箱已验证的账号**（与 `invitations.list` 的 `received` 同一条规则，§4.4）。

### 7.2 撤销、签发索引与持久化（`cloud/relayctl`）

- 接口 `relayctl.Control`：`IndexIssued(ctx, accountID, sourceID, deviceID, jti, exp)`、`RevokeWhere(ctx, Filter)`、`Kick(ctx, KickFilter, reason)`、`CloseTunnel(ctx, sourceID)`、`Route(ctx, sourceID)`、`Nodes(ctx)`、`Drain(ctx, node)`、`SetTeamQuota(ctx, teamID, Quota)`、`SetIncident(ctx, Incident)`。S1b–S7 用内存实现（行为相同，测试断言发出的命令与外壳）；S8 换成包装 `control/redis.Plane` 的实现。
- 实现 `core.RelayPort`（`Route`、`CloseTunnel`、`Disconnect`）与 `core.RevocationSink`，所以核心的撤销路径（登出、链接撤销、刷新重放等）自动走到这里。
- **落盘**（Redis 里的撤销与索引都能丢，PostgreSQL 里的不能）：
  - 签发：先 `INSERT cloud_issued_tokens`，再写 Redis 的两个有序集合。
  - 签发索引的查询、重放、清理一律带 `tenant_id = $1`（`cloud_issued_tokens` 的主键与三个索引都以 `tenant_id` 打头），不跨租户扫表。
  - 撤销：先 `INSERT relay_revoked_tokens (jti, exp_ms) ON CONFLICT DO NOTHING`（0002 的表），再 `Plane.Tokens().Revoke`。
  - **重放**：`serve` 启动时与 Redis 订阅每次重新就绪时（`Plane` 的就绪回调），把未过期的 `relay_revoked_tokens` 全部重写进 Redis（`SET … PXAT … NX`，pipeline 分批），并从 `cloud_issued_tokens`（`WHERE tenant_id = $1 AND exp_ms > now`）重建 `cloud:issued:*`；重放完成前不对外答 `/ready`。
  - 过滤（§4.8 的「acct ∩ src」等）以 PostgreSQL 为准，Redis 的有序集合只是快路径。
- Redis 本身开 AOF（`appendfsync everysec`，§2.3），重启后多数状态直接在；落盘与重放兜住 AOF 丢的那一秒与整库丢失。
- 命令：总是经 `relay:ctl` 广播；G5 之后对已知持有节点再额外发 `relay:node:<id>`（§2.4）。
- **团队额度全量重写**：`serve` 启动时与 Redis 订阅每次重新就绪时，按 `quota.Plans` 加各团队的 overrides 把全部团队的 `relay:quota:team:<teamId>` 重写一遍（pipeline 分批），之后变化照常 `SET` + `kind: quota` 广播。节点在额度键缺失或坏了时按最严格的缺省（Free：并发 10，速率 600 次 / 分钟，可用 `RELAY_TEAM_CONCURRENT` / `RELAY_TEAM_CONNECT_PER_MINUTE` 覆盖），所以 Redis 丢数据之后要靠这次重写恢复各团队的真实额度。

### 7.3 节点表与 `heartbeatHint`

- `Nodes()` 读 `relay:nodes`，去掉 `draining` 的节点；表为空时 `heartbeatHint` 答 `503 unavailable`（不兜底到共用地址）。
- 排序按 sourceId 做 rendezvous 哈希（`sha256(sourceId + "\0" + node)` 取前 8 字节为 64 位整数，加权：`-ln(u) / weight`），答前三个，`weight` 递减为 3、2、1（Go 设计 §3.6）；`nodes[].url` 是各节点自己的隧道地址。同一函数放 `internal/control`，G5 节点侧共用。

### 7.4 S8 实施注记（relayctl 的 Redis 实现，PR 第一部分）

- **`relayctl.Redis`**：持有一个 `control/redis.Plane`（前缀 `relay:`，`cluster.Options.Origin = cloud/<实例 id>`，S8 为此加了 `Origin` 选项），路由读取（`Plane.Routes().Get`）、撤销（`Plane.Tokens().Revoke`）、命令、在线事件订阅都经它；线上格式一律取 `cluster` 包。为了让写入返回错误，`Plane` 加了 `CommandContext`（同步版 `Command`，可同时发 `relay:node:<id>`）、`PublishQuota`、`PublishIncident`。
- **命令**：总是经 `relay:ctl` 广播；`kick` / `closeTunnel` 对路由表里已知的持有节点再额外发 `relay:node:<id>`，`drain` 发到被排空的节点；`kick` 缺 `sourceId` 直接拒绝（不发）。
- **签发索引**：`cloud:issued:acct:<账号>`、`cloud:issued:src:<主机>` 两个有序集合如 §2.4（写前 `ZREMRANGEBYSCORE -inf <now>`，`PEXPIRE 3600000`）；**新增** `cloud:issued:jti:<jti>`（`SET … PXAT exp`，值是成员串）：签发方复核只知道 jti 时按它找回条目（`Filter{JTI}`）。选法：给了 `Subject` 走账号集合，否则给了 `SourceID` 走主机集合，否则只给 `JTI` 走 jti 键；只给设备或只给 subject 前缀而没有主机时 Redis 里选不出来，那种撤销以 PostgreSQL 为准（`Durable` 把 PostgreSQL 命中的逐枚交给 Redis 撤）。隧道令牌（`accountId = -`）只进主机集合。
- **撤销写失败**：`revokeToken` 重试 3 次（100 / 200 毫秒退避），仍失败记错误日志并调 `RevokeFailed`，装配方把它排成 `relay.revoke` 作业（按 jti 去重，`Redis.ReplayRevoke` 再走一遍 `Tokens().Revoke`，失败由作业运行器退避重试；PostgreSQL 里已有这条撤销）。命中的条目照常返回，调用方据此踢人，错误合并返回。
- **重放（`relayctl.Sync`）**：`Plane` 的就绪回调（`OnSubscribed`，每次订阅重新就绪都调）与 `serve` 启动（监听之前，显式一次）各跑 `Sync.Run`：未到期的 `relay_revoked_tokens` 全部 `SET … PXAT … NX`（pipeline 每批 500，已有的不动）、`cloud_issued_tokens`（`WHERE tenant_id = $1 AND exp_ms > now`）重建 `cloud:issued:*`、按 `quota.Plans` + 各团队 `entitlements.overrides` 把全部 `relay:quota:team:*` 重写（存储端口加 `TeamEntitlements`；不限写成 `MaxInt32`，节点的解码器不收负数）。`Sync.Ready` 在重放进行中答 `ErrSyncing`，装配进 `/ready` 的探针 `relay`。第一次就绪时 `Sync` 还没建好（它依赖 `Plane`），回调直接放行，由 `serve` 的显式一次接上；就绪回调失败则这次订阅作废、退避重连再来（`Plane` 既有行为）。
- **部分故障的兜底作业**（审查后）：Redis 部分故障（写被拒而订阅与读仍健康）时撤销不能只留在 PostgreSQL：`Durable`（打开 `EnableRetryJobs`，Redis 实现才开）在落盘撤销的**同一个 PostgreSQL 事务**里为每枚命中的令牌插 `relay.revoke` 作业（`DedupeKey = jti`，首次到期延后 2 秒给正常路径留时间），内层写成功后取消，失败则由作业运行器退避重试；补登记索引失败不再跳过撤销（Redis 实现直接凭 jti 与到期撤）。踢人命令发布失败排 `relay.kick` 作业（按 源|subject|设备 去重）。`nodes revoke` 同样在写 PostgreSQL 的事务里排 `relay.revoke`，由运行中的控制面重试。
- **额度重放**：`WriteQuotas` 先 `MGET` 旧值，键丢了或值变了的团队补发 `kind: quota` 广播；重放写完再读 PostgreSQL 核对，不一致就再写（最多三轮），收敛重放与额度变更的交错。
- **`ClearTeamQuota`**：只 `DEL` 键；在线节点保留旧值到下次订阅就绪时全量重读（团队清除后没有流量）。
- **`relay:presence`**：装配订阅 `Plane.Presence()`，在内存累计主机的最近在线与核心版本，每 5 分钟（与设备最近使用同一个定时器）和关闭时批量落 `sources.last_seen_at_ms` / `core_version`，只往后推；多实例各记各的、落盘取较大值。
- **节点表**：`Nodes()` 读 `relay:nodes`（`HGETALL`，坏记录跳过、`draining` 去掉）；`heartbeatHint` 的 rendezvous 沿用 S5a 已有的 `control.RankNodes`，节点表为空仍答 `503`。
- **`nodes token`**：`--node <节点名> --days <1–365，缺省 30>`，令牌打印到 stdout 一行，jti 与到期打印到 stderr；头 `typ = armadra-node`，声明 `{iss, sub, aud = relay-node, iat, exp, jti = n.<节点名>.<24 位 hex>, ver}`。上限 365 天是 **新增约束**（`nodes revoke` 不知道令牌何时到期，撤销记录按 366 天保留，到期自然消失）。`nodes revoke --jti <jti>`：只认节点令牌的 jti 形状（不拿它撤别的令牌）；先 `INSERT relay_revoked_tokens … ON CONFLICT DO NOTHING`，再 `SET relay:revoked:<jti>` 并广播（重试 3 次）；PostgreSQL 写成功而 Redis 失败时报错退出 1（撤销已落盘，控制面启动与重新连上 Redis 时重放）。
- **逐账号查询批量化**：存储端口加 `AccessBases(accountIDs, sourceID)`（固定四条查询，答案与逐个 `AccessBasis` 逐字段一致，有等价测试），访问清单按 200 个候选一批、收窄快照一次取齐。
- **登录失败计数的 Redis 故障**：失败关闭，见 §4.3。
- **测试装配**：`server.Deps.Relay` 注入内存实现（S1b–S7 的断言继续用它）；`rigOptions.redisRelay` 用真正的 Redis 实现，键前缀随机（`relay-test:<随机>:`、`cloud-test:<随机>:`），结束时只删这两个前缀，不碰别的键；「Redis 清空」测试同样只清自己的前缀。

### 7.5 S8 实施注记（cloud:events 与 me-stream，PR 第二部分）

- **事件总线**：`core.EventBus` 加 `SetPublisher` 与 `Deliver`：装配时 `Hub` 成为 publisher，所有 `c.Emit(...)`（`sourceRevoked`、`accessRevoked`、`linkUsed` 以及新的领域事件）先进 `Hub`；`Hub.Publish` 在本机排队分发的同时异步 `PUBLISH cloud:events`，订阅到别的实例的消息同样进本机队列，由一条协程按序「`Prepare`（可做 I/O）→ `Bus.Deliver`」。外壳 `{origin, event, audience}`，`origin = cloud/<实例 id>`，自己发的消息不再投一次；不认识的 `type` 与坏外壳忽略；新的领域事件种类用泛型函数 `events.Register`（类型参数是事件的 Go 类型，参数是事件的 `type`）登记（S6 / S7 的信任请求、计划变更走这条）。队列满了丢事件并记警告（事件只是提示，客户端重拉）。订阅断线重新就绪后 `OnResubscribed` 以 1012 关闭本机全部 me-stream 连接，客户端重连并重新拉取（断线期间的事件丢了）。
- **受众**：`core.Audience` 加 `AccountIDs`（`Includes` 判定；空受众不投给任何人）。`events.Publisher`（nil 安全）在事务里登记，**提交之后**才读库算受众并发出：`memberChanged` = 团队全体成员加变化的账号（被移除的人也收到；团队删除不带 `accountId`，受众是此刻的全体成员）；`invitationChanged` = 邀请人、团队 owner / admin、被邀请邮箱对应的账号——**只有邮箱已验证的账号**（主机邀请再加主机所有者）；`grantChanged` = 主机所有者、团队 owner / admin、授予主体涉及的账号（`team` 主体是团队全体成员）。发出点：`teams.create`（创建者 added）、`memberRole`（roleChanged）、`transferOwnership`（两人 roleChanged）、移除与离开（removed）、`delete`（团队删除）、邀请的创建 / 重发（pending）/ 撤销 / 接受 / 拒绝 / 到期作业（expired）与接受时的 added、授予的创建 / 更新 / 撤销 / 到期 / `accessRevoke`。
- **可见集合缓存**：连接建立时 `Policy.Watch` 解析一次可见集合（`visibleSet`）。事件本机分发前，`Hub` 的 `Prepare`（装配里的 `accessPreparer`）把领域事件转成可见集合更新：`grantChanged` → 受众里在本实例有连接的账号逐个重算对这台主机的可见性，作为内部事件 `grants.AccessChanged` 交给 `Watcher.Refresh`（`core.InternalEvent`，不进客户端队列）；`memberChanged` → 变动账号在本实例的连接整份重算（`core.Resyncer`，团队里一次变很多台主机）。重算完成之后才把事件投给客户端，所以客户端收到事件时可见集合已是新的。
- **主机上下线**：`coreCtx.Presence` 接 `Plane.Presence()`（`relay:presence`），`core.MeStream` 整个只订阅一次、每个事件查一次源再分发给各连接；`sourceRevoked` 走 `cloud:events`。
- **修正 `core` 的一个顺序**：`attachment.offer` 现在在 `Refresh` 之前判可见性——否则 SaaS 的 `Watcher` 在 `sourceRevoked` 上先把主机从可见集合删掉、事件随后被「看不到」过滤掉，撤销之前看得到它的人收不到 `sourceRevoked`。
- **票**：`me.streamTicket` 的票写进 Redis `cloud:ticket:<sha256>`（值 `{accountId, deviceId}`，`accountId` 在访客会话里是 sub，30 秒，`GETDEL` 一次性），任一实例发的票可在任一实例的 `GET /v1/me/stream` 兑换；兑换时由设备记录重建调用方（`core.CallerFor`，与访问令牌认证共用）并复核设备仍有效。WebSocket 外壳在 `cloud/mestream`（与个人中转同一套协议：子协议 `armadra-ticket.<票>`、30 秒 ping 与会话复核、有界队列），关闭时先以 1001 关闭所有连接再停 HTTP。
- **审查后的补强**：① 30 秒心跳在复核设备之后调连接自己的重算函数（`AttachResyncable`），Prepare / 发布失败或事件丢了，可见集合最多过期一个心跳；② 团队邀请的邀请人只在仍是 owner / admin 时才进受众；③ `Attach` 先登记、先订阅总线再解析可见集合，窗口里到的事件先存着、解析完按序补投；④ 准备阶段每个账号单独超时（3 秒）、8 路并发，`grantChanged` 仍逐账号走 `Policy.Access`（可见性不等于有效权限非空，团队 owner / admin 看得到全部主机，所以不用 `AccessBases` 合成），同一账号的整份重算合并（在跑时来的多次请求只多算一轮）；⑤ 订阅重连后的 1012 关闭在 0–10 秒内随机错开；`teams.restore` 发 `memberChanged`（不带 `accountId`）；`RevokeByInviter` 级联撤销的邀请逐条发 `invitationChanged`（`RevokeByInviter` 现在答被撤的邀请）。
- **测试**：两个控制面实例 + Redis：票跨实例兑换（一次性、过期、设备登出作废、Redis 里不含票明文）；邀请 → 接受 → 改角色 → 移除的事件按受众到达另一个实例上的连接（未验证邮箱的受邀账号、无关账号收不到）；授予后另一个实例上的连接开始收到这台主机的上下线、撤销后不再收到、主机撤销跨实例到达；实例关闭时连接以 1001 关闭；`Hub` 的顺序与「只投一次」、断线重连回调（只杀自己的订阅连接）。

## §8 测试

### 8.1 合同与夹具

- `packages/platform-protocol/fixtures/cloud-api/<procedure>.json` 的入参全部过生成的 schema（S0a 后没有例外）。
- 每个启用的 cloud 过程有一张表驱动测试：在 PG 上跑处理器，答案用 `testutil/schema` 按 `cloud-openapi.json` 校验；错误码必须在该过程契约的错误列表里。
- 核心的 15 + 19 例端口与服务合同在 `AccessPolicy` 抽出后照旧对 personal 跑全（S1a 的验收）；另加 SaaS 策略下的同名服务合同（可见性、断言、链接）。

### 8.2 PostgreSQL 与 Redis 集成

- PG 读 `ARMADRA_CLOUD_TEST_DATABASE_URL`（`pnpm dev:up`），每个测试一个随机 schema，跑全量迁移；没有变量就跳过并说明。
- Redis 读 `REDIS_URL`（AGENTS.md 的约定），每个测试一个随机前缀、只删自己的键；CI 里没有变量即失败。
- 并发：10 个同时接受 `max_uses = 5` 的链接恰好 5 个成功（能力设计 §11.3）；10 个同时 `teams.create` / 主机登记不越过 Free 额度；两个并发事务各降一个 owner 恰好 1 个成功。
- 持久化：撤销后清空 Redis（只删测试前缀）→ 触发重放 → 撤销表与签发索引恢复。

### 8.3 邮件

- mailpit：注册 → 取信 → 解析链接片段 → `auth.emailVerify`；重发后旧链接 `email_token_invalid`；双语模板渲染快照（不含令牌）。

### 8.4 端到端 `pnpm e2e --mode saas`（S9）

拓扑：`armadra-cloud` + 两个 `armadra-relay saas serve`（a、b，各自的隧道地址）+ PG + Redis + mailpit + 两个假 core（复用 `go/internal/testutil/relayenv` 的假 core，报 `e2e` 与 1.4 的 `coreVersion`）。客户端走 Bearer（原生形态）与 Cookie（控制台形态）各一遍。断言一律是「在测试超时内最终成立」（轮询直到满足或超时失败），不写具体毫秒数。

| #   | 场景                                                      | 断言                                                                                   |
| --- | --------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| 1   | A 注册 → 未验证时建团队 `403 email_unverified` → 取信验证 | 门生效                                                                                 |
| 2   | A 建团队 T → 邀请 B（member）→ B 注册、验证、接受         | `memberChanged` 送达 A；`me.spaces` 含 T                                               |
| 3   | A 设备码登录假 core H1 → 选空间 T 登记                    | `ownerAccountId = A`；`sources.list(space=T)` 含 H1                                    |
| 4   | A 授予 T 全员 `editor`                                    | `grants.effective(B, H1)` = editor，来源 team                                          |
| 5   | H1 隧道在节点 a，B 的客户端连节点 b（强制）               | 断言 `team = {T, member}`、`role = editor`；节点间转发通；流量只在 b 计、`team_id = T` |
| 6   | A 撤销授予                                                | B 的通道最终被关；B 的旧中继令牌重连 4401；B 再要断言 `403 source_access_denied`       |
| 7   | A 把 B 移出团队（重新授予后）                             | 同 6；`memberChanged`                                                                  |
| 8   | A 撤销 H1                                                 | 隧道关、`sourceRevoked`、心跳 `410 source_revoked`                                     |
| 9   | 老版本 core H2 登记进 T                                   | `426 protocol_unsupported`；登记到个人空间成功、断言不带 `team`                        |
| 10  | 第 4 台主机登记进 Free 团队                               | `429 limit_reached`，`details.quota = hosts`                                           |
| 11  | 运营登录（口令 + TOTP）→ 停用团队 T                       | 在线连接最终被踢、断言 `403 team_suspended`；审计 `actor.kind = operator`、带 `reason` |
| 12  | 节点 a 排空                                               | H1 收 GOAWAY rebalance 转到 b；`heartbeatHint` 不再含 a                                |
| 13  | 撤销后重启 Redis（清空数据）                              | 控制面重放后，B 的旧中继令牌仍被拒                                                     |
| 14  | 泄密扫描（沿用 personal 的 secret scan）                  | 日志、库转储、Redis 转储里没有任何令牌、口令、邮件令牌明文                             |

### 8.5 安全测试（各批随功能带上）

| 类别          | 用例                                                                                                                                                                                                                             |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CSRF          | Cookie 写请求缺头 / 错值 → 403；`auth.refresh` 不要求；`auth.logout` 要求；运营 Cookie 用普通会话的值 → 403                                                                                                                      |
| 来源          | Origin 缺失、重复、为 `https://x.src.<域>` → 403；跨源请求带 Cookie 拿不到 `Allow-Credentials`                                                                                                                                   |
| Cookie 作用域 | 四枚 Cookie 的 `__Host-` 属性逐项断言；同名 Cookie 重复 → 401 且答复清掉这些 Cookie；登出两枚都清；Cookie + Bearer 同带 → 400；Cookie 设备的令牌放进 `Authorization` → 401                                                       |
| 提权          | `elevatedUntilMs` 过期后失效（用可控时钟）；提权失败计入限流                                                                                                                                                                     |
| 运营隔离      | 普通会话调 `admin.*` → 401；运营会话调 `teams.list` → 401；运营会话过了 1 小时刷新失败（可控时钟）；Bearer 运营会话能调 `admin.*`；非运营账号 `scope: operator` → 403、没 TOTP → `operator_mfa_required`；`mfaVerify` 按账号限流 |
| 团队隔离      | 「跨团队读不到」：对每个带 `teamId` 的过程，用 T2 的成员调 T1 → `404 not_found`；`TeamScope` 只能由 `teams.Require` 构造（编译期 + `repo.rules.json`）；`tenant_id = default` 的行在 SaaS 接口里不可见                           |
| 撤销          | §4.8 每一行：撤销后节点最终拒绝旧令牌；Redis 不可用时作业兜底；Redis 清空后重放                                                                                                                                                  |
| 枚举          | 登录、找回、`invitations.get` 对存在与不存在的邮箱 / 邀请答同样的形状与错误码；未知账号走等时派生（单测断言派生被调用，不比计时）；注册与未注册邮箱连续失败 5 次后第 6 次答案形状一致（`account_locked` 带 `retryAfterMs`）      |
| 泄密          | 每个模板、日志字段、审计行不含令牌（单测断言 + e2e 扫描）；邮件链接里的令牌只在 `#` 之后                                                                                                                                         |

## §9 实施批次

规则：每批一个 worktree（`worktrees/armadra-cloud/g6-s<N>`）、一个 PR（`Refs #60, #59`），每个模块一个提交，CI 绿后由维护者合并；Sonnet 执行，账号、会话、授予与撤销由 Opus 复核（Go 设计 §7）。行数含测试。

### 9.1 批次表

| 批  | 内容                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | 依赖                                   | 验收                                                                                                                | 规模  |
| --- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------- | ------------------------------------------------------------------------------------------------------------------- | ----- |
| S0a | 入参解释器补 `minItems`、`format`（email / uri）、`propertyNames`；`schemagen` 生成；query 数组；`UnsupportedInputs` 清空；`core.Error.Details` 与 `WriteError` 输出 `details`                                                                                                                                                                                                                                                                                                                                                           | —                                      | `pnpm go:check`；全部 `cloud-api` 夹具都有入参 schema；email 正则与协议包同源的一致性测试                           | ~800  |
| S0b | §3.4 的六个迁移 + `migrations.lock` + 迁移测试（§3.5）；`internal/store/saas` 端口骨架（含 `TeamScope`）、`(*postgres.DB).SaaS()`、`Tx` / `AfterCommit` / 错误映射；CHANGELOG 写明个人中转 `--store postgres` 会多出空的 SaaS 表                                                                                                                                                                                                                                                                                                         | —（与 S0a 并行）                       | 迁移测试全过；端口合同 15 例照旧过；**合并前 Opus DDL 冻结审查**；编号合并时取 main 最大号 +1                       | ~1400 |
| S1a | `core.AccessPolicy` 抽出 + `personalPolicy`、源地址函数、`BuildSourceRecord`（§4.2 标 S1a 的三项），personal 行为逐项不变                                                                                                                                                                                                                                                                                                                                                                                                                | S0a                                    | 15 + 19 例合同对 personal 全过；`pnpm e2e --mode personal` 不回归；新增策略接口的合同测试                           | ~900  |
| S1b | `cmd/armadra-cloud`（`serve`、`migrate`、`version`、`keys rotate`、`health`）、配置、租户引导、签名钥包裹、审计分区启动检查；§4.2 标 S1b 的扩展；`httpx`（四枚 Cookie、重复 Cookie、CSRF、Origin、CORS、独立限流器、挑战）；注册、登录、刷新、登出、改口令、提权、`me.get/update/devices/deviceRevoke`、`platform.*`；`relayctl` 内存实现；`TxPorts` 与统一加锁顺序（团队→账号→…），SaaS 免掉核心端口的 `tenants` 行 `FOR UPDATE`，`devices.Insert` 清理失效设备改为按账号（§4.11）                                                      | S0b、S1a                               | §8.5 的 CSRF / 来源 / Cookie 用例；`platform.info.capabilities` 与 §5.3 一致                                        | ~1500 |
| S2  | `jobs`（dedupe / slot、NOTIFY、续租）、`mail`（SMTP、模板、发件箱、`outbox.gc`）、`audit` 写入与安全日志写入；`emailVerify`、`emailVerifyResend`、`passwordResetStart/Finish`、`me.emailChange`、新设备提醒                                                                                                                                                                                                                                                                                                                              | S1b                                    | mailpit 集成；令牌只在片段与内存（扫描库与日志）；作业重放幂等；同一时间槽只跑一次                                  | ~1300 |
| S3  | 设备码五个过程                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | S1b（与 S2 并行）                      | 状态机全分支；轮询过快 429；猜码限流                                                                                | ~600  |
| S4  | `teams`（14 个过程）、团队邀请、`quota`（团队、成员、guest）、`me.spaces`、`me.get.teams`、团队并发额度写入（经 `relayctl`）；`repo-check` 加白名单：只有 `cloud/teams` 能 import `store/saas/teamscope`                                                                                                                                                                                                                                                                                                                                 | S2                                     | 角色矩阵（5 角色 × 每过程）；最后一个 owner 并发；删除 / 恢复 / 清除作业；跨团队读不到                              | ~1500 |
| S5a | `hosts`：登记到空间（含版本门）、目录筛选分页、改名标签设置、撤销；主机配额                                                                                                                                                                                                                                                                                                                                                                                                                                                              | S4                                     | 登记事务与延迟外键；目录筛选与游标；撤销后令牌进撤销表（内存实现断言）                                              | ~1100 |
| S5b | `grants`（五个过程、有效权限）、`sources.assertion` 的合成与版本门、签发索引（含 `cloud_issued_tokens`）、访问清单、撤销流程（§4.8 前三行）；`grants.Policy`                                                                                                                                                                                                                                                                                                                                                                             | S5a；按人踢的验收依赖 **G5 N2**        | 有效权限表驱动（主体 × 角色 × 范围 × 到期）；版本门取在线隧道版本；收窄即撤销                                       | ~1400 |
| S6  | 链接加 `space` 与团队设置、主机邀请（两种空间，落成授予）、`trust.list` / `trust.reject`、`sources.trustRequest` / `trustResolve`（#72）                                                                                                                                                                                                                                                                                                                                                                                                 | S5b、#72（P1d）                        | 链接并发 `max_uses`；匿名链接开关；主机邀请成 guest 授予；信任请求幂等、冷却、24 小时上限、按背书人收窄、不含校验码 | ~1200 |
| S7  | `audit.list`、`me.securityLog`、保留期与分区作业；`teams.usage`、`billing.plan`、`billing.usage`（含按天）、`traffic.retention`；运营：`operators add/totp`、`auth.login{operator}`、`auth.mfaVerify`、`admin.*` M1、事故开关                                                                                                                                                                                                                                                                                                            | S5a；停用即踢的验收依赖 **G5 N2**      | 运营隔离用例；事故开关三项；保留期删除只删本团队                                                                    | ~1500 |
| S8  | `relayctl` 包装 `control/redis.Plane`（广播外壳与命令取 `cluster/wire.go`，新增字段与种类同 G5 一起加）、撤销与签发索引的落盘与重放、`relay:presence` 订阅与 `last_seen`、`cloud:events` 与 me-stream（票、受众、可见集合缓存）、节点表与 rendezvous、`nodes token` / `revoke`、启动与订阅重新就绪时按 `quota.Plans` + overrides 全量重写 `relay:quota:team:*`（§7.2）；**定登录失败计数在 Redis 故障时的回落策略（上多实例前，见 §4.3）**；**把访问清单与收窄快照的逐账号 `AccessBasis` 查询批量化**（S5b 按账号逐个查，M1 规模可接受） | S5b（与 S6、S7 并行）                  | 两个控制面实例 + Redis 的事件互通；撤销最终到达假节点；Redis 清空后重放恢复；外壳与 `wire.go` 的往返测试            | ~1300 |
| S9  | `deploy/saas/compose.yml`（AOF、每节点隧道地址、流量角色）、`armadra-cloud` 镜像（distroless）、`WEB_ROOT` 托管与 CSP、`scripts/e2e.mjs --mode saas`、`deploy/README.md` 与 runbook；TS `apps/cloud` 的去留（§10 Q9）；**登录失败计数的 Redis 故障策略已由 S8 定为失败关闭（§4.3），S9 的 runbook 写明故障期间登录不可用与告警指标 `armadra_cloud_lockout_unavailable_total`**                                                                                                                                                           | S6、S7、S8、**G5**（经 G3b → G4 → G5） | §8.4 全部场景；`pnpm images:build` 冒烟                                                                             | ~1200 |

并行：S0a ∥ S0b；S2 ∥ S3；S6 ∥ S7 ∥ S8。关键路径：S0a → S1a → S1b → S2 → S4 → S5a → S5b → S8 → S9；另一条是中继侧的 G3b → G4 → G5 → S9，两条在 S9 汇合。

### 9.2 G5 依赖（`armadra-relay saas serve` 要提供的）

| #   | 项                                                                                                                                                                                                                                                                                                                                                                       | 用在      |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------- |
| N1  | 按 §2.4 读写 `relay:route`、`relay:presence`、`relay:revoked`（G3a 的 `control/redis` 已实现）                                                                                                                                                                                                                                                                           | S5、S8    |
| N2  | `cluster/wire.go` 的 `commandWire` 加 `subject`、`subjectPrefix`、`deviceId`，新命令 `drain`；节点按过滤字段踢人                                                                                                                                                                                                                                                         | S5b、S7   |
| N3  | `relay:nodes` 心跳（`HPEXPIRE`）与字段 `{url, weight, version, draining, connections, tunnels}`，`url` 是本节点自己的隧道地址（字段写回 Go 设计 §3.6）                                                                                                                                                                                                                   | S7、S8    |
| N4  | `edge.concurrent.team` 跨节点计数（有序集合 `relay:conc:team:<teamId>`，每条连接一个成员、分数即到期毫秒，脚本里清到期、`ZCARD`、没满才 `ZADD`，节点每 20 秒 `ZADD XX` 续期）与 `edge.connect.team` 速率；额度来自 `relay:quota:team:*`（缺键或坏了按最严格的缺省，§7.2）、开关来自 `relay:incident`：订阅就绪时全量读，之后按 `kind: quota` / `kind: incident` 广播更新 | S4、S7    |
| N5  | 流量桶写 `team_id`（中继令牌的 `team`），只在入口节点计；用 `armadra_relay_traffic` 角色写 PG；SaaS 模式**关掉节点自己的保留期清理**（单独的开关，不借 `--traffic-retention-days 0`——那表示不记录）                                                                                                                                                                      | S7        |
| N6  | 节点令牌验签：`typ = armadra-node`、`aud = relay-node`、撤销表（`n.` 前缀）                                                                                                                                                                                                                                                                                              | S8        |
| N7  | `/x/v1` 节点间转发、子域寻址、GOAWAY 再平衡                                                                                                                                                                                                                                                                                                                              | S9（e2e） |

### 9.3 Armadra 侧对应（cloud-console §7）

| 控制台批 | 用到的 G6 批 | 说明                                                                 |
| -------- | ------------ | -------------------------------------------------------------------- |
| U1b      | S1b          | 数据层先接 mock；联调要 Cookie 会话与 `csrfToken`                    |
| U2a      | S1b、S2      | 登录、注册（含暂停注册态）、验证邮箱、找回（令牌从片段取）           |
| U2b      | S3、S4、S6   | `/device`、`/invite/`（令牌从片段取）、`/j/` 的 SaaS 分支            |
| U3       | S1b、S5a、S8 | 个人空间、主机目录（筛选分页、`ownerName`）、设备、安全日志（S7）    |
| U4       | S4           | 团队、成员与邀请、设置、危险操作                                     |
| U5       | S5b          | 团队主机、授予、有效权限                                             |
| U6       | S6、S7       | 链接、设备信任（**只列表与拒绝，不渲染「信任」**）、审计、用量与计划 |
| U7a–c    | S3、S5a、S5b | core 侧设备码、登记到空间、侧栏按团队分组                            |
| U8       | S7           | 运营后台                                                             |

### 9.4 Armadra 要跟着做的事（各开 Issue）

| #   | 事项                                                                                                                                                                       | 关联          |
| --- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- |
| A1  | core 发现新设备 `device_pending` 时调 `sources.trustRequest`（`pendingId` 作幂等键），背书完成或过期调 `sources.trustResolve { result }`                                   | §4.7、#72     |
| A2  | 个人空间访客准入：只对 `remote_services.kind = saas` 的 issuer，断言带 `role` 不带 `team` 也准入，按 `role` / `scp` 同步授予；版本不低于 `MinCoreVersionRoleOnlyAssertion` | §10 Q2、#72   |
| A3  | 经 SaaS 断言建立的 core 会话寿命限在约 1 小时，到期必须带新断言重新 `cloud/login`（协调方开 Issue）                                                                        | §4.8、§10 Q11 |
| A4  | 新设备背书只在桌面与原生 App 里做；控制台 `/t/:team/trust` 只列表与拒绝，行尾引导到 App（改 cloud-console §3.3.9）                                                         | §1.2、§4.7    |
| A5  | 邀请、验证、找回页面从 `#` 片段取令牌，页面 `Referrer-Policy: no-referrer`                                                                                                 | §6.3          |

## §10 已定问题（2026-10-23 设计审查定下）

| #   | 问题                                                                    | 决定                                                                                                                                                                                                                                                                                                                           |
| --- | ----------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Q1  | 待信任请求怎么进云端（契约 §31 原先只有 `trust.list` / `trust.reject`） | 主机上报（契约 §31，#72）：`sources.trustRequest {pendingId, accountId, deviceId, expiresAtMs}`（`pendingId` 作幂等键，`expiresAtMs` 至多 24 小时，设备名与平台由云端从设备表填，被拒后冷却期内答 `status: rejected`，按源限流）与 `sources.trustResolve`（按 `pendingId`，`{result: trusted \| expired}`）；core 侧见 §9.4 A1 |
| Q2  | 个人空间主机的访客：断言有 `role` 没有 `team`                           | core 只对 `remote_services.kind = saas` 的 issuer 准入这类断言；最低版本是单独的协议常量 `MIN_CORE_VERSION_ROLE_ONLY_ASSERTION`（#72）；契约 §22（#72）已改成「主机邀请 = 云端授予 + guest 成员关系，分享链接仍落成 core 邀请」，能力设计 §11.3 同步改                                                                         |
| Q3  | Cookie 名与路径（`__Host-` 必须 `Path=/`）                              | 契约 §1（#72）：普通会话 `__Host-armadra-cloud` + `__Host-armadra-cloud-refresh`，运营会话 `__Host-armadra-ops` + `__Host-armadra-ops-refresh`，四枚都是 `Secure; HttpOnly; SameSite=Strict; Path=/`、不带 `Domain`；同名 Cookie 重复的请求一律拒绝。长期把源地址迁到独立的可注册域                                            |
| Q4  | SaaS 节点的流量桶怎么进 PG                                              | 节点直写，用独立角色 `armadra_relay_traffic`（流量表读写 + `cloud_migrations` 只读）；保留期由控制面作业负责，节点关掉自己的清理（G5 N5）                                                                                                                                                                                      |
| Q5  | 个人空间的「同时连接」额度                                              | M1 不限个人空间（节点照旧按账号 256 防滥用）；M2 给中继令牌加可选声明 `space` 再按空间计                                                                                                                                                                                                                                       |
| Q6  | `limit_reached` 的 `details` 形状                                       | `limitReachedDetailsSchema = { quota, used, limit }`，`quota ∈ QUOTAS`（`teams`、`hosts`、`members`、`guests`、`concurrent`、`traffic`），与用量的 `metric` 同表（契约 §21，#72）                                                                                                                                              |
| Q7  | 哪些过程要求提权                                                        | M1 不要求；M2 起 `teams.delete`、`teams.transferOwnership`、`me.delete` 要求，未提权答 `401 mfa_required`                                                                                                                                                                                                                      |
| Q8  | 关掉团队的 `allowGuests` 时已有 guest 怎么办                            | M1 不动已有 guest（只挡新增）；控制台在设置项旁显示现有 guest 数；M2 再议                                                                                                                                                                                                                                                      |
| Q9  | TS `apps/cloud`（全 501）的去留                                         | S9 把 `armadra-cloud` 镜像换成 Go；TS `apps/cloud` 在下一个版本删除（同 G4.1 的节奏），`control-store-postgres` 的迁移目录保留为唯一的源                                                                                                                                                                                       |
| Q10 | 团队删除的前提（契约写「没有在线主机」，能力设计写「先撤销或转走」）    | 按契约：只要求没有在线主机；主机在恢复期内保留，清除时撤销；控制台在删除确认里列出将被撤销的主机                                                                                                                                                                                                                               |
| Q11 | 授予的到期与收窄怎么在 core 生效                                        | 到期由 `grant.expire` 准时写撤销、撤令牌、踢人；收窄（降级、缩范围）在 core 侧于下次 `cloud/login` 生效，Armadra 把经 SaaS 断言的 core 会话限在约 1 小时（§9.4 A3），所以最迟约 1 小时生效；断言不新增到期声明                                                                                                                 |
| Q12 | 审计分区由谁建                                                          | `serve` 启动时同步建当月与后两个月，失败拒绝启动；每天的作业往后补；兜底分区有行时由 `audit_ensure_partition` 摘下、搬移、挂回                                                                                                                                                                                                 |
| Q13 | 撤销与签发索引只在 Redis 里会随 Redis 丢失                              | 两者都落 PostgreSQL（`relay_revoked_tokens`、`cloud_issued_tokens`），`serve` 启动与 Redis 重连时重放；Redis 另开 AOF（`appendfsync everysec`）                                                                                                                                                                                |
| Q14 | 控制面与节点的 Redis 线上格式                                           | 不手写：`relayctl` 包装 `control/redis.Plane`，外壳与命令取 `control/cluster/wire.go`；跨主机的踢人按签发索引展开成逐源 `kick {sourceId, subject}`；`relay:node:<id>` 只在 G5 之后、且总与广播同发                                                                                                                             |
| Q15 | 新设备背书能否在网页控制台做                                            | 不能：运营方下发的页面可被篡改；背书只在桌面与原生 App，控制台只列表、拒绝并引导到 App                                                                                                                                                                                                                                         |
| Q16 | 节点令牌与最低版本常量放哪里                                            | 节点令牌头 `typ = armadra-node`、`aud = relay-node`、声明 `nodeTokenClaimsSchema`（契约 §30，#72）；`MIN_CORE_VERSION_TEAM_ASSERTION`、`MIN_CORE_VERSION_ROLE_ONLY_ASSERTION` 在 `version.ts` 与 `schema/constants.json` 的 `minCoreVersion`（#72，占位 `0.10.0`）                                                             |
