# armadra-cloud 契约

> 控制面 `/v1/*`、`/.well-known/*` 与中继客户端面的线上形状。真相源是协议包 `@armadra/platform-protocol`（`packages/platform-protocol/`）；本文的形状表是生成块，由 `pnpm contract:generate` 从协议包重写，`pnpm contract:check` 校验不漂移。章节号 §N 只追加、不改号。

## §1 编码规则

- JSON 字段一律 camelCase；时间用毫秒整数，字段名以 `AtMs` / `Ms` 结尾。
- 错误形状固定为 `{ code, message, requestId?, details? }`；`code` 只取协议包 `./errors` 注册表里的 snake_case 码，HTTP 状态码查同一张表。
- 每个响应带 `Armadra-Protocol: <major>.<minor>` 头；客户端对未知 major 拒绝，对更高 minor 忽略未知字段（schema 非 strict）。
- 预留：SaaS 服务端的业务路由（总计划 §12.2）已在契约与路由表里，实现落地前统一答 501 `{ code: "not_implemented", message, requestId }`；`GET /health` 已实现。
- 认证：云会话用 `Authorization: Bearer <accessToken>`；源对控制面用 `Authorization: Source <jws>`；浏览器来源改用 `__Host-armadra-cloud` Cookie + `X-Armadra-CSRF`。

## §2 账号与会话

<!-- rpc:begin contract=§2 -->

| procedure                     | 方法与路径                               | scope       | 错误码                                                                                                                                         |
| ----------------------------- | ---------------------------------------- | ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `platform.info`               | `GET /.well-known/armadra-platform`      | `anonymous` | —                                                                                                                                              |
| `platform.jwks`               | `GET /.well-known/jwks.json`             | `anonymous` | —                                                                                                                                              |
| `platform.health`             | `GET /health`                            | `anonymous` | `unavailable`                                                                                                                                  |
| `auth.register`               | `POST /v1/auth/register`                 | `anonymous` | `email_taken`、`rate_limited`、`password_too_short`、`password_too_long`、`password_too_common`、`password_contains_name`、`password_breached` |
| `auth.login`                  | `POST /v1/auth/login`                    | `anonymous` | `credentials_invalid`、`account_locked`、`account_disabled`、`rate_limited`                                                                    |
| `auth.mfaVerify`              | `POST /v1/auth/mfa/verify`               | `anonymous` | `mfa_invalid`、`mfa_expired`、`account_locked`                                                                                                 |
| `auth.refresh`                | `POST /v1/auth/refresh`                  | `anonymous` | `session_expired`、`session_revoked`                                                                                                           |
| `auth.logout`                 | `POST /v1/auth/logout`                   | `session`   | —                                                                                                                                              |
| `auth.deviceStart`            | `POST /v1/auth/device/start`             | `anonymous` | `rate_limited`                                                                                                                                 |
| `auth.devicePoll`             | `POST /v1/auth/device/poll`              | `anonymous` | `rate_limited`                                                                                                                                 |
| `auth.deviceApprove`          | `POST /v1/auth/device/approve`           | `session`   | `device_code_expired`、`not_found`                                                                                                             |
| `auth.passkeyRegisterOptions` | `POST /v1/auth/passkey/register/options` | `session`   | —                                                                                                                                              |
| `auth.passkeyRegister`        | `POST /v1/auth/passkey/register`         | `session`   | —                                                                                                                                              |
| `auth.passkeyLoginOptions`    | `POST /v1/auth/passkey/login/options`    | `anonymous` | `rate_limited`                                                                                                                                 |
| `auth.passkeyLogin`           | `POST /v1/auth/passkey/login`            | `anonymous` | `credentials_invalid`、`rate_limited`                                                                                                          |
| `auth.totpEnroll`             | `POST /v1/auth/totp/enroll`              | `session`   | `conflict`                                                                                                                                     |
| `auth.totpConfirm`            | `POST /v1/auth/totp/confirm`             | `session`   | `mfa_invalid`                                                                                                                                  |
| `auth.totpDisable`            | `POST /v1/auth/totp/disable`             | `session`   | `mfa_invalid`                                                                                                                                  |
| `auth.recoveryCodesRotate`    | `POST /v1/auth/recovery-codes/rotate`    | `session`   | `mfa_invalid`                                                                                                                                  |
| `me.get`                      | `GET /v1/me`                             | `session`   | —                                                                                                                                              |
| `me.update`                   | `PATCH /v1/me`                           | `session`   | `bad_request`                                                                                                                                  |
| `me.sources`                  | `GET /v1/me/sources`                     | `session`   | —                                                                                                                                              |
| `me.devices`                  | `GET /v1/me/devices`                     | `session`   | —                                                                                                                                              |
| `me.deviceRevoke`             | `DELETE /v1/me/devices/{deviceId}`       | `session`   | `not_found`                                                                                                                                    |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §3 组织

<!-- rpc:begin contract=§3 -->

| procedure           | 方法与路径                                    | scope                 | 错误码                                                       |
| ------------------- | --------------------------------------------- | --------------------- | ------------------------------------------------------------ |
| `orgs.create`       | `POST /v1/orgs`                               | `session`             | `org_slug_taken`、`bad_request`                              |
| `orgs.get`          | `GET /v1/orgs/{orgId}`                        | `session`             | `not_found`                                                  |
| `orgs.update`       | `PATCH /v1/orgs/{orgId}`                      | `org:admin`           | `not_found`、`org_role_insufficient`                         |
| `orgs.delete`       | `DELETE /v1/orgs/{orgId}`                     | `org:owner`           | `not_found`、`org_role_insufficient`                         |
| `orgs.members`      | `GET /v1/orgs/{orgId}/members`                | `session`             | `not_found`、`org_role_insufficient`                         |
| `orgs.memberRole`   | `PATCH /v1/orgs/{orgId}/members/{accountId}`  | `org:admin`           | `not_found`、`org_role_insufficient`                         |
| `orgs.memberRemove` | `DELETE /v1/orgs/{orgId}/members/{accountId}` | `org:admin`           | `not_found`、`org_role_insufficient`                         |
| `orgs.sources`      | `GET /v1/orgs/{orgId}/sources`                | `session`             | `not_found`、`org_role_insufficient`                         |
| `orgs.sourceAttach` | `POST /v1/orgs/{orgId}/sources`               | `owner`               | `not_found`、`org_role_insufficient`、`source_access_denied` |
| `orgs.sourceDetach` | `DELETE /v1/orgs/{orgId}/sources/{sourceId}`  | `owner` / `org:admin` | `not_found`、`org_role_insufficient`                         |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §4 源目录与断言

<!-- rpc:begin contract=§4 -->

| procedure                   | 方法与路径                                         | scope       | 错误码                                                                               |
| --------------------------- | -------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------ |
| `sources.registrationToken` | `POST /v1/sources/registration-tokens`             | `session`   | `email_unverified`、`limit_reached`、`org_role_insufficient`                         |
| `sources.register`          | `POST /v1/sources/register`                        | `anonymous` | `registration_token_invalid`、`source_taken`、`protocol_unsupported`、`rate_limited` |
| `sources.heartbeatHint`     | `GET /v1/sources/me/relay`                         | `source`    | `source_revoked`、`unauthenticated`                                                  |
| `sources.list`              | `GET /v1/sources`                                  | `session`   | —                                                                                    |
| `sources.get`               | `GET /v1/sources/{sourceId}`                       | `session`   | `not_found`                                                                          |
| `sources.update`            | `PATCH /v1/sources/{sourceId}`                     | `owner`     | `not_found`、`bad_request`                                                           |
| `sources.revoke`            | `DELETE /v1/sources/{sourceId}`                    | `owner`     | `not_found`                                                                          |
| `sources.assertion`         | `POST /v1/sources/{sourceId}/assertion`            | `session`   | `source_access_denied`、`source_revoked`、`rate_limited`                             |
| `sources.accessList`        | `GET /v1/sources/{sourceId}/access`                | `owner`     | `not_found`                                                                          |
| `sources.accessRevoke`      | `DELETE /v1/sources/{sourceId}/access/{accountId}` | `owner`     | `not_found`                                                                          |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §5 链接

<!-- rpc:begin contract=§5 -->

| procedure      | 方法与路径                       | scope                   | 错误码                                                                                  |
| -------------- | -------------------------------- | ----------------------- | --------------------------------------------------------------------------------------- |
| `links.create` | `POST /v1/links`                 | `owner` / `org:admin`   | `source_access_denied`、`org_role_insufficient`、`bad_request`                          |
| `links.get`    | `GET /v1/links/{linkId}`         | `anonymous`             | `link_invalid`                                                                          |
| `links.accept` | `POST /v1/links/{linkId}/accept` | `session` / `anonymous` | `link_invalid`、`link_expired`、`link_exhausted`、`link_secret_invalid`、`rate_limited` |
| `links.list`   | `GET /v1/links`                  | `owner` / `org:admin`   | —                                                                                       |
| `links.revoke` | `DELETE /v1/links/{linkId}`      | `owner` / `org:admin`   | `not_found`                                                                             |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §6 me/stream

<!-- rpc:begin contract=§6 -->

| procedure         | 方法与路径                  | scope     | 错误码 |
| ----------------- | --------------------------- | --------- | ------ |
| `me.streamTicket` | `POST /v1/me/stream/ticket` | `session` | —      |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §7 中继客户端面

客户端经 `relayBaseUrl` 访问源：HTTP 请求带 `Armadra-Relay-Token` 头，WebSocket 用子协议 `armadra-relay.<jwt>`。中继校验令牌的 `aud`、`exp`、`jti` 与 `src`，剥掉令牌后把请求原样转进隧道。

## §8 隧道 `t/v1`

帧布局、握手消息、窗口与关闭码以协议包 `./tunnel` 为准（常量 `TUNNEL_PATH = "/t/v1"`）；黄金字节在 `packages/platform-protocol/fixtures/tunnel/`。

## §9 计费（预留）

<!-- rpc:begin contract=§9 -->

| procedure       | 方法与路径                     | scope       | 错误码                               |
| --------------- | ------------------------------ | ----------- | ------------------------------------ |
| `billing.plan`  | `GET /v1/orgs/{orgId}/billing` | `org:admin` | `not_found`、`org_role_insufficient` |
| `billing.usage` | `GET /v1/orgs/{orgId}/usage`   | `org:admin` | `not_found`、`org_role_insufficient` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §10 personal 模式子集

单人中转（`armadra-relay personal`）实现 `/v1/*` 的一个子集：`/.well-known/armadra-platform`、`/.well-known/jwks.json`、账号登录 / 刷新 / 登出、`GET /v1/me`、源注册与目录、断言、分享链接与 `GET /v1/me/stream`。形状与 saas 完全相同；不实现的路由答 `404 not_found`。

## §11 TLS 约定

personal 模式默认自签 CA + 指纹钉扎（指纹 = CA 证书 DER 的 SHA-256），可选 ACME 或运维给证书；saas 模式由运营方的通配证书终止 TLS。
