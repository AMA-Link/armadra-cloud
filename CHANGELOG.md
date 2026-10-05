# Changelog

## Unreleased

### 新增 / Added

- 个人中转控制面（`armadra-relay personal`）：`init / serve / status / passwd / rotate-key / links` 子命令；单个状态文件（账号、口令哈希、签名钥、源 / 设备 / 链接登记）；账号口令登录与锁定、限流；访问 / 刷新 / 断言 / 中继 / 隧道令牌签发与即时撤销；源注册与源 JWS 换隧道令牌；分享链接与访客；`/v1/me/stream`；自签 CA（指纹钉扎）/ ACME / 文件证书，明文只允许回环；托管 Armadra 页面产物与错误页；`pnpm relay:personal`、`deploy/personal/compose.yml`。
  Personal relay control plane (`armadra-relay personal`): `init / serve / status / passwd / rotate-key / links`; a single state file (account, password hash, signing keys, source / device / link records); password sign-in with lockout and rate limits; access / refresh / assertion / relay / tunnel tokens with immediate revocation; source registration and source-JWS tunnel tokens; share links and guests; `/v1/me/stream`; self-signed CA with fingerprint pinning / ACME / file certificates, plaintext only on loopback; hosting of the Armadra web build with an error page; `pnpm relay:personal` and `deploy/personal/compose.yml`.

- 中继内核（`apps/relay`）：隧道终端 `/t/v1`（握手、多路复用流、信用窗口、心跳、GOAWAY、同源替换）、客户端边缘（HTTP 与 WebSocket 转发、中继令牌校验、限流）、`ControlPlane` 接口与内存实现、`/health` 与单独监听的 `/metrics`；日志只记元数据。
  Relay kernel (`apps/relay`): tunnel endpoint `/t/v1` (handshake, multiplexed streams, credit windows, heartbeats, GOAWAY, same-source replacement), client edge (HTTP and WebSocket forwarding, relay-token checks, rate limits), the `ControlPlane` interface with an in-memory implementation, `/health` and `/metrics` on a separate listener; logs carry metadata only.

- `@armadra/platform-protocol` 0.1.0（协议 1.0）：`tunnel`、`assertion`、`cloud-api`、`core-api`、`errors`、`identity-vectors`、`fixtures` 七个子路径；黄金夹具与 OpenAPI 快照。
  `@armadra/platform-protocol` 0.1.0 (protocol 1.0): seven subpaths `tunnel`, `assertion`, `cloud-api`, `core-api`, `errors`, `identity-vectors`, `fixtures`; golden fixtures and an OpenAPI snapshot.

- 仓库骨架：pnpm workspace、TypeScript、ESLint、Prettier、Vitest；`apps/cloud` 与 `apps/relay` 的健康检查、配置加载与启动脚本；Dockerfile 与本地 compose；CI。
  Repository skeleton: pnpm workspace, TypeScript, ESLint, Prettier, Vitest; health checks, config loading and start scripts for `apps/cloud` and `apps/relay`; Dockerfiles and local compose; CI.
