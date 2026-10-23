# 中继 Go 重写：设计与分批计划

> 状态：定稿（2026-10-22），§9 已按建议定下；实施跟踪 AMA-Link/armadra-cloud#59。
> 与旧方案的关系见 §10。草稿在工作区 `docs/cloud/drafts/go-relay-rewrite-design.md`，只留作追溯。
> 输入：
>
> - 性能对比报告：工作区 `docs/cloud/reports/relay-go-vs-node-bench-2026-10.md`
> - armadra-cloud `main`（`4e7c74f`，0.9.1，协议 1.3）的源码盘点
> - `docs/contracts/cloud-api.md`
> - `docs/design/multi-tenant-saas-design.md`
> - AMA-Link/armadra-cloud#51（节点间转发）、#57（同隧道队头阻塞）
>
> 已定的四件事（维护者 2026-10-22）：
>
> | #   | 决定                                                                                                        |
> | --- | ----------------------------------------------------------------------------------------------------------- |
> | D1  | 中继数据面与个人中转的 `/v1` 子集全部用 Go，个人中转是一个 Go 二进制；SaaS 控制面也在同一个 Go 代码库里实现 |
> | D2  | Cloudflare Workers 版中继保留 TypeScript，冻结功能，只跟协议走                                              |
> | D3  | 兼容现有数据目录：沿用 SQLite / PostgreSQL 的迁移与账本，老库直接启动；`state.json` 只读，迁到 SQLite       |
> | D4  | Go 代码放在 armadra-cloud 仓的 `go/` 目录                                                                   |

## §0 摘要

- **范围**：用 Go 重写中继（SaaS 要提供的完整能力与「团队」模型另见 [SaaS 能力设计](saas-design.md)），交付两样东西：
  - 与现有 Node 版行为一致的**个人中转**：同一个镜像名、同一套命令行与数据目录。
  - 第一次真正能用的 **SaaS 中继节点**：子域寻址、多节点共享路由、节点间转发。
  - 之后在同一个 Go 代码库里实现 **SaaS 控制面**，接替现在全部答 501 的 `apps/cloud`。
- **现状**：
  - Node 的 `saas serve` 只是空壳。
  - 现在线上的「SaaS 形态」只有 Workers 版。
  - 所以 SaaS 部分是新建，不是迁移。
- **协议不变**：
  - 协议包 `@armadra/platform-protocol` 仍是唯一的源；Go 的类型与常量从它导出的 `schema/` 生成。
  - 黄金夹具、身份向量、错误码注册表做跨语言一致性测试。
  - Armadra 的客户端与 core 一行不改。
- **连接模型**：沿用压测里验证过的做法。客户端通道走事件驱动（Linux 上用 epoll，其他平台退回每连接一个 goroutine），隧道先是每隧道两个 goroutine。事件循环小批处理、主动让出，信用在字节交给内核之后才补。
- **分批**：G0 到 G6 七批，见 §7。
  - G0–G4 完成个人中转的切换：发布一个版本，`armadra-relay` 镜像换成 Go。
  - G5 是 SaaS 中继节点，同时关掉 armadra-cloud#51。
  - G6 是 SaaS 控制面。

## §1 范围

**做**

- **数据面**：隧道 `/t/v1`、端到端通道 `/s/<id>/_relay/e2e`（personal）与 `<id>.src.<域>/_relay/e2e`（saas）、令牌校验与 JWKS 缓存、限流、撤销、流量统计、指标、日志、健康检查。
- **个人中转**：
  - `armadra-relay personal` 的全部子命令：`init`、`serve`、`status`、`passwd`、`rotate-key`、`links`、`traffic`、`migrate`、`backup`、`doctor`。
  - `/v1` 个人子集（cloud-api §10），以及 `/v1/me/stream`。
  - 托管页面 `/app/*`、`/j/<id>`、`/ca.crt`。
  - 四种 TLS：自签 CA、ACME http-01、文件证书热换、回环明文。
  - 三种控制面：memory、redis、postgres。
- **SaaS 中继节点**：`armadra-relay saas serve`，包括子域寻址、Redis 控制面、节点间转发、节点令牌、GOAWAY 再平衡。
- **SaaS 控制面**：`armadra-cloud serve|migrate|version`，按 `multi-tenant-saas-design.md` 的模型，见 §3.9。

**不做**

- Workers 版中继（D2）：`relay-runtime-workers`、`apps/relay-workers`，以及它依赖的 TS 包 `relay-kernel`、`control-core`、`control-store-*` 都保留，冻结功能。
- 协议本身的改动。#57 的修法单独走协议流程；Go 版写端的结构要预留按流轮转，见 §3.3。
- Armadra 仓的产品功能；只有探针脚本要跟着改路径，见 §4.3。

## §2 现状（2026-10-22）

| 部分                                          | 现状                                              | 规模（非测试行） | Go 重写后                                                                 |
| --------------------------------------------- | ------------------------------------------------- | ---------------: | ------------------------------------------------------------------------- |
| `relay-kernel`                                | 完整，Node 与 Workers 共用                        |         约 3,100 | Go `internal/tunnel` `edge` `auth` `control` `traffic`；TS 版留给 Workers |
| `relay-runtime-node`                          | 完整（ws 外壳、redis / postgres 控制面）          |         约 1,700 | 由 Go 取代后删除                                                          |
| `apps/relay` personal                         | 完整（命令行、TLS、ACME、存储、托管页面、doctor） |         约 6,100 | Go `cmd/armadra-relay` + `internal/personal`，切换后删除                  |
| `apps/relay` saas                             | 空壳：只有 `/health` 与 `/ready`                  |                — | G5 新建                                                                   |
| `control-core`                                | 个人子集完整，其余预留                            |         约 2,600 | Go `internal/core`；TS 版留给 Workers 的 ControlDO                        |
| `control-store-sql` / `-sqlite` / `-postgres` | SQLite 迁移 0001–0002、PG 迁移 0001–0003、账本    |         约 4,700 | Go `internal/store`，迁移文件逐字节复用                                   |
| `apps/cloud`                                  | 只有 `/health`、`/ready`，其余 501                |           约 600 | G6 由 Go `cmd/armadra-cloud` 取代                                         |
| Workers 版                                    | 线上 `relay.armadra.com`（personal 形态）         |         约 3,900 | 不动                                                                      |

盘点时顺带发现：personal 模式把 `options.host` 当成隧道层的节点名，`--node-id` 只作用于控制面（`personal/serve.ts`）。Go 版改为两层都用 `--node-id`，缺省仍取主机名，在更新记录里注明。

## §3 架构

### 3.1 目录（`armadra-cloud/go/`，一个 Go 模块）

```text
go/
  go.mod                      module github.com/AMA-Link/armadra-cloud/go
  cmd/armadra-relay/          personal、saas serve、health、version、help
  cmd/armadra-cloud/          SaaS 控制面：serve、migrate、version（G6）
  internal/protocol/          帧、关闭码、RST 码、限制、握手消息；gen/ 是从 schema/ 生成的类型、常量与错误码
  internal/jose/              EdDSA JWS 签与验、JWKS 缓存（10 分钟、未知 kid 10 秒最多刷一次）
  internal/tunnel/            握手、流与信用、心跳、GOAWAY / 替换、隧道表
  internal/edge/              寻址、准入策略、e2e 管道、刷新拒绝、跨源回显
  internal/netpoll/           Linux epoll 循环（经 Go netpoller 等待）；其他平台用 goroutine 实现同一接口
  internal/control/           ControlPlane 接口与 memory / redis / postgres 三种实现
  internal/traffic/           流量计量（小时桶、60 秒落库、保留期）
  internal/obs/               slog JSON 日志（字段白名单）、Prometheus 文本指标、健康与就绪
  internal/core/              控制核服务：账号、会话、签名、源、链接、me-stream、流量、挑战、撤销
  internal/api/               /v1 路由（路由表从 OpenAPI 生成）、错误答复、CORS、Armadra-Protocol 头
  internal/store/             存储端口；sqlite/、postgres/；迁移 embed、账本、拒绝规则；statefile/ 只读导入
  internal/tlsx/              自签 CA 与叶证书、ACME（x/crypto/acme）、文件热换、指纹
  internal/web/               /app、/j、根目录文件、CSP 与缓存头
  internal/personal/          个人中转的装配与全部子命令
  internal/saas/              SaaS 中继节点的装配；节点间转发
  internal/cloud/             SaaS 控制面的业务（G6）
  bench/                      压测程序与套件（从 scratch/relay-bench 搬来），用作性能回归
  tools/schemagen/            从 packages/platform-protocol/schema/ 生成 internal/protocol/gen 与路由表
```

依赖保持最少，全部写精确版本：

| 用途       | 依赖                                                                                           |
| ---------- | ---------------------------------------------------------------------------------------------- |
| WebSocket  | `gorilla/websocket`（隧道与 me-stream 的升级、核心侧测试）；边缘的帧收发自己写（事件驱动要求） |
| PostgreSQL | `jackc/pgx/v5`                                                                                 |
| Redis      | `redis/go-redis/v9`                                                                            |
| SQLite     | `modernc.org/sqlite`（纯 Go，静态链接、交叉编译；§9 Q3）                                       |
| 加密等     | `golang.org/x/crypto`（scrypt、acme）、`golang.org/x/sys`（epoll）                             |
| 不引入     | Web 框架、ORM、指标库（文本格式自己写，十来行）                                                |

### 3.2 协议一致性（不分叉的保证）

- **生成**：`tools/schemagen` 读 `packages/platform-protocol/schema/`，生成 `internal/protocol/gen` 与 `/v1` 路由表（方法、路径、scope）。输入是：
  - `constants.json`、`errors.json`；
  - `assertion/*.json`、`tunnel/*.json`、`e2e/accept.json`；
  - `cloud-openapi.json`。
- **新鲜度检查**：CI 跑 `go generate ./... && git diff --exit-code`，协议包一改，Go 侧必须同 PR 重新生成。并进现有的 `pnpm protocol:check`。
- **夹具测试**：`packages/platform-protocol/fixtures/` 全部用作 Go 的表驱动测试。
  - `tunnel/*.bin` 编解码逐字节一致，`handshake.json` 走完四步。
  - `assertion/*.jwt` 验签与声明校验，含负例 `wrong-alg`、`wrong-aud`、`expired`。
  - `identity-vectors/vectors.json` 测 scrypt 与令牌哈希。
  - `cloud-api/<procedure>.json` 全部：每个过程的输入与输出都要通过 JSON Schema 校验。只在测试里用校验库。
  - `errors.json` 校验错误码与 HTTP 状态的映射。
- **端口与服务合同**：`control-core/src/testing/ports-contract.ts`（15 例）与 `services-contract.ts`（19 例）逐例翻译成 Go 测试，三种存储都要过。今后改合同时两边同改。

### 3.3 连接与并发模型（来自压测 §7）

- **客户端通道（边缘）**：
  - Linux 上每核一个 epoll 循环，epoll fd 经 Go netpoller 等可读。
  - 一批最多 64 个事件，处理完让出。
  - 读进循环共用的暂存区，只有半帧跨读才临时分配。
  - 下行由隧道读协程非阻塞直写，写不完的排队、等 EPOLLOUT。
  - 上行没信用就挂起并停读。
  - ping 用全局时间轮，每秒处理 1/30。
  - 其他平台（macOS 开发机）用每连接 goroutine 实现同一个 `netpoll.Conn` 接口。行为相同，只是内存不同。
- **隧道**：先是每隧道一个读、一个写 goroutine，写端是由信用约束的队列。写端按流轮转出队（每流每轮最多一个 DATA 块），这在 Go 内部就能做，不改线协议，正好对应 #57 的方案 A。隧道改成事件驱动放到 G5 之后，按压测数据决定做不做。
- **信用与背压**：字节真正交给内核之后才记「已消费」、补 WINDOW。流窗与隧道窗沿用协议值，单流 256 KiB、隧道 8 MiB。
- **锁序**：隧道锁 → 连接锁 → 轮询器锁；连接写队列只在持连接锁时改。
- **性能回归**：`bench/` 的套件进夜间 CI（Linux runner，中继与压测分核），看四个指标：
  - 每连接内存 ≤ 16 KB；
  - 15000 连接小消息 p99 ≤ 100 ms；
  - 单核双向转发 ≥ 300 MB/s；
  - 调度延迟（`runtime/trace` 的 sched）不回退。

### 3.4 存储与兼容（D3）

- **迁移文件**：SQL 文件仍以 TS 包里的为准：
  - SQLite：`packages/control-store-sql/migrations/`
  - PostgreSQL：`packages/control-store-postgres/migrations/`

  `go generate` 把它们拷进 `go/internal/store/{sqlite,postgres}/migrations/` 并 `go:embed`。repo-check 按 `migrations.lock` 校验两份逐字节一致。新迁移仍只在 TS 包里加，编号接 main 最大号。

- **账本与拒绝规则**照搬：
  - SQLite：`relay_migrations(id, name, sha384, applied_at_ms)`；`application_id = 0x41524d44`；`quick_check`；WAL、`busy_timeout` 5 秒、`BEGIN IMMEDIATE`；新建文件 0600。拒绝 `not_found` / `exists` / `unknown` / `corrupt` / `newer`。
  - PostgreSQL：`cloud_migrations`，在 `pg_advisory_xact_lock(0x41524d44)` 下单事务执行。表存在而账本缺失时拒绝；错误信息不带连接串。
  - 都不自动清库、不重建。
- **数据格式不变**：
  - 签名私钥：PKCS#8 base64。
  - 口令记录：scrypt 参数存在记录里。
  - 令牌哈希：`sha256("armadra/...\0" + value)`。
  - 跨进程变更检测：SQLite 用 `data_version`，PostgreSQL 用 tenant `revision`。
  - Go 版打开 TS 版写过的库，状态完全一致；反过来也行，这样能回滚。
- **state.json**：Go 版不再写文件存储。
  - `serve` 遇到 `state.json` 时拒绝，提示用 `personal migrate --to sqlite`。
  - `migrate` 读 `state.json`（schema v1）、写进新库、逐字段核对，旧文件改名成 `state.json.migrated-<时间>`。
- **流量统计**：`relay_traffic_buckets` 同表同主键，加法 upsert，每 60 秒批量写，50,000 行积压上限。

### 3.5 个人中转（与 Node 版逐项对齐）

| 项                   | 要点                                                                                                                                                                                                          | Go 包                                 |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- |
| 命令行               | 子命令、参数、环境变量逐个保留（`deploy/README.md` 是参数表）；退出码 0 / 64 / 1；参数优先于环境变量，环境变量优先于缺省                                                                                      | `internal/personal`                   |
| `/v1` 子集           | cloud-api §10 全部过程；body ≤ 64 KiB；CORS `*`；`Armadra-Protocol: 1.3`；错误 `{code, message, requestId, details?}` 加 `Retry-After`                                                                        | `internal/api` + `internal/core`      |
| 账号                 | scrypt N=2^15、r=8、p=1；口令策略四条；锁定（5 次后 1 分钟起翻倍，15 分钟封顶，1 小时清零）；未知账号名做等时派生                                                                                             | `internal/core`                       |
| 会话                 | 访问令牌 15 分钟；刷新令牌 30 天、每次旋转、重放即撤销整台设备；`lastSeen` 每 5 分钟落库                                                                                                                      | `internal/core`                       |
| 签名                 | `k1…kN`，轮换后旧钥 1 小时内仍在 JWKS 里；`jti` 形如 `t.` / `r.` / `a.` 前缀，按形状判撤销                                                                                                                    | `internal/core` + `internal/jose`     |
| 源与链接             | 注册令牌 10 分钟、最多 20 个；源 JWS ≤ 120 秒、`jti` 防重放；链接 30 天、秘密只在 `#` 片段里、访客主体 `guest:<linkId>:<8 hex>`                                                                               | `internal/core`                       |
| me-stream            | 票 30 秒、一次性；每 30 秒复核会话；事件按 owner / 访客过滤                                                                                                                                                   | `internal/core` + `internal/personal` |
| 客户端 IP 与受信代理 | `--trusted-proxy`（CIDR 列表）、`--trusted-proxy-prefer forwarded-for\|real-ip`；只有直连对端在受信网段内才读头；`X-Forwarded-For` 从右往左取第一个不受信的地址，任一段坏了整头不信；`X-Real-IP` 含逗号即不信 | `internal/edge`                       |
| 撤销生效             | 每秒查一次存储变化：源撤销 → 关隧道并断开该源的客户端连接；链接撤销 → 断开 `guest:<linkId>:` 前缀的连接                                                                                                       | `internal/personal`                   |
| TLS                  | 自签 CA（P-256、10 年，叶证书 90 天、剩 7 天换）；ACME http-01（P-256，剩 30 天续期，12 小时重试；挑战监听对其余请求 308）；文件证书每 2 秒轮询、500 ms 防抖热换；明文只许回环                                | `internal/tlsx`                       |
| 托管页面             | `/` 与 `/app` 308 到 `/app/`；SPA 回退；HTML 加 `no-store` 与 CSP；带哈希的资源 `immutable`；路径限定在根目录内、realpath 复核                                                                                | `internal/web`                        |
| 维护命令             | `status`、`passwd`、`rotate-key`、`links`、`traffic`、`migrate`、`backup`（SQLite 用 `VACUUM INTO`）、`doctor`                                                                                                | `internal/personal`                   |
| 关闭顺序             | 隧道 GOAWAY → 断边缘连接 → 关服务 → 关指标 → 流量落库 → 释放 `serve.pid` → 关存储                                                                                                                             | `internal/personal`                   |

### 3.6 SaaS 中继节点（G5，新建）

- **寻址**：`<sourceId>.src.<RELAY_DOMAIN>`，转小写、去端口，路径原样。TLS 用运营方的通配证书，或在前面的反代终止。
- **控制面**：
  - **Redis**（key 前缀 `relay:`）：
    - 路由 `route:src:<id>`，`SET PX` 写入、`PEXPIRE` 续期，Lua 按 tunnelId 比较删除；
    - 限流 Lua `INCR` 加窗口；
    - 撤销 `revoked:<jti>`，配合 pub/sub；
    - 频道 `presence`、`ctl`、`revoked`；
    - 订阅连接每次就绪都全量 `SCAN` 撤销表。
  - **PostgreSQL**（`relay_routes` 等三张表 + `NOTIFY`）是同一接口的另一实现，给不想引入 Redis 的多节点个人中转用。
- **节点间转发**（关掉 #51）：
  - 客户端连到节点 A，源的隧道在节点 B。A 按路由表找到 B，经一条常驻的节点间 WebSocket（`/x/v1`，节点令牌鉴权，scope `relay-node`）把这条 e2e 通道转过去。
  - 节点间连接直接复用隧道帧（OPEN / DATA / WINDOW / END / RST，A 每个心跳周期 PING、B 答 PONG），信用照样逐跳计。e2e 的 OPEN 规定路径固定、不带头，没处放目标源，所以节点之间的 OPEN 用 `kind: ws`、路径寻址 `/s/<sourceId>/_relay/e2e`（与 personal 的源地址同一种写法，`edge.InternodeOpen` / `ParseInternodeOpen`），B 再以标准的 `kind: e2e` OPEN 转给源，`remoteIp`、`clientOrigin`、`relayNode`（入口节点）照抄。
  - A 这一侧的节点间连接就是一条 `tunnel.Tunnel`（A 是开流方，`Link()` 是对端节点名），每个对端一条，断了下次用到时重连，连不上 2 秒内不再试（客户端直接 4404）。对端地址取节点表的 `internodeUrl`，同样要是 `wss`（开发时 `RELAY_ALLOW_INSECURE_TUNNEL` 才许 `ws`）。没配节点令牌的节点不转发。
  - 链路的限制（两端同一份 `linkConfig`）：每条链路最多 4096 条流（跨租户共用，满了答 `limit_reached`）；链路级窗口取「流数上限 × 流窗」= 1 GiB，链路级信用永远不是瓶颈，慢客户端只占满自己那条流的 256 KiB，拖不住同一条链路上的别人。内存上界：每条链路每一端「在途流数 × 256 KiB」（全卡住时 1 GiB，实际受边缘的账号 / 团队并发上限约束）。同一个源被自己的慢客户端占满 8 MiB 隧道窗口是协议本身的限制，与转发无关。
  - B 丢掉一条转来的流（源 RST、A RST、链路断开）时，把挂着还没发出去的下行字节记作已消费、还给本地隧道，否则源的隧道级窗口会被一点点漏光。
  - B 记下 A 出示的节点令牌的 `jti` 与 `exp`：到 `exp` 断开；每 5 秒查一次撤销表（内存副本），命中就断开（`nodes revoke` 对已建立的链路也生效）。
  - 源的 e2e 能力由 B 判：A 不替远端的源声明能力；源没报 e2e 时 B 答 RST `refused`，A 以 4404 `e2e_unsupported` 关客户端，与本地判定同一结果（只是多一次往返，先完成了升级）。
  - B 侧把转来的流当作本地边缘流接进隧道。
  - 所有节点共享路由表；源端按 `heartbeatHint.nodes` 的权重选节点。控制面按 sourceId 做 rendezvous 哈希给节点排序，让客户端与源大概率落在同一节点，转发只兜底。
- **再平衡**：
  - 节点下线前给本地隧道发 GOAWAY `rebalance`（grace 30 秒）。
  - 源按 hint 重连到别的节点；客户端通道随旧隧道结束，由客户端重连。
- **节点注册表与命令**（沿用 Armadra `docs/design/platform/relay.md` §5）：
  - **节点表**：每个节点每 20 秒写一次 Redis 哈希 `relay:nodes`（node → `{url, weight, version, draining, connections, tunnels, internodeUrl?}`，每个字段 `HPEXPIRE` 60 秒，没续就过期；正常下线时 `HDEL`）。`url` 是该节点自己的隧道地址（如 `wss://a.edge.<域>/t/v1`，`RELAY_TUNNEL_URL`），不是共用入口；`internodeUrl` 是别的节点连它 `/x/v1` 的地址（`RELAY_INTERNODE_URL`，缺省由 `url` 把路径换成 `/x/v1`，可以缺）。控制面的 `heartbeatHint` 从这里取节点列表，去掉 `draining` 的，按 rendezvous 排序（[SaaS M1 实施设计](saas-m1-implementation.md) §2.4、§7.3；排序函数是 `internal/control` 的 `RankNodes`，两边共用）。编解码在 `control/cluster/wire.go`（`EncodeNode` / `DecodeNode`）。
  - **命令频道**：每个节点订阅自己的 `relay:node:<id>`（`closeTunnel`、`kick`），广播仍走 `relay:ctl`。
  - **共享 JWKS**：缓存在 Redis 的 `jwks:<issuer 哈希>` 里，所有节点共用，只有一个节点回源刷新。
  - **跨节点并发**：`edge.concurrent.team` 用有序集合 `relay:conc:team:<teamId>` 计数：成员是 `<节点>:<随机>`（每条连接一个），分数是到期毫秒（Redis 的 `TIME` 加 60 秒），一个脚本里先清到期的、再 `ZCARD`、没满才 `ZADD`；节点每 20 秒给自己的成员续期，连接结束 `ZREM`，节点崩溃后成员最多留 60 秒。上限取 `relay:quota:team:<teamId>`，缺键或坏了用节点缺省（最严格的套餐：并发 10、速率 600 次 / 分钟，`RELAY_TEAM_CONCURRENT` / `RELAY_TEAM_CONNECT_PER_MINUTE` 可覆盖）；续期用 `ZADD XX`，只续还在的成员。Redis 出错时团队并发放行，计 `relay_team_concurrency_fail_open_total`，日志 10 秒最多一条。（原写法是 `SADD` / `SREM` / `SCARD`，集合的成员不能单独过期，改成分数即到期的有序集合，语义相同。）`edge.concurrent.acct` 仍只在本节点计。
  - **kick 与 drain**：`kick` 只踢客户端连接（按 `sourceId`，再按可选的 `subject`、`subjectPrefix`、`deviceId` 收窄），隧道保留；`closeTunnel` 关隧道；`drain {node}` 只由该节点执行：节点表标 `draining`、`/ready` 不就绪、不再收新隧道、隧道 GOAWAY `rebalance`。
  - **Redis 配置**：`maxmemory-policy noeviction`，撤销记录不能被挤掉；`maxmemory` 128 MB；不备份，重启后路由与限流自然重建。
- **节点间鉴权**（与 #51 对齐）：
  - #51 原来的写法是每次转发带 `X-Armadra-Internode: HMAC(secret, sourceId|ts)`。改为常驻连接 + 节点令牌：
    - 控制面给每个节点签一枚 `relay-node` 令牌（Ed25519，经密钥下发，定期轮换），节点间 `/x/v1` 升级时出示。
    - 持有节点按签名与 `aud` 校验，过期或伪造即拒。
  - **#51 的验收照旧**：
    - 伪造、过期被拒；
    - 持有节点下线后，入口节点给客户端 4404，客户端重连后落到新节点；
    - 流量**只在入口节点计**，入口节点验过中继令牌、知道 `sub` 与 `dev`；持有节点对转来的流不计数，也不再验中继令牌。
- **节点上报**：§17 的 `relay.nodePresence` 是给不连 Redis 的 Workers 用的。Go 节点直接写 Redis，不调它。
- **指标**：在个人中转的指标之上，加 `relay_internode_streams`、`relay_internode_bytes_total{direction}`、`relay_routes_remote_total`。

### 3.7 日志、指标、健康（两种模式共用）

- **日志**：
  - `slog` JSON 一行一条：`{time, level, name:"relay", msg, ...}`，`msg` 是简体中文短句。
  - 字段走白名单：sourceId、tunnelId、streamId、node、kind、result、code、reason、bytesIn、bytesOut、durationMs、tunnels、streams、mode、url、listen、error。
  - 错误只记类名。
  - 绝不记正文、路径、查询串、头、令牌、口令、连接串。CI 用现有的泄密扫描（e2e 的 secret scan）守住。
- **指标**：只在 `--metrics-listen` 上开，可选 Bearer 令牌（比较前先做 SHA-256，再常量时间比较）。指标名与桶全部沿用（`relay_tunnels`、`relay_streams`、`relay_bytes_total`、`relay_edge_connections_total`、`relay_edge_streams_total`、`relay_handshake_total`、`relay_heartbeat_miss_total`、`relay_handshake_seconds`），已有的看板不用改。
- **健康**：`/health` 答 `{ok, mode, node, version, tunnels, streams}`；`/ready` 并行探测，每项 2 秒；`armadra-relay health` 子命令给容器健康检查用。

### 3.8 镜像与发布

| 项         | 做法                                                                                                           |
| ---------- | -------------------------------------------------------------------------------------------------------------- |
| 构建       | 多阶段：`golang` 构建静态二进制（`CGO_ENABLED=0`，linux amd64 与 arm64）                                       |
| 运行镜像   | 沿用 `debian:bookworm-slim` + tini，或 distroless static（§9 Q6）；镜像从约 250 MB 降到约 30 MB                |
| 运行约定   | uid 10002、`/data` 卷、端口 8443；`WEB_IMAGE` 构建参数照旧把 `/app/web` 拷进来                                 |
| 入口与检查 | `ENTRYPOINT ["armadra-relay"]`，缺省 `saas serve`；健康检查 `armadra-relay health`                             |
| 镜像名     | `ghcr.io/ama-link/armadra-relay:<v>` 与 `:latest` 不变；`release.yml` 的规则不变（tag 等于版本号、冒烟后推送） |
| 切换版本   | 统一版本号，切换算功能变化，用切换时的下一个 minor（§9 Q5）                                                    |

### 3.9 SaaS 控制面（G6，Go）

完整的能力、租户模型（个人空间 + 团队、团队内分组）、角色、数据模型、API 与分期见 [SaaS 能力设计](saas-design.md)。

G6 只管怎么落地：

- **复用**：G2 的 `internal/core`（账号、会话、签名、源、链接、me-stream、流量）。
- **新增**：
  - `internal/cloud`：团队、分组、授予、邀请、访问申请、审计、配额与计费、邮件与作业队列。
  - `cmd/armadra-cloud`：`serve`、`migrate`、`version`、`keys rotate`。
- **迁移**：PostgreSQL 接 `control-store-postgres` 现有的 0003 往后加，与中继的 `--store postgres` 共用账本。
- **部署**：Go 不能跑在 Workers 上，所以部署位置从「Render 还是 Cloudflare」变成「VM / 容器平台」，见 §9 Q2。

### 3.10 SaaS 部署拓扑

合并自 `relay-on-cloudflare-design.md` §2.11、§3、§4 与 `multi-tenant-saas-design.md` §4.2、§4.4、§5.2、§5.6、§5.7。

| 域名                 | 用途                                                                     | 证书                                                                                |
| -------------------- | ------------------------------------------------------------------------ | ----------------------------------------------------------------------------------- |
| `app.armadra.com`    | 控制面与控制台；**issuer = APP_ORIGIN**，同一个来源，用 `__Host-` Cookie | 普通证书                                                                            |
| `edge.armadra.com`   | 源的隧道入口 `/t/v1`（SaaS 节点）                                        | 普通证书                                                                            |
| `*.src.armadra.com`  | 源地址 `<sourceId>.src.armadra.com/_relay/e2e`，每个源一个浏览器来源     | 二级通配：经 Cloudflare 代理时要 Advanced Certificate Manager；不代理时用 DNS-01 签 |
| `status.armadra.com` | 状态页                                                                   | 普通证书                                                                            |
| `relay.armadra.com`  | 现有的 Workers 个人中转，不动（D2）                                      | 现状                                                                                |

- **单机 M1**：一台 VM 上用 compose 起这些服务：
  - `armadra-cloud`、2 个 `armadra-relay saas serve`；
  - PostgreSQL、Redis（独立容器，不复用别的服务的 Redis）；
  - Armadra 的 `apps/push-relay`。

  前面放一个反向代理终止 TLS。

- **反向代理的 WebSocket 设置**：
  - `proxy_read_timeout 3600s`
  - `proxy_buffering off`、`proxy_request_buffering off`
  - keepalive 75 秒
  - `client_max_body_size` 1 MiB
  - 登录与链接接受加 `limit_req`

  中继按 §3.5 开受信代理。

- **证书先于登记**：真证书签好之前不给客户端登记源。指纹一换，所有人都要重新登记。
- **升级顺序**：先升中继节点（GOAWAY rebalance 排空），再升控制面。协议兼容窗口是当前与上一个 major，12 个月。
- **备份与监控**：
  - PostgreSQL 按库备份；签名私钥用 `ARMADRA_CLOUD_MASTER_KEY` 包裹，主密钥另存。
  - Redis 不备份。
  - 监控 `/metrics` 与 `/health`；中继日志的 IP 只留 /24 或 /48 前缀。
- **放在 Cloudflare 后面时**（可选）：
  - 一条 zone 级限速规则；
  - 关掉 Bot Fight Mode，它会挑战原生 App 与 core；
  - Turnstile 只用在登录、注册、找回口令、链接接受；
  - Terraform 管规则；
  - 保留 kill switch 与花费告警（`docs/runbooks/cloudflare-protection.md`）。
- **中国大陆访问**：Cloudflare 没有大陆节点（RTT 150–300 ms），Argo 对 WebSocket 无效。需要时在大陆放一个直连节点，归入 SaaS 设计 M3 的「区域」。

## §4 不能变的外部契约

### 4.1 线上与数据

- **线协议**：隧道帧、握手、关闭码、RST 码、端到端通道、令牌声明、`/v1` 的形状与错误码，全部以协议包为准。
- **数据目录**：
  - 布局 `relay.db`、`postgres.json`、`tls/`、`acme/`、`serve.pid`；
  - 文件权限：目录 0700，文件 0600；
  - 「同时存在多种存储就拒绝启动」的规则。

### 4.2 运维接口

- 命令行、环境变量、退出码。
- 指标名、日志形状、健康检查的 JSON。
- 镜像名、入口、端口、uid、卷。入口在 G4.1 由 `tini -- armadra-relay` 变成 `armadra-relay`（distroless，无 tini 与 shell），经 §9 Q6 的 G4.1 评估接受；覆盖 entrypoint 调 tini 或 `exec` 进 shell 的用法不再可用。

### 4.3 Armadra 那边要跟着改的（切换批 G4 同时开一个 Armadra PR）

- **探针找中继的方式**：`tools/probes/cloud-source.mjs` 与 `platform-lib.mjs` 现在用 `node apps/relay/src/cli.ts` 起中继，并检查这个文件存在。改成按优先级找：
  1. `ARMADRA_RELAY_BIN`
  2. cloud 仓的 `go/bin/armadra-relay`（不存在就 `go build`）
  3. 镜像
- **dev-stack**：`docker-compose.cloud-src.yml` 在 G4 照旧用 `apps/relay/Dockerfile` 构建，路径不变，内容换成 Go；G4.1 起 Dockerfile 挪到 `go/Dockerfile`，Armadra 同步改（Armadra PR：https://github.com/AMA-Link/Armadra/pull/492，探针 `cloud-source.mjs` 与文档一并改；先合并 cloud #95，再合并这个 PR）。
- **两个 devstack 集成测试**：读的环境变量与 `.data/personal/dev.env` 保持不变。`scripts/relay-personal.mjs` 改成启动 Go 二进制。
- **探针里的旧代码**：还在发 `armadra-relay-token` 头（这条路径 0.5.0 起答 404），顺手清理。

## §5 测试与验收

| 层             | 内容                                                                                                                                               | 验收                           |
| -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| 单元与夹具     | §3.2 的生成物新鲜度、夹具、身份向量、错误码                                                                                                        | 全过；`go test -race`          |
| 端口与服务合同 | 15 + 19 例，跑 SQLite 与 PostgreSQL（`pnpm dev:up` 的库）                                                                                          | 全过                           |
| 模糊测试       | 帧解码、边缘 WS 帧解析、JWS 解析、HTTP/1 升级头                                                                                                    | 夜间各跑 5 分钟，没有崩溃      |
| 兼容           | 用 TS 版建的 SQLite 库与 PG schema（固定在测试夹具里）由 Go 版打开并读写；再交回 TS 版能读；`state.json` 导入                                      | 全过                           |
| 跨实现端到端   | 现有 `scripts/e2e.mjs --mode personal` **不改用例**，对 Go 镜像跑：登录、端到端通道、HTTP / 事件 / 终端 / 画布、明文 404、访客链接、撤销、泄密扫描 | 全过；作为切换门槛             |
| Armadra 探针   | `personal-roundtrip`、`multi-source`、`link-join`、`nat-core-offline`、`webkit-roundtrip`、`relay-challenge-desktop` 对 Go 中继跑                  | 全过                           |
| SaaS           | 两个节点 + Redis：客户端与源在不同节点（走转发）、节点下线再平衡、撤销跨节点生效                                                                   | `pnpm e2e --mode saas`（新增） |
| 性能           | §3.3 的四个指标；对照 Node 版留一份基线                                                                                                            | 夜间不回退                     |

## §6 工程

- **工具链**：`mise.toml` 加 `go = "1.27.1"`。根 `package.json` 加 `go:check`（gofmt、`go vet`、staticcheck 固定版本、`go test -race`、`go generate` 新鲜度），`pnpm check` 里调用。
- **repo 规则**：
  - `repo.rules.json` 根目录白名单加 `go/`。
  - 文件行数上限扩到 `.go`，生成物除外。
  - `go/` 只许 import 标准库与登记过的模块。
  - `internal/core` 不许 import `internal/edge` / `internal/tunnel`，延续现在「控制核不碰中继内核」的边界。
- **许可**：`tools/notices.mjs` 增加 Go 模块（读 `go.mod` 与模块 LICENSE），写进同一个 `THIRD_PARTY_NOTICES.md`。
- **CI**：
  - `ci.yml` 加 `go` 作业：linux amd64 与 arm64，含 race；改动只碰 `go/` 时 TS 作业跳过，反之亦然。
  - `images:build` 构建 Go 版中继镜像并冒烟。
  - `nightly.yml` 加模糊测试与性能套件。
- **AGENTS.md**：补 Go 的边界、错误码只从生成物取、日志字段白名单、「协议包是源」。

## §7 分批计划

规则：

- 每批一个 worktree（`worktrees/armadra-cloud/go-g<N>`）、一个 PR，每个模块一个提交，CI 全绿后用 merge commit 合并。
- 每批引用总 Issue。
- 复杂或安全相关的由 Opus 做，机械、范围清楚的由 Sonnet 做。

| 批   | 内容                                                                                                                                                                          | 依赖              | 交付与验收                                                                                                        | 模型                                     | 规模 |
| ---- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------- | ----------------------------------------------------------------------------------------------------------------- | ---------------------------------------- | ---- |
| G0   | Go 模块骨架、mise、CI `go` 作业、repo 规则、notices、`tools/schemagen`；`internal/protocol` 与 `internal/jose`，以及夹具测试                                                  | —                 | 夹具与身份向量全过；生成物新鲜度检查进 CI                                                                         | Sonnet                                   | M    |
| G1   | 数据面：`tunnel`、`edge`（含 `netpoll`）、`control/memory`、限流、`traffic`（内存）、`obs`；`armadra-relay` 只有数据面的开发装配                                              | G0                | 夹具驱动的隧道测试；把 `relay-kernel` 测试用例翻译过来；`bench` 达到 §3.3 的指标                                  | Opus                                     | L    |
| G2   | `store`（sqlite / postgres、迁移 embed、账本、拒绝规则、statefile 导入）、`core` 全部服务、`api` 路由                                                                         | G0                | 15 + 19 例合同测试；全部 cloud-api 夹具做形状校验；TS 与 Go 交替读写同一个库                                      | Opus                                     | L    |
| G3   | `personal` 装配与全部子命令、`tlsx`（四种 TLS）、`web`、me-stream、撤销监视、`control/redis` 与 `control/postgres`、关闭顺序                                                  | G1、G2            | 命令行参数与 `deploy/README.md` 逐项对照；ACME 对 pebble（`pnpm dev:up --profile acme`）；doctor、backup、migrate | Sonnet（ACME 与撤销用 Opus）             | L    |
| G4   | 切换：`apps/relay/Dockerfile` 改成 Go，`scripts/relay-personal.mjs`、`e2e.mjs` 指向 Go；Armadra 探针 PR（§4.3）；更新记录与部署文档；发切换时的下一个 minor                   | G3                | §5 的端到端与 Armadra 探针全过；数据目录升级与回滚各演练一次                                                      | Opus                                     | M    |
| G4.1 | 删除 TS 的 `apps/relay` 与 `relay-runtime-node`（`relay-kernel` / `control-core` / `control-store-*` 留给 Workers）；更新 AGENTS.md 边界                                      | G4 发布后一个版本 | `pnpm check`；Workers 端到端照旧过                                                                                | Sonnet                                   | S    |
| G5   | SaaS 中继节点：子域寻址、Redis 控制面接线、节点令牌、`/x/v1` 节点间转发、rendezvous 排序的 hint、GOAWAY 再平衡、新指标；`pnpm e2e --mode saas`                                | G4                | 两节点端到端；关掉 #51                                                                                            | Opus                                     | L    |
| G6   | SaaS 控制面，按 [SaaS 能力设计](saas-design.md) §9 分期：M1 按模块拆 PR（账号 → 团队 → 主机与授予 → 分享 → 审计与邮件 → 配额），之后是 M2、M3；控制台页面在 Armadra 仓另开 PR | G2、G5            | 各模块的集成测试；`pnpm e2e --mode saas` 走真实账号与团队流程                                                     | Opus（账号、团队、授予）/ Sonnet（其余） | XL   |

- G1 与 G2 互不依赖，可以并行（两个 worktree）。
- #57 的按流轮转在 G1 里实现（Go 内部，不改线协议）。TS 侧与协议层的改法跟着 #57 自己的流程走。

## §8 风险

| 风险                                           | 应对                                                                        |
| ---------------------------------------------- | --------------------------------------------------------------------------- |
| 两套控制核（TS 给 Workers、Go 给节点）慢慢分叉 | Workers 冻结功能；合同测试两边都跑；新功能只进 Go，Workers 要用就先评估     |
| 逐项对齐漏了边角行为                           | 现有 e2e 用例不改，直接对 Go 镜像跑；把 Node 版的单测逐条翻译，不是重新设计 |
| `modernc.org/sqlite` 的兼容性或性能            | G2 先跑兼容测试与压测；不行就换 cgo 驱动，代价是交叉编译与静态链接          |
| JSON 的 null 与「字段缺省」和 zod 不一致       | 全部夹具做形状校验；协议包导出的 JSON Schema 是判据                         |
| 事件循环在少核上调度饿死（压测里出现过）       | 小批处理 + 让出写进实现规范；性能套件里查调度延迟                           |
| 切换时老部署升级失败                           | 同一个库格式，可以回滚到 TS 镜像；G4 演练升级与回滚                         |

## §9 已定问题（2026-10-22 按建议定下）

| #   | 问题                                                                                  | 建议                                                                                                                                |
| --- | ------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| Q1  | SaaS 多节点怎么让客户端到达持隧道的节点：节点间转发，还是负载均衡按 Host 做一致性哈希 | 节点间转发为主（任何 LB 都能用），rendezvous 排序的 hint 提高同节点命中；鉴权与计数见 §3.6                                          |
| Q2  | SaaS 控制面部署在哪里（Go 不能上 Workers）                                            | M1 与中继节点同机 compose（一台 VM + PG + Redis）；规模上去再拆                                                                     |
| Q3  | SQLite 驱动：纯 Go（modernc）还是 cgo（mattn）                                        | modernc，保住静态二进制                                                                                                             |
| Q4  | 切换后删除 TS 的 `apps/relay` 与 `relay-runtime-node` 的时机                          | 切换版之后的下一个版本（G4.1）                                                                                                      |
| Q5  | 切换版的版本号                                                                        | 切换时的下一个 minor，三仓统一（协议 1.4 先随 v0.10.0 发布，切换版不预先占号）                                                      |
| Q6  | 运行镜像底座：`debian:bookworm-slim` + tini，还是 distroless static                   | distroless static（没有 shell，体积小）；需要排障时用调试镜像。G4 为保持入口逐项一致暂用 bookworm-slim，distroless 留到 G4.1 再评估 |
| Q7  | 线上 `relay.armadra.com` 要不要换成 Go 节点                                           | 先不换（D2，Workers 继续服务）；Go 节点在 SaaS 域名上线并稳定后再评估                                                               |

### G4.1 的 Q6 评估结论：换 distroless static

2026-10-23 在 `go/Dockerfile` 上实测，结论是换，已随 G4.1 落地（`gcr.io/distroless/static-debian12`）。

| 项           | bookworm-slim + tini（0.10.0）        | distroless static                                               |
| ------------ | ------------------------------------- | --------------------------------------------------------------- |
| 镜像体积     | 106 MB                                | 22.8 MB                                                         |
| 入口         | `tini -- armadra-relay`               | `armadra-relay`（Go 当 PID 1，不起子进程，不需要回收僵尸进程）  |
| 健康检查     | `armadra-relay health`（exec 形式）   | 同；不依赖 shell 与 curl                                        |
| 运行用户     | 镜像内建 `armadra`（uid / gid 10002） | `USER 10002:10002`，无 passwd 条目；`/data` 在镜像里先 chown    |
| `/data` 卷   | 命名卷首次挂载继承目录属主            | 同（实测命名卷可写，`personal init` / `serve` 正常）            |
| 优雅停止     | SIGTERM 经 tini 转发                  | `docker stop` 约 1 秒退出、退出码 0（Go 自己处理 SIGTERM）      |
| CA 证书      | `apt-get install ca-certificates`     | 镜像自带；ACME 与 HTTPS 出站不受影响                            |
| 排障         | `exec relay sh`                       | 没有 shell；子命令（`status`、`doctor`、`traffic`…）照常 `exec` |
| 构建依赖外网 | 构建时 `apt-get`                      | 构建时只拉基础镜像，不再 `apt-get`                              |

可接受的理由：运维用到的全是 `armadra-relay` 子命令，`exec relay armadra-relay …` 照用；没有 shell 的损失只在临场排障，用 `docker run --rm -it --network container:<名> --pid container:<名> busybox` 或 `docker debug` 补，`go/Dockerfile` 头注释写了办法。`pnpm e2e --mode personal` 对 distroless 镜像全部通过。回滚到 0.10.0 或更早的镜像不受影响（镜像是自包含的，入口各用各的）。

## §10 与旧方案的关系

| 旧方案                                                                                                   | 处理                                                           |
| -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| `multi-tenant-saas-design.md` §4.5（控制面留 TS，Go 只在触发条件后做数据面）                             | 被 D1 取代                                                     |
| `multi-tenant-saas-design.md` §5（49 服务器拓扑）、§7 Q2                                                 | 49 服务器已退役；被 §3.10 取代                                 |
| `multi-tenant-saas-design.md` §4.2、§4.4、§5.2、§5.6、§5.7                                               | 合并进 §3.10                                                   |
| `relay-on-cloudflare-design.md` §6（控制面放 49，Workers `MODE=saas` 当 SaaS 节点）                      | 被 D2 与 §3.10 取代；`relay.nodePresence` 只留给冻结的 Workers |
| `relay-on-cloudflare-design.md` §2.11、§3、§4                                                            | 合并进 §3.10                                                   |
| Armadra `docs/design/platform/relay.md` §5（Redis 适配）                                                 | 合并进 §3.6                                                    |
| Armadra `docs/design/platform/cloud-control-plane.md` §1–§2（Node、oRPC、postgres.js 等选型与目录）      | 被 §3.1 取代                                                   |
| Armadra `docs/design/platform-implementation-plan.md` 的 SaaS 工作包（C2-x、C3-3、R3、C4-2、C6-x、A6-3） | 被 §7 的 G5、G6 取代                                           |
| #51 的 HMAC 头写法                                                                                       | 被 §3.6 的节点令牌取代；#51 的验收保留，并入 G5                |
| 工作区报告 §0 第 6 条（个人中转留 TS）                                                                   | 被 D1 取代                                                     |
