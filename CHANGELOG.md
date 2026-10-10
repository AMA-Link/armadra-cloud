# Changelog

## Unreleased

### 新增 / Added

- 服务端名称（cloud-api §15，协议包 0.3.1，cloud 各包同版本，协议仍是 1.1）：`platform.info`（`/.well-known/armadra-platform`）多一个可选的 `name`，客户端添加服务时用它作缺省显示名。个人中转依次取 `--name`、`RELAY_NAME`、状态文件 `settings.displayName`，缺省 `--host`；SaaS 预留同一字段。旧客户端忽略它，旧状态文件照常读。
  Server name (cloud-api §15, protocol package 0.3.1, all cloud packages in lockstep, protocol still 1.1): `platform.info` (`/.well-known/armadra-platform`) gains an optional `name` that clients use as the default label when adding the service. The personal relay takes `--name`, then `RELAY_NAME`, then `settings.displayName` in the state file, falling back to `--host`; SaaS reserves the same field. Older clients ignore it and older state files still load.
- 协议包 0.3.2（cloud 各包同版本，协议仍是 1.1）：错误码 `bad_gateway`（502，源的 HTTP 响应不合法）；黄金夹具加 `fixtures/http1/`（隧道流里的 HTTP/1.1 响应与请求字节，含 `chunked`、关闭定界、`HEAD`、`1xx` / `101` 与各类非法响应）与 `fixtures/ws/`（WebSocket 帧，含 RFC 6455 §5.7 示例与违规帧），期望结果在各自的 `cases.json`，与语言无关。
  Protocol package 0.3.2 (all cloud packages in lockstep, protocol still 1.1): error code `bad_gateway` (502, the source sent an invalid HTTP response); new golden fixtures `fixtures/http1/` (HTTP/1.1 response and request bytes on a tunnel stream, covering `chunked`, close-delimited bodies, `HEAD`, `1xx` / `101` and malformed responses) and `fixtures/ws/` (WebSocket frames, including the RFC 6455 §5.7 examples and protocol violations), with language-neutral expectations in each `cases.json`.

### 变更 / Changed

- 中继内核去 Node 化：`packages/relay-kernel` 只留运行时无关的逻辑（隧道帧流与信用窗口改为 `Uint8Array` 队列 + 显式信用，另有 Web Streams 版；握手与隧道表只看 `TunnelSocket`；边缘判定只看 `EdgeRequest`；新增 HTTP/1.1 编解码 `http1/` 与 WebSocket 帧编解码 `ws/`），源码只许 `node:crypto` / `node:buffer`。Node 专有部分搬进新包 `packages/relay-runtime-node`（`ws` 隧道终端、边缘的 HTTP / WebSocket 字节进出、clientIp、HTTP 分派、`startRelay`），`apps/relay` 只改 import。线上行为、CLI 与状态文件不变；边界由 `repo.rules.json`、两个包的 `boundaries.test.ts` 与 ESLint 守住。契约 §7 的 WebSocket 保活改写成活性保证（客户端 120 秒内无字节可能被断开；Node 中继仍是 30 秒 ping、60 秒断开）。
  Relay kernel made runtime-neutral: `packages/relay-kernel` keeps only runtime-independent logic (tunnel streams and credit windows are now a `Uint8Array` queue with explicit credit, plus a Web Streams adapter; the handshake and tunnel table see only a `TunnelSocket`; edge decisions see only an `EdgeRequest`; new HTTP/1.1 codec `http1/` and WebSocket frame codec `ws/`), and its sources may import only `node:crypto` / `node:buffer`. The Node-specific parts move to the new `packages/relay-runtime-node` (the `ws` tunnel endpoint, HTTP / WebSocket byte plumbing at the edge, clientIp, HTTP dispatch, `startRelay`); `apps/relay` only changes imports. Wire behavior, the CLI and the state file are unchanged; the boundary is enforced by `repo.rules.json`, both packages' `boundaries.test.ts` and ESLint. The WebSocket keepalive in contract §7 is restated as a liveness guarantee (clients idle for 120 s may be disconnected; the Node relay still pings every 30 s and disconnects after 60 s).

## 0.3.0（2026-10-10）

### 新增 / Added

- 协议 1.1（协议包 0.3.0，cloud 各包同版本）：断言范围与团队声明（cloud-api §14）。断言加可选的 `scp`（`ws:<id>`、`sess:<id>`、`ro`）、`role`（viewer / editor / operator / driver）与 `team`；中继令牌加 `org` 与 `ro`，隧道令牌加 `org`。`links.create` 的 `source_invite` 收 `scope { workspaceId, sessionId, readOnly }`（会话范围要带工作空间、总是只读），访客的断言带对应的 `scp`，只读时中继令牌带 `ro: true`；`links.get` / `links.list` 报范围，个人中转把它存进状态文件并报能力 `links.scope`。边缘对 `ro` 令牌只放行 `GET` / `HEAD` / `OPTIONS`、WebSocket 升级、媒体票与 `/api/identity/*`、`/api/rpc/identity/*`，其余答 `403 forbidden`（WS 以 4403 关闭），判定函数 `isReadOnlyAllowed` 在协议包里。缺省 scope 与 1.0 签出的令牌逐字同形，旧客户端与旧 core 不受影响。
  Protocol 1.1 (protocol package 0.3.0, all cloud packages in lockstep): assertion scope and team claims (cloud-api §14). Assertions gain optional `scp` (`ws:<id>`, `sess:<id>`, `ro`), `role` (viewer / editor / operator / driver) and `team`; relay tokens gain `org` and `ro`, tunnel tokens gain `org`. `links.create` for `source_invite` accepts `scope { workspaceId, sessionId, readOnly }` (a session scope needs its workspace and is always read-only); the guest's assertion carries the matching `scp` and read-only links get relay tokens with `ro: true`. `links.get` / `links.list` report the scope, and the personal relay stores it in the state file and advertises the `links.scope` capability. For `ro` tokens the edge only lets through `GET` / `HEAD` / `OPTIONS`, WebSocket upgrades, media tickets and `/api/identity/*`, `/api/rpc/identity/*`; everything else gets `403 forbidden` (WS closes with 4403). The predicate `isReadOnlyAllowed` lives in the protocol package. Without a scope the issued tokens are identical in shape to 1.0, so older clients and cores are unaffected.
- 个人中转支持受信代理：`--trusted-proxy 127.0.0.1/32,::1/128`（或 `RELAY_TRUSTED_PROXY`、状态文件 `settings.trustedProxy`），`--trusted-proxy-prefer forwarded-for|real-ip` 选先读的头。只有直连对端在列表内才读 `X-Forwarded-For`（自右向左剔除受信跳数）或 `X-Real-IP`，其余对端的转发头一律忽略；限流、登录锁定、转给源的客户端 IP 与 WebSocket 升级都走同一个 `clientIp`。缺省为空，行为不变。放在 OpenResty / CDN 后面时按 IP 的限流不再是全站一个桶。
  Personal relay supports trusted proxies: `--trusted-proxy 127.0.0.1/32,::1/128` (or `RELAY_TRUSTED_PROXY`, state file `settings.trustedProxy`), with `--trusted-proxy-prefer forwarded-for|real-ip`. Forwarding headers are read only when the direct peer is in the list (`X-Forwarded-For` is walked right to left past trusted hops) and ignored from any other peer; rate limits, login lockout, the client IP forwarded to sources and WebSocket upgrades all use one `clientIp`. Empty by default, so behavior is unchanged. Per-IP limits no longer collapse into one site-wide bucket behind OpenResty / a CDN.

### 变更 / Changed

- 抽离共享内核：新包 `packages/relay-kernel`（隧道终端、多路复用与信用窗口、心跳、边缘转发、限流、clientIp、媒体票、令牌校验、`ControlPlane` 与内存实现）与 `packages/control-core`（存储端口、账号 / 会话 / 签发 / 源 / 链接 / me-stream 服务、`createCloudRouter`）。个人中转改为「内核 + 控制核 + 状态文件端口」装配，cloud 的预留路由改用同一个路由工厂；线上行为、状态文件格式与 CLI 不变。包边界由各包的边界测试、ESLint 与 `repo.rules.json` 新增的 `packageDependencies` 规则守住。协议包 0.2.3 增加语言无关的 `schema/` 导出（令牌声明与握手消息的 JSON Schema、错误码表、常量、`/v1` OpenAPI），随 tarball 发布，`pnpm protocol:check` 校验不漂移。
  Extracted shared kernels: the new `packages/relay-kernel` (tunnel endpoint, multiplexing and credit windows, heartbeats, edge forwarding, rate limits, clientIp, media tickets, token verification, `ControlPlane` with its in-memory implementation) and `packages/control-core` (storage ports, account / session / signing / source / link / me-stream services, `createCloudRouter`). The personal relay is now assembled from kernel + control core + state-file ports, and the cloud reserved routes use the same router factory; wire behavior, the state file format and the CLI are unchanged. Package boundaries are enforced by per-package boundary tests, ESLint and a new `packageDependencies` rule in `repo.rules.json`. Protocol package 0.2.3 adds a language-neutral `schema/` export (JSON Schema for token claims and handshake messages, the error-code table, constants and the `/v1` OpenAPI), shipped in the tarball and kept in sync by `pnpm protocol:check`.

### 修复 / Fixed

- 中继不再带 `node-forge`（GHSA-86w9-cpqp-85rv，上游暂无修复版）：给 `acme-client` 打补丁去掉从不调用的 forge 旧接口，依赖树、镜像与 `armadra-relay` 单文件都不再含它；ACME 签发仍走 `acme-client` 与 `node:crypto`，行为不变。
  The relay no longer ships `node-forge` (GHSA-86w9-cpqp-85rv, no upstream fix yet): a patch to `acme-client` drops its unused legacy forge interface, so the dependency tree, images and the `armadra-relay` bundle no longer contain it; ACME issuance still uses `acme-client` and `node:crypto` with unchanged behavior.

## 0.2.1（2026-10-07）

### 新增 / Added

- 媒体票（cloud-api §12）：客户端带中继令牌 `POST <源地址>/_relay/media-tickets` 换一张票，浏览器取 `<源地址>/_relay/m/<票>`，边缘按票把 GET / HEAD 转到源上签票时指定的路径；票绑源、单个路径与签票令牌，闲置 5 分钟或签出 30 分钟作废，路径与票不进日志。经中继的源上 `<video src>` 可以按 `Range` 直接播放。
  Media tickets (cloud-api §12): a client holding a relay token calls `POST <source>/_relay/media-tickets` for a ticket, and the browser fetches `<source>/_relay/m/<ticket>`; the edge forwards GET / HEAD to the path fixed at issue time. Tickets are bound to the source, a single path and the issuing token, expire after 5 idle minutes or 30 minutes in total, and neither the path nor the ticket is logged. `<video src>` on relayed sources now plays directly with `Range`.

### 修复 / Fixed

- 中继与 cloud 的 HTTP 监听空闲 keep-alive 由 Node 缺省 5 秒改为 75 秒（请求头超时 76 秒、整条请求 5 分钟）：iPad / iOS 的 WebKit 复用刚被服务端关掉的连接发 POST 时不重试，偶发「这一轮没有完成」。
  The relay and cloud HTTP listeners keep idle keep-alive connections for 75 s instead of Node's 5 s default (headers timeout 76 s, request timeout 5 min): WebKit on iPad / iOS does not retry a POST on a connection the server has just closed, which occasionally ended an agent turn.

### 变更 / Changed

- 边缘限流缺省额度放宽：CORS 预检按 IP 每分钟 1200（原 300），有效令牌 1200（原 600），同一账号 3600（原 1800），同一账号并发 WebSocket 256（原 64）；中继预检与控制面 `Access-Control-Max-Age` 由 600 改为 7200。
  Relaxed default edge limits: CORS preflights 1200 per IP per minute (was 300), 1200 per relay token (was 600), 3600 per account (was 1800), 256 concurrent WebSockets per account (was 64); `Access-Control-Max-Age` on relay preflights and the control API goes from 600 to 7200.

## 0.2.0（2026-10-07）

### 新增 / Added

- 分享链接改备注：`@armadra/platform-protocol` 0.2.0 加 `links.update`（`PATCH /v1/links/{linkId}`，只改 `label`）与能力 `links.update`；个人中转实现它，`platform.info` 报 `links.update`。
  Share-link notes: `@armadra/platform-protocol` 0.2.0 adds `links.update` (`PATCH /v1/links/{linkId}`, `label` only) and the `links.update` capability; the personal relay implements it and reports the capability in `platform.info`.

- 个人中转控制面（`armadra-relay personal`）：`init / serve / status / passwd / rotate-key / links` 子命令；单个状态文件（账号、口令哈希、签名钥、源 / 设备 / 链接登记）；账号口令登录与锁定、限流；访问 / 刷新 / 断言 / 中继 / 隧道令牌签发与即时撤销；源注册与源 JWS 换隧道令牌；分享链接与访客；`/v1/me/stream`；自签 CA（指纹钉扎）/ ACME / 文件证书，明文只允许回环；托管 Armadra 页面产物与错误页；`pnpm relay:personal`、`deploy/personal/compose.yml`。
  Personal relay control plane (`armadra-relay personal`): `init / serve / status / passwd / rotate-key / links`; a single state file (account, password hash, signing keys, source / device / link records); password sign-in with lockout and rate limits; access / refresh / assertion / relay / tunnel tokens with immediate revocation; source registration and source-JWS tunnel tokens; share links and guests; `/v1/me/stream`; self-signed CA with fingerprint pinning / ACME / file certificates, plaintext only on loopback; hosting of the Armadra web build with an error page; `pnpm relay:personal` and `deploy/personal/compose.yml`.

- 中继内核（`apps/relay`）：隧道终端 `/t/v1`（握手、多路复用流、信用窗口、心跳、GOAWAY、同源替换）、客户端边缘（HTTP 与 WebSocket 转发、中继令牌校验、限流）、`ControlPlane` 接口与内存实现、`/health` 与单独监听的 `/metrics`；日志只记元数据。
  Relay kernel (`apps/relay`): tunnel endpoint `/t/v1` (handshake, multiplexed streams, credit windows, heartbeats, GOAWAY, same-source replacement), client edge (HTTP and WebSocket forwarding, relay-token checks, rate limits), the `ControlPlane` interface with an in-memory implementation, `/health` and `/metrics` on a separate listener; logs carry metadata only.

- `@armadra/platform-protocol` 0.1.0（协议 1.0）：`tunnel`、`assertion`、`cloud-api`、`core-api`、`errors`、`identity-vectors`、`fixtures` 七个子路径；黄金夹具与 OpenAPI 快照。
  `@armadra/platform-protocol` 0.1.0 (protocol 1.0): seven subpaths `tunnel`, `assertion`, `cloud-api`, `core-api`, `errors`, `identity-vectors`, `fixtures`; golden fixtures and an OpenAPI snapshot.

- 仓库骨架：pnpm workspace、TypeScript、ESLint、Prettier、Vitest；`apps/cloud` 与 `apps/relay` 的健康检查、配置加载与启动脚本；Dockerfile 与本地 compose；CI。
  Repository skeleton: pnpm workspace, TypeScript, ESLint, Prettier, Vitest; health checks, config loading and start scripts for `apps/cloud` and `apps/relay`; Dockerfiles and local compose; CI.
