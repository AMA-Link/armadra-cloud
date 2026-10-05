# Changelog

## Unreleased

### 新增 / Added

- 中继内核（`apps/relay`）：隧道终端 `/t/v1`（握手、多路复用流、信用窗口、心跳、GOAWAY、同源替换）、客户端边缘（HTTP 与 WebSocket 转发、中继令牌校验、限流）、`ControlPlane` 接口与内存实现、`/health` 与单独监听的 `/metrics`；日志只记元数据。
  Relay kernel (`apps/relay`): tunnel endpoint `/t/v1` (handshake, multiplexed streams, credit windows, heartbeats, GOAWAY, same-source replacement), client edge (HTTP and WebSocket forwarding, relay-token checks, rate limits), the `ControlPlane` interface with an in-memory implementation, `/health` and `/metrics` on a separate listener; logs carry metadata only.

- `@armadra/platform-protocol` 0.1.0（协议 1.0）：`tunnel`、`assertion`、`cloud-api`、`core-api`、`errors`、`identity-vectors`、`fixtures` 七个子路径；黄金夹具与 OpenAPI 快照。
  `@armadra/platform-protocol` 0.1.0 (protocol 1.0): seven subpaths `tunnel`, `assertion`, `cloud-api`, `core-api`, `errors`, `identity-vectors`, `fixtures`; golden fixtures and an OpenAPI snapshot.

- 仓库骨架：pnpm workspace、TypeScript、ESLint、Prettier、Vitest；`apps/cloud` 与 `apps/relay` 的健康检查、配置加载与启动脚本；Dockerfile 与本地 compose；CI。
  Repository skeleton: pnpm workspace, TypeScript, ESLint, Prettier, Vitest; health checks, config loading and start scripts for `apps/cloud` and `apps/relay`; Dockerfiles and local compose; CI.
