# 中继上 Cloudflare、防护与成本、Node 中继存储可选：设计（2026-10-10）

> **2026-10-22 起：** Workers 中继、防护与存储部分已实现，Workers 版冻结功能；§6（控制面放 49、Workers 当 SaaS 节点）由 [中继 Go 重写](go-relay-design.md) §3.6、§3.10 取代；§2.11 的域名规划与 §4 的防护已合并进那一节。
> 状态：设计稿（只设计，不改代码）。对应 AMA-Link/Armadra#256（Refs #229）。输入：[多租户与 SaaS 设计](multi-tenant-saas-design.md) §4、§5；armadra-cloud `main 362cb7b`（0.3.0：`packages/relay-kernel`、`packages/control-core`、`packages/cloud-shared`、`packages/platform-protocol` 协议 `1.1`、`apps/relay` personal 的状态文件端口、`apps/cloud` 骨架）；Armadra `main 462be14d`（`apps/desktop/src/core/relay/` 隧道客户端、`core/sources/` 客户端源表，契约最大节号 §59，迁移最大 `0045`）。Cloudflare 事实以文内引用的官方文档为准（读取日期 2026-10-10）。
> 现状（2026-10-11）：中继只部署在 Cloudflare 正式环境（`relay.armadra.com`），不再有 staging 与 49 服务器部署；人机验证缺省关闭。运维以 [runbooks/relay-on-cloudflare.md](../runbooks/relay-on-cloudflare.md) 为准，文中个人域名已换成 `example.org`。
> 编号基线：cloud-api 契约已用到 §14，前一份设计预留了 §15；本文从 **§16** 起。协议包 / 两个 app 均 `0.3.0`，协议 `1.1`。
> 本文不出现任何第三方参考项目的名字。

## §0 结论

1. **可以上，而且不用分叉协议**：隧道（`/t/v1`）、客户端边缘（`/s/<id>/…` 或 `<id>.src.<域>`）、`/v1/*` 控制面、媒体票、只读令牌（`ro`）、分享范围（`scp`）全部在 Workers + Durable Objects（DO）上按原样实现。唯一的线上差异是 CF 中继在 `sources.heartbeatHint` 里答的节点地址带 `?src=<sourceId>`（Node 版早已忽略 query，`http/server.ts:85`；core 的 `tunnelUrl` 原样保留 query，`client.ts:66-70`），Worker 靠它在握手前把隧道 WebSocket 路由到「这个源的 DO」。
2. **一源一个 DO（`SourceDO`）+ 一租户一个 DO（`ControlDO`）**。`SourceDO` 用 Hibernation API 持有隧道 WS 与经它的客户端 WS，自己做 HTTP/1.1 请求序列化与响应解析、WS 帧编解码、信用窗口、路由、媒体票、按源 / 按令牌 / 按账号的限流与撤销；`ControlDO` 用 DO SQLite 实现 `control-core` 的全部端口（账号、设备、源、注册令牌、链接、签名钥、设置）。不用 D1：个人中转的状态量很小，DO SQLite 与对象同处、事务同步、不多一跳；D1 留给「控制面上 CF」（§6）。
3. **内核去 Node 化是前置**：`relay-kernel` 今天直接 import `node:http` / `node:stream` / `node:net` / `ws`（`edge/server.ts:24-30`、`tunnel/server.ts:9-14`、`tunnel/stream.ts:18`、`http/client-ip.ts:9`）。拆成三层：`relay-kernel`（纯逻辑：帧、流、信用、握手、令牌、限流判定、CORS、地址、媒体票、**新增** HTTP/1.1 编解码与 WS 帧编解码）、`relay-runtime-node`（`ws`、`node:http`、监听、TLS、ACME）、`relay-runtime-workers`（Worker 入口、两个 DO、Hibernation 粘合、静态资源）。`control-core` 不动：它只用 `node:crypto`（scrypt、Ed25519、`timingSafeEqual`），Workers 的 `nodejs_compat` 原生支持这些（§2.9）。
4. **心跳改成「源主动、中继被动」**：协议已规定源每 `heartbeatMs` 发 `PING`、中继立即回 `PONG`，而且 `heartbeatMs` 由中继在 `ready` 里下发（core 按它来，`client.ts:567-578`）。CF 版下发 `heartbeatMs = 45_000`，自己不再起 `setInterval` 发 `PING`（会阻止休眠），用一个 60 秒 alarm 看 `lastHeardAtMs` 判死。CF 边缘的空闲断连实测在 100 秒上下（官方只说「一段时间无数据即关」并要求客户端心跳），45 秒留足两倍余量。
5. **大陆访问**：Free / Pro / Business 没有大陆节点，大陆流量就近落到香港 / 日本 / 美西，WebSocket 能通但 RTT 150–300 ms、个别运营商对 CF 任播段有丢包；China Network 要 Enterprise + ICP，现在不做。对用户最优的做法不是二选一，而是 **两条路并存**：49 服务器的 Node 中继（`armadra-personal.example.org`，大陆直达）+ CF 中继（海外 / 手机漫游 / 公网分享），Armadra 已合并的「一源多路」（迁移 `0044`，契约 §55）让同一台主机同时登记两条中继，客户端探测后选快的。Argo Smart Routing 对 WebSocket 不生效，不买。
6. **防护与账单**：zone `armadra.com` 已在 CF（Free Website）；Workers 套餐未确认，按 Free 设计、给出 Free / Paid 两套配置（§4.6）。Free 就能做到：L7 DDoS 托管规则、Free 托管 WAF、1 条按 IP 的限速规则（10 秒窗）、mTLS（CF 托管 CA）、Turnstile、Workers 内的 Rate Limiting 绑定（按路径 / 账号，10 或 60 秒窗，按节点近似）、Workers Free 的 100k 请求 / 天 **本身就是硬封顶**（超了直接失败，不会产生账单），够 2–3 台主机常驻。Workers Paid 没有硬花费上限，靠三道闸：预算告警（2026-06-15 起 PAYG 账号缺省开启）、Worker 自己的日配额计数（超过就 429）、`limits.cpu_ms` 与 DO 内的连接 / 消息大小上限。Free zone 上 **Cloudflare Managed / OWASP 托管 WAF 不可用**（Pro 起），只有 Free Managed Ruleset。Bot Fight Mode **不开**（它会挑战原生 App 与 core 的非浏览器请求，且 WAF 规则绕不过）；Pro 以上用 Super Bot Fight Mode 并对 `/t/v1`、源地址加 Skip。
   6a. **子域**（§2.11）：`relay.armadra.com` = CF 个人 / 公共中继（Worker Custom Domain，证书自动）；`cn.relay.armadra.com` = 49 的 Node 中继（灰云，替代 `armadra-personal.example.org`——它还没给任何客户端登记，直接改名，无迁移）；`app.armadra.com` = SaaS 控制面；`edge.armadra.com` + `*.src.armadra.com` = SaaS 中继与源地址（通配是二级子域，Universal SSL 不覆盖，SaaS 上线时订 Advanced Certificate Manager）；`status.armadra.com` = 状态页。媒体票不需要单独域名。
7. **Node 中继存储**：`--store file`（现状，保留）、`--store sqlite`（`node:sqlite`，Node 22.18 的 `engines` 已覆盖，单文件 `relay.db`，**新装缺省**）、`--store postgres`（多节点 / SaaS，复用 C8-1 的 PG 适配器）。Redis 改成只有多节点才需要：单节点全部在进程内（现有 `MemoryControlPlane`）；多节点二选一：Redis，或 PG `LISTEN/NOTIFY` + 表。`personal migrate --to sqlite` 一次性把 `state.json` 搬进 SQLite，原文件改名留底。
8. **SaaS 控制面**：M1 留在 49 服务器（PG + 进程内 jobs + SMTP）；CF 上先只跑数据面（personal 与 saas 两种 relay 都能跑在同一个 Worker 项目里，按 `MODE` 变量装配）。控制面日后可迁到 Workers + Hyperdrive 连同一个 PG，但要先解决 jobs（Cron Triggers + Queues 代替 `LISTEN/NOTIFY`）与邮件（HTTP 邮件 API 代替 SMTP）——不是 M1 的事。
9. **拆包**：K1 内核去 Node 化（Opus，XL）→ K2 Node 运行时与 `apps/relay` 重装配（Opus，L，行为零改动）→ K3 Workers 运行时与 `apps/relay-workers`（Opus，XL）∥ S1 SQLite 存储（Opus，L）→ K4 CF 防护 / Turnstile / IaC（Sonnet，M）∥ A8-1 客户端携带挑战令牌（Sonnet，M）→ S2 PG 存储与 Redis 可选（Opus，L）→ V4 Workers 端到端探针（Sonnet，M）→ D3 用户部署。编号预分配在 §7.4。
10. 需要用户决定的问题在 §9（5 条）。

## §1 现状核对（只读，行号以两仓 main 为准）

| 事实                                                                                                                                                                                                                       | 位置                                                                    |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| 隧道限制：帧 ≤ 1 MiB、DATA 块 64 KiB、流窗 256 KiB、隧道窗 8 MiB、每源 256 条流、心跳 20 s × 2 次、握手 10 s、GOAWAY 排空 30 s                                                                                             | `platform-protocol/src/tunnel/limits.ts`                                |
| 中继每 `heartbeatMs` 发 `PING`（`setInterval`），连续 2 次无 `PONG` 断；源发来的 `PING` 立即回 `PONG`；路由 TTL 45 s，`PONG` 时 `touch`                                                                                    | `relay-kernel/src/tunnel/tunnel.ts:127,264-271`、`control/types.ts:120` |
| core：`ready.heartbeatMs` 决定自己的 `PING` 周期，静默看门狗 = `heartbeatMisses × heartbeatMs`；重连指数退避 1 s → 60 s                                                                                                    | `apps/desktop/src/core/relay/client.ts:56-70,567-578,739-762`           |
| core 取节点：`GET /v1/sources/me/relay` → `nodes[].url`，`tunnelUrl()` 只在没路径时补 `/t/v1`，query 原样保留                                                                                                              | `core/relay/nodes.ts`、`client.ts:66-70`                                |
| Node 中继 `/t/v1` 匹配时先去掉 `?…`                                                                                                                                                                                        | `relay-kernel/src/http/server.ts:85-86`                                 |
| 边缘对客户端 WS：每 30 s `ping` 控制帧，60 s 无字节即断                                                                                                                                                                    | `edge/server.ts:336-337`、`edge/ws-downstream.ts`                       |
| 边缘 HTTP 转发用 `http.request({ createConnection: () => 流 })`，由 Node 解析源的 HTTP/1.1 响应                                                                                                                            | `edge/server.ts:5-8,24`                                                 |
| 限流九种（`tunnel.connect.src`、`edge.connect.ip/token/acct`、`edge.concurrent.acct`、`edge.preflight.ip`、`login.ip/acct`、`link.accept.ip`），缺省值                                                                     | `control/types.ts:104-117`                                              |
| 媒体票只在进程内存（≤ 4096 张，闲置 5 分钟 / 签出 30 分钟作废）                                                                                                                                                            | `edge/media-tickets.ts`                                                 |
| `control-core` 的 Node 依赖只有 `node:crypto`（`signer.ts`、`secrets.ts`、`passwords.ts`、`sources.ts`、`sessions.ts`）                                                                                                    | `packages/control-core/src/`                                            |
| `relay-kernel` 的 Node 依赖：`node:http`（类型 + `request` + `STATUS_CODES`）、`node:stream`（`Duplex`、`Writable`）、`node:net`（`BlockList`、`isIP`）、`node:crypto`（`randomBytes`、`verify`、`createPublicKey`）、`ws` | 见 §2.9 表                                                              |
| 状态文件 schema v1：`account`、`signingKeys[]`、`sources[]`、`registrationTokens[]`、`devices[]`、`links[]`、`settings{ allowGuests, trustedOrigins, rateLimits }`、`revision`                                             | `apps/relay/src/personal/state.ts`                                      |
| 端口接口（首参 `tenantId`）：`AccountStore`、`DeviceStore`、`SourceStore`、`RegistrationTokenStore`、`LinkStore`、`TenantStore`、`KeyStore`；中继侧 `RateLimiter`、`PresenceBus`、`RelayPort`                              | `packages/control-core/src/ports.ts`                                    |
| personal 装配：`StateStore` → `createStatePorts` → `createMemoryControlPlane` → `startRelay` + `createPersonalApi` + `createMeStream` + 托管页面                                                                           | `apps/relay/src/personal/serve.ts`                                      |
| `repo.rules.json` 的包依赖禁令：kernel 不得依赖 control-core / postgres / redis / `@orpc/*`；control-core 不得依赖 kernel / postgres / redis / ws                                                                          | `repo.rules.json packageDependencies`                                   |
| PG 迁移目录为空（下一个 `0001`，与前一份设计的 C8-1 预留一致）                                                                                                                                                             | `apps/cloud/src/db/migrations/`、`migrations.lock`                      |

## §2 Cloudflare 中继架构

### 2.1 拓扑

```text
core（桌面 / 服务器壳）
  │ wss://relay.<域>/t/v1?src=<sourceId>                 ← heartbeatHint 答的节点地址
  ▼
CF 边缘（TLS 终止、WAF、限速、DDoS）
  │
  ▼
Worker `armadra-relay`（无状态，每请求一次）
  ├─ /t/v1?src=…            → SourceDO(idFromName("src:"+sourceId)).fetch（WS 升级，Hibernation）
  ├─ /s/<id>/… 或 <id>.src.<域>/…  → 同上（HTTP 转发 / WS 升级 / 媒体票签与兑）
  ├─ /v1/*、/.well-known/*  → ControlDO(idFromName("tenant:"+tenantId)).fetch（oRPC 路由 = createCloudRouter）
  ├─ /v1/me/stream          → ControlDO（Hibernation 持有 me-stream 的客户端 WS）
  ├─ /app/*、/j/<linkId>    → Workers Static Assets（Armadra apps/web 构建产物；SPA 回落）
  └─ /health、/ready、/metrics → Worker 自己答（/metrics 需 `RELAY_METRICS_TOKEN`）
```

- **SourceDO**：每源一个；持有这个源当前的隧道 WS（新隧道就绪 → 旧的 `GOAWAY replaced`，与 Node 版相同）、经它的客户端 WS、流表与信用、路由行（就是「我这里有没有活隧道」）、媒体票、按源 / 令牌 / 账号的计数、`revoked:jti` 表。隧道与客户端都在同一个对象里，所以「边缘 → 隧道」不需要跨节点查路由：Node 版 `ControlPlane.routes` 在这里退化成对象内一个字段，`presence` 变成对 `ControlDO` 的一次 RPC。
- **ControlDO**：每租户一个（personal 恒 `tenant:default`）；DO SQLite 实现 `ControlPorts` 与 `KeyStore`；`RelayPort.route/closeTunnel/disconnect` 经 `env.SOURCE.get(idFromName(...))` 的 RPC 打到 `SourceDO`；`PresenceBus` 由 `SourceDO` 上下线时反向 RPC 推过来，再扇出到 me-stream 的 WS。
- **放置**：DO 第一次创建时落在离首个请求最近的 CF 数据中心，之后固定。隧道由 core 发起，所以 `SourceDO` 天然靠近主机；客户端在别处时请求多走一跳到 DO 所在地，这是 DO 模型的固有代价。`ControlDO` 用 `locationHint` 钉到用户主要所在区域（§9 Q2）。

### 2.2 为什么不是「Worker 直接做边缘、DO 只做隧道」

Worker 没有长期状态，每个客户端请求都要先找到隧道；隧道 WS 只能活在一个 DO 里，Worker 到 DO 的 `fetch` 本来就是一跳，所以把边缘逻辑整个放进 `SourceDO` 反而少一次 RPC，还顺带让「按账号并发 WS」「媒体票」「撤销表」这些状态有了确定的归属。代价是一个源的全部流量串行经过一个对象（单线程，软上限 1000 请求 / 秒 / 对象）——个人中转一台主机几十条流完全够；SaaS 的热源也远到不了这个量级（Node 版每源 256 条流的上限不变）。

### 2.3 隧道与心跳（Hibernation）

- 握手与帧处理照搬 `relay-kernel` 的 `Handshake` / `Tunnel` 状态机，只是 I/O 换成 `webSocketMessage(ws, data)` 入、`ws.send(bytes)` 出。`ws.serializeAttachment({ kind: "tunnel", tunnelId, sourceId, coreVersion, readyAtMs })` 让对象被唤醒后能认出这条 WS（附件上限 16 KiB）；流表、信用、未确认字节这些热状态留在内存，并在每次事件后写一份紧凑快照到 SQLite（`tunnel_state` 一行），唤醒时恢复。
- **心跳**：`ready.heartbeatMs = 45_000`（协议只规定「每 `heartbeatMs`」，数值由中继下发；core 的看门狗随之 90 s）。`SourceDO` 不起 `setInterval`（会阻止休眠），收到 core 的 `PING` 立即回 `PONG` 并更新 `lastHeardAtMs`；`alarm()` 每 60 s 一次（Workers Free 配置下 120 s，§4.6）：`now - lastHeardAtMs > 2 × heartbeatMs` → 关 4xxx `heartbeat_timeout`、发 `sourceOffline`；没有隧道时不再排 alarm。
- **成本估算**（DO 计费口径：入站 WS 消息按 20:1 折算请求、出站不计、协议层 ping 不计、休眠期间不计 Duration）：一条隧道空闲时每 45 s 一条入站消息 + 每 60 s 一次 alarm ≈ 每天 1,440 次 alarm + 96 次折算请求；Duration 按「每次唤醒在内存停留约 10 s」估 ≈ 每天 (1,920 + 1,440) × 10 s × 0.128 GB ≈ 4,300 GB-s。Free 套餐每天 100k 请求 / 13,000 GB-s：**2–3 条空闲隧道在 Free 内**；Paid 每月 100 万请求 / 40 万 GB-s 内含，**约 3 条常驻隧道在内含额度内**，之后每条隧道每月约 $1.6（Duration 为主）。活跃使用（终端流、画布事件流）期间对象常驻，按实际时长计。这是 Workers Paid 的最低 $5 / 月之外的数。
- **替换与多节点**：同源第二条隧道到同一个 DO，`GOAWAY replaced` 流程不变；没有「节点」概念，`Route.node` 固定填 `"cf"`。
- **部署重启**：每次 `wrangler deploy` 会重启所有 DO 并断开全部 WS（官方文档明说）。core 的退避重连 1–60 s 会自动接回；发布窗口里客户端会看到几秒到一分钟的 `source_offline`。发布前先把 `GOAWAY shutdown` 发出去做不到（DO 没有「即将重启」钩子），接受。

### 2.4 客户端 HTTP 转发（DO 内）

Node 版靠 `http.request` 把请求写成 HTTP/1.1 字节并解析响应；Workers 没有这条路，内核要补两个纯函数模块（K1）：

- `http1/request.ts`：`Request` → 请求头字节（已有 `serializeRequestHead`）+ 请求体 `ReadableStream<Uint8Array>` 切 64 KiB `DATA`，按流信用背压（`pull` 驱动，不预读整个体；请求体上限由 CF 套餐决定：Free / Pro 100 MB）。
- `http1/response.ts`：增量解析状态行、头、`Content-Length` / `chunked` / 关闭定界三种体；输出 `Response(status, headers, ReadableStream)`；`HEAD` 与 `1xx` / `204` / `304` 无体；头部上限沿用 `openHeadersMaxBytes`；非法响应 → `RST` + 502 `bad_gateway`（新错误码，登记进协议包）。
- 逐跳头剥离、`X-Forwarded-For` / `X-Forwarded-Proto`、CORS 预检、只读令牌门、媒体票：全部复用现有 `edge/` 逻辑，只把 `IncomingMessage` 换成一个 `EdgeRequest { method, path, headers: HeaderPair[], body, clientIp, origin }` 的内核类型（Node 运行时与 Workers 运行时各自构造它）。
- 客户端 IP：Workers 用 `request.headers.get("CF-Connecting-IP")`（CF 自己写的，不可伪造）；Node 版保持 `client-ip.ts`（直连或 `--trust-proxy`）。

### 2.5 客户端 WebSocket（DO 内）

Node 版 `ws-downstream.ts` 只跟踪帧边界、字节透传。Workers 里客户端 WS 由运行时终止（拿到的是消息，不是帧），所以内核要补 `ws/frames.ts`：

- 下行（core → 客户端）：从隧道流的字节里切 WS 帧（服务端帧不带掩码），合并分片，`ws.send(payload)`；`close` 帧 → `ws.close(code, reason)`；`ping` / `pong` 控制帧吞掉（CF 边缘与运行时自己维持 ping）。
- 上行（客户端 → core）：`webSocketMessage` 的每条消息打成一帧（客户端帧必须带 4 字节掩码，RFC 6455 §5.1，否则 core 的 WS 服务端会拒），写进隧道流；`webSocketClose` → 发 `close` 帧并结束流。
- 升级：把升级请求序列化进新流 → 解析 core 的 `101`（复用 `http1/response.ts`）→ 成功才 `ctx.acceptWebSocket(server, ["client", `acct:${sub}`])`（tag 便于按账号 / 访客前缀 `disconnect`，每条 WS 最多 10 个 tag）；core 答非 101 → 用同样的状态码拒绝客户端。
- 30 s ping / 60 s 空闲断：运行时不提供向客户端发 ping 控制帧的 API，改为依赖 CF 边缘的空闲断连 + `alarm()` 顺带检查 `lastClientMs`（`serializeAttachment` 里记），> 120 s 无字节就关 4000。契约 §7 的「每 30 秒 ping、60 秒断」改成「中继维持连接活性；客户端 120 秒内无字节可能被断开」——两版都满足。
- 并发上限：`edge.concurrent.acct` 在 `SourceDO` 内按 `(sub)` 计——语义从「账号在整个中继」变成「账号在这个源」（Node 版 personal 其实也只有一个节点，差别只在多源同账号时更宽）；DO 自身上限 32,768 条 WS，远高于 256。

### 2.6 存储：DO SQLite

**ControlDO**（`control-store-do-sqlite`，实现 `ControlPorts` + `KeyStore`）：

```sql
-- 账本：与 core / cloud 一样的纪律：编号连续、SHA-384 记录、未知 / 损坏拒绝服务（答 503 `storage_unavailable`），绝不自动重建。
CREATE TABLE relay_migrations (id INTEGER PRIMARY KEY, name TEXT NOT NULL, sha384 TEXT NOT NULL, applied_at_ms INTEGER NOT NULL);
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, issuer TEXT NOT NULL, allow_guests INTEGER NOT NULL, trusted_origins_json TEXT NOT NULL, rate_limits_json TEXT NOT NULL DEFAULT '{}', revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE accounts (tenant_id TEXT NOT NULL, account_id TEXT NOT NULL, name TEXT NOT NULL, password_json TEXT NOT NULL, failures INTEGER NOT NULL, locked_until_ms INTEGER NOT NULL, last_failure_at_ms INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL, PRIMARY KEY (tenant_id, account_id));
CREATE UNIQUE INDEX accounts_name ON accounts (tenant_id, name);
CREATE TABLE signing_keys (tenant_id TEXT NOT NULL, kid TEXT NOT NULL, private_key_pkcs8_enc TEXT NOT NULL, public_jwk_json TEXT NOT NULL, active_from_ms INTEGER NOT NULL, retired_at_ms INTEGER, PRIMARY KEY (tenant_id, kid));
CREATE TABLE sources (tenant_id, source_id, name, kind, public_jwk_json, core_version, capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json, PRIMARY KEY (tenant_id, source_id));
CREATE TABLE registration_tokens (tenant_id, token_hash, account_id, expires_at_ms, PRIMARY KEY (tenant_id, token_hash));
CREATE TABLE devices (tenant_id, device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms, PRIMARY KEY (tenant_id, device_id));
CREATE INDEX devices_subject ON devices (tenant_id, subject);
CREATE TABLE links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash, max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json, PRIMARY KEY (tenant_id, link_id));
```

- 列与 `ports.ts` 的记录一一对应（camelCase ↔ snake_case），`update<T>` 类方法 = `ctx.storage.transactionSync` 里「读行 → 在副本上跑 `fn` → 变了才写」，与状态文件实现的「不改不写」语义一致。
- 签名私钥列 `private_key_pkcs8_enc`：用 Worker secret `RELAY_MASTER_KEY`（32 字节 hex）AES-256-GCM 包一层再落盘。DO 存储本身静态加密，但多一层让「导出 / 迁移 / 误配」时私钥不以明文出现，与 cloud 的 `ARMADRA_CLOUD_MASTER_KEY` 同一思路。丢了 master key = 换签名钥（所有客户端重新登录），写进 runbook。
- 大小：个人中转一个租户几十行；Free 套餐 DO SQLite 每账号 5 GB、每天 500 万行读 / 10 万行写，绑定在热路径上的只有 `devices.touch`（批量、每 `LAST_SEEN_FLUSH_MS` 一次）。
- **SourceDO** 的 SQLite 只存可丢但需要跨休眠的东西：`tunnel_state`（1 行快照）、`revoked_jti (jti, exp_ms)`、`media_tickets (ticket_hash, source_id, path, subject, jti, issued_at_ms, last_used_at_ms)`、`counters (kind, key, window_start_ms, count)`。都带过期，alarm 顺手清扫。

### 2.7 令牌、JWKS、撤销、限流

| 项                                                                                                     | Node 版                                                | Workers 版                                                                                                                                                              |
| ------------------------------------------------------------------------------------------------------ | ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| JWKS                                                                                                   | personal：自己的签名钥；saas：从 cloud issuer 拉、缓存 | `SourceDO` 向 `ControlDO` RPC `jwks()`（缓存 60 s，`kid` 未命中立即重取）；saas 模式改 `fetch(<issuer>/.well-known/jwks.json)`，同样缓存                                |
| 验签                                                                                                   | `node:crypto verify("ed25519")`                        | 同一段代码（`nodejs_compat` 原生支持），或 WebCrypto `Ed25519`；K1 把 `auth/` 改成接受 `CryptoKey                                                                       | KeyObject`的`Verifier` 接口，两边各给一个实现 |
| `aud` / `exp` / `jti` / `src` 校验                                                                     | `auth/relay-token.ts`                                  | 逐字相同（AGENTS 的 P0 红线）                                                                                                                                           |
| 撤销                                                                                                   | 内存表 + personal 的「`jti` 编码源 / 设备」判断        | `SourceDO.revoked_jti` + 同一个编码判断（查 `ControlDO` 的源 / 设备是否已撤，RPC，结果缓存 10 s）                                                                       |
| `tunnel.connect.src` 10 / 分                                                                           | 内存桶                                                 | `SourceDO` 内存桶（休眠丢失即放宽，可接受）                                                                                                                             |
| `edge.connect.token` / `.acct`                                                                         | 内存桶                                                 | `SourceDO` 内存桶（按源）                                                                                                                                               |
| `edge.connect.ip` 60 / 分、`edge.preflight.ip` 1200 / 分、`login.ip` 20 / 分、`link.accept.ip` 20 / 分 | 内存桶                                                 | Worker 入口的 **Rate Limiting 绑定**（`[[ratelimits]]`，`simple.limit / period` 只支持 10 或 60 秒，按 CF 节点各自计数、最终一致）+ 区域级 Rate Limiting 规则兜底（§4） |
| `login.acct` 锁定（5 次翻倍、15 分钟封顶）                                                             | 状态文件 `account.failures`                            | `ControlDO.accounts` 表，同一段 `control-core/accounts.ts`                                                                                                              |

限流「按 CF 节点各自计数」意味着全球分布的攻击者能拿到 N 倍额度；兜底靠区域级 Rate Limiting 规则（全局计数）与账号锁定（`ControlDO` 内精确）。

### 2.8 personal 与 saas 两种模式在同一个 Worker 项目

`wrangler.jsonc` 的 `vars.MODE = "personal" | "saas"`：

- personal：`ControlDO` 启用，issuer = Worker 的自定义域（`https://relay.armadra.com`；staging `https://relay-staging.armadra.com`），`platform.info` 答 `mode: personal`、`relay.addressing: "path"`、`capabilities` 与 Node 版相同再加 `"challenge.turnstile"`（§4.3）；`/app/*`、`/j/*` 由 Static Assets 答。初始化：没有 `wrangler secret` 里的 `RELAY_PERSONAL_ACCOUNT` / `RELAY_PERSONAL_PASSWORD` 时 `/v1/*` 答 503 `not_initialized`；第一次启动在 `ControlDO` 里建账号后把口令 secret 删掉（runbook 步骤）。
- saas：`ControlDO` 不启用；`RELAY_CLOUD_ISSUER` 指向 49 服务器上的 cloud；`addressing: "host"`（`<id>.src.<域>` 要 CF 上的泛域名记录 + Workers 自定义域的通配支持，或改用路径寻址 `relay.<域>/s/<id>/`——saas 的子域隔离在前一份设计 Q1 里选了子域，CF 泛域名自定义域目前要在 DNS 里配 `*.src` 的 CNAME 到 Worker 路由，K3 验证）。

### 2.9 Workers 不支持 / 不该用的 Node API 与替换

| Node API（现用位置）                                                                                                               | Workers 状况                                                            | 替换                                                                                                                |
| ---------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `node:http` `request` + `createConnection`（`edge/server.ts`）                                                                     | `node:http` 客户端在 `nodejs_compat` 下存在，但不能挂到自定义 socket    | 内核自带 HTTP/1.1 编解码（§2.4）；Node 运行时也改用它（去掉对 `http.request` 的依赖，两边同一份解析器、同一组夹具） |
| `node:http` `IncomingMessage` / `ServerResponse` / `STATUS_CODES`（类型与 `http/errors.ts`）                                       | 不适用（Workers 是 `Request` / `Response`）                             | 内核类型 `EdgeRequest` / `EdgeResponse`；状态文案表自带                                                             |
| `node:stream` `Duplex` / `Writable`（`tunnel/stream.ts`、`ws-downstream.ts`）                                                      | 存在，但与 Web Streams 混用麻烦                                         | 内核改成 `Uint8Array` 队列 + 显式信用；对外暴露 `ReadableStream` / `WritableStream`（Web Streams 两边都有）         |
| `ws` `WebSocketServer`（`tunnel/server.ts`、`edge/server.ts`、`personal/stream.ts`）                                               | 不能用（Workers 用 `WebSocketPair` + `ctx.acceptWebSocket`）            | 运行时各自：Node 用 `ws`；Workers 用 Hibernation API；内核只看 `TunnelSocket { send, close }` 接口                  |
| `node:net` `BlockList` / `isIP`（`http/client-ip.ts`）                                                                             | `node:net` 有，但 Workers 不需要（`CF-Connecting-IP`）                  | `client-ip.ts` 移到 Node 运行时                                                                                     |
| `node:crypto` `randomBytes` / `verify` / `createPublicKey` / `scrypt` / `timingSafeEqual` / `createHash`（kernel 与 control-core） | **原生支持**（文档：除 DSA / DH 生成、argon2、ed448 / x448 外全部支持） | 不改；`relay-kernel` 与 `control-core` 允许继续 import `node:crypto`                                                |
| `Buffer`                                                                                                                           | 原生支持                                                                | 内核逐步换 `Uint8Array`（帧编解码已以 `Uint8Array` 为主），不是前置条件                                             |
| `node:fs`（状态文件、TLS、ACME、页面）                                                                                             | 存在但无持久文件系统                                                    | 这些本来就在 `apps/relay`（Node 壳），不进内核；Workers 用 DO SQLite + Static Assets + CF 证书                      |
| `node:sqlite`                                                                                                                      | **桩，不可用**                                                          | Workers 用 DO SQLite（`ctx.storage.sql`）；Node 的 SQLite 存储适配器只在 Node 运行时                                |
| `setInterval`（心跳、清扫）                                                                                                        | 可用但阻止 DO 休眠                                                      | DO 用 `alarm()`；内核把「定时」抽成 `Scheduler` 接口                                                                |
| `performance.now()`、`setTimeout`（握手超时）                                                                                      | 可用                                                                    | 握手 10 s 超时用 `alarm` 或在握手期间不休眠（握手只有几十毫秒，用 `setTimeout` 即可，握手完成即清）                 |

### 2.10 包结构

```text
packages/platform-protocol        不变（+ 错误码 bad_gateway、storage_unavailable、not_initialized、challenge_required、challenge_invalid；
                                  cloud-api 的 auth.login / links.accept 入参加可选 challenge；platform.info 的 capabilities 加 challenge.turnstile）
packages/relay-kernel             只剩纯逻辑：tunnel/{handshake,tunnel,stream,frames}、edge/{policy,cors,addressing,forward,media-tickets,read-only}、
                                  auth/、control/{types,memory}、http1/{request,response}（新）、ws/frames（新）、limits/、metrics/
                                  禁止 import：node:http、node:https、node:net、node:stream、node:fs、node:tls、node:os、ws（repo.rules.json + boundaries.test.ts）
                                  允许：node:crypto、node:buffer
packages/relay-runtime-node  NEW  ← 今天 kernel 里的 Node 部分：listen（cloud-shared/http）、ws 适配、TLS 热加载、client-ip、
                                  dispatchUpgrade、startRelay（签名不变）；apps/relay 只改 import 路径
packages/relay-runtime-workers NEW Worker 入口（router、Rate Limiting 绑定、Static Assets、/health /ready /metrics）、SourceDO、ControlDO、
                                  Hibernation 粘合、DO SQLite 的 ControlPorts 实现（control-store-do-sqlite 子目录）、DO 内迁移账本
packages/control-store-sqlite NEW Node 的 node:sqlite 端口实现（S1）；与 DO 版共享同一份 SQL 文本与迁移编号（`packages/control-store-sql/migrations/`
                                  放 SQL，两个实现各自执行；账本表名相同）
packages/control-store-postgres  （S2；若 C8-1 先做，就是把 apps/cloud/src/adapters/postgres 提升成包）
apps/relay                        Node CLI（personal / saas），装配 relay-runtime-node + control-core + {state-file | sqlite | postgres}
apps/relay-workers           NEW  wrangler 项目：wrangler.jsonc、src/index.ts（re-export runtime-workers）、assets/（构建时从 Armadra web 产物复制）、
                                  scripts/{build,deploy,rollback}.mjs、README
```

- `repo.rules.json` 新增两条 `packageDependencies`：`relay-runtime-workers` 禁 `ws` / `postgres` / `redis` / `node:fs`；`relay-kernel` 禁上面那串 Node 模块（规则检查 import 说明符，`node:` 前缀直接列）。
- `apps/relay-workers` 的 `tsconfig` 用 `@cloudflare/workers-types`；CI 用 `wrangler dev --local`（workerd）跑 V4 的端到端。依赖写精确版本，`THIRD_PARTY_NOTICES.md` 同步。

### 2.11 子域规划：`armadra.com`

现场（2026-10-10）：zone `armadra.com` 已在 CF 上 `active`（Free Website 套餐，NS 已切到 CF），本机 `wrangler 4.149.0` 已 OAuth 登录；Workers 套餐未确认（按 Workers Free 看，§4.6 给两套配置）。

| 主机                             | 用途                                                                                                                                                  | 跑在哪                                         | CF 侧接法                                                                                                    | 证书                                                                                                                                                                                                                                                                          |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `armadra.com`、`www.armadra.com` | 官网 / 下载页（可先空着答 302 到 GitHub）                                                                                                             | Pages 或静态 Worker                            | 代理（橙云）                                                                                                 | Universal SSL                                                                                                                                                                                                                                                                 |
| `relay.armadra.com`              | **CF 个人 / 公共中继**（`MODE=personal`）：issuer、`/v1/*`、`/t/v1`、源地址 `/s/<id>/…`、媒体票 `/s/<id>/_relay/m/<票>`、页面 `/app/*`、落地页 `/j/*` | Worker                                         | **Custom Domain**（CF 自动建 DNS 记录、自动签 Advanced 证书，不需要 ACM 订阅）                               | 自动                                                                                                                                                                                                                                                                          |
| `relay-staging.armadra.com`      | 同上的 staging（`[env.staging]`）                                                                                                                     | Worker                                         | Custom Domain                                                                                                | 自动                                                                                                                                                                                                                                                                          |
| `cn.relay.armadra.com`           | **49 服务器的 Node 中继**（大陆直达；替代 `armadra-personal.example.org`）                                                                            | 49（OpenResty 终止 TLS）                       | **DNS only（灰云）**，A 记录指 49                                                                            | 1Panel HTTP-01 签发（不经 CF；二级子域不在 Universal 范围内，但灰云记录本来就不用 CF 证书）                                                                                                                                                                                   |
| `app.armadra.com`                | SaaS 控制面（issuer = 页面来源，单来源，`__Host-` Cookie）                                                                                            | M1 在 49（灰云）；日后 Worker（Custom Domain） | M1 灰云；迁 CF 时改 Custom Domain                                                                            | 1Panel → 自动                                                                                                                                                                                                                                                                 |
| `edge.armadra.com`               | SaaS 的 CF 中继节点（`MODE=saas`）：`/t/v1` 隧道入口；控制面答给 core 的节点地址                                                                      | Worker                                         | Custom Domain                                                                                                | 自动                                                                                                                                                                                                                                                                          |
| `*.src.armadra.com`              | SaaS 的源地址（`hostAddressing`，`RELAY_DOMAIN=armadra.com`，代码里的 `.src.` 字面不改）；媒体票同主机                                                | 同一个 Worker                                  | **Route** `*.src.armadra.com/*` + 代理的通配 DNS 记录 `*.src`（AAAA `100::` 占位即可；通配记录全套餐可代理） | **需要 Advanced Certificate Manager**：Universal SSL 只覆盖 apex 与一级（`*.armadra.com`），不覆盖 `*.src.armadra.com`；ACM 是按 zone 的付费附加（官方页不列价，面板显示，历来 $10 / 月），一张证书列 `*.src.armadra.com` + `src.armadra.com`。**SaaS 上线时才订**，M1 不需要 |
| `status.armadra.com`             | uptime-kuma 状态页                                                                                                                                    | 49                                             | 代理或灰云皆可（建议代理，顺便吃 DDoS 防护）                                                                 | Universal                                                                                                                                                                                                                                                                     |
| `dl.armadra.com`（可选）         | 安装包 / 自动更新产物                                                                                                                                 | R2 + Custom Domain                             | Custom Domain                                                                                                | 自动                                                                                                                                                                                                                                                                          |

- **为什么源地址不用一级子域**：`<id>.armadra.com` 能白嫖 Universal 的 `*.armadra.com`，但会把整个 zone 的子域空间交给中继，任何未来的 `foo.armadra.com` 都与 32 位 hex 竞争；`src` 这一层隔离值 ACM 的月费。个人中继坚持路径寻址（`/s/<id>/`），不需要通配证书。
- **媒体票不需要单独域名**：票路径就在源地址之下（契约 §12），浏览器取媒体时与源同来源，没有 Cookie 参与；单独域名只会多一张证书。
- **Workers 路由规则**：Custom Domain 是精确主机名、吃全部路径；`*.src.armadra.com/*` 用 Route（通配只能在最左）。两者可以挂同一个 Worker（saas 的 Worker 同时有 `edge.armadra.com` 的 Custom Domain 与 `*.src` 的 Route）。
- **example.org 的去留**：`armadra-personal.example.org` 今天 CNAME 到另一家 CDN、公网 443 尚未接入、还没给任何客户端登记（`deploy-relay-49.md`），所以**不存在迁移成本**——直接把 49 的中继以 `cn.relay.armadra.com` 上线（`--host cn.relay.armadra.com --public-origin https://cn.relay.armadra.com`，状态文件重新 init 或改 `issuer` 字段后 `rotate-key`），`example.org` 只作内部冒烟与过渡，证书到期不续、记录删除。若用户希望保留 example.org 作「国内测试域」，就让它一直是另一个独立的 issuer（客户端把它当第三条中继登记），互不影响。
- **DNS 记录清单（K4 的 Terraform 管）**：`relay`（由 Custom Domain 自动建，Terraform 只 `import`）、`relay-staging`（同）、`cn.relay` A → 49 灰云、`app` A → 49 灰云、`status` A → 49、`*.src` AAAA `100::` 橙云（SaaS 时）、`edge`（Custom Domain）、`_dmarc` / SPF（若 SaaS 发邮件用 `@armadra.com`）。

## §3 大陆访问的现实与最优配置

| 事实                                                                                                      | 来源                                                                        |
| --------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| China Network 是单独订阅，只对 Enterprise；每个顶级域要 ICP 备案 / 许可；由合作方运营；不是所有产品可用   | https://developers.cloudflare.com/china-network/                            |
| 普通套餐：WebSocket 全套餐支持；边缘在无数据一段时间后关连接（Enterprise 可定制时长）；官方建议客户端心跳 | https://developers.cloudflare.com/network/websockets/                       |
| 代理连接超时：空闲 900 s、读 125 s（Enterprise 可调）、写 30 s                                            | https://developers.cloudflare.com/fundamentals/reference/connection-limits/ |
| Argo 与 WebSocket 不兼容                                                                                  | 同上 WebSockets 页                                                          |

建议（用户已决定上 CF，这里只讲怎么用得最好）：

1. **两条路并存**：49 服务器的 Node 中继给大陆直达（已在部署，前一份设计 §5），CF 中继给海外 / 漫游 / 对外分享。同一台主机在桌面「远程访问」里登记两个中转账号；手机与第二台桌面经「一源多路」自动选路（直连探测 → 首选中继 → 其余按 `lastOkAtMs`）。分享链接按场景选签发方：给大陆的人发 49 的链接，给海外的发 CF 的。
2. **域名与 DNS**：CF 中继用 `relay.armadra.com`（Worker Custom Domain，自动证书）；49 的 Node 中继用 `cn.relay.armadra.com`，记录在 CF 上但 **DNS only（灰云）**，避免被代理后反而绕路（§2.11）。
3. **心跳**：隧道 45 s（§2.3）、客户端 WS 由 CF 边缘与 DO alarm 维持；core 看门狗 90 s。大陆到 CF 的 NAT 表项有时 60 s 失效——45 s 低于它。
4. **Argo Smart Routing 不买**：对 WebSocket 不生效，而中继的全部长连接都是 WebSocket；`/v1/*` 短请求省的几十毫秒不值月费。
5. **HTTP/3 开着无妨**（浏览器对 `/v1`、页面产物有益），WebSocket 仍走 HTTP/1.1。
6. **手机端**：iOS App 的 `capacitor://localhost` 来源对 CF 中继照样跨源，CORS 预检由 `SourceDO` 答（与 Node 版同一段逻辑）；原生 App 不会被 WAF 挑战（我们不开 Bot Fight Mode，见 §4）。

## §4 CF 防护与成本控制

### 4.1 免费与付费分别能做到什么

| 项                                                     | Free                                          | Pro（$20 / 月 / 域）                                                                | Business / Enterprise                    | 来源                                                                                         |
| ------------------------------------------------------ | --------------------------------------------- | ----------------------------------------------------------------------------------- | ---------------------------------------- | -------------------------------------------------------------------------------------------- |
| HTTP DDoS 托管规则                                     | 有，1 条无表达式的覆盖（可调动作 / 灵敏度）   | 同                                                                                  | Enterprise 高级版 10 条带表达式          | https://developers.cloudflare.com/ddos-protection/managed-rulesets/http/                     |
| WAF 托管规则                                           | Free Managed Ruleset                          | + Cloudflare Managed + OWASP Core                                                   | + Sensitive Data Detection（Ent）        | https://developers.cloudflare.com/waf/managed-rules/                                         |
| 区域级限速规则                                         | **1 条**，只按 IP，10 s 窗、10 s 封           | 2 条，1 分钟窗、最长封 1 小时                                                       | 5 条 / 100 条，按头、Cookie、JSON 字段等 | https://developers.cloudflare.com/waf/rate-limiting-rules/                                   |
| Bot                                                    | Bot Fight Mode（**不开**）                    | Super Bot Fight Mode + Skip 规则                                                    | Bot Management                           | https://developers.cloudflare.com/bots/get-started/bot-fight-mode/ 、…/super-bot-fight-mode/ |
| Turnstile                                              | 有                                            | 有                                                                                  | Enterprise 多 `ephemeral_id` 等          | https://developers.cloudflare.com/turnstile/get-started/server-side-validation/              |
| mTLS（CF 托管 CA）                                     | 有（所有套餐）                                | 有                                                                                  | Ent 可自带 CA                            | https://developers.cloudflare.com/api-shield/security/mtls/                                  |
| Workers Rate Limiting 绑定                             | 有                                            | 有                                                                                  | 有                                       | https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/                  |
| Workers 请求                                           | 10 万 / 天硬封顶、10 ms CPU                   | Workers Paid $5 / 月：1,000 万请求 / 月内含、3,000 万 CPU-ms，之后 $0.30 / 百万请求 | 同 Paid                                  | https://developers.cloudflare.com/workers/platform/pricing/ 、…/limits/                      |
| DO                                                     | 10 万请求 / 天、13,000 GB-s / 天、5 GB SQLite | 100 万请求、40 万 GB-s / 月内含；超出 $0.15 / 百万请求、$12.5 / 百万 GB-s           | 同 Paid                                  | https://developers.cloudflare.com/durable-objects/platform/pricing/                          |
| 请求体上限                                             | 100 MB                                        | 100 MB                                                                              | 200 MB / 5 GB                            | https://developers.cloudflare.com/workers/platform/limits/                                   |
| 预算告警（账号级美元阈值，邮件，次日生效，**不封顶**） | PAYG 账号有，2026-06-15 起缺省开启            | 同                                                                                  | Enterprise 合同账号无                    | https://developers.cloudflare.com/billing/manage/budget-alerts/                              |
| 用量通知（单产品指标阈值）                             | 无                                            | 有                                                                                  | 有                                       | https://developers.cloudflare.com/notifications/notification-available/                      |

结论：**zone 留 Free（已是），Workers 先 Free 跑通、第三台主机起升 Paid**（两套配置见 §4.6）。Free zone 的 1 条限速规则给「登录 + 加入链接」合用；路径细分的限速在 Worker 里做（Rate Limiting 绑定免费、不占区域规则）。Pro 的增量（托管 WAF、OWASP、2 条限速、SBFM）对中继这种「全是 JSON API + WebSocket」的站点收益有限，先不买；流量或攻击真来了再升。

### 4.2 Worker 内的闸（与套餐无关，两种中继都有）

| 闸                | 位置                                              | 规则                                                                                                                                                                                                                                                                          |
| ----------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 路径限速（按 IP） | Worker 入口，Rate Limiting 绑定，命名空间按路径分 | `/v1/auth/login` 20 / 60 s；`/v1/links/*/accept` 20 / 60 s；`/v1/sources/registration-token` 与 `/v1/sources/register` 10 / 60 s；`/t/v1` 升级 10 / 60 s（再加 `SourceDO` 内按源 10 / 分）；`/_relay/media-tickets` 60 / 60 s；无令牌的源地址请求 60 / 60 s；预检 1200 / 60 s |
| 消息大小          | `SourceDO`                                        | 隧道帧 > `maxFrameBytes + 16`（1 MiB）→ 关 4400（DO 收消息上限 32 MiB，内核自己先挡）；客户端 WS 单条消息 > 1 MiB → 关 1009；媒体票签票体 > 4 KiB → 400                                                                                                                       |
| 并发              | `SourceDO`                                        | 每源 256 条流；每账号 256 条 WS（§2.5）；每 DO 硬上限 32,768 条 WS                                                                                                                                                                                                            |
| CPU               | `wrangler.jsonc limits.cpu_ms = 5000`             | 单次调用 CPU 上限，防失控循环（转发是 I/O 型，正常远低于此）                                                                                                                                                                                                                  |
| 日配额            | `ControlDO.counters`（租户级）                    | `RELAY_DAILY_REQUEST_BUDGET`（缺省 200 万）与 `RELAY_DAILY_TUNNEL_BYTES_BUDGET`（缺省 50 GB）：Worker 每请求原子累加（`SourceDO` 每 1 MiB 汇报一次给 `ControlDO`，减少 RPC），超了对非身份域路径答 429 `limit_reached`，`/metrics` 暴露 `relay_budget_used`                   |
| 令牌即时撤销      | `ControlDO → SourceDO.revoke`                     | 与 Node 版同一语义（撤源 / 撤链接 / 登出 / 改口令即时生效）                                                                                                                                                                                                                   |

### 4.3 Turnstile（登录与加入链接）

- 协议：`auth.login` 与 `links.accept` 入参加可选 `challenge: { provider: "turnstile", token }`；`platform.info.capabilities` 含 `"challenge.turnstile"` 且 `platform.info.challenge = { provider: "turnstile", siteKey }` 时客户端必须带；中继校验：`POST https://challenges.cloudflare.com/turnstile/v0/siteverify`（`secret`、`response`、`remoteip`、`idempotency_key`），令牌 300 s 有效、一次性；失败答 `400 challenge_invalid`、缺失答 `400 challenge_required`（都登记进错误码表，cloud-api **§16**「挑战」）。Node 中继也实现同一段（`--challenge turnstile --turnstile-secret-file`），这样两版行为一致，只是 Node 版缺省关。
- 客户端：页面（`/app/*`、`/j/<linkId>` 落地页、桌面与服务器壳的「中转账号」登录框）嵌 Turnstile 组件（隐式模式，无交互时不显示；需要时在表单上方渲染，复用 `ui` 的卡片容器，不新造组件）；**原生 App**：Turnstile 不能原生跑，官方做法是 WebView 载一个带组件的页面——中继托管一个 `/app/challenge`（同源页，只有组件，`postMessage` 把 token 发回 App），手机端在登录 / 加入时弹一个 `Sheet` 装 WebView（WebView 需允许 `challenges.cloudflare.com`、`about:blank`、`about:srcdoc` 与中继域，User-Agent 不能中途变）。来源 https://developers.cloudflare.com/turnstile/get-started/mobile-implementation/ 。
- core 作为客户端（`sources.mount` 经 `links.accept`）：core 没有 WebView。两条路：(a) `links.accept` 的挑战只在 `platform.info.challenge.scope` 含 `"links.accept"` 时要求，personal 缺省只对 `auth.login` 要求；(b) 分享链接落地页在浏览器里做完挑战后把 `assertion` 交给 App（现有深链 `armadra://join?…` 流程已经是「浏览器落地 → App」）。先按 (a)，登录要挑战、链接接受不要（链接本身带 128 位秘密，爆破面是 `linkId + secret`，限速已够）。

### 4.4 mTLS 还是令牌校验

隧道已有「隧道令牌（JWS，1 小时）+ 握手里源私钥签 nonce」两道门，边缘有中继令牌。mTLS 的收益是把无令牌的扫描挡在 TLS 握手，不耗 Worker 请求数——这是 Workers Paid 上真正的省钱点。做法：CF 托管 CA 给每台主机签一张客户端证书（API 创建、10 年），core 的隧道客户端在 `cloud_registrations` 里存证书 + 私钥（新列，Armadra 契约 §60 追加 `clientCert`），`relay.<域>` 的 `/t/v1` 开 mTLS 且 WAF 规则 `not cf.tls_client_auth.cert_verified and http.request.uri.path eq "/t/v1"` → block。客户端边缘（浏览器、手机）不能上 mTLS。**第一版不做**，列为 K4 后的可选项（§9 Q4）：签发与分发证书要走 CF API，而个人中转的登记流程今天只有一个注册令牌。

### 4.5 IaC 与回滚

- `apps/relay-workers/wrangler.jsonc`：`compatibility_flags: ["nodejs_compat"]`、`durable_objects.bindings`（`SOURCE` → `SourceDO`，`CONTROL` → `ControlDO`）、`migrations: [{ tag: "v1", new_sqlite_classes: ["SourceDO", "ControlDO"] }]`、`ratelimits`（每条路径一个 `namespace_id`）、`assets: { directory: "./assets", not_found_handling: "single-page-application" }`、`limits.cpu_ms`、`observability.logs.enabled`（日志同样不记路径 / 头 / 令牌）、`routes: [{ pattern: "relay.<域>", custom_domain: true }]`、`[env.staging]` 另一套域与 DO 命名空间；secrets（`RELAY_MASTER_KEY`、`RELAY_TURNSTILE_SECRET`、`RELAY_METRICS_TOKEN`、初始化用的账号 / 口令）只经 `wrangler secret put`，仓库里只有 `secrets.required` 的名字。来源 https://developers.cloudflare.com/workers/wrangler/configuration/ 。
- 区域设置（WAF 自定义规则、限速规则、Bot 开关、DNS、mTLS 主机）：**Terraform**（`deploy/cloudflare/`，`cloudflare` provider，state 放本地或 R2，账号 / 区域 id 走变量，API Token 走环境变量），或等价的 `tools/cf/zone-apply.mjs`（幂等地按一份 `zone.rules.json` 调 API）。推荐 Terraform：有 plan / diff、能回滚到上一份 state。
- 回滚：Worker 代码 `wrangler rollback`（保留 100 个版本；**跨 DO 类生命周期变更不能回滚**，所以 DO 类只在首版建一次，之后的 schema 变化全部走对象内的 `relay_migrations` 账本，用「加列 / 加表、不改已发布迁移」的纪律与 core 相同）；区域规则 `terraform apply` 上一份 state；DO 数据不随代码回滚变化，老代码遇到新 schema 要能读（迁移只追加）。来源 https://developers.cloudflare.com/workers/configuration/versions-and-deployments/rollbacks/ 、https://developers.cloudflare.com/durable-objects/reference/durable-objects-migrations/ 。
- 「拉闸」：`scripts/kill-switch.mjs` 把 `relay.<域>` 路由切到一个只答 503 `maintenance` 的静态 Worker（预先部署好），账单失控或被打时一条命令止血；恢复就是切回。

### 4.6 两套可行配置：Workers Free 与 Workers Paid（zone 都是 Free Website）

现场：zone `armadra.com` 是 **Free Website**；Workers 套餐未确认，先按 **Workers Free** 设计，升级 Paid 只改配额参数，不改代码。

| 项                   | 配置 F（Workers Free + Free zone）                                                                                                       | 配置 P（Workers Paid $5 / 月 + Free zone）          |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| 能承载的常驻隧道     | **2–3 台主机**（§2.3 估算：每条空闲隧道每天 ≈ 800 次请求 + ≈ 3,400 GB-s；Free 每天 10 万请求 / 13,000 GB-s；超额**直接失败**，不是账单） | 内含额度约 3 条常驻 + 活跃使用；之后每条每月约 $1.6 |
| 日请求               | 10 万 / 天硬封顶（含 DO 请求、alarm；WS 入站消息 20:1 折算）                                                                             | 1,000 万 / 月内含，之后 $0.30 / 百万                |
| Worker CPU           | 10 ms / 次（转发是 I/O 型，单次 CPU 远低于此；大文件上传切块时每块一次事件，仍在内）                                                     | 30 s 缺省，`limits.cpu_ms` 下调到 5,000             |
| DO SQLite            | 5 GB、每天 500 万行读 / 10 万行写（`devices.touch` 批量写，几台设备一天几百行）                                                          | 5 GB-month 内含                                     |
| `alarm` 周期         | **120 s**（每条隧道每天 720 次 alarm；判死最长 2 × 45 s + 120 s ≈ 3.5 分钟）                                                             | 60 s                                                |
| 心跳 `heartbeatMs`   | 45 s（§9 Q1）                                                                                                                            | 45 s                                                |
| 静态资源（`/app/*`） | 免费、不计请求                                                                                                                           | 同                                                  |
| Rate Limiting 绑定   | 可用                                                                                                                                     | 可用                                                |
| 区域限速规则         | 1 条：`/v1/auth/login` 与 `/v1/links/*/accept` 合写一条（按 IP，10 s 窗、10 s 封）                                                       | 同（zone 不升级就还是 1 条）                        |
| WAF                  | 只有 Free Managed Ruleset；**Cloudflare Managed / OWASP 不可用**                                                                         | 同                                                  |
| DDoS L7              | 全套餐                                                                                                                                   | 同                                                  |
| Bot                  | 不开 Bot Fight Mode                                                                                                                      | 同                                                  |
| mTLS                 | 可用（CF 托管 CA）                                                                                                                       | 同                                                  |
| Turnstile            | 可用                                                                                                                                     | 同                                                  |
| 预算告警             | Free 没有付费用量，不需要                                                                                                                | PAYG 缺省开启，阈值自设                             |
| 日配额自限（§4.2）   | `RELAY_DAILY_REQUEST_BUDGET = 80_000`（留 20% 给隧道心跳与 alarm，超了对非身份域路径答 429，避免整天失败）                               | `2_000_000`                                         |
| 升级触发             | 第三台主机、或连续 3 天 `/metrics` 的 `relay_budget_used` > 70%                                                                          | —                                                   |

Workers Free 的 10 ms CPU 是每次**事件**（HTTP 请求、WS 消息、alarm）的 CPU 时间，不是墙钟；DO 自己的 CPU 上限按 DO 限制页是 30 s。K3 要在 workerd 本地跑 `--cpu-limit` 等价的断言：转发 1 MiB 帧的 CPU < 5 ms。

### 4.7 `wrangler` 操作清单（用户在本机执行；agent 不接触令牌）

```text
wrangler whoami                                    # 已登录（OAuth）；确认 account 与 zone
wrangler deploy --env staging                      # 先 staging：relay-staging.armadra.com
wrangler secret put RELAY_MASTER_KEY --env staging # 32 字节 hex；其余 secret 同法
wrangler tail --env staging --format pretty        # 验证时看日志（不含路径 / 头 / 令牌）
wrangler deploy                                    # 生产：relay.armadra.com
wrangler rollback                                  # 回滚到上一个版本（跨 DO 类生命周期变更不可）
wrangler versions list                             # 查版本
```

## §5 Node 中继的存储可选与 Redis 可选

### 5.1 三种存储

| `--store`                | 适用                      | 实现                                                                                     | 多写者                                                                                                                                                                      | 备份                                                 |
| ------------------------ | ------------------------- | ---------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `file`（现状）           | 已部署的个人中转          | `state.json` + 目录锁 + 原子改名                                                         | 文件锁 + revision                                                                                                                                                           | `tar` 数据目录                                       |
| `sqlite`（**新装缺省**） | 单节点个人 / 小团队       | `node:sqlite` `DatabaseSync`，`<data-dir>/relay.db`，WAL，`busy_timeout 5000`，文件 0600 | 同一文件多进程（`serve` 与 `passwd` / `rotate-key` / `links revoke`）由 SQLite 自己的锁保证，去掉 `state.lock` 与「每秒看一眼」：`serve` 改为每次读走库（量小，读走页缓存） | `VACUUM INTO '<path>'`（在线、一致）；或停服后拷文件 |
| `postgres`               | 多节点、SaaS 的中继控制面 | `control-store-postgres`（与 cloud 共用）；`--database-url`                              | PG                                                                                                                                                                          | 现有 PG 备份                                         |

- SQL 文本与迁移编号在 `packages/control-store-sql/migrations/00NN_*.sql`，SQLite（Node）、DO SQLite 共用同一份（PG 方言不同，单独目录但编号对齐）；账本 `relay_migrations`，`migrations.lock` 增加这个目录（`repo.rules.json migrations.sources` 加一项）；未知 / 损坏拒绝启动、不自动重建（P0 红线不变）。
- `--store sqlite` 的 `node:sqlite`：Node 22.13 起无需 flag，仓库 `engines >=22.18` 已覆盖；Docker 镜像基于 Node 22 LTS 即可；`DatabaseSync` 是同步 API，与端口「`update` 原子读-改-写」天然契合（`BEGIN IMMEDIATE … COMMIT`）。
- `personal` 模式与 PG 的红线：AGENTS 说 personal 不得引入 PG / Redis **运行时依赖**。方案：`--store postgres` 走动态 `import("@armadra/control-store-postgres")`，`apps/relay` 的 `dependencies` 里用 `optionalDependencies`，`boundaries.test.ts` 扫静态 import 图不含 `postgres`；AGENTS 这条改写成「personal 的缺省装配（`file` / `sqlite`）不得静态依赖 PG / Redis；`--store postgres` 与多节点是显式选项」（§9 Q3）。

### 5.2 Redis 改可选：单节点进程内，多节点再接

| 功能                 | 单节点（缺省）               | 多节点：Redis                     | 多节点：PG 替代                                                                         |
| -------------------- | ---------------------------- | --------------------------------- | --------------------------------------------------------------------------------------- |
| 路由 `route:src:*`   | `MemoryControlPlane`（现有） | `SET … EX` + `GET`（R3）          | `relay_routes (source_id PK, node, tunnel_id, expires_at_ms)` + `LISTEN relay_presence` |
| 在线事件             | 进程内事件总线               | pub/sub                           | `NOTIFY relay_presence, '<json>'`（8000 字节内）                                        |
| 限流桶               | 内存                         | `INCR` + `EXPIRE`                 | 内存（按节点近似；登录锁定在 `accounts` 表精确）                                        |
| 撤销 `revoked:jti`   | 内存 + 状态判断              | `SET … PXAT`                      | `revoked_tokens (jti PK, exp_ms)` + `NOTIFY relay_revoked` 让各节点清本地缓存           |
| 节点命令 `relay:ctl` | 进程内                       | pub/sub                           | `NOTIFY relay_ctl`                                                                      |
| 媒体票               | 内存                         | 仍只在签票节点（契约 §12 已声明） | 同                                                                                      |

`ControlPlane` 接口不变，多一个实现 `control/postgres.ts`（放 `relay-runtime-node`，用 `cloud-shared/db`），由 `--control memory|redis|postgres` 选；缺省 `memory`，只有一个节点时就是正确的。`LISTEN/NOTIFY` 要一条常驻连接（`postgres` 驱动的 `listen()`），断线重连后全量重读路由表。

### 5.3 从状态文件迁到 SQLite

`armadra-relay personal migrate --to sqlite [--data-dir …]`：

1. 拒绝条件：`relay.db` 已存在；`state.json` 版本未知 / schema 不符；`serve` 正在跑（目录锁被持有）。
2. 在目录锁内读 `state.json` → 开 `relay.db` → 跑迁移到最新 → 一个事务写入全部行（账号、签名钥私钥照旧以 PKCS#8 base64 存，**不**在 Node 版加 master key 包裹——与现状一致，文件权限 0600 保护）→ `COMMIT` → `state.json` 改名 `state.json.migrated-<时间戳>`。
3. 之后 `serve` 看到 `relay.db` 就用 `sqlite`；看到只有 `state.json` 且没写 `--store` 就继续 `file`（老部署零改动）；两者都在 → 拒绝启动并提示。
4. 反向（`--to file`）也提供，便于回退：从库导出成 schema v1 的 `state.json`。
5. 测试：夹具 `state.json` → 迁移 → 逐表比对；再 `--to file` 回来字节级等价（`revision` 除外）。

### 5.4 缺省即安全

- `--tls plain` 时只绑回环或 `--trust-proxy` 网段内地址（R6 已合并）；缺省自签 + 指纹钉扎不变。
- 口令：策略与常见口令表已有；`--password-file` 读后不再读取；`sqlite` 模式 `relay.db` 0600、目录 0700。
- 令牌寿命：访问 15 分钟、刷新 30 天旋转、断言 5 分钟、中继 / 隧道 1 小时（不变）。
- 限流缺省开启（`DEFAULT_LIMIT_RULES`），`settings.rateLimits` 只允许在上限内调。
- 新增 `personal doctor`：检查监听地址是否公网 + `plain`、数据目录权限、证书剩余天数、`relay.db` 完整性（`PRAGMA integrity_check`）、备份目录是否存在。

## §6 与 SaaS 的关系：控制面放哪

| 方案                                                                         | 优点                                                                                                    | 代价                                                                                                                            | 结论        |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- | ----------- |
| A：控制面留 49（PG + Redis + 进程内 jobs + SMTP），数据面（saas relay）上 CF | 前一份设计的 C8-x 原样推进；PG 是真相；jobs 的 `SKIP LOCKED` + `LISTEN/NOTIFY`、nodemailer 都是 Node 的 | 控制面单点在一台 2 vCPU 机器；大陆用户访问控制面快、海外慢（但控制面调用少）                                                    | **M1 采用** |
| B：控制面上 Workers + Hyperdrive 连 49 的 PG                                 | 全球就近；Hyperdrive Free 每天 10 万次查询、Paid 不限；连接池与缓存免费                                 | jobs 要改 Cron Triggers + Queues；邮件要 HTTP API；passkey / TOTP 可做；PG 仍在 49，每次查询跨洋一跳（Hyperdrive 缓存只对只读） | M2 评估     |
| C：控制面上 Workers + D1                                                     | 全在 CF                                                                                                 | D1 单库 10 GB、单查询 30 s、无 `LISTEN/NOTIFY`；与 Node 的 PG 适配器是两套 SQL 方言；迁移纪律要再写一套                         | 不采用      |

分工（M1）：

```text
49 服务器   armadra-cloud（控制面，PG + 独立 Redis）、armadra-relay personal（大陆直达）、日后 armadra-relay saas（可选第二节点）
Cloudflare  armadra-relay-workers：MODE=personal（个人海外中继，独立 issuer）与 MODE=saas（RELAY_CLOUD_ISSUER = 49 的 cloud，JWKS 从它拉）
            ——两个 Worker 部署（两个 env），同一份代码
```

saas 中继多节点（49 的 Node 节点 + CF 节点）时，`heartbeatHint` 的 `nodes[]` 由 cloud 按源的地域给两条，core 按 `cloud.relay.preferredNode` 排序；路由真相在 cloud（PG 或 Redis），CF 节点上线 / 下线经 `POST <issuer>/v1/relay/nodes/<node>/presence`（内部接口，中继用节点令牌，cloud-api **§17** 「中继节点回报」）告诉 cloud——这是 R3 的 Redis `relay:nodes` 在跨网络时的替代，K3 的 saas 模式先只实现这一个回报接口，Redis 方案留给同机节点。

## §7 实现包

规则沿用前一份设计 §6：一包一 worktree 一 PR，CI 绿后合并（合并提交）；cloud 仓 app 版本与协议包 lockstep；跨仓改形状先改协议包；Opus = 复杂 / 安全相关，Sonnet = 机械、范围清楚。worktree 不用 `codex` 作名字或前缀。

### 7.1 第一波（K1 单独先行；S1 可并行）

| 包                | 仓库  | worktree                           | 文件边界                                                                                                                                                                                                                                                                                                                                   | 契约 / 版本                                                                                                                      | 迁移       | 测试与验收                                                                                                                                                                                                                                                                             | 模型 | 规模 |
| ----------------- | ----- | ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- | ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---- | ---- |
| K1 内核去 Node 化 | cloud | `armadra-cloud-wt/kernel-portable` | `packages/relay-kernel/src/{tunnel,edge,auth,control,http1(新),ws(新),limits,metrics}`；Node 专有部分搬到 **新** `packages/relay-runtime-node/src/{listen,ws-server,tls,client-ip,dispatch,start-relay}.ts`；`apps/relay` 只改 import；`repo.rules.json`、各包 `boundaries.test.ts`、`knip.json`                                           | 行为零改动；协议包 `0.3.1`（加错误码 `bad_gateway`）；两 app `0.3.1`；cloud-api §7 的「每 30 秒 ping」措辞改成活性保证（不改号） | —          | 全部既有测试搬家后通过；`http1/response` 与 `ws/frames` 各一套黄金夹具（`packages/platform-protocol/fixtures/http1/`、`fixtures/ws/`，语言无关字节）；`personal-e2e`、`pnpm e2e --mode personal` 不变；边界扫描：kernel 无 `node:http` / `node:net` / `node:stream` / `node:fs` / `ws` | Opus | XL   |
| S1 SQLite 存储    | cloud | `armadra-cloud-wt/store-sqlite`    | 新 `packages/control-store-sql/migrations/0001_init.sql`、新 `packages/control-store-sqlite/src/{open,ports,keys,migrate}.ts`、`apps/relay/src/personal/{args,serve,commands}.ts`（`--store`、`migrate --to`、`doctor`）、`deploy/README.md`、`docs/runbooks/personal-storage.md`、`repo.rules.json migrations.sources`、`migrations.lock` | 无线上形状改动；两 app `0.3.2`                                                                                                   | SQL `0001` | 端口合同测试（现有 `control-core/testing/memory-ports` 的用例对 sqlite 实现再跑一遍）；多进程并发 `passwd` 与 `serve`；`migrate` 往返等价；损坏库拒绝启动；`VACUUM INTO` 备份可恢复                                                                                                    | Opus | L    |

### 7.2 第二波

| 包                  | 仓库    | worktree                            | 文件边界                                                                                                                                                                                                                                                                                                                                                         | 契约 / 版本                                                                                                                                                                                                                                                                                                        | 迁移                  | 测试与验收                                                                                                                                                                                                                                                 | 模型   | 规模 |
| ------------------- | ------- | ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ | ---- |
| K3 Workers 运行时   | cloud   | `armadra-cloud-wt/relay-workers`    | 新 `packages/relay-runtime-workers/src/{index,router,source-do,control-do,hibernation,store-do-sqlite/,budget}.ts`、新 `apps/relay-workers/{wrangler.jsonc,src/index.ts,scripts/,README.md}`、`packages/control-store-sql/migrations/`（复用 S1 的 0001）、`deploy/cloudflare/README.md`、`docs/runbooks/relay-on-cloudflare.md`、`repo.rules.json`              | cloud-api **§16**「挑战」（schema 先定，K4 实现校验）与 **§17**「中继节点地址与节点回报」（`nodes[].url` 可带 query；`relay/nodes/<node>/presence`）；`platform.info` 加 `challenge`；协议 **minor 1.2**；协议包与两 app **`0.4.x` 之后的下一个 minor**（与 SaaS 波次的 `0.4.0` / `0.5.0` 按合并顺序取号，不预占） | SQL 0001（DO 内账本） | `wrangler dev --local` 下：隧道握手、HTTP 转发（含 chunked、大体、`Range`）、WS 透传（含分片、close 码）、媒体票、只读令牌、CORS、撤销即时断开、休眠后唤醒恢复流表、alarm 判死、日配额 429；跨仓 e2e 用真 core 对 workerd 跑 `personal` 场景；`pnpm check` | Opus   | XL   |
| A8-1 客户端挑战令牌 | Armadra | `Armadra-worktrees/relay-challenge` | `apps/web/src/shell/RelaySignIn.tsx`、`sources/hosted.ts`、`panels/settings/pages/RemoteAccessPage.tsx`（中转账号登录框）、`mobile/ConnectScreen.tsx` + 新 `mobile/ChallengeSheet.tsx`（WebView `Sheet`）、`apps/web/src/api/remote-services.ts`、`core/sources/service.ts`（`platform.info.challenge` 透传、`challenge_required` 错误码映射）、`i18n/remote.ts` | 契约 §33 的远程服务登录入参加 `challenge`（追加节 **§60**，minor +1）；钉协议包含 §16 的版本                                                                                                                                                                                                                       | —                     | 页面测试：有 `challenge` 能力时渲染组件、无则不渲染；手机 `Sheet` 取 token；错误码文案中英同步；B 档 e2e 对假中继（返回 `challenge_required`）走通                                                                                                         | Sonnet | M    |

### 7.3 第三波

| 包                      | 仓库            | worktree                                                              | 文件边界                                                                                                                                                                                                                                                                                                                                                                                                     | 契约 / 版本                               | 迁移                                                                                   | 测试与验收                                                                                                                                      | 模型                                  | 规模                                                                                                                                  |
| ----------------------- | --------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------- | -------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- | ---- | --- |
| K4 防护与 IaC           | cloud           | `armadra-cloud-wt/cf-hardening`                                       | `packages/control-core/src/challenge.ts`（siteverify 校验，经端口 `ChallengeVerifier` 注入；Node 与 Workers 各给实现）、`apps/relay/src/personal/args.ts`（`--challenge`）、`apps/relay-workers/wrangler.jsonc`（ratelimits、limits、env）、`deploy/cloudflare/terraform/{main,waf,ratelimit,dns}.tf`、`scripts/kill-switch.mjs`、`docs/runbooks/cloudflare-protection.md`（含预算告警设置步骤、回滚、拉闸） | §16 从「预留」变实现；两 app 下一个 patch | —                                                                                      | siteverify 假服务器单测（成功 / 过期 / 重放 / 网络错 → `challenge_invalid`）；Terraform `plan` 在 CI 只做语法校验（不连账号）；runbook 演练清单 | Sonnet                                | M                                                                                                                                     |
| S2 PG 存储与 Redis 可选 | cloud           | `armadra-cloud-wt/store-postgres`                                     | `packages/control-store-postgres`（若 C8-1 已落地则从 `apps/cloud/src/adapters/postgres` 提升）、`packages/relay-runtime-node/src/control/postgres.ts`（`ControlPlane` 的 PG + `LISTEN/NOTIFY` 实现）、`apps/relay` `--store postgres` / `--control memory                                                                                                                                                   | redis                                     | postgres`（动态 import）、`deploy/compose.yml`、`AGENTS.md` 措辞、`boundaries.test.ts` | 无线上形状改动                                                                                                                                  | PG 迁移接 cloud 的最大号（C8-x 之后） | 集成（需 `pnpm dev:up`）：两节点路由互见、撤销广播、`NOTIFY` 断线重连后重读；personal 缺省装配静态 import 图不含 `postgres` / `redis` | Opus | L   |
| V4 Workers 探针         | cloud + Armadra | `armadra-cloud-wt/probes-workers`、`Armadra-worktrees/probes-workers` | `scripts/e2e.mjs --mode personal-workers`（起 workerd）、Armadra `tools/probes/personal-roundtrip.mjs --issuer`（已在 V3）；nightly 工作流加一列                                                                                                                                                                                                                                                             | —                                         | —                                                                                      | nightly 跨仓对 workerd 跑通；对线上 `relay.<域>` 只跑只读探针                                                                                   | Sonnet                                | M                                                                                                                                     |
| D3 用户部署             | 用户操作        | —                                                                     | §8 的步骤                                                                                                                                                                                                                                                                                                                                                                                                    | —                                         | —                                                                                      | `/.well-known/armadra-platform`、`/health`、桌面登记第二条中转、手机双路由切换、预算告警邮件到达                                                | —                                     | —                                                                                                                                     |

**依赖图**：K1 → K3 ∥ S1；K3 → A8-1（客户端要有带 `challenge` 的 `platform.info` 可测）→ K4；S1 → S2（S2 还等 C8-1 的 PG 适配器，谁先合并谁提供）；K3 + K4 → V4 → D3。K1 与前一份设计的 C7-x 都改 `apps/relay/src`：C7-2 已合并（0.3.0），K1 直接基于 main。

### 7.4 编号预分配汇总

| 仓库    | 契约                                                                                               | 协议 / 版本                                                                                           | 迁移                                                                       |
| ------- | -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| cloud   | cloud-api **§16** 挑战；**§17** 中继节点地址与节点回报（§15 仍留给 SaaS 波次的「团队与主机授予」） | 协议 minor **1.2**（K3）；包版本按合并顺序：K1 `0.3.1`、S1 `0.3.2`、K3 在 SaaS 波次之后取下一个 minor | SQL（SQLite / DO）`0001_init`；PG 接 cloud 最大号                          |
| Armadra | **§60** 远程服务登录的挑战令牌（可选的 `clientCert` 若做 mTLS 也在 §60 追加）                      | 契约 minor +1（接 main 当前）                                                                         | 无（`cloud_registrations` 若加 `client_cert` 列才需要 `0046`，第一版不做） |

## §8 需要用户做的事（不在对话里传任何凭据）

已完成：zone `armadra.com` 在 CF 上 `active`（Free Website）；本机 `wrangler 4.149.0` 已 OAuth 登录。

1. 确认 Workers 套餐：面板「Workers & Pages → Plans」。按 §4.6，1–2 台主机先 Free；要上第三台或日配额连续吃紧时升 **Workers Paid**（$5 / 月），同时确认账号级 **预算告警** 已开并设阈值（例如 $15 / 月）。
2. CI 若要自动部署，在面板建一个只有「Workers Scripts:Edit、Durable Objects:Edit、Workers Routes:Edit、DNS:Edit、Zone WAF:Edit」的 API Token，放进 GitHub secret `CLOUDFLARE_API_TOKEN`——由用户自己粘贴，agent 只读取是否存在。本机部署用已登录的 OAuth 即可。
3. DNS（§2.11）：`cn.relay.armadra.com` A → 49（灰云）；`app`、`status` 同法；`relay` / `relay-staging` 由 Custom Domain 自动建，不要手建。49 上用 1Panel 为 `cn.relay.armadra.com` 申请证书并把现有站点从 `armadra-personal.example.org` 改名（或新建站点），中继容器 `--host` / `--public-origin` 改成新域后重新初始化状态文件（还没有客户端登记，无迁移成本）。
4. 在 CF 面板建 Turnstile 站点（域名 `relay.armadra.com` 与 `relay-staging.armadra.com`，隐式模式），secret 用 `wrangler secret put RELAY_TURNSTILE_SECRET` 自己录入；siteKey 不是秘密，写进 `wrangler.jsonc vars`。
5. 用 `wrangler secret put` 录入 `RELAY_MASTER_KEY`（`openssl rand -hex 32`）、`RELAY_METRICS_TOKEN`、首次初始化的 `RELAY_PERSONAL_ACCOUNT` / `RELAY_PERSONAL_PASSWORD`（初始化完成后 `wrangler secret delete` 后两个）。
6. SaaS 上线前：订 Advanced Certificate Manager（`*.src.armadra.com`），建 `*.src` 通配记录（橙云）。M1 不需要。
7. 证书：Worker Custom Domain 自动签发，无需操作；若日后做 mTLS，再在面板为 `edge.armadra.com` / `relay.armadra.com` 的 `/t/v1` 开启。

## §9 需要用户决定的问题

| #   | 问题                                                                                                                                   | 默认（实现先按它走）                       |
| --- | -------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------ |
| Q1  | CF 中继的隧道心跳：45 s（省 DO 费用，掉线最长 90 s 才被判定）还是保持 20 s（判定 40 s，DO Duration 约翻倍）                            | 45 s；`RELAY_HEARTBEAT_MS` 可调，上限 60 s |
| Q2  | `ControlDO` 的 `locationHint`（账号 / 链接 / me-stream 所在地）：`apac`（香港 / 日本，离大陆近）还是 `wnam`（美西）                    | `apac`                                     |
| Q3  | Node 中继 `--store postgres` 是否允许在 `personal` 模式下以可选依赖出现（需改 AGENTS 的那条红线措辞），还是只给 `saas` / 新模式 `team` | 允许，动态 import + 边界测试守静态图       |
| Q4  | 隧道 mTLS（CF 托管 CA，core 持客户端证书）：第一版不做，还是和 K4 一起做（多一个 Armadra 包、契约 §60 加 `clientCert`、迁移 `0046`）   | 不做                                       |
| Q5  | Turnstile 的范围：只登录（缺省）还是登录 + 链接接受（core 作为客户端挂载链接时需走浏览器落地页做挑战）                                 | 只登录                                     |

附：Argo Smart Routing 不买、Bot Fight Mode 不开、Pro 先不买——这三条按 §3 / §4 的分析直接定，不单独请示。
