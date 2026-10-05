# armadra-cloud 契约

> 控制面 `/v1/*`、`/.well-known/*` 与中继客户端面的线上形状。真相源是协议包 `@armadra/platform-protocol`（`packages/platform-protocol/`）；本文的形状表是生成块，由 `pnpm contract:generate` 从协议包重写，`pnpm contract:check` 校验不漂移。章节号 §N 只追加、不改号。

## §1 编码规则

- JSON 字段一律 camelCase；时间用毫秒整数，字段名以 `AtMs` / `Ms` 结尾。
- 错误形状固定为 `{ code, message, requestId?, details? }`；`code` 只取协议包 `./errors` 注册表里的 snake_case 码，HTTP 状态码查同一张表。
- 每个响应带 `Armadra-Protocol: <major>.<minor>` 头；客户端对未知 major 拒绝，对更高 minor 忽略未知字段（schema 非 strict）。
- 认证：云会话用 `Authorization: Bearer <accessToken>`；源对控制面用 `Authorization: Source <jws>`；浏览器来源改用 `__Host-armadra-cloud` Cookie + `X-Armadra-CSRF`。

## §2 账号与会话

<!-- rpc:begin contract=§2 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §3 组织

<!-- rpc:begin contract=§3 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §4 源目录与断言

<!-- rpc:begin contract=§4 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §5 链接

<!-- rpc:begin contract=§5 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §6 me/stream

<!-- rpc:begin contract=§6 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §7 中继客户端面

客户端经 `relayBaseUrl` 访问源：HTTP 请求带 `Armadra-Relay-Token` 头，WebSocket 用子协议 `armadra-relay.<jwt>`。中继校验令牌的 `aud`、`exp`、`jti` 与 `src`，剥掉令牌后把请求原样转进隧道。

## §8 隧道 `t/v1`

帧布局、握手消息、窗口与关闭码以协议包 `./tunnel` 为准（常量 `TUNNEL_PATH = "/t/v1"`）；黄金字节在 `packages/platform-protocol/fixtures/tunnel/`。

## §9 计费（预留）

<!-- rpc:begin contract=§9 -->

_形状表由协议包的 `/v1` 契约生成（C0-2）。_

<!-- rpc:end -->

## §10 personal 模式子集

单人中转（`armadra-relay personal`）实现 `/v1/*` 的一个子集：`/.well-known/armadra-platform`、`/.well-known/jwks.json`、账号登录 / 刷新 / 登出、`GET /v1/me`、源注册与目录、断言、分享链接与 `GET /v1/me/stream`。形状与 saas 完全相同；不实现的路由答 `404 not_found`。

## §11 TLS 约定

personal 模式默认自签 CA + 指纹钉扎（指纹 = CA 证书 DER 的 SHA-256），可选 ACME 或运维给证书；saas 模式由运营方的通配证书终止 TLS。
