# SaaS 控制面与节点的运维手册

适用：`deploy/saas/compose.yml` 的单机 M1 部署（`armadra-cloud` + 两个 `armadra-relay saas serve` 节点 + PostgreSQL + Redis），设计见 [SaaS 控制面 M1 实施设计](../design/saas-m1-implementation.md) §2.3、§7。变量表见 [deploy/README.md](../../deploy/README.md)。

## 当前状态

- 控制面是 Go 实现（`go/cmd/armadra-cloud`），镜像由 `go/Dockerfile.cloud` 构建；S8 起生产装配走 Redis 实现的跨节点约定（`relayctl.Redis`），内存实现只在测试里用，所以此前「S8 之前不部署」的限制已经解除。
- TS 的 `apps/cloud` 不再构建镜像，源码留到下一个版本删除（设计 §10 Q9）；它的迁移目录 `packages/control-store-postgres/migrations/` 仍是 PostgreSQL 迁移的唯一来源。

## 首次部署

1. 域名与证书：`app.<域>`、`a.edge.<域>`、`b.edge.<域>`、`*.src.<域>` 四类名字，一张证书覆盖（`*.src` 是二级通配：DNS-01 签发，或经 Cloudflare 代理时用高级证书）。**证书先于登记**：真证书签好之前不要给客户端登记主机，指纹一换所有人都要重新登记。
2. 准备文件：`cp .env.example .env` 填域名、版本、口令；`secrets/master_key`（`openssl rand -hex 32`，**另存一份**，丢了无法解开签名私钥与 TOTP 种子）、`secrets/smtp_password`（没有口令也要建空文件）。
3. 先起控制面：`docker compose up -d cloud`（顺序 postgres → migrate → cloud；`pg-tls` 首次生成 postgres 的自签证书，控制面用 `sslmode=require` 连）。
4. 签节点令牌：`docker compose exec cloud armadra-cloud nodes token --node a > secrets/node_token_a`，`--node b` 同理（缺省 30 天，`--days` 最长 365）。令牌只出现在这条命令的 stdout，jti 与到期时间打印在 stderr，记下 jti（撤销要用）。
5. 起全部：`docker compose up -d`（`db-roles` 建节点的流量角色 `armadra_relay_traffic`，只有 `relay_traffic_buckets` 的 SELECT / INSERT / UPDATE 与 `cloud_migrations` 的 SELECT）；反向代理用 `--profile proxy`（`nginx.conf.template`），或接你自己的。
6. 运营账号：先在控制台注册并验证一个账号，再 `docker compose exec cloud armadra-cloud operators add --email <地址>` 与 `operators totp --email <地址>`（otpauth 地址只打印这一次，丢了用 `--reset`）。
7. 验收：`/ready` 三项（postgres、redis、relay）为 ok；`docker compose exec redis redis-cli hgetall relay:nodes` 有 a、b 两项且 `url` 是各自的 `wss://<节点>.edge.<域>/t/v1`。

开发或端到端用 `docker compose -f compose.yml -f compose.mailpit.yml up -d`，邮件进 mailpit，不外发（不要用在生产）。

## 节点令牌：签发、轮换与撤销

- 节点令牌（头 `typ = armadra-node`，`aud = relay-node`，`sub` = 节点名）只在节点间 `/x/v1` 转发时出示；没有它的节点照常服务本节点的隧道与通道，只是不向别的节点转发，也不接受别的节点转来的通道。
- **轮换**：到期前一周重新签（步骤 4），覆盖 `secrets/node_token_*` 后逐个重启节点（`docker compose up -d relay-a`，GOAWAY rebalance 排空，源重连到别的节点）。
- **撤销**：`docker compose exec cloud armadra-cloud nodes revoke --jti <jti>`，走普通撤销（落 PostgreSQL 再写 Redis，节点最迟 5 秒内断开该令牌的 `/x/v1` 连接）。令牌泄露时先撤销、再签新令牌、再重启对应节点。
- `nodes revoke` 只认 `n.<节点名>.<24 位 hex>` 形状的 jti，不能拿它撤别的令牌。

## 升级、回滚

- **升级顺序**：先升中继节点，再升控制面。节点逐个：`ARMADRA_CLOUD_VERSION=<新版> docker compose up -d relay-a`（停止宽限 40 秒大于排空宽限 30 秒；源收到 GOAWAY rebalance 转到 b），等 `relay:nodes` 里 a 回来后再升 b；然后 `docker compose up -d migrate cloud`（先跑 `migrate`，`serve` 不自动迁移，账本落后会拒绝启动）。
- **回滚**：迁移只加不改，旧版本二进制能读新版本库的前提是没有新迁移被应用：升级前备份（下一节），回滚时换回旧版本号并 `up -d`。已经应用了新迁移的库，旧二进制会因「账本比二进制新」拒绝启动（退出 1），此时从备份恢复，或前滚修复；**不要**手动改账本。
- 镜像回滚不影响数据卷；`pg-tls`、`redis` 数据卷在升级中保留。

## 备份

- **PostgreSQL**：按库备份，`docker compose exec postgres pg_dump -U armadra -Fc armadra_cloud > backup.dump`；恢复到空库后 `armadra-cloud migrate` 核对账本。签名私钥在库里，由 `secrets/master_key` 包裹，**主密钥必须另存**，丢了库里的签名钥与 TOTP 种子作废（所有会话与令牌失效，运营要重新登记 TOTP）。
- **Redis 不备份**：它只放可重建的东西。撤销与签发索引另存 PostgreSQL（`relay_revoked_tokens`、`cloud_issued_tokens`），控制面 `serve` 启动与 Redis 重连时重放回 Redis；Redis 开着 AOF（`appendfsync everysec`）只为减少重启后的空窗。Redis 清空后，节点表 20 秒内由节点写回，撤销由控制面重放（`pnpm e2e --mode saas` 场景 13 演练过）。

## 登录失败计数与 Redis 故障

- 登录失败计数共享在 Redis。Redis 不可用时**失败关闭**：`auth.login` 对已注册与未注册的邮箱一律答 `503 unavailable`（带 `retryAfterMs`），**登录不可用**，不回落到各实例进程内计数（多实例下计数会分叉，可借此枚举账号）。已登录的会话与不经过登录失败计数的接口不受这条规则影响。
- 告警：控制面的指标 `armadra_cloud_lockout_unavailable_total`（计数器，每次因 Redis 不可用而拒绝登录加一）。配 `ARMADRA_CLOUD_METRICS_LISTEN`（与 `ARMADRA_CLOUD_METRICS_TOKEN_FILE`）后在该监听的 `/metrics` 读到。建议规则：`increase(armadra_cloud_lockout_unavailable_total[5m]) > 0` 就告警（页面或值班），同时监控 `/ready` 的 `redis` 项。处置：恢复 Redis（`docker compose up -d redis`，AOF 会自己回放）；恢复后登录自动恢复，不需要重启控制面。

## 排空、下线与事故开关

- 单个节点下线：控制台运营后台（`admin.relayNodeDrain`）或 `SIGTERM`；节点表标 `draining`、`/ready` 不就绪、隧道收 GOAWAY `rebalance`，源按 `heartbeatHint`（已去掉排空节点）重连到别的节点，经该节点转发的通道以 4404 结束，客户端重连。
- 事故开关（`admin.incidentSet`）：`pauseAssertions`、`pauseRegistrations`、`rejectNewConnections`（节点拒绝新的隧道与端到端升级，在线连接保留）。
- 停用团队（`admin.teamStatus`）：在线连接被踢，断言答 `403 team_suspended`，审计写 `actor.kind = operator` 与理由。

## 端到端验收

`pnpm e2e --mode saas`：真实控制面 + 两个节点 + 一次性 PostgreSQL / Redis / mailpit 容器，覆盖设计 §8.4 的 14 个场景（注册与验证邮件、团队与邀请、设备码登记、授予与断言、经节点转发、撤销、老版本 core、配额、运营停用、排空、Redis 清空后重放、泄密扫描）。`pnpm e2e --mode saas-nodes` 是只有节点的用例（测试签发者代替控制面：10 MB 双向转发、慢客户端、节点下线）。
