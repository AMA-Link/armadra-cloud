# 部署

| 文件                                         | 用途                                                                                                                                          |
| -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| [saas/compose.yml](saas/compose.yml)         | SaaS 单机 M1：控制面 + 两个中继节点 + PostgreSQL 17 + Redis 8（AOF）；`--profile proxy` 带参考反向代理，`compose.mailpit.yml` 给开发 / 端到端 |
| [compose.yml](compose.yml)                   | 个人中转的多节点参考：`personal-cluster` profile（两节点，PostgreSQL 存储与控制面）                                                           |
| [personal/compose.yml](personal/compose.yml) | 单人中转，一个服务、不带数据库                                                                                                                |
| [dev/compose.yml](dev/compose.yml)           | 本地开发依赖（`pnpm dev:up`）                                                                                                                 |
| [e2e/compose.yml](e2e/compose.yml)           | 跨仓端到端（`pnpm e2e --mode personal`，脚本驱动）                                                                                            |
| [compat.json](compat.json)                   | 验证过的 Armadra 服务器镜像区间与协议版本                                                                                                     |
| [cloudflare/README.md](cloudflare/README.md) | Cloudflare Workers 上的个人中转（wrangler 项目在 `apps/relay-workers`）                                                                       |

## 环境变量

控制面（`armadra-cloud serve`，Go；镜像由 `go/Dockerfile.cloud` 构建；命令 `serve`、`migrate`、`keys rotate`、`nodes token|revoke`、`operators add|totp`、`health`、`version`；完整说明见 [SaaS 实施设计](../docs/design/saas-m1-implementation.md) §2）。秘密用 `*_FILE` 或环境变量给，不进日志：

| 变量                                                                                       | 必填 | 说明                                                                                                 |
| ------------------------------------------------------------------------------------------ | ---- | ---------------------------------------------------------------------------------------------------- |
| `ARMADRA_CLOUD_ISSUER`                                                                     | 是   | 断言与令牌的 `iss`，`https` 来源（回环地址可用 `http`）                                              |
| `ARMADRA_CLOUD_APP_ORIGIN`                                                                 | 是   | 控制台来源（Cookie 与 CSP）；M1 必须等于 `ISSUER`                                                    |
| `ARMADRA_CLOUD_LISTEN`                                                                     |      | 缺省 `0.0.0.0:8100`，明文，TLS 由前面的反向代理终止                                                  |
| `ARMADRA_CLOUD_DATABASE_URL`                                                               | 是   | `postgres://…`；非回环地址必须 `sslmode=verify-full` 或 `require`                                    |
| `ARMADRA_CLOUD_REDIS_URL`                                                                  | 是   | 与中继节点共用的那一个 Redis，要求 ≥ 7.4                                                             |
| `ARMADRA_CLOUD_MASTER_KEY` / `_FILE`                                                       | 是   | 32 字节 hex；包裹签名私钥与 TOTP 种子（**另存一份**）                                                |
| `ARMADRA_CLOUD_RELAY_DOMAIN`                                                               | 是   | `<sourceId>.src.<域>` 的 `<域>`，与节点的 `RELAY_DOMAIN` 一致                                        |
| `ARMADRA_CLOUD_WEB_ROOT`                                                                   |      | 控制台页面（Armadra `apps/web` 的构建产物）；镜像里缺省 `/app/web`；目录里没有 `index.html` 就不托管 |
| `ARMADRA_CLOUD_SMTP_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD_FILE` / `_FROM` / `_TLS`    |      | 发信；`_TLS` 取 `starttls`（缺省）/ `tls` / `none`，`none` 只许回环（mailpit）                       |
| `ARMADRA_CLOUD_REQUIRE_EMAIL_VERIFIED`                                                     |      | 缺省 `1`；只在开发环境设 `0`                                                                         |
| `ARMADRA_CLOUD_TRUSTED_PROXY` / `_TRUSTED_PROXY_PREFER`                                    |      | 受信反向代理的 CIDR 与先读的头，规则同中继                                                           |
| `ARMADRA_CLOUD_TURNSTILE_SITE_KEY` / `_TURNSTILE_SECRET_FILE` / `_CHALLENGE_SCOPE`         |      | 人机挑战；两个都配才要求，只配一个是配错                                                             |
| `ARMADRA_CLOUD_METRICS_LISTEN` / `_METRICS_TOKEN_FILE`                                     |      | 指标监听（含 `armadra_cloud_lockout_unavailable_total`）                                             |
| `ARMADRA_CLOUD_NAME`、`_JOB_WORKERS`、`_TRAFFIC_RETENTION_DAYS`、`_HIBP_URL`、`_LOG_LEVEL` |      | 见设计 §2.2                                                                                          |

中继（`armadra-relay saas serve`，SaaS 中继节点，Go 设计 §3.6；TLS 在前面的反向代理终止，明文监听）：

| 变量                                                      | 必填 | 说明                                                                                                                                                          |
| --------------------------------------------------------- | ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RELAY_LISTEN`                                            |      | 缺省 `0.0.0.0:8443`                                                                                                                                           |
| `RELAY_NODE_ID`                                           |      | 节点名（节点表与命令频道 `relay:node:<id>` 的 id），`^[a-z0-9][a-z0-9-]{0,62}$`；缺省由主机名换出（转小写、其余字符换成 `-`）；与节点令牌的 `sub` 一致        |
| `RELAY_DOMAIN`                                            | 是   | `<sourceId>.src.<域>` 的 `<域>`                                                                                                                               |
| `RELAY_CLOUD_ISSUER`                                      | 是   | 控制面 issuer（令牌的 `iss`）                                                                                                                                 |
| `RELAY_JWKS_URL`                                          |      | 缺省 `<issuer>/.well-known/jwks.json`；各节点共用 Redis 里的副本，同一时刻只有一个节点回源                                                                    |
| `RELAY_REDIS_URL` / `RELAY_REDIS_URL_FILE`                | 是   | 与控制面共用的那一个 Redis（≥ 7.4，节点表用 `HPEXPIRE`）；可带口令                                                                                            |
| `RELAY_TUNNEL_URL`                                        | 是   | 本节点自己的隧道地址 `wss://<node>.edge.<域>/t/v1`，写进节点表，源按 `heartbeatHint` 连它                                                                     |
| `RELAY_INTERNODE_URL`                                     |      | 别的节点连本节点 `/x/v1` 的地址，缺省由 `RELAY_TUNNEL_URL` 把路径换成 `/x/v1`                                                                                 |
| `RELAY_NODE_WEIGHT`                                       |      | 节点表里的权重（rendezvous 加权），1–1000，缺省 1                                                                                                             |
| `RELAY_NODE_TOKEN` / `RELAY_NODE_TOKEN_FILE`              |      | 本节点的节点令牌（`armadra-cloud nodes token`），节点间转发时出示；没有就不向别的节点转发                                                                     |
| `RELAY_DATABASE_URL` / `RELAY_DATABASE_URL_FILE`          |      | 流量统计的 PostgreSQL（`armadra_relay_traffic` 角色）；账本要有 0009，没有就拒绝启动；不配就不记流量                                                          |
| `RELAY_METRICS_LISTEN`                                    |      | 指标监听（`/metrics`）                                                                                                                                        |
| `RELAY_METRICS_TOKEN` / `RELAY_METRICS_TOKEN_FILE`        |      | 指标的 Bearer 令牌                                                                                                                                            |
| `RELAY_TRUSTED_PROXY` / `RELAY_TRUSTED_PROXY_PREFER`      |      | 受信反向代理的 IP / CIDR（逗号分隔）与先读的头（`forwarded-for` 缺省 / `real-ip`），规则同 personal                                                           |
| `RELAY_TEAM_CONCURRENT` / `RELAY_TEAM_CONNECT_PER_MINUTE` |      | 控制面没写额度（或额度坏了）时的团队并发与建连速率，缺省 10 与 600 / 分钟（最严格的套餐）                                                                     |
| `RELAY_FORWARDED_PER_ACCOUNT`                             |      | 每个入口节点上、每个账号经节点间连接转发的端到端通道数上限（不是全集群合计）（链路的 4096 个流位各租户共用），1–1000000，缺省 64；超出答 4429 `limit_reached` |
| `RELAY_DRAIN_GRACE_MS`                                    |      | 排空（GOAWAY rebalance）的宽限，缺省 30000；容器的停止宽限要比它长（compose 的 `stop_grace_period: 40s`）                                                     |
| `RELAY_ALLOW_INSECURE_TUNNEL`                             |      | `1` 允许 `ws://` 的隧道与 `/x/v1` 地址，只用于本地开发                                                                                                        |
| `RELAY_LOG_LEVEL`                                         |      | 缺省 `info`                                                                                                                                                   |

- Redis 不可达时拒绝启动（退出 1）；`/ready` 报 `redis`、`node`（排空中不就绪）与配了流量库时的 `database`。
- `SIGTERM` 与控制面的 `drain`：节点表标 `draining`、隧道 GOAWAY `rebalance`、不再收新隧道；`SIGTERM` 等隧道走完（或宽限到）再关。
- 流量只在入口节点计，带中继令牌的 `team`；节点不清理保留期（控制面的作业清）。

中继（`armadra-relay personal serve`，参数优先，其次环境变量）：

| 参数 / 变量                                                   | 必填 | 说明                                                                                                                              |
| ------------------------------------------------------------- | ---- | --------------------------------------------------------------------------------------------------------------------------------- |
| `--data-dir` / `RELAY_DATA_DIR`                               | 是   | 数据目录（0700）：`relay.db` 或 `state.json`、`tls/`、`acme/`                                                                     |
| `--store` / `RELAY_STORE`                                     |      | `sqlite` / `postgres`；缺省按数据目录里已有的，新装 `sqlite`；`state.json` 先迁到 SQLite                                          |
| `--database-url` / `RELAY_DATABASE_URL`                       |      | `postgres://…`；`--store postgres` 与 `--control postgres` 必填                                                                   |
| `--database-url-file` / `RELAY_DATABASE_URL_FILE`             |      | 从文件读连接串（口令不出现在进程参数里）                                                                                          |
| `--database-schema` / `RELAY_DATABASE_SCHEMA`                 |      | 用哪个 schema，缺省连接的缺省 schema                                                                                              |
| `--control` / `RELAY_CONTROL`                                 |      | `memory`（缺省，单节点）/ `redis` / `postgres`（多节点）                                                                          |
| `--redis-url` / `RELAY_REDIS_URL`                             |      | `redis://…`；`--control redis` 必填，要 Redis 7 以上                                                                              |
| `--node-id` / `RELAY_NODE_ID`                                 |      | 本节点的名字，多节点时各不相同，缺省 `--host`                                                                                     |
| `--host` / `RELAY_HOST`                                       | 是   | 对外的 IP 或域名（issuer 与证书 SAN）                                                                                             |
| `--port` / `RELAY_PORT`                                       |      | 对外端口，缺省 8443（443 时 issuer 省略端口）                                                                                     |
| `--public-origin` / `RELAY_PUBLIC_ORIGIN`                     |      | 对外来源，缺省 `https://<host>:<port>`                                                                                            |
| `--tls` / `RELAY_TLS`                                         |      | `self-signed`（缺省）/ `acme` / `file` / `plain`（只允许回环监听）                                                                |
| `--tls-cert`、`--tls-key` / `RELAY_TLS_CERT`、`RELAY_TLS_KEY` |      | `--tls file` 的证书与私钥，变化时热加载                                                                                           |
| `--acme-email` / `RELAY_ACME_EMAIL`                           |      | `--tls acme` 必填                                                                                                                 |
| `--acme-directory` / `RELAY_ACME_DIRECTORY`                   |      | 缺省 Let's Encrypt 生产目录                                                                                                       |
| `--acme-ca-bundle` / `RELAY_ACME_CA_BUNDLE`                   |      | 信任 ACME 目录服务器的 PEM（测试 CA）                                                                                             |
| `--acme-http-listen` / `RELAY_ACME_HTTP_LISTEN`               |      | `http-01` 挑战监听，缺省 `0.0.0.0:80`                                                                                             |
| `--web-root` / `RELAY_WEB_ROOT`                               |      | Armadra `apps/web` 构建产物；镜像缺省 `/app/web`（构建参数 `WEB_IMAGE`）                                                          |
| `--account` / `RELAY_PERSONAL_ACCOUNT`                        |      | 存储不存在时自动初始化的账号                                                                                                      |
| `--password-file` / `RELAY_PERSONAL_PASSWORD_FILE`            |      | 自动初始化的口令文件（之后不再读取）                                                                                              |
| `--listen` / `RELAY_LISTEN`                                   |      | 缺省：回环 host 绑 `127.0.0.1`，否则 `0.0.0.0`                                                                                    |
| `--log-level` / `RELAY_LOG_LEVEL`                             |      | `debug` / `info`（缺省）/ `warn` / `error`                                                                                        |
| `--name` / `RELAY_NAME`                                       |      | 服务端名称（`platform.info.name`），客户端添加时的缺省显示名；缺省取状态里的 `displayName`，再取 `--host`                         |
| `--trusted-proxy` / `RELAY_TRUSTED_PROXY`                     |      | 受信反向代理的 IP / CIDR，逗号分隔，如 `127.0.0.1/32,::1/128`；直连对端在列表内才读 `X-Forwarded-For` / `X-Real-IP`，缺省不信任   |
| `--trusted-proxy-prefer` / `RELAY_TRUSTED_PROXY_PREFER`       |      | 先读哪个头：`forwarded-for`（缺省）/ `real-ip`；要和 `--trusted-proxy` 一起给                                                     |
| `--metrics-listen` / `RELAY_METRICS_LISTEN`                   |      | `/metrics` 的单独监听（只绑内网地址），缺省不开                                                                                   |
| `RELAY_METRICS_TOKEN`                                         |      | 设了则 `/metrics` 要 `Authorization: Bearer <令牌>`                                                                               |
| `--limit-anonymous` / `RELAY_LIMIT_ANONYMOUS`                 |      | 无令牌或令牌无效的请求，按 IP 每分钟上限，缺省 60                                                                                 |
| `--limit-preflight` / `RELAY_LIMIT_PREFLIGHT`                 |      | 0.5.0 起不再使用（中继不答 CORS 预检），仍接受                                                                                    |
| `--limit-token` / `RELAY_LIMIT_TOKEN`                         |      | 有效中继令牌每分钟上限，缺省 1200                                                                                                 |
| `--limit-account` / `RELAY_LIMIT_ACCOUNT`                     |      | 同一账号全部令牌每分钟上限，缺省 3600                                                                                             |
| `--challenge` / `RELAY_CHALLENGE`                             |      | 人机挑战：`turnstile` / `off`（缺省）；开了以后 `platform.info` 报 `challenge`，登录缺令牌答 `400 challenge_required`（契约 §16） |
| `--turnstile-site-key` / `RELAY_TURNSTILE_SITE_KEY`           |      | Turnstile 站点密钥（不是秘密）；`--challenge turnstile` 必填                                                                      |
| `--turnstile-secret-file` / `RELAY_TURNSTILE_SECRET_FILE`     |      | Turnstile secret 文件（compose 用 secret 挂载）；`--challenge turnstile` 必填                                                     |
| `--turnstile-hostnames` / `RELAY_TURNSTILE_HOSTNAMES`         |      | siteverify 答复里允许的主机名，逗号分隔；空 = 拒绝一切；有 https 的 `--public-origin` 时不得含本机地址                            |
| `--challenge-scope` / `RELAY_CHALLENGE_SCOPE`                 |      | 要挑战的 procedure，`auth.login`（缺省）、`links.accept`，逗号分隔                                                                |
| `--traffic-retention-days` / `RELAY_TRAFFIC_RETENTION_DAYS`   |      | 流量统计保留天数，缺省 90，`0` = 不记录（契约 §19）                                                                               |

限流额度也可写在状态文件 `settings.rateLimits`（`anonymous`、`preflight`、`token`、`account`，都是 1–1000000 的整数，可省略），优先级：命令行 > 环境变量 > 状态文件 > 缺省；改动后重启 `serve` 生效。登录、链接接受与隧道登记的限流不可配。

其余子命令（`init`、`status`、`passwd`、`rotate-key`、`links list|revoke`、`traffic`、`backup`、`doctor`）只读写存储，可以在 `serve` 运行时执行（`docker compose exec relay armadra-relay personal …`），改动一秒内生效；`migrate --to sqlite|postgres` 要先停 `serve`（多节点要停掉所有节点）。数据目录用 PostgreSQL 时每个子命令都要连接串（参数或环境变量）。

存储（[运维手册](../docs/runbooks/personal-storage.md)）：

| `--store`  | 文件                                     | 适用                                                                | 备份                                        |
| ---------- | ---------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------- |
| `sqlite`   | `relay.db`（WAL，0600）                  | 新装缺省；单节点个人 / 小团队                                       | `personal backup --out F`（在线、一致）     |
| `file`     | `state.json`（0600）                     | 0.3.x 之前的部署；Go 版只读，`serve` 拒绝，先 `migrate --to sqlite` | `personal backup --out F` 或 `tar` 数据目录 |
| `postgres` | PostgreSQL（数据目录留 `postgres.json`） | 多节点（显式选，配 `--control redis\|postgres`）                    | 库自己的备份（`pg_dump`）                   |

两种以上存储都在、`--store` 与目录里已有的不符、库认不出或损坏、账本对不上，一律拒绝启动，不自动清库或重建。`personal doctor` 检查目录与文件权限、库的完整性、监听地址与 TLS、受信代理与口令，有失败项时退出码为 1。

`--control redis` 需要 Redis 7.0 以上（撤销全量重读用 `PEXPIRETIME`）；版本更低时 Go 版中继启动报错并说明，Node 版同样要求。

## 端点

- `GET /health`：进程存活，200 即活着（镜像的 `HEALTHCHECK` 经 `armadra-relay health` 打它，先 HTTPS 再明文）。
- `GET /ready`：依赖就绪（cloud 报 PostgreSQL 与 Redis，saas relay 报 Redis 与排空状态（配了流量库另报 `database`），personal relay 报数据目录，多节点时另报 `database` 或 `redis`），任一不可达答 503。
- 控制面 `/v1/*` 与 `/.well-known/*`：SaaS M1 的账号、团队、主机、授予、审计、运营等过程已实现，M2 的过程仍答 501 `{ code: "not_implemented", message, requestId }`；`/health`、`/ready`；配了 `WEB_ROOT` 时托管控制台页面（`/`、`/login`、`/verify`、`/reset`、`/t/*`、`/h/*`、`/j/*`、`/device`、`/invite/*`、`/ops/*` 回落 `index.html`，带 `Referrer-Policy: no-referrer` 与 CSP）。
- 个人中转另有 `/v1/*` 子集、`/.well-known/*`、`/ca.crt`、`/app/*` 与 `/j/<linkId>`（[契约](../docs/contracts/cloud-api.md) §10–§11）。

## 切换到 Go 版中继（0.10.0）与回滚

0.10.0 起 `ghcr.io/ama-link/armadra-relay` 是 Go 实现，镜像由 `go/Dockerfile` 构建（Node 版源码删除后，原 `apps/relay/Dockerfile` 挪到这里）。下面这些与之前的 Node 镜像一致，入口与底座的变化见表后：

| 项       | 值                                                                                           |
| -------- | -------------------------------------------------------------------------------------------- |
| 镜像名   | `ghcr.io/ama-link/armadra-relay:<版本>` 与 `:latest`                                         |
| 入口     | 0.10.0：`tini -- armadra-relay`；下一个版本起：`armadra-relay`；缺省命令 `saas serve`        |
| 运行用户 | uid / gid 10002（0.10.0 镜像里名为 `armadra`；distroless 镜像无该 passwd 条目，只有数字 id） |
| 卷与端口 | `/data`、8443                                                                                |
| 环境     | `RELAY_WEB_ROOT=/app/web`、`RELAY_DATA_DIR=/data`、`RELAY_LISTEN=0.0.0.0:8443`               |
| 健康检查 | 每 30 秒、超时 5 秒、3 次，`health` 子命令                                                   |
| 页面     | `WEB_IMAGE` 构建参数把 `/app/web` 拷进来                                                     |
| 库       | 同一份迁移与账本（SQLite `relay_migrations`、PostgreSQL `cloud_migrations`）                 |

变了的只有：下一个版本起运行镜像底座从 `debian:bookworm-slim` 换成 distroless static（约 23 MB，没有 shell、`tini` 与包管理器；Go 二进制直接当 PID 1，入口由 `tini -- armadra-relay` 变成 `armadra-relay`，覆盖 entrypoint 调 tini 或 `exec` 进 shell 的用法不再可用；`docker compose exec relay armadra-relay …` 照用，要 shell 排障见 `go/Dockerfile` 头注释）；容器里的命令行是 `armadra-relay`（不再是 `node /app/out/cli.js`）；Go 版不写 `state.json`；`--node-id` 同时作用于隧道层。`saas serve` 在切换版里仍是占位；之后的版本换成真正的 SaaS 中继节点（上面的变量表），新增必填的 `RELAY_TUNNEL_URL`。

升级：备份（`personal backup` 或 `pg_dump`）→ 数据目录是 `state.json` 的先停服并 `run --rm relay personal migrate --data-dir /data --to sqlite` → 换成 0.10.0 镜像起服 → `exec relay armadra-relay personal status` 与 `doctor` 核对。回滚：停服，镜像换回上一版（如 `0.9.3`）再起，Go 版写过的库 Node 版照常读写。步骤与注意事项见[运维手册](../docs/runbooks/personal-storage.md#切换到-go-版中继0100与回滚)。

## SaaS 单机 M1（`deploy/saas`）

`saas/compose.yml`：口令与连接串全部是 `secrets/` 下的文件（`init-secrets.sh` 生成；PostgreSQL 用 `POSTGRES_PASSWORD_FILE`，控制面用 `DATABASE_URL_FILE` 与 `REDIS_URL_FILE`，节点用 `RELAY_DATABASE_URL_FILE` 与 `RELAY_REDIS_URL_FILE`）；网络分 `edge` 与 `backend`（internal），代理只进 `edge` 且 IP 固定，Redis 要口令；`postgres`（首次启动生成自签证书，超级用户只用于初始化，控制面用另建的库属主角色 `armadra_cloud` 以 `sslmode=require` 连；生产换托管库并用 `verify-full`）→ `migrate`（跑一次 `armadra-cloud migrate`）→ `db-roles`（节点的流量角色 `armadra_relay_traffic`，只有流量表读写与账本只读）→ `cloud` → `relay-a`、`relay-b`（每个节点有自己的隧道地址 `wss://<节点>.edge.<域>/t/v1`，各自一枚节点令牌）；`redis` 开 AOF（`appendfsync everysec`）、`maxmemory 128mb`、`noeviction`；`--profile proxy` 起参考 nginx（TLS 终止，WebSocket 设置与登录 / 链接接受限流，访问日志用 `$uri` 不记 query）；`compose.mailpit.yml` 把邮件送进 mailpit（开发 / 端到端，不要用在生产）。步骤、节点令牌的签发与撤销、升级与回滚、备份、Redis 故障时登录失败关闭与告警，见 [SaaS 运维手册](../docs/runbooks/saas.md)；变量在 `saas/.env.example`。

- 登录失败计数共享在 Redis，Redis 不可用时登录一律答 `503 unavailable`（失败关闭，不回落到进程内计数）；控制面指标 `armadra_cloud_lockout_unavailable_total` 计这类拒绝，应当告警。
- 验收：`pnpm e2e --mode saas`（真实控制面 + 两个节点 + 一次性 PostgreSQL / Redis / mailpit，设计 §8.4 的 14 个场景）。

## 升级顺序

先升 relay、后升 cloud；PostgreSQL 迁移（`packages/control-store-postgres/migrations/`）由 `armadra-cloud migrate` 执行，个人中转 `--store postgres` / `--control postgres` 打开时也会补跑同一份（advisory lock 保证只跑一次）。个人中转的 SQLite 迁移（`packages/control-store-sql/migrations/`）在打开 `relay.db` 时自动补跑；库比程序新（降级）时拒绝启动，先备份再升回去。
