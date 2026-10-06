# Changelog

## Unreleased

### 新增 / Added

- 媒体票（cloud-api §12）：客户端带中继令牌 `POST <源地址>/_relay/media-tickets` 换一张票，浏览器取 `<源地址>/_relay/m/<票>`，边缘按票把 GET / HEAD 转到源上签票时指定的路径；票绑源、单个路径与签票令牌，闲置 5 分钟或签出 30 分钟作废，路径与票不进日志。经中继的源上 `<video src>` 可以按 `Range` 直接播放。
  Media tickets (cloud-api §12): a client holding a relay token calls `POST <source>/_relay/media-tickets` for a ticket, and the browser fetches `<source>/_relay/m/<ticket>`; the edge forwards GET / HEAD to the path fixed at issue time. Tickets are bound to the source, a single path and the issuing token, expire after 5 idle minutes or 30 minutes in total, and neither the path nor the ticket is logged. `<video src>` on relayed sources now plays directly with `Range`.

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
