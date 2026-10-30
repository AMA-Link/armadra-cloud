# SaaS 控制面与节点的运维手册

适用：`deploy/saas/compose.yml` 的单机 M1 部署（`armadra-cloud` + 两个 `armadra-relay saas serve` 节点 + PostgreSQL + Redis），设计见 [SaaS 控制面 M1 实施设计](../design/saas-m1-implementation.md) §2.3、§7。变量表见 [deploy/README.md](../../deploy/README.md)。

## 当前状态

- 控制面是 Go 实现（`go/cmd/armadra-cloud`），镜像由 `go/Dockerfile.cloud` 构建；S8 起生产装配走 Redis 实现的跨节点约定（`relayctl.Redis`），内存实现只在测试里用，所以此前「S8 之前不部署」的限制已经解除。
- TS 的 `apps/cloud` 不再构建镜像，源码留到下一个版本删除（设计 §10 Q9）；它的迁移目录 `packages/control-store-postgres/migrations/` 仍是 PostgreSQL 迁移的唯一来源。

## 首次部署

1. 域名与证书：`app.<域>`、`a.edge.<域>`、`b.edge.<域>`、`*.src.<域>` 四类名字，一张证书覆盖（`*.src` 是二级通配：DNS-01 签发，或经 Cloudflare 代理时用高级证书；放在 Cloudflare 后面时按 `nginx.conf.template` 头部的注释取真实 IP）。**证书先于登记**：真证书签好之前不要给客户端登记主机，指纹一换所有人都要重新登记。
2. 准备文件：`cp .env.example .env` 填域名与版本；`sh init-secrets.sh` 生成 `secrets/` 下的口令与连接串（PostgreSQL 超级用户口令、库属主角色 `armadra_cloud` 与流量角色 `armadra_relay_traffic` 的口令、Redis 口令、主密钥、对应的连接串文件）；`secrets/smtp_password` 写发信口令（没有就留空）。**`secrets/master_key` 必须另存一份**，丢了无法解开签名私钥与 TOTP 种子。
   - 权限：`secrets/` 目录 0700、文件 0444。容器里的 uid 10002（控制面与节点）、70（postgres）、999（redis）都要能读它们，所以文件对其他人可读，靠目录 0700 挡住宿主上的其他用户；节点令牌文件是 0644（方便 `nodes token` 的输出直接覆盖）。想更严就把文件 `chown` 给对应的 uid 再 0400（控制面、节点共用 10002，postgres 与 redis 各用自己的口令文件）。不要把 `secrets/` 与 `tls/` 提交进版本库：`init-secrets.sh` 在这两个目录各写一个内容为 `*` 的 `.gitignore`（`tls/` 由它一并建好），本仓的 `.gitignore` 也忽略了 `deploy/saas/secrets/` 与 `deploy/saas/tls/`；部署目录在别处的版本库里时，确认 `git status` 里看不到它们。
   - 超级用户只在 postgres 首次初始化时用（`postgres-init.sh` 建库属主角色、流量角色与库）；`migrate`、`serve`、`db-roles` 都以非超级用户 `armadra_cloud` 连。Redis 要口令（`requirepass`），连接串在 `secrets/redis_url`。
   - 托管 PostgreSQL / Redis：直接把 `cloud_database_url`、`relay_database_url`、`redis_url` 写成托管服务的连接串（`sslmode=verify-full`），并去掉 compose 里的 `postgres`、`redis`、`pg-tls` 服务与依赖。
3. 控制台页面从哪来：镜像里的 `/app/web` 是构建参数 `WEB_IMAGE` 拷进去的 Armadra 服务器镜像里的 web 产物（发布构建取 `deploy/compat.json` 的 `web` 版本，0.10.0 起带控制台外壳）；没有页面（目录里没有 `index.html`）时控制面照常服务 API，`/` 答 404。要换页面（例如先验收新版本的页面）就把目录挂到 `/app/web`（`compose.yml` 里 `cloud` 的 `volumes` 注释打开，`WEB_DIR` 指向构建产物目录）。
4. 先起控制面：`docker compose up -d cloud`（顺序 postgres → migrate → cloud；`pg-tls` 首次生成 postgres 的自签证书，控制面用 `sslmode=require` 连）。
5. 签节点令牌：`docker compose exec cloud armadra-cloud nodes token --node a > secrets/node_token_a`，`--node b` 同理（缺省 30 天，`--days` 最长 365）。令牌只出现在这条命令的 stdout，jti 与到期时间打印在 stderr，记下 jti（撤销要用）。
6. 起全部：`docker compose up -d`（`db-roles` 给节点的流量角色授权：只有 `relay_traffic_buckets` 的 SELECT / INSERT / UPDATE 与 `cloud_migrations` 的 SELECT）；反向代理用 `--profile proxy`（`nginx.conf.template`，TLS 证书放 `tls/fullchain.pem` 与 `tls/privkey.pem`），或接你自己的——自己的代理要把 `TRUSTED_PROXY` 设成它的地址。参考代理缺省绑全部网卡的 80 / 443，只给本机用或换地址、端口时在 `.env` 设 `PROXY_BIND`（如 `127.0.0.1`）、`PROXY_HTTP_PORT`、`PROXY_HTTPS_PORT`。
7. 运营账号：先在控制台注册并验证一个账号，再 `docker compose exec cloud armadra-cloud operators add --email <地址>` 与 `operators totp --email <地址>`（otpauth 地址只打印这一次，丢了用 `--reset`）。
   - 临时（AMA-Link/Armadra#580 的修复发布后删除这一条）：当前发布的页面在 HTTPS 的控制台地址上进不了控制台（空白页），注册这一步做不通。替代办法是自己构建 Armadra 的页面，构建时设 `VITE_RUNTIME_URL`（任意一个不会被访问的地址，如 `VITE_RUNTIME_URL=http://127.0.0.1:9 pnpm --filter @armadra/web build`），按第 3 步把产物目录挂到 `/app/web`。
   - 临时（AMA-Link/Armadra#584 的修复发布后删除这一条）：运营登录目前要先有普通会话，先用这个账号在控制台正常登录，再进运营登录输入 TOTP。
8. 验收：`/ready` 三项（postgres、redis、relay）为 ok；`docker compose exec redis sh -c 'REDISCLI_AUTH="$(cat /run/secrets/redis_password)" redis-cli hgetall relay:nodes'` 有 a、b 两项且 `url` 是各自的 `wss://<节点>.edge.<域>/t/v1`。`/ready` 的 `relay` 项只表示当前不在从库往 Redis 重放（撤销、签发索引、团队额度），不代表节点在线，也不代表重放成功过；一个节点都没有时也是 ok，节点是否在线以节点表为准。节点间转发还要满足下面「节点间转发」的前提。

网络：`edge`（代理、控制面、节点；代理 IP 固定为 `PROXY_IP`，控制面与节点只信它转发的 `X-Forwarded-For`）与 `backend`（postgres、redis、migrate、db-roles；internal，不出网）。代理够不到数据库与 Redis。

开发或端到端用 `docker compose -f compose.yml -f compose.mailpit.yml up -d`，邮件进 mailpit，不外发（不要用在生产）。

## 节点令牌：签发、轮换与撤销

- 节点令牌（头 `typ = armadra-node`，`aud = relay-node`，`sub` = 节点名）只在节点间 `/x/v1` 转发时出示；没有它的节点照常服务本节点的隧道与通道，只是不向别的节点转发，也不接受别的节点转来的通道。
- **轮换**：到期前一周重新签（步骤 4），覆盖 `secrets/node_token_*` 后逐个重启节点（`docker compose up -d relay-a`，GOAWAY rebalance 排空，源重连到别的节点）。
- **撤销**：`docker compose exec cloud armadra-cloud nodes revoke --jti <jti>`，走普通撤销（落 PostgreSQL 再写 Redis，节点最迟 5 秒内断开该令牌的 `/x/v1` 连接）。令牌泄露时先撤销、再签新令牌、再重启对应节点。
- `nodes revoke` 只认 `n.<节点名>.<24 位 hex>` 形状的 jti，不能拿它撤别的令牌。

## 升级、回滚

- **升级顺序**（`serve` 不自动迁移，账本比二进制新或旧都会拒绝启动，所以**升级期间不要重启旧版本的控制面**——它会因账本比自己新而起不来）：
  1. 备份（下一节）。
  2. 改 `.env` 的 `ARMADRA_CLOUD_VERSION`，拉新镜像：`docker compose pull`。
  3. 跑迁移：`docker compose run --rm migrate`（用新镜像；迁移只加不改，旧版本的节点与控制面在迁移期间照常服务）。
  4. 逐个节点：`docker compose up -d --no-deps relay-a`（`--no-deps` 不连带重建别的服务；停止宽限 40 秒大于排空宽限 30 秒，源收到 GOAWAY rebalance 转到 b），等 `relay:nodes` 里 a 回来、`/ready` 为 ok 后，再同样升 b。
  5. 最后控制面：`docker compose up -d --no-deps cloud`。
- **回滚**：迁移只加不改，但已应用的新迁移不能撤销：旧二进制遇到「账本比二进制新」退出 1。所以回滚要看有没有新迁移：没有就换回旧版本号，按「节点 → 控制面」同样用 `up -d --no-deps` 重建；有新迁移就从升级前的备份恢复（见下一节），**不要**手动改账本。
- 镜像回滚不影响数据卷；`pg-tls`、`redis` 数据卷在升级中保留。

## 备份与恢复

- **备份 PostgreSQL**：`docker compose exec -T postgres pg_dump -U postgres -d armadra_cloud -Fc --no-owner --exclude-table-data=outbox_emails > backup.dump`。`--exclude-table-data=outbox_emails` 保留发件箱的表结构、只不备份它的数据（含收件地址等个人信息，只是发信中转，发出或失败满 30 天由作业清理）。签名私钥在库里，由 `secrets/master_key` 包裹，**主密钥必须另存**，丢了库里的签名钥与 TOTP 种子作废（所有会话与令牌失效，运营要重新登记 TOTP）。
- **恢复**（演练过一遍）：停控制面与节点（`docker compose stop cloud relay-a relay-b`）→ 重建空库：`docker compose exec -T postgres psql -U postgres -c 'DROP DATABASE armadra_cloud WITH (FORCE)' -c 'CREATE DATABASE armadra_cloud OWNER armadra_cloud'`（不要用 `pg_restore --clean`：它按依赖顺序删对象，会因分区表与外键报一串错）→ `docker compose exec -T postgres pg_restore -U postgres -d armadra_cloud --no-owner --role=armadra_cloud < backup.dump`（`--role` 让恢复出来的对象归库属主 `armadra_cloud`，不归超级用户）→ **重跑 `docker compose run --rm db-roles`**（新库没有对流量角色的授权）→ `docker compose run --rm migrate` 核对账本（备份比二进制旧会补迁移，比二进制新会拒绝，换匹配的版本）→ `docker compose up -d --no-deps relay-a relay-b cloud`。恢复后 Redis 里的撤销与签发索引由控制面 `serve` 启动时从库重放。
- **Redis 不备份**：它只放可重建的东西。撤销与签发索引另存 PostgreSQL（`relay_revoked_tokens`、`cloud_issued_tokens`），控制面 `serve` 启动与 Redis 重连时重放回 Redis；Redis 开着 AOF（`appendfsync everysec`）只为减少重启后的空窗。Redis 清空后，节点表 20 秒内由节点写回，撤销由控制面重放（`pnpm e2e --mode saas` 场景 13 演练过）。

## 登录失败计数与 Redis 故障

- 登录失败计数共享在 Redis。Redis 不可用时**失败关闭**：`auth.login` 对已注册与未注册的邮箱一律答 `503 unavailable`（带 `retryAfterMs`），**登录不可用**，不回落到各实例进程内计数（多实例下计数会分叉，可借此枚举账号）。已登录的会话与不经过登录失败计数的接口不受这条规则影响。
- 告警：控制面的指标 `armadra_cloud_lockout_unavailable_total`（计数器，每次因 Redis 不可用而拒绝登录加一）。配 `ARMADRA_CLOUD_METRICS_LISTEN`（与 `ARMADRA_CLOUD_METRICS_TOKEN_FILE`）后在该监听的 `/metrics` 读到。建议规则：`increase(armadra_cloud_lockout_unavailable_total[5m]) > 0` 就告警（页面或值班，即下文「告警」里的 `ArmadraCloudLoginFailClosed`），同时监控 `/ready` 的 `redis` 项。处置：恢复 Redis（`docker compose up -d redis`，AOF 会自己回放）；恢复后登录自动恢复，不需要重启控制面。

## 节点间转发

- **前提（绕回代理）**：节点表里每个节点的 `/x/v1` 地址缺省是 `wss://<节点>.edge.<域>/x/v1`（由 `RELAY_TUNNEL_URL` 换路径得到），别的节点按这个地址经反向代理连回来。所以每个节点容器都要：能解析 `<节点>.edge.<域>`（解析到代理的地址，不是容器自己）、能连到代理的 443（宿主或网络要允许这种绕回连接）、信任代理的证书（节点镜像只带系统根证书；自建 CA 时挂一个含该 CA 的证书包进节点并设 `SSL_CERT_FILE` 指向它；它只替换默认的证书包文件，`/etc/ssl/certs` 目录仍会读取，镜像自带的系统根照样生效）。不满足时源连 a、客户端落到 b 的连接无法转发，大约一半的客户端连接答 4404 或 503。`*.localhost` 这类名字在节点里一律解析到回环，连到的是容器自己，不能用作节点间地址。
- **排查**：节点日志有「连不上对端节点，通道不转发」就是这一步失败；这一行的 `reason` 是失败分类（`dns` 名字解析失败、`refused` 被拒绝连接、`timeout` 超时、`tls` 证书或 TLS 握手失败、`rejected` 对端答了 HTTP 状态而不是升级（`code` 是状态码，401 是节点令牌被拒）、`reset`、`unreachable`、`node_unknown` 对端不在节点表、`insecure_url` 地址不是 wss、`node_lookup` 读节点表出错、`network` 其余），`host` 是节点表里对端地址的主机与端口；在一个节点的网络里试连另一个节点（节点镜像没有 shell，用临时容器：`docker run --rm --network armadra-saas_edge curlimages/curl -sv -H 'Connection: Upgrade' -H 'Upgrade: websocket' https://b.edge.<域>/x/v1`，自建 CA 时挂进 CA 并加 `--cacert`）：报解析失败是名字没解析到代理，报证书错误是不信任代理的证书，连到了却超时或被拒是代理不接绕回连接；答 401（没带节点令牌）就说明链路通。再看 `relay:nodes` 里各节点的地址是否正确、节点令牌是否已签（`secrets/node_token_*` 非空）且未撤销。
- **走内部网络**：`RELAY_INTERNODE_URL` 可把 `/x/v1` 地址改成 edge 网络里的直连地址（如 `ws://relay-a:8443/x/v1`），不再绕回代理，但目前节点只监听明文，直连只能是 `ws://`。每个节点各设自己的地址（`relay-a` 设 `ws://relay-a:8443/x/v1`，`relay-b` 设 `ws://relay-b:8443/x/v1`），并且所有节点都要设 `RELAY_ALLOW_INSECURE_TUNNEL=1`（连出的一端靠它接受对端的 `ws://` 地址，只改一个节点会报「对端的 /x/v1 地址不是 wss」）；这个开关只用于开发与本机测试，节点令牌会以明文在网络里传，生产不要用。
- 参考 nginx 的 `relay_any`（客户端入口）不排除排空中的节点：排空节点仍会接客户端连接，再经 `/x/v1` 转发到源所在的节点，功能正常。
- 每个账号经节点间连接（`/x/v1`）转发的通道数受 `RELAY_FORWARDED_PER_ACCOUNT`（缺省 64）限制，计数在每个入口节点上、按账号各算各的，不是全集群合计，超出的新通道答 4429 `limit_reached`；链路上的 4096 个流位各租户共用，没有这个上限一个账号就能占满。本地通道不受它限制。
- 指标：`relay_forward_account_refused_total`（计数器，因账号转发名额满而拒绝的通道数）与 `relay_internode_unwritten_bytes`（仪表，入口节点出向连接上还没写出的转发字节：挂着等信用的加已排队没写进内核的）。建议规则：`relay_internode_unwritten_bytes > 64 * 1024 * 1024` 持续 5 分钟就告警（对端或链路慢，或某个租户在占住链路）；`increase(relay_forward_account_refused_total[10m]) > 0` 作提示，反复出现时看是哪个账号、是否该调高上限（下文「告警」里的 `ArmadraRelayInternodeBacklog` 与 `ArmadraRelayForwardAccountCapHit`）。
- 节点令牌撤销（`armadra-cloud nodes revoke`）经 `relay:revoked` 广播即时断开已建立的节点间连接；5 秒一次的复查留作兜底。

## 排空、下线与事故开关

- 单个节点下线：控制台运营后台（`admin.relayNodeDrain`）或 `SIGTERM`；节点表标 `draining`、`/ready` 不就绪、隧道收 GOAWAY `rebalance`，源按 `heartbeatHint`（已去掉排空节点）重连到别的节点，经该节点转发的通道以 4404 结束，客户端重连。
- 恢复排空的节点：目前没有「取消排空」，排空后节点一直是 `draining`，要重建这个节点的容器：`docker compose up -d --force-recreate --no-deps relay-a`（只重建 a，不连带别的服务；隧道已经排空，重建很快）；节点表里 a 回到 `draining: false`、`/ready` 为 ok 后，新的隧道与客户端连接会重新分到它上面（已在别的节点上的源不会自动迁回）。用 `SIGTERM` 停掉的节点同样用这条命令起回来。
- 事故开关（`admin.incidentSet`）：`pauseAssertions`、`pauseRegistrations`、`rejectNewConnections`（节点拒绝新的隧道与端到端升级，在线连接保留）。
- 停用团队（`admin.teamStatus`）：在线连接被踢，断言答 `403 team_suspended`，审计写 `actor.kind = operator` 与理由。

## 告警

规则文件 [`deploy/saas/prometheus/armadra-saas.rules.yml`](../../deploy/saas/prometheus/armadra-saas.rules.yml)，接运维自己的 Prometheus（本仓不带 Prometheus 与 Alertmanager）。

1. 打开指标：`docker compose -f compose.yml -f compose.metrics.yml up -d`。叠加文件让控制面与两个节点在容器里监听 `:9100`，只发布到宿主的 `127.0.0.1:9101`（控制面）、`9102`（a）、`9103`（b）（端口见 `.env.example`），要 `Authorization: Bearer <secrets/metrics_token>`（`init-secrets.sh` 生成，已有部署补跑一次即可，不覆盖别的文件）。指标端口不经反向代理。之后的升级命令都要带上这两个 `-f`，否则会把指标监听关掉。
2. 抓取（宿主上的 Prometheus；任务名要与规则里的 `up{job=…}` 一致，`instance` 写节点名，告警里看得懂）：

   ```yaml
   rule_files:
     - /etc/prometheus/armadra-saas.rules.yml
   scrape_configs:
     - job_name: armadra-cloud
       authorization:
         { credentials_file: /etc/prometheus/armadra_metrics_token }
       static_configs:
         - targets: ["127.0.0.1:9101"]
           labels: { instance: cloud }
     - job_name: armadra-relay
       authorization:
         { credentials_file: /etc/prometheus/armadra_metrics_token }
       static_configs:
         - targets: ["127.0.0.1:9102"]
           labels: { instance: a }
         - targets: ["127.0.0.1:9103"]
           labels: { instance: b }
   ```

3. 改了规则先校验：`docker run --rm -v "$PWD/prometheus:/r:ro" --entrypoint promtool prom/prometheus:v3.7.3 check rules /r/armadra-saas.rules.yml`，再让 Prometheus 重新加载。

| 告警                                         | 指标与条件                                                                                                     | 级别               | 处置                                                     |
| -------------------------------------------- | -------------------------------------------------------------------------------------------------------------- | ------------------ | -------------------------------------------------------- |
| `ArmadraCloudDown` / `ArmadraRelayNodeDown`  | `up == 0` 持续 2 分钟                                                                                          | critical           | 容器状态、`/ready`、`relay:nodes`                        |
| `ArmadraRelayTunnelsDropped`                 | `sum(relay_tunnels)` 比 30 分钟前少一半以上（当时至少 10 条），持续 5 分钟                                     | warning            | 握手失败、JWKS、Redis、是否有节点在排空                  |
| `ArmadraRelayHandshakeFailures`              | `relay_handshake_total{result!="ok"}` 占比 > 50%（10 分钟至少 20 次），持续 10 分钟                            | warning            | 按 `result` 拆开看；日志「隧道握手失败」                 |
| `ArmadraRelayHeartbeatMisses`                | 10 分钟的 `relay_heartbeat_miss_total` 增量 > 在线隧道的 20%（至少 5 条），持续 5 分钟                         | warning            | 宿主网络与负载，对照调度延迟                             |
| `ArmadraRelayForwardFailureRate`             | `relay_routes_remote_failed_total` /（它 + `relay_routes_remote_total`）> 5%（10 分钟至少 5 次），持续 10 分钟 | critical           | 上面「节点间转发」的排查                                 |
| `ArmadraRelayInternodeBacklog`               | `relay_internode_unwritten_bytes` > 64 MiB 持续 5 分钟                                                         | warning            | 对端负载与链路                                           |
| `ArmadraRelayForwardAccountCapHit`           | `increase(relay_forward_account_refused_total[10m]) > 0`                                                       | info               | `RELAY_FORWARDED_PER_ACCOUNT`                            |
| `ArmadraRelayTeamConcurrencyFailOpen`        | `increase(relay_team_concurrency_fail_open_total[5m]) > 0`                                                     | warning            | 节点到 Redis 的连接                                      |
| `ArmadraRelaySchedLatencyHigh` / `…Critical` | `relay_sched_latency_seconds` 的 p99 > 50 ms 持续 10 分钟 / > 250 ms 持续 5 分钟                               | warning / critical | 宿主 CPU 与容器限额；严重时先排空这个节点                |
| `ArmadraCloudLoginFailClosed`                | `increase(armadra_cloud_lockout_unavailable_total[5m]) > 0`                                                    | critical           | 上面「登录失败计数与 Redis 故障」                        |
| `ArmadraCloudMailFailing`                    | 15 分钟内 `armadra_cloud_mail_send_failures_total` 增量 ≥ 3                                                    | warning            | 日志「邮件没发出」的错误类名；SMTP 主机、端口、口令、TLS |
| `ArmadraCloudMailAbandoned`                  | 1 小时内 `armadra_cloud_mail_abandoned_total` 有增量                                                           | critical           | 修好 SMTP 后让用户重新触发（重发验证、重新邀请）         |
| `ArmadraCloudMailNotConfigured`              | 1 小时内 `armadra_cloud_mail_backlog_dropped_total` 有增量                                                     | warning            | 配 SMTP 后 `up -d --no-deps cloud`                       |

- 指标的含义：`relay_routes_remote_failed_total` 是路由在别的节点、却拿不到到那个节点的连接（连不上 `/x/v1` 或没配节点令牌）而答 4404 的通道；`relay_sched_latency_seconds` 是 Go 运行时的调度延迟（goroutine 就绪到开始运行，`runtime/metrics` 的 `/sched/latencies:seconds`，自进程启动累计，各桶是偏小的下界估计，没有 `_sum`）；发信的四个计数器（`armadra_cloud_mail_sent_total`、`_send_failures_total`（每次尝试计一次，作业按退避重试，最多 8 次）、`_abandoned_total`、`_backlog_dropped_total`）只有次数，不带地址与正文，每个控制面实例各计各的。
- 阈值是 0.12 内测的起点，按实际流量调整后在这里与规则文件里同步改。

## 日志保留

- **容器日志**：`compose.yml` 的每个服务都用 json-file 驱动并轮转：单个文件到 `LOG_MAX_SIZE`（缺省 `20m`）换下一个，最多留 `LOG_MAX_FILE`（缺省 `5`）个，所以每个服务的日志在宿主上最多约 100 MB，按流量折算的保留时长随负载变化；改这两个值后 `docker compose up -d`（重建容器才生效，旧容器的日志随容器删掉）。`docker compose down` 或重建容器会连同它的日志一起删掉，要更长的保留就把日志转到外部的日志系统（本仓不带）。
- **日志里有什么**：控制面与节点的日志只有白名单字段（`internal/obs`：各种 id、结果、计数、错误类名），不记流正文、终端输出、画布内容、口令与令牌明文；参考代理（nginx）的访问日志有客户端 IP、方法、路径（`$uri`，不含 query 串）与状态码，是这里唯一带客户端 IP 的日志。导出或转存日志时按个人信息处理。
- **库里的数据保留**（控制面的定时作业负责，节点不清理）：

| 数据                                  | 保留                                                                                                                     | 作业                                  |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------- |
| 团队审计                              | 按团队此刻的计划：free 7 天、pro 30 天、team 90 天、enterprise 365 天（未知计划按 free）；已清除团队的行只随整月分区删除 | `audit.retention`，每天               |
| 账号安全日志                          | 固定 90 天                                                                                                               | `audit.retention`，每天               |
| 审计分区                              | 整月早于 13 个月前的分区整个删除（运营动作只随它删除）；每天补建当月与后两个月                                           | `audit.retention`、`audit.partitions` |
| 流量统计（`relay_traffic_buckets`）   | `ARMADRA_CLOUD_TRAFFIC_RETENTION_DAYS`，缺省 400 天                                                                      | `traffic.retention`，每天             |
| 发件箱（`outbox_emails`，含收件地址） | 发出或判失败满 30 天；备份时不导出它的数据（见「备份与恢复」）                                                           | `outbox.gc`，每天                     |
| 邮件令牌（`email_tokens`，只有哈希）  | 过期满 24 小时                                                                                                           | `emailTokens.gc`，每小时              |
| 作业（`jobs`）                        | 完成满 7 天、失败满 30 天                                                                                                | `jobs.gc`，每天                       |

- Redis 里只放可重建的状态（见「备份与恢复」），不另做保留。

## 端到端验收

`pnpm e2e --mode saas`：真实控制面 + 两个节点 + 一次性 PostgreSQL / Redis / mailpit 容器，覆盖设计 §8.4 的 14 个场景（注册与验证邮件、团队与邀请、设备码登记、授予与断言、经节点转发、撤销、老版本 core、配额、运营停用、排空、Redis 清空后重放、泄密扫描）。`pnpm e2e --mode saas-nodes` 是只有节点的用例（测试签发者代替控制面：10 MB 双向转发、慢客户端、节点下线）。
