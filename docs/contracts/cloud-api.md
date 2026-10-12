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
| `auth.login`                  | `POST /v1/auth/login`                    | `anonymous` | `credentials_invalid`、`account_locked`、`account_disabled`、`rate_limited`、`challenge_required`、`challenge_invalid`                         |
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

| procedure      | 方法与路径                       | scope                   | 错误码                                                                                                                             |
| -------------- | -------------------------------- | ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `links.create` | `POST /v1/links`                 | `owner` / `org:admin`   | `source_access_denied`、`org_role_insufficient`、`bad_request`                                                                     |
| `links.get`    | `GET /v1/links/{linkId}`         | `anonymous`             | `link_invalid`                                                                                                                     |
| `links.accept` | `POST /v1/links/{linkId}/accept` | `session` / `anonymous` | `link_invalid`、`link_expired`、`link_exhausted`、`link_secret_invalid`、`rate_limited`、`challenge_required`、`challenge_invalid` |
| `links.list`   | `GET /v1/links`                  | `owner` / `org:admin`   | —                                                                                                                                  |
| `links.update` | `PATCH /v1/links/{linkId}`       | `owner` / `org:admin`   | `not_found`                                                                                                                        |
| `links.revoke` | `DELETE /v1/links/{linkId}`      | `owner` / `org:admin`   | `not_found`                                                                                                                        |

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

0.5.0 起中继只转端到端通道（§20）：源地址上唯一的入口是 `_relay/e2e` 的 WebSocket 升级，子协议 `armadra-e2e.1` + `armadra-relay.<jwt>`。中继校验令牌的 `aud`、`exp`、`jti` 与 `src`，开一条 `OPEN kind: "e2e"` 的流，之后只转密文。明文的应用请求（HTTP 与普通 WebSocket 经中继转进隧道、`Armadra-Relay-Token` 头、CORS 预检）与媒体票（§12）一并删除。

- 地址：saas 是 `https://<sourceId>.src.<域>/…`，personal 是 `https://<中继>/s/<sourceId>/…`。源地址上除 `_relay/e2e` 的升级外，任何请求（含带令牌的 HTTP、`OPTIONS` 预检、`/_relay/media-tickets`、`/_relay/m/<票>`、别的路径上的 WebSocket 升级）一律 `404 not_found`，不验令牌、不进隧道。
- 边缘限流分层（WebSocket 以 4429 关闭）：带有效中继令牌的升级先按令牌 `jti`（缺省每分钟 1200 次）、再按账号 `sub`（缺省 3600 次）计数，不看 IP；无令牌、令牌无效或 `src` 不符的按 IP 严格限流（缺省每分钟 60 次）；同一账号同时开着的通道缺省最多 256 条（不可配），满了 4429 `limit_reached`。令牌校验是本地验签，先验后计。personal 的额度可由 `serve` 参数、环境变量或状态文件 `settings.rateLimits` 配置（见 deploy/README.md；`preflight` 一项 0.5.0 起不再使用）；登录（按 IP 每分钟 20 次）与账号锁定不可配、不变。
- HTTP 监听（中继、cloud）的空闲 keep-alive 为 75 秒，请求头超时 76 秒，整条请求的接收上限 5 分钟（升级后的 WebSocket 不受它管）。
- 中继自己返回的错误答复（源地址上的 404 与升级被拒的 HTTP 答复）按名单补 CORS 头：来源是原生 App（`capacitor://localhost`、`https://localhost`）或登记时发给源的 `trustedOrigins` 之一 → 按 Origin 回显 `Access-Control-Allow-Origin`，带 `Vary: Origin`，`Access-Control-Expose-Headers: retry-after`，不发 `Allow-Credentials`，不用 `*`；名单外或没有 `Origin` 不加。
- 活性：中继维持连接活性（ping 由中继或它前面的边缘网络发出，浏览器自动应答）；客户端 120 秒内没有任何字节可能被断开。Node 中继每 30 秒发 `ping`、60 秒无字节即断开。
- 拒绝：

  | 情形                          | WebSocket（先完成升级再关闭） |
  | ----------------------------- | ----------------------------- |
  | 不是源地址或不是 `_relay/e2e` | 404，不升级                   |
  | 按 IP / 账号限流              | 4429                          |
  | 令牌缺失或无效                | 4401                          |
  | 令牌的 `src` 不是此源         | 4403                          |
  | 缺版本子协议                  | 4409                          |
  | 源离线（或隧道排空中）        | 4404                          |
  | 源没报 `e2e`（旧 core）       | 4404，原因 `e2e_unsupported`  |
  | 隧道流数已满 / 并发通道       | 4429                          |

  已建立的通道在源下线时以 4404 结束。0.5.0 之前的明文路径（HTTP 转发、普通 WebSocket、预检）见 0.4.x 的本节。

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
- 端到端通道流（协议 1.3）：`OPEN.kind = "e2e"`，`DATA` 里是 `armadra-e2e/1` 的密文单元，只开给 `hello.capabilities` 含 `e2e` 的源（§20）。

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

| procedure                                                                                        | 说明                                                                                                                                                                                                                                             |
| ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `platform.info`、`platform.jwks`                                                                 | `mode: "personal"`、`relay.addressing: "path"`、`capabilities: ["auth.password", "auth.password-change", "links.source-invite", "links.update", "links.scope", "me.stream", "me.traffic"]`；有页面产物时 `webApp = <issuer>/app/`；`name` 见 §15 |
| `auth.login`、`auth.refresh`、`auth.logout`、`auth.changePassword`                               | `account` 是账号名；会话的 `accountId` 是 `acct:<账号名>`，`email` 为 `null`；改口令见 §18                                                                                                                                                       |
| `me.get`、`me.sources`、`me.devices`、`me.deviceRevoke`、`me.streamTicket` + `GET /v1/me/stream` | 访客（`guest:<linkId>:<随机>`）只看到链接指向的那个源                                                                                                                                                                                            |
| `me.traffic`                                                                                     | 流量统计（§19）：账号本人看全部源，访客只看自己的 `subject`                                                                                                                                                                                      |
| `sources.registrationToken`、`sources.register`、`sources.heartbeatHint`                         | 注册令牌只有账号本人能要（访客答 `403 forbidden`）                                                                                                                                                                                               |
| `sources.list`、`sources.get`、`sources.revoke`、`sources.assertion`                             | `relayBaseUrl = <issuer>/s/<sourceId>`；撤销即断开隧道与经中继的 WS，`me/stream` 发 `sourceRevoked`                                                                                                                                              |
| `links.create`、`links.get`、`links.accept`、`links.list`、`links.update`、`links.revoke`        | 只有 `source_invite`；`create` 收 `scope`（§14）；`accept` 匿名；`update` 只改备注（去首尾空白，已撤销的也能改）；撤销即作废该链接的全部访客设备并断开其 WS                                                                                      |

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

## §12 媒体票

0.5.0 起删除（随明文转发一起，§7）。`/_relay/media-tickets` 与 `/_relay/m/<票>` 答 `404 not_found`；经中转的媒体由客户端经端到端通道取回。之前：带令牌的请求换一张中继侧的票，浏览器按票直接取，边缘转到源上签票时的路径。

## §14 断言范围与团队声明

协议 1.1（协议包 0.3.0）。全部是可选的新声明与字段：缺省时签出的令牌与 1.0 逐字同形，core 按 1.0 处理（老中继签的断言照常可用）。core 是唯一的读写判定点（0.3.11 起中继边缘不再按 `ro` 过滤，见下文「只读门」）。

**断言**（`aud = sourceId`）新增：

| 声明   | 形状                                                                                                          | 含义                                                                              |
| ------ | ------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `scp`  | `string[]`，每项 `ws:<workspaceId>`、`sess:<sessionId>` 或 `ro`（id 为 `[A-Za-z0-9_.:-]{1,128}`，最多 64 项） | 范围：列出的工作空间 / 会话；`ro` = 只读。缺省或空 = 整台主机、按角色可写         |
| `role` | `viewer` / `editor` / `operator` / `driver`                                                                   | 主机授予的角色（SaaS 按 `source_grants` 合成）；缺省时 core 按邀请 / 本地设置决定 |
| `team` | `{ teamId, role: "lead" \| "member" }`                                                                        | 经团队授予时的团队；`org`（已有，`{ orgId, role }`）不变                          |

`scp` 的顺序固定为 `ws:`、`sess:`、`ro`；会话项总是伴随它所在的工作空间项与 `ro`。协议包的 `scopeClaims(scope)` / `parseScopeClaims(scp)` 是两个方向的唯一实现。

**中继令牌**（`aud = "relay"`）新增 `org`（源所属组织，限流与计量；personal 不发）与 `ro`（`true` = 只读）。**隧道令牌**（`aud = "relay-tunnel"`）新增 `org`。

**只读门**（0.3.11 起停用）：中继令牌仍带 `ro: true`（形状不变，供观察与计量），但边缘不再据它过滤请求，所有方法照常转给源。core 的 RPC 一律 `POST`，按方法挡会把只读访客整个挡在 RPC 之外；读写只由 core 按授予判定（断言的 `scp` 与本机邀请）。

0.3.0–0.3.10 的边缘对 `ro` 令牌只放行 `GET` / `HEAD` / `OPTIONS`、WebSocket 升级、媒体票（§12）与身份域 `/api/identity/`、`/api/rpc/identity/` 之下的路径，其余 HTTP 答 `403 forbidden`、WebSocket 以 4403 关闭。协议包的 `isReadOnlyAllowed(method, path)` 与 `schema/constants.json` 的 `tokens.readOnly` 保留（已弃用），中继不再调用。

**分享范围**：`links.create { kind: "source_invite", …, scope?: { workspaceId?, sessionId?, readOnly? } }`。

- 都缺省、`{}` 或只有 `readOnly: false` = 整台、按角色（不存、不回显）；
- `sessionId` 必须同时给 `workspaceId`，会话范围总是只读，显式 `readOnly: false` 答 `400 bad_request`；
- `links.get` 与 `links.list` / `links.update` 的摘要带规范化后的 `scope`（整台时不出现）；
- `links.accept` 与访客之后的 `sources.assertion` 签出的断言带 `scp`，只读时中继令牌带 `ro: true`；owner 的令牌没有范围。

服务端报能力 `links.scope` 才认 `scope`；不报的中继（0.2.x）会把它当未知字段丢掉、签出整台的断言，客户端在 `platform.info.capabilities` 里没看到它时不要提供范围分享，core 侧的邀请 `target_workspace_id` 仍按原样生效。

## §15 服务端名称

协议包 0.3.1（协议仍是 1.1）。`platform.info`（`GET /.well-known/armadra-platform`）多一个可选的 `name`：服务端给自己起的可读名称（去首尾空白后 1–128 个字符），客户端添加这个服务时用它作缺省显示名；用户在本机改过名的以本地名为准，清空恢复成它。旧客户端忽略这个字段，旧服务端不报时客户端用地址的主机部分兜底。

- **personal**：依次取 `--name`、`RELAY_NAME`、状态文件 `settings.displayName`（可省略，旧文件照常读），都没有时是 `--host`；每次请求现读，状态文件被别的进程改了即时生效。
- **saas**：字段预留——`platform.info` 现在与其余业务路由一样答 501，实现时按同一语义报（运营方配置的服务名）。

## §16 挑战

协议 1.2（协议包 0.3.5 定形状，0.3.6 起个人中转实现校验）。服务端可以要求登录、接受分享链接之前先过一道人机挑战（Cloudflare Turnstile）。在服务端报出 `platform.info.challenge` 之前，客户端带不带 `challenge` 行为都不变。

- **服务端声明**：`platform.info.challenge = { provider: "turnstile", siteKey, scope }`，`scope` 是要求挑战的 procedure（`auth.login`、`links.accept`，0.3.10 起还有 `auth.changePassword`，见 §18），`siteKey` 不是秘密；同时 `capabilities` 含 `challenge.turnstile`。不报 `challenge` = 不要求。
- **请求**：`auth.login` 与 `links.accept` 的输入多一个可选的 `challenge: { provider: "turnstile", token }`（`token` 1–2048 字符），是页面或手机里挑战组件交回的一次性令牌（有效 300 秒、只能用一次）。原生 App 在中继托管的同源挑战页里取令牌。
- **答复**：`scope` 列了这条 procedure 而请求没带 → `400 challenge_required`；令牌无效、过期、用过或提供方不认 → `400 challenge_invalid`。挑战先于口令校验与按账号的锁定计数，挑战失败不计入登录失败次数；按 IP 的限流照常先计。
- **core 作为客户端**（`sources.mount` 经 `links.accept`）没有挑战组件：personal 缺省只对 `auth.login` 要求，`links.accept` 由链接自带的 128 位秘密与按 IP 限流保护。
- **校验**（0.3.6 起）：中继向 Turnstile 的 siteverify 校验（带 `remoteip` 与幂等键；提供方没答或 5xx 时同一个幂等键重试一次，仍不行答 `challenge_invalid`）；同一个令牌在 300 秒内再出现直接 `challenge_invalid`。只在配置了站点密钥与 secret 时要求：Workers 中继是 `vars.RELAY_TURNSTILE_SITE_KEY` + secret `RELAY_TURNSTILE_SECRET`（录了 secret 却没有站点密钥是配错，答 `503 unavailable`），Node 中继是 `--challenge turnstile --turnstile-site-key --turnstile-secret-file`（缺省关）；范围是 `RELAY_CHALLENGE_SCOPE` / `--challenge-scope`，缺省 `auth.login`。日志只记原因码，令牌与 secret 不进日志、存储与响应。

## §17 中继节点地址与节点回报

协议 1.2（协议包 0.3.5）。

- **节点地址可带 query**：`sources.heartbeatHint`（`GET /v1/sources/me/relay`）答的 `nodes[].url` 是完整的 WebSocket 地址，可以带路径与 query，源原样连接；没有路径时补 `/t/v1`。Workers 上的中继答 `wss://<中继域>/t/v1?src=<sourceId>`，靠 `src` 在握手前把隧道路由到这个源的对象；Node 中继匹配 `/t/v1` 时忽略 query，两种地址对它等价。`src` 只是路由提示，身份仍以握手里隧道令牌的 `sub` 为准，两者不一致时中继拒绝这条隧道（4490 `tunnel_token_invalid`）。
- **Workers 中继的心跳**：心跳「源主动、中继被动」——`ready.heartbeatMs` 缺省 45 秒（`RELAY_HEARTBEAT_MS` 可调，10–60 秒），源按它发 `PING`，中继立即回 `PONG`、自己不发 `PING`；中继每个检查周期（缺省 60 秒）看一次，超过 `2 × heartbeatMs` 没收到源的任何帧就以 1001 关闭隧道并发 `sourceOffline`。源的静默看门狗按 `heartbeatMisses × heartbeatMs` 计，不受影响。
- **节点回报** `relay.nodePresence`（`POST /v1/relay/nodes/{node}/presence`）：不和控制面共用 Redis 的节点（Workers 中继的 saas 模式）用它把隧道上下线告诉控制面。认证是节点令牌（`Authorization: Bearer <节点令牌>`，scope `relay-node`，运营方给每个节点发一枚，经 secret 下发）。体 `{ node, ttlMs, events }`：`events` 1–256 条 `sourceOnline { sourceId, tunnelId, coreVersion, atMs }` / `sourceOffline { sourceId, tunnelId, atMs }`，可以重复报（幂等，`sourceOffline` 只删 `tunnelId` 相同的那条）；`ttlMs`（10 秒到 10 分钟）内没再报 `sourceOnline` 的路由由控制面当作过期。答 `{ accepted }`。personal 不实现（`404`），cloud 落地前答 `501 not_implemented`。

<!-- rpc:begin contract=§17 -->

| procedure            | 方法与路径                             | scope        | 错误码                         |
| -------------------- | -------------------------------------- | ------------ | ------------------------------ |
| `relay.nodePresence` | `POST /v1/relay/nodes/{node}/presence` | `relay-node` | `unauthenticated`、`forbidden` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §18 改口令

协议包 0.3.10（协议仍是 1.2）。服务端报能力 `auth.password-change` 才有这条；不报的中继答 `404 not_found`，客户端不要提供入口。

- **请求**：`POST /v1/auth/password`，`Authorization: Bearer <accessToken>`（账号本人；访客答 `403 forbidden`），体 `{ password, newPassword, challenge? }`——`password` 是旧口令。
- **次序**：按 IP 限流（与 `auth.login` 同一个桶，`rate_limited`）→ 挑战（`platform.info.challenge.scope` 含 `auth.changePassword` 才要，action `armadra-password`，规则同 §16）→ 锁定检查（`account_locked`）→ 新口令策略（与注册、初始化同一套，`password_too_short` 等）→ 比对旧口令（不对答 `401 credentials_invalid`，计入与登录同一个锁定计数）。
- **成功**：换口令记录、清锁，并在同一个写入里撤销账号本人除当前设备以外的全部设备（它们的访问令牌与刷新令牌即时失效；访客设备不受影响，由链接管）；当前设备的刷新令牌也换新（旧的作废、不当作重放），答 `{ session }`——当前设备的新会话，客户端用它替换保存的刷新令牌。
- **运维**：Node 中继的 `personal passwd` 走同一个实现，但不留设备（全部重新登录）；Workers 中继由 `apps/relay-workers/scripts/rotate-password.sh` 在运维本机调这条接口，新口令只打印在运维终端。口令、令牌不进日志、存储与响应。

<!-- rpc:begin contract=§18 -->

| procedure             | 方法与路径               | scope     | 错误码                                                                                                                                                                                                                           |
| --------------------- | ------------------------ | --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auth.changePassword` | `POST /v1/auth/password` | `session` | `credentials_invalid`、`account_locked`、`rate_limited`、`forbidden`、`challenge_required`、`challenge_invalid`、`password_too_short`、`password_too_long`、`password_too_common`、`password_contains_name`、`password_breached` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §20 端到端通道

协议 1.3（协议包 0.4.0）。客户端与源（core）之间经中继建一条端到端加密的通道 `armadra-e2e/1`，中继只转发密文，看不到方法、路径、头与正文。0.5.0 起这是源地址上唯一的入口（§7），明文路径已删除；源不报能力 `e2e`（旧 core）时答 4404 `e2e_unsupported`，客户端提示升级。

- **边缘面**：saas `wss://<sourceId>.src.<域>/_relay/e2e`，personal `wss://<中继>/s/<sourceId>/_relay/e2e`。`_relay/` 是中继的保留段，不进隧道。只认 WebSocket 升级；普通 HTTP 请求答 `404 not_found`。
- **子协议**：`armadra-e2e.1`（版本，中继选它答复）+ `armadra-relay.<中继令牌>`（与 §7 相同，中继剥掉）。缺版本子协议或版本不认识以 4409 关闭。
- **判定**：令牌校验、限流、按 `sourceId` 路由与 §7 相同（同一张拒绝表，WebSocket 一列）；源在线但隧道 `hello.capabilities` 没报 `e2e` → 4404，原因 `e2e_unsupported`（错误码仍是 `source_offline`）。中继令牌的 `ro` 对这条面不生效（中继看不到内层请求），读写由源判定。
- **隧道侧**：中继开一条流 `OPEN { kind: "e2e", method: "GET", path: "/_relay/e2e", headers: [], clientOrigin, remoteIp, relayNode }`，此后客户端 WebSocket 的二进制消息原样写进 `DATA`，`DATA` 原样作为二进制消息发回客户端；文本消息以 4400 关闭。消息边界不保证（中继可以合并或切分），通道自己按长度切单元。客户端关闭 → `END`；源 `END` / `RST` → 以 1000 / 4404 关闭；窗口与背压同隧道。`e2e` 流计入同一账号的并发 WebSocket 上限（256）。
- **中继令牌 `dev`**：可选声明，issuer 侧签发这张令牌的会话所属设备 id（与断言的 `device.deviceId` 相同）；中继只用它计数「来自哪些设备」，不参与鉴权。老令牌没有它。
- **通道格式**：以协议包 `./e2e` 为准（握手三条消息、AES-256-GCM 记录、r0 `accept`），黄金向量在 `packages/platform-protocol/fixtures/e2e/handshake.json`。中继不解析它。
- **日志与指标**：通道结束时记一条元数据（源、流号、两个方向的字节数、时长、关闭码），不记账号、设备、IP 与任何内容；指标 `relay_edge_streams_total{kind}` 计开出的流（0.5.0 起只有 `e2e`）。
- **活性**：Node 中继每 30 秒发 `ping`、60 秒没有任何消息或 `pong` 即断开（与 §7 相同）；Workers 中继由检查周期按空闲时长断开。客户端的通道自己另有内层 `PING`（协议包 `LIMITS_E2E.pingIntervalMs`）。
- **访客链接的身份指纹**（0.5.0 起）：`links.accept`（§10）的答案多可选的 `identityFp`——源登记的公钥（也就是它端到端通道的身份公钥）的指纹 `base32(SHA-256(pub))`，52 字符。它来自中继，只作提示与旧链接的兜底；core 生成的分享链接片段第三段（`<secret>.<邀请令牌>.<指纹前 16 位>`）才是访客钉扎的依据，两者不符时客户端拒绝。
- **实现**：Node 中继（`relay-runtime-node` 的 `e2e-pipe.ts`）与 Workers 中继（`SourceDO` 的 `e2e-bridge.ts`，休眠后照样接上）行为相同；签发方从 0.4.0 起在中继令牌里带 `dev`。

## §19 流量统计

协议包 0.5.0（协议仍是 1.3）。服务端报能力 `me.traffic` 才有这条；不报的中继答 `404 not_found`，客户端不要提供入口。

- **口径**：中继按「租户 × UTC 整小时 × 节点 × 源 × 中继令牌 `sub` × `dev` × 类别」累计连接数与两个方向的字节。字节是边缘经隧道流转的载荷（与 `/metrics` 的 `relay_bytes_total` 同源；明文 HTTP 含中继重新序列化的请求行与头，WebSocket 含帧头；端到端通道是记录层字节，与 core 的 `transport` 行相等）。`connections` 是开流次数：一个 HTTP 请求、一条 WebSocket、一次媒体票兑取或一条端到端通道各算一次。`dev` 是中继令牌的可选声明（0.4.0 起签发方都带），令牌没带时为空串。
- **类别** `kind`：`e2e`（端到端通道）、`http`（明文 HTTP）、`ws`（明文 WebSocket）、`media`（媒体票，§12）。
- **请求**：`GET /v1/me/traffic?fromMs&toMs&sourceId?&granularity=hour|day`，`Authorization: Bearer <accessToken>`。返回桶起点落在 `[fromMs, toMs)` 的行；`granularity=day` 按 UTC 自然日合并（缺省 `hour`）；`fromMs ≥ toMs` 答 `400 bad_request`。
- **可见范围**：账号本人看本租户全部源的统计（含已撤销的源留下的历史）；访客只看 `subject` 等于自己的行。`sourceId` 只是过滤，看不到的源答空。
- **答复**：`{ buckets: [{ bucketStartMs, node, sourceId, subject, deviceId, kind, connections, bytesToSource, bytesToClient }] }`，按 `bucketStartMs`、`node`、`sourceId`、`subject`、`deviceId`、`kind` 排序；超过 10 000 行答 `400 bad_request`，客户端缩小时间范围或按源过滤。
- **时效**：中继在内存里累加，每 60 秒批量写一次（Workers 中继在攒下新行后 30 秒内写，查询前也先写），刚发生的流量最多晚这么久才查得到；不按帧写库。
- **保留**：Node 中继 `--traffic-retention-days`（缺省 90，`0` = 不记录），Workers 中继 `RELAY_TRAFFIC_RETENTION_DAYS`（同义）；每天清一次早于保留期的桶。状态文件存储（`--store file`）只在内存里累计，接口照常答，重启归零。
- **隐私**：只记上面的元数据，不记路径、query、头、IP 与正文；限流桶不入库。

<!-- rpc:begin contract=§19 -->

| procedure    | 方法与路径           | scope     | 错误码        |
| ------------ | -------------------- | --------- | ------------- |
| `me.traffic` | `GET /v1/me/traffic` | `session` | `bad_request` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->
