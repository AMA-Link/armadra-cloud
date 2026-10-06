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

- 地址：saas 是 `https://<sourceId>.src.<域>/…`，路径原样；personal 是 `https://<中继>/s/<sourceId>/…`，中继剥掉 `/s/<sourceId>` 再转。路径与源的 Gateway 相同（含源自己的 `/health`）。
- 转给源的头：保留 `Host`、`Authorization` 等；剥掉中继令牌、逐跳头与客户端带来的 `X-Forwarded-*` / `Forwarded` / `X-Real-IP`；加 `X-Forwarded-For: <客户端 IP>`、`X-Forwarded-Proto: https`。源应以隧道 `OPEN` 的 `remoteIp` / `clientOrigin` 为准。
- CORS 预检（`OPTIONS` 且带 `Origin` 与 `Access-Control-Request-Method`）由中继自己回答，不验令牌、不进隧道、不开流，请求体丢弃；与普通请求共用按 IP 的限流。来源是原生 App（`capacitor://localhost`、`https://localhost`）或登记时发给源的 `trustedOrigins` 之一 → `204`，按 Origin 回显 `Access-Control-Allow-Origin`，带 `Vary: Origin`，不发 `Access-Control-Allow-Credentials`；`Allow-Methods` 为 `GET,POST,PUT,PATCH,DELETE,OPTIONS`，`Allow-Headers` 为 `authorization, content-type, x-armadra-csrf, armadra-relay-token`，`Max-Age` 600。其余来源 `403 forbidden`，不带 CORS 头。答案与源是否在线、是否登记无关（saas 模式暂未接入可信来源，只认原生 App）。不是预检的请求（包括缺 `Access-Control-Request-Method` 的 `OPTIONS`）照常要令牌；真请求的 CORS 头由源按它自己的可信来源回答。
- WebSocket：除 `armadra-relay.<jwt>` 外至少再带一个子协议（`armadra-ticket.<票>`），否则剥掉令牌后源选不到子协议，浏览器会拒绝这次握手。中继每 30 秒对客户端发 `ping`，60 秒收不到客户端任何字节即断开。
- 拒绝：

  | 情形                   | HTTP                        | WebSocket（先完成升级再关闭） |
  | ---------------------- | --------------------------- | ----------------------------- |
  | 不是源地址             | 404 `not_found`             | 404，不升级                   |
  | 预检的来源不在名单里   | 403 `forbidden`             | —                             |
  | 按 IP / 账号限流       | 429 `rate_limited`          | 4429                          |
  | 令牌缺失或无效         | 401 `relay_token_invalid`   | 4401                          |
  | 令牌的 `src` 不是此源  | 403 `relay_source_mismatch` | 4403                          |
  | 源离线（或隧道排空中） | 503 `source_offline`        | 4404                          |
  | 隧道流数已满 / 并发 WS | 429 `limit_reached`         | 4429                          |

  已建立的 WebSocket 在源下线时以 4404 结束（帧中间则直接断开）；升级途中源下线答 503 `source_offline`。

## §8 隧道 `t/v1`

帧布局、握手消息、窗口与关闭码以协议包 `./tunnel` 为准（常量 `TUNNEL_PATH = "/t/v1"`）；黄金字节在 `packages/platform-protocol/fixtures/tunnel/`。

中继侧的行为：

- 握手 10 秒内完成，否则关 4400；格式错（非 JSON、不符 schema、顺序错、`ready` 前发二进制）直接关 4400，不发 `reject`。
- `reject` 后的关闭码：`protocol_unsupported` 4409；`tunnel_token_invalid` / `tunnel_token_expired` / `signature_invalid` 4490；`rate_limited` 4429（按 sourceId 每分钟 10 次，令牌验过后才计）。
- 同一源的新隧道就绪时，旧隧道收 `GOAWAY replaced`，排空或 `goawayGraceMs` 后关 4491。中继关闭时各隧道收 `GOAWAY shutdown`，排空后关 4492。源发 `GOAWAY` 后中继不再给它开流。
- 心跳：中继每 `heartbeatMs` 发 `PING`，连续 `heartbeatMisses` 次没有 `PONG` 即断开；源发来的 `PING` 立即回 `PONG`。
- 流控：`ready.limits` 给出的窗口即初始信用；接收方把交给上层的字节累计到窗口一半时用 `WINDOW` 补回。对端超发 → `RST 5` 并关 4400。只有中继发 `OPEN`，源发来的 `OPEN` 答 `RST 1`。
- 只为客户端的真请求开流：CORS 预检由中继在边缘回答（§7），不产生 `OPEN`，源收不到预检。
- HTTP 流：`DATA` 里是完整的 HTTP/1.1 请求字节，末尾带 `connection: close`，一条流只跑一个请求；源答完响应后结束流。

## §9 计费（预留）

<!-- rpc:begin contract=§9 -->

| procedure       | 方法与路径                     | scope       | 错误码                               |
| --------------- | ------------------------------ | ----------- | ------------------------------------ |
| `billing.plan`  | `GET /v1/orgs/{orgId}/billing` | `org:admin` | `not_found`、`org_role_insufficient` |
| `billing.usage` | `GET /v1/orgs/{orgId}/usage`   | `org:admin` | `not_found`、`org_role_insufficient` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §10 personal 模式子集

单人中转（`armadra-relay personal`）实现 `/v1/*` 的一个子集，形状与 saas 完全相同；不实现的路由（包括契约里的组织、计费、设备码、passkey、TOTP）答 `404 not_found`。`/v1/*` 与 `/.well-known/*` 允许任意来源跨源调用（只认 `Authorization` 头，不用 Cookie），登录类答案总是带 `refreshToken`。

| procedure                                                                                        | 说明                                                                                                                                                         |
| ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `platform.info`、`platform.jwks`                                                                 | `mode: "personal"`、`relay.addressing: "path"`、`capabilities: ["auth.password", "links.source-invite", "me.stream"]`；有页面产物时 `webApp = <issuer>/app/` |
| `auth.login`、`auth.refresh`、`auth.logout`                                                      | `account` 是账号名；会话的 `accountId` 是 `acct:<账号名>`，`email` 为 `null`                                                                                 |
| `me.get`、`me.sources`、`me.devices`、`me.deviceRevoke`、`me.streamTicket` + `GET /v1/me/stream` | 访客（`guest:<linkId>:<随机>`）只看到链接指向的那个源                                                                                                        |
| `sources.registrationToken`、`sources.register`、`sources.heartbeatHint`                         | 注册令牌只有账号本人能要（访客答 `403 forbidden`）                                                                                                           |
| `sources.list`、`sources.get`、`sources.revoke`、`sources.assertion`                             | `relayBaseUrl = <issuer>/s/<sourceId>`；撤销即断开隧道与经中继的 WS，`me/stream` 发 `sourceRevoked`                                                          |
| `links.create`、`links.get`、`links.accept`、`links.list`、`links.revoke`                        | 只有 `source_invite`；`accept` 匿名；撤销即作废该链接的全部访客设备并断开其 WS                                                                               |

- 账号：一个；口令策略与 scrypt 参数取协议包 `identity-vectors`，策略码与 core 相同（`password_too_short` 等）。
- 防暴力破解：按 IP 每分钟 20 次（`rate_limited`）；按账号连续 5 次失败锁 1 分钟、每次翻倍、15 分钟封顶、最后一次失败一小时后清零。锁定时答 `429 account_locked`，`details.retryAfterMs` 与 `Retry-After` 头给出剩余时间。
- 令牌：访问令牌 15 分钟（`aud: "cloud"`，`sid` = 设备）；刷新令牌 30 天、每次刷新旋转，旧令牌再出现即撤销整台设备（`session_revoked`）；断言 5 分钟；中继令牌、隧道令牌 1 小时。签名钥 Ed25519，`rotate-key` 后旧钥一小时内仍在 JWKS 里。登出、改口令、撤销源或链接对已签出的令牌即时生效。
- 源认证：`GET /v1/sources/me/relay` 用 `Authorization: Source <jws>`（头 `kid = sourceId`，`iss = sourceId`、`aud = issuer`、寿命不超过 120 秒、`jti` 不重放），中继用登记时的公钥验；答一个节点 `wss://<issuer 主机>/t/v1`。
- 分享链接：`links.create` 答 `{ linkId, url: "<issuer>/j/<linkId>", secret, expiresAtMs }`（有效期不超过 30 天）。分享出去的是 `<url>#<secret>.<core 邀请令牌>`——秘密只在 `#` 片段里，浏览器不会发给任何服务器；落地页（或深链 `armadra://join?link=<linkId>&issuer=<issuer>&s=<片段>`）取片段后 `links.accept { secret }`，再拿答案里的 `assertion` 与邀请令牌向 core `cloud/login`。
- 托管页面：`/app/*` 与 `/j/<linkId>` 答 Armadra `apps/web` 构建产物的 `index.html`；没有产物时 `/app/*` 答 503 错误页，`/j/<linkId>` 答只有「在 Armadra 中打开」的最小落地页。

## §11 TLS 约定

personal 模式默认自签 CA + 指纹钉扎，可选 ACME 或运维给证书；saas 模式由运营方的通配证书终止 TLS。

- 自签（`--tls self-signed`，缺省）：`<data-dir>/tls/` 下 CA（ECDSA P-256，10 年）与叶证书（90 天，SAN = `--host` + `localhost` + `127.0.0.1`），文件 0600；叶证书剩 7 天或不再覆盖 `--host` 时自动换，CA 不变；CA 坏了或过期拒绝启动。`GET /ca.crt` 发 CA。
- 指纹 = 信任锚 DER 的 SHA-256，小写十六进制、无分隔符（与 core Gateway `tls.source = localCa` 同一定义）；启动日志与 `personal status` 打印。文件证书的信任锚是链里最后一张（只有一张时是它自己）；ACME 证书由系统信任，不钉扎。
- ACME（`--tls acme`）：`http-01`，挑战监听缺省 `0.0.0.0:80`；剩 30 天续期，失败每 12 小时重试并继续用旧证书；首签失败拒绝启动。
- 文件（`--tls file`）：`--tls-cert` / `--tls-key`，文件变化时热加载。
- 明文（`--tls plain`）：只允许监听回环地址，给同机终止 TLS 的反向代理用；issuer 仍是对外的 `https` 来源。
