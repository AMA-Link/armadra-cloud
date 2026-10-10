# Cloudflare Workers 上的个人中转：运维

部署步骤与面板设置见 [deploy/cloudflare/README.md](../../deploy/cloudflare/README.md)。本文是部署之后的日常：初始化、心跳与检查周期、日配额、撤销、存储拒绝服务、主密钥、回滚与排障。

## 初始化

- 第一次打到 `/v1/*`、`/.well-known/*` 或 `/ready` 的请求让 `ControlDO` 建库：跑与 Node 中继同一份 SQL 迁移（`packages/control-store-sql/migrations/`，账本 `relay_migrations` 记 SHA-384），用 secret `RELAY_PERSONAL_ACCOUNT` / `RELAY_PERSONAL_PASSWORD` 建账号（口令策略与 Node 中继相同、scrypt 哈希），生成第一把签名钥（用 `RELAY_MASTER_KEY` 做 AES-256-GCM 包裹后落盘）。
- 缺 `RELAY_MASTER_KEY`（或不是 64 位十六进制）、缺账号 / 口令、口令不合策略：`/v1` 与 `/ready` 答 503 `not_initialized`，什么也不建；`wrangler tail` 里有一行「还没初始化」带原因码（`missing_credentials`、`password_too_short`……）。
- 初始化成功后删掉两个初始化 secret；还在的话每次对象启动记一行提醒。之后改这两个 secret 不会改账号。
- 改口令、轮换签名钥：Workers 版暂时没有入口（Node 中继的 `personal passwd` / `rotate-key` 对应的管理接口在后续批次）。

## 心跳与检查周期

- 心跳「源主动、中继被动」（契约 §17）：`ready.heartbeatMs` = `RELAY_HEARTBEAT_MS`（缺省 45 秒，可改 10–60 秒），源每个周期发一次 `PING`，中继立即回 `PONG`，自己不发 `PING`——对象在两次心跳之间可以休眠，不计时长。
- 检查周期 `RELAY_ALARM_MS`（配置 F 120 秒、配置 P 60 秒）：超过 `2 × heartbeatMs` 没收到源的任何帧就以 1001 `heartbeat_timeout` 关隧道、源变离线；最长判死时间 ≈ `2 × heartbeatMs + RELAY_ALARM_MS`。
- 客户端 WS 两个方向都超过 `RELAY_CLIENT_IDLE_MS`（缺省 120 秒）没有字节就以 4000 `idle` 断开（契约 §7 的活性保证）；Cloudflare 边缘本身也会断开 100 秒上下没有数据的连接。
- 改心跳只改 `wrangler.jsonc` 的 `vars` 后部署；已连着的隧道在下一次重连时拿到新值。

## 日配额

- 按 UTC 自然日计：请求数（Worker 进对象的请求、隧道与客户端 WS 的入站消息按 20:1 折算）与隧道载荷字节，计数在 `ControlDO`。
- 超了 `RELAY_DAILY_REQUEST_BUDGET` 或 `RELAY_DAILY_TUNNEL_BYTES_BUDGET`：源地址的请求与新隧道答 429 `limit_reached`，`/v1`、`/health`、`/ready` 照常；UTC 零点自动恢复。`0` = 不限。
- 看用量：`curl -H "Authorization: Bearer <指标令牌>" https://relay.armadra.com/metrics`，`relay_budget_used{kind="requests"}` 与 `relay_budget_limit`。连续几天超过七成考虑升 Workers Paid（配置 P）。
- 计数按对象各自攒一批才报，Worker 入口的 Rate Limiting 绑定按 Cloudflare 节点各自计数：都是近似值，账单的真相看 Cloudflare 面板。

## 撤销即时生效

- 撤销源：先以 4403 断开经它的客户端 WS，再以 4492 关隧道；之后这个源换不到隧道令牌（410 `source_revoked`），已签出的中继令牌 401。
- 撤销分享链接、登出、撤销设备、改口令：`SourceDO` 里「没撤销」的判断最多缓存 10 秒，`ControlDO` 写入这些变化时立即通知各源的对象作废缓存；访客的客户端 WS 以 4403 断开。

## 存储拒绝服务（`storage_unavailable`）

- `ControlDO` 的库认不出（没有账本却有业务表）、迁移账本对不上（名字或 SHA-384 不同、编号不连续）、库是更新的版本写的、签名私钥解不开（主密钥不对或数据损坏）：`/v1` 与 `/ready` 一律答 503 `storage_unavailable`，`wrangler tail` 有「存储不可用，拒绝服务」。
- **中继绝不自动清库或重建**。先确认部署的是哪个版本（`wrangler versions list`）：库比程序新就回滚到写它的版本；主密钥录错就重新 `wrangler secret put RELAY_MASTER_KEY` 录对的值。
- 主密钥确实丢了：签名钥解不开，只能放弃这份数据重建——在 `wrangler.jsonc` 加一条迁移把 `ControlDO` 换成新类名（`renamed_classes` 不行，要 `deleted_classes` + `new_sqlite_classes`），这是人工、不可逆的决定；之后所有源重新登记、所有客户端重新登录。

## 部署、回滚与重启

- 每次部署重启全部 DO、断开全部 WS，core 按退避自动重连；发布窗口里客户端会看到几秒到一分钟的 `source_offline`。
- 代码回滚：`pnpm exec wrangler rollback`。DO 数据不随代码回滚变化，迁移只追加，老代码遇到新 schema 要么能读、要么按上一节拒绝服务。
- DO 类（`SourceDO`、`ControlDO`）只在首版建一次，不改名、不删。

## 排障

| 现象                               | 先看                                                                                                           |
| ---------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `/health` 503 `unavailable`        | `vars` 不合法（`RELAY_ISSUER` 不是来源、心跳越界……），`wrangler tail` 有「配置不对」与原因                     |
| `/ready` 503 `not_initialized`     | 初始化 secret 与主密钥，见「初始化」                                                                           |
| `/ready` 503 `storage_unavailable` | 见「存储拒绝服务」                                                                                             |
| 源一直离线                         | core 日志里的隧道关闭码：4490 令牌或 `?src=` 不符、4429 限流、4413 帧太大；中继日志「隧道握手失败」的 `result` |
| 客户端 429 `rate_limited`          | Worker 入口的闸（按 IP）或对象里的桶（按令牌 / 账号）；`Retry-After` 给出等待秒数                              |
| 客户端 429 `limit_reached`         | 日配额、单源 256 条流或单账号 256 条 WS                                                                        |
| 经中继的 WS 1009                   | 客户端单条消息超过 1 MiB                                                                                       |
| 经中继的请求 502 `bad_gateway`     | 源的 HTTP 响应不合法（中继自己解析响应）                                                                       |

日志只记元数据（源、隧道、流号、结果、关闭码），不记路径、头、令牌、正文。
