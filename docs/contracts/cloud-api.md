# armadra-cloud 契约

> 控制面 `/v1/*`、`/.well-known/*` 与中继客户端面的线上形状。真相源是协议包 `@armadra/platform-protocol`（`packages/platform-protocol/`）；本文的形状表是生成块，由 `pnpm contract:generate` 从协议包重写，`pnpm contract:check` 校验不漂移。章节号 §N 只追加、不改号。

## §1 编码规则

- JSON 字段一律 camelCase；时间用毫秒整数，字段名以 `AtMs` / `Ms` 结尾。
- 错误形状固定为 `{ code, message, requestId?, details? }`；`code` 只取协议包 `./errors` 注册表里的 snake_case 码，HTTP 状态码查同一张表。
- 每个响应带 `Armadra-Protocol: <major>.<minor>` 头；客户端对未知 major 拒绝，对更高 minor 忽略未知字段（schema 非 strict）。
- 预留：SaaS 服务端的业务路由（总计划 §12.2）已在契约与路由表里，实现落地前统一答 501 `{ code: "not_implemented", message, requestId }`；`GET /health` 已实现。
- 认证：云会话用 `Authorization: Bearer <accessToken>`；源对控制面用 `Authorization: Source <jws>`；浏览器来源改用 `__Host-armadra-cloud` Cookie + `X-Armadra-CSRF`。
- **CSRF 值从哪来**（1.4 补充）：Cookie 会话的 CSRF 值由 `auth.login`、`auth.mfaVerify`、`auth.refresh` 的响应字段 `csrfToken` 给出，页面刷新后由 `me.get` 的 `csrfToken` 取回；前端只放内存。Cookie 会话上每个非 GET 请求必须带 `X-Armadra-CSRF`，值等于它，对不上答 `403 forbidden`。同一请求不同时带 Cookie 与 Bearer。Bearer 会话与 personal 不用 Cookie，响应里没有 `csrfToken`（字段可缺）。
- **Cookie 名与属性**（1.4 补充，最终）：普通会话两枚——访问 Cookie `__Host-armadra-cloud`、刷新 Cookie `__Host-armadra-cloud-refresh`；运营会话两枚——`__Host-armadra-ops`、`__Host-armadra-ops-refresh`。四枚都是 `Secure; HttpOnly; SameSite=Strict; Path=/`，不带 `Domain`（`__Host-` 前缀的要求），**不做路径限定**。请求里带了同名的重复 Cookie（同一个名字出现两次以上）一律答 `401 unauthenticated`，并在响应里清掉这些 Cookie，客户端重新登录。刷新 Cookie 免 CSRF，见下。
- **运营会话 Cookie**（1.4 补充）：运营范围的会话是另一枚 Cookie `__Host-armadra-ops`，不覆盖普通会话 `__Host-armadra-cloud`，两者可同时存在；它同样配 `X-Armadra-CSRF`（值取自签出它的那次 `auth.login` / `auth.mfaVerify` 响应的 `csrfToken`，用于 `__Host-armadra-ops` 的请求），寿命 1 小时，只对 `admin.*` 与它自己的 `auth.logout` / `auth.refresh` 有效，其余 procedure 一律不认。详见 §28。
- **Cookie 会话的刷新与登出**（1.4 补充）：`auth.refresh` 的 `refreshToken` 可省——Cookie 会话的刷新凭据在刷新 Cookie 里（`__Host-armadra-cloud-refresh` / `__Host-armadra-ops-refresh`），不进请求体；Bearer 客户端（原生、核心）照旧必带。`auth.refresh` 与 `auth.logout` 多可选入参 `scope?: "operator"`：不带作用于普通控制台会话（`__Host-armadra-cloud`），带则作用于运营会话（`__Host-armadra-ops`）；各自要对应那枚 Cookie 的 `X-Armadra-CSRF`。personal 不用 Cookie、没有运营账号：`refreshToken` 缺了答 `session_revoked`（与令牌无效同码），`scope: "operator"` 答 `403 forbidden`（与 `auth.login` 一致）。两个字段都是入参上的可选追加，已部署中继不受影响。
- **`auth.refresh` 免 CSRF**（1.4 补充）：页面刷新后前端还没有 CSRF 值，所以 `auth.refresh` 不要求 `X-Armadra-CSRF`（普通与运营会话都一样）；刷新 Cookie（`__Host-armadra-cloud-refresh` / `__Host-armadra-ops-refresh`）是 `SameSite=Strict`，跨站请求带不上，所以不需要；它不做路径限定（`Path=/`，`__Host-` 前缀不允许别的路径），但服务端只在 `auth.refresh` 与 `auth.logout` 上读它。其余所有 Cookie 认证的写请求（含 `auth.logout`）照旧必带；`auth.refresh` 的响应返回新的 `csrfToken`。
- **GET 里的对象入参**（1.4 补充）：对象入参一律写成方括号的 deepObject，如 `space[kind]=team&space[teamId]=t1`；不用点号，也不把 JSON 串塞进一个参数。服务端（含 Go 的 personal 路由）只按这种写法解析。

## §2 账号与会话

1.4 补充（控制台所需，Refs #60）：

- `auth.stepUp`（`POST /v1/auth/step-up`，scope `session`）：在已有会话上再验证。入参 `{ password?, totpCode?, passkeyAssertion? }`，至少给一项（都缺答 `400 bad_request`），口令或验证码不对答 `401 credentials_invalid` / `401 mfa_invalid`；成功答 `{ elevatedUntilMs }`，该会话在这之前（自此 10 分钟）算已提权，注销账号、转移所有权等敏感操作据此放行。
- `auth.login` 入参 `scope?: "operator"`：要运营范围的 Cookie 会话，规则见 §28；响应字段 `csrfToken` 见 §1。
- `auth.refresh` 入参 `refreshToken` 可省，`auth.refresh` / `auth.logout` 多可选 `scope?: "operator"`：规则见 §1「Cookie 会话的刷新与登出」与 §28。
- **SaaS 的链接访客**（`guest:<linkId>:<随机>`，经 `links.accept` 拿到 `guestSession`，Bearer 传输）：`auth.refresh` 照常轮换，答复的 `session.account` 与 `guestSession` 同形（`accountId` 是访客主体、`email` 为 `null`），刷新出来的仍是访客会话。访客须仍站得住才续得上：链接未撤销且指向一台未撤销的主机、团队没停用也不在恢复期、团队与主机的 `allowGuests` 都开着；任一不成立答 `401 session_revoked`，不区分原因（站不住时不轮换，条件恢复后同一枚刷新令牌还能用；撤链接会同时作废访客设备）。`me.sources` 对访客只答链接指向的那一台，`sourceSummary` 只带必需字段（`owner = false`、`via = "link"`，不带 `ownerName`、`space`、`tags`），站不住时答空列表。访客能调的 `me.*` 只有 `me.sources` 与 `me.streamTicket`（加 `GET /v1/me/stream`）；`me.get`、`me.update`、`me.devices`、`me.deviceRevoke`、`me.spaces`、`me.emailChange`、`me.securityLog` 与 §29 其余的 `me.*` 一律 `403 forbidden`（个别先校验入参的可能先答 `400 bad_request`），`sources.*` 与 `links.*` 的账号过程同样。
- `auth.register` 在运营方事故开关关闭注册时答 `503 registrations_paused`；`platform.info.registrationsOpen` 报当前是否开放（personal 恒为 `true`），注册页据此直接显示关闭态。已部署的服务端不报这个字段：缺省时客户端按开放处理。

<!-- rpc:begin contract=§2 -->

| procedure                     | 方法与路径                               | scope       | 错误码                                                                                                                                                                                                            |
| ----------------------------- | ---------------------------------------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `platform.info`               | `GET /.well-known/armadra-platform`      | `anonymous` | —                                                                                                                                                                                                                 |
| `platform.jwks`               | `GET /.well-known/jwks.json`             | `anonymous` | —                                                                                                                                                                                                                 |
| `platform.health`             | `GET /health`                            | `anonymous` | `unavailable`                                                                                                                                                                                                     |
| `auth.register`               | `POST /v1/auth/register`                 | `anonymous` | `email_taken`、`rate_limited`、`registrations_paused`、`challenge_required`、`challenge_invalid`、`password_too_short`、`password_too_long`、`password_too_common`、`password_contains_name`、`password_breached` |
| `auth.login`                  | `POST /v1/auth/login`                    | `anonymous` | `credentials_invalid`、`account_locked`、`account_disabled`、`rate_limited`、`challenge_required`、`challenge_invalid`、`forbidden`、`operator_mfa_required`                                                      |
| `auth.mfaVerify`              | `POST /v1/auth/mfa/verify`               | `anonymous` | `mfa_invalid`、`mfa_expired`、`account_locked`                                                                                                                                                                    |
| `auth.refresh`                | `POST /v1/auth/refresh`                  | `anonymous` | `session_expired`、`session_revoked`、`forbidden`                                                                                                                                                                 |
| `auth.stepUp`                 | `POST /v1/auth/step-up`                  | `session`   | `credentials_invalid`、`mfa_invalid`、`rate_limited`、`bad_request`                                                                                                                                               |
| `auth.logout`                 | `POST /v1/auth/logout`                   | `session`   | `forbidden`                                                                                                                                                                                                       |
| `auth.deviceStart`            | `POST /v1/auth/device/start`             | `anonymous` | `rate_limited`                                                                                                                                                                                                    |
| `auth.devicePoll`             | `POST /v1/auth/device/poll`              | `anonymous` | `rate_limited`                                                                                                                                                                                                    |
| `auth.deviceApprove`          | `POST /v1/auth/device/approve`           | `session`   | `device_code_expired`、`not_found`                                                                                                                                                                                |
| `auth.passkeyRegisterOptions` | `POST /v1/auth/passkey/register/options` | `session`   | —                                                                                                                                                                                                                 |
| `auth.passkeyRegister`        | `POST /v1/auth/passkey/register`         | `session`   | —                                                                                                                                                                                                                 |
| `auth.passkeyLoginOptions`    | `POST /v1/auth/passkey/login/options`    | `anonymous` | `rate_limited`                                                                                                                                                                                                    |
| `auth.passkeyLogin`           | `POST /v1/auth/passkey/login`            | `anonymous` | `credentials_invalid`、`rate_limited`                                                                                                                                                                             |
| `auth.totpEnroll`             | `POST /v1/auth/totp/enroll`              | `session`   | `conflict`                                                                                                                                                                                                        |
| `auth.totpConfirm`            | `POST /v1/auth/totp/confirm`             | `session`   | `mfa_invalid`                                                                                                                                                                                                     |
| `auth.totpDisable`            | `POST /v1/auth/totp/disable`             | `session`   | `mfa_invalid`                                                                                                                                                                                                     |
| `auth.recoveryCodesRotate`    | `POST /v1/auth/recovery-codes/rotate`    | `session`   | `mfa_invalid`                                                                                                                                                                                                     |
| `me.get`                      | `GET /v1/me`                             | `session`   | —                                                                                                                                                                                                                 |
| `me.update`                   | `PATCH /v1/me`                           | `session`   | `bad_request`                                                                                                                                                                                                     |
| `me.sources`                  | `GET /v1/me/sources`                     | `session`   | —                                                                                                                                                                                                                 |
| `me.devices`                  | `GET /v1/me/devices`                     | `session`   | —                                                                                                                                                                                                                 |
| `me.deviceRevoke`             | `DELETE /v1/me/devices/{deviceId}`       | `session`   | `not_found`                                                                                                                                                                                                       |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §3 组织（已移除，被 §21 起取代）

协议 1.4 起删除。`orgs.*`（组织、成员、主机挂接）从未实现、从未签发过 `org` 声明；SaaS 的租户改为团队，见 §21 起各节与设计 `saas-design.md`。`org` 断言声明保留给以后团队之上的企业组织层（§30）。错误码 `org_role_insufficient`、`org_slug_taken` 只增不删地保留在注册表里，但不再被任何 procedure 使用，新代码用 `team_role_insufficient`、`team_slug_taken`。

## §4 源目录与断言

1.4 补充（控制台所需，Refs #60）：

- `sourceSummary.ownerName`：主机所有者的显示名（`owner: boolean` 保留）；personal 是账号名，访客看不到所有者时为空串。已部署的中继不报这个字段：缺省时客户端什么都不显示。
- `sources.list` 入参加 `status`（`online` / `offline` / `revoked`）、`tag`（多个标签取「且」；GET 里重复写）、`q`（名称模糊）、`cursor`、`limit`（1–200，缺省 50）；出参加 `nextCursor`（`null` 或缺省 = 没有下一页；已部署的中继不报这个字段，客户端按没有下一页处理）。personal 的目录一页放得下，没有标签：`status = revoked` 与给了 `tag` 都筛出空，`nextCursor` 恒为 `null`。
- 主机登记（1.4 补充，Refs #60）：`sources.register` 在 SaaS 里于一个事务内消耗注册令牌、核版本门（团队主机的 `coreVersion` 低于 `MIN_CORE_VERSION_TEAM_ASSERTION` 或解析不了答 `426 protocol_unsupported`；个人空间不设门）、锁空间行后核主机配额（`429 limit_reached`，`details = { quota: "hosts", used, limit }`）与团队状态（`403 team_suspended`）；任何一步失败整个事务回滚，注册令牌不被消耗（它十分钟后自己过期）。令牌签发之后账号不再是团队的可登记成员、团队进入恢复期、或成员登记开关关了，令牌作废，答 `401 registration_token_invalid`。已登记且未撤销的 `sourceId` 只能由登记它的账号在同一空间里用同一把钥重新登记（幂等，不占第二个名额），其余答 `409 source_taken`。SaaS 里每个账号同时有效的注册令牌至多 20 枚。`sources.heartbeatHint` 在 SaaS 的节点表为空时答 `503 unavailable`，不兜底到共用地址。

<!-- rpc:begin contract=§4 -->

| procedure                   | 方法与路径                                         | scope                  | 错误码                                                                                                                                              |
| --------------------------- | -------------------------------------------------- | ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sources.registrationToken` | `POST /v1/sources/registration-tokens`             | `session`              | `email_unverified`、`limit_reached`、`team_role_insufficient`、`team_policy_denied`、`host_registration_disabled`、`team_suspended`、`team_deleted` |
| `sources.register`          | `POST /v1/sources/register`                        | `anonymous`            | `registration_token_invalid`、`source_taken`、`protocol_unsupported`、`rate_limited`、`limit_reached`、`team_suspended`                             |
| `sources.heartbeatHint`     | `GET /v1/sources/me/relay`                         | `source`               | `source_revoked`、`unauthenticated`、`unavailable`                                                                                                  |
| `sources.list`              | `GET /v1/sources`                                  | `session`              | —                                                                                                                                                   |
| `sources.get`               | `GET /v1/sources/{sourceId}`                       | `session`              | `not_found`                                                                                                                                         |
| `sources.update`            | `PATCH /v1/sources/{sourceId}`                     | `owner`                | `not_found`、`bad_request`                                                                                                                          |
| `sources.revoke`            | `DELETE /v1/sources/{sourceId}`                    | `owner`                | `not_found`                                                                                                                                         |
| `sources.assertion`         | `POST /v1/sources/{sourceId}/assertion`            | `session`              | `source_access_denied`、`source_revoked`、`rate_limited`、`team_suspended`                                                                          |
| `sources.accessList`        | `GET /v1/sources/{sourceId}/access`                | `owner`                | `not_found`                                                                                                                                         |
| `sources.accessRevoke`      | `DELETE /v1/sources/{sourceId}/access/{accountId}` | `owner`                | `not_found`                                                                                                                                         |
| `sources.transferStart`     | `POST /v1/sources/{sourceId}/transfer`             | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`transfer_pending`、`limit_reached`、`bad_request`                                   |
| `sources.transferList`      | `GET /v1/source-transfers`                         | `session`              | —                                                                                                                                                   |
| `sources.transferAccept`    | `POST /v1/source-transfers/{transferId}/accept`    | `owner` / `team:admin` | `not_found`、`team_role_insufficient`、`transfer_expired`、`limit_reached`、`conflict`                                                              |
| `sources.transferCancel`    | `DELETE /v1/source-transfers/{transferId}`         | `owner` / `team:admin` | `not_found`、`team_role_insufficient`、`conflict`                                                                                                   |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §5 链接

<!-- rpc:begin contract=§5 -->

| procedure      | 方法与路径                       | scope                   | 错误码                                                                                                                                                                        |
| -------------- | -------------------------------- | ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `links.create` | `POST /v1/links`                 | `owner` / `team:admin`  | `source_access_denied`、`team_role_insufficient`、`team_policy_denied`、`guests_disabled`、`bad_request`                                                                      |
| `links.get`    | `GET /v1/links/{linkId}`         | `anonymous`             | `link_invalid`                                                                                                                                                                |
| `links.accept` | `POST /v1/links/{linkId}/accept` | `session` / `anonymous` | `link_invalid`、`link_expired`、`link_exhausted`、`link_secret_invalid`、`link_frozen`、`anonymous_links_disabled`、`rate_limited`、`challenge_required`、`challenge_invalid` |
| `links.list`   | `GET /v1/links`                  | `owner` / `team:admin`  | —                                                                                                                                                                             |
| `links.update` | `PATCH /v1/links/{linkId}`       | `owner` / `team:admin`  | `not_found`                                                                                                                                                                   |
| `links.revoke` | `DELETE /v1/links/{linkId}`      | `owner` / `team:admin`  | `not_found`                                                                                                                                                                   |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

`links.get` 的答复带可选 `ownerName`（链接主人的名字，协议包 0.11.0 起，只加不改）：落地页据此写「加入 {ownerName} 的 {sourceName}」。只给显示名，不给邮箱与账号 id；匿名可读，所以团队主机的链接给团队名（不暴露登记主机的成员），个人空间给主人的显示名；账号停用、清理后为空、读取失败时不出现该字段（不回退成邮箱前缀，落地页照常）。

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
- 边缘限流分层（WebSocket 以 4429 关闭）：带有效中继令牌的升级先按令牌 `jti`（缺省每分钟 1200 次）、再按账号 `sub`（缺省 3600 次）计数，不看 IP；无令牌、令牌无效或 `src` 不符的按 IP 严格限流（缺省每分钟 60 次）；同一账号同时开着的通道缺省最多 256 条（不可配），满了 4429 `limit_reached`。经节点间转发的通道另按每账号每入口节点缺省 64 条（`RELAY_FORWARDED_PER_ACCOUNT`）限制，超出同样 4429 `limit_reached`。令牌校验是本地验签，先验后计。personal 的额度可由 `serve` 参数、环境变量或状态文件 `settings.rateLimits` 配置（见 deploy/README.md；`preflight` 一项 0.5.0 起不再使用）；登录（按 IP 每分钟 20 次）与账号锁定不可配、不变。
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

## §9 计费（已移除，被 §21 起取代）

协议 1.4 起删除。原 `billing.plan` / `billing.usage` 挂在 `/v1/orgs/{orgId}/…` 之下；计费现在按空间（个人 / 团队），路径 `/v1/billing/…`，见 §26。

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

> 1.4 变更：`team` 断言声明改为租户团队的 `{ teamId, role }`，中继令牌与隧道令牌的 `org` 换成 `team`；1.1 里的部门语义（`lead` / `member`）从未签发过。以下表格是 1.1 当时的文字，现行形状见 §30。

协议 1.1（协议包 0.3.0）。全部是可选的新声明与字段：缺省时签出的令牌与 1.0 逐字同形，core 按 1.0 处理（老中继签的断言照常可用）。core 是唯一的读写判定点（0.3.11 起中继边缘不再按 `ro` 过滤，见下文「只读门」）。

**断言**（`aud = sourceId`）新增：

| 声明   | 形状                                                                                                          | 含义                                                                                |
| ------ | ------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `scp`  | `string[]`，每项 `ws:<workspaceId>`、`sess:<sessionId>` 或 `ro`（id 为 `[A-Za-z0-9_.:-]{1,128}`，最多 64 项） | 范围：列出的工作空间 / 会话；`ro` = 只读。缺省或空 = 整台主机、按角色可写           |
| `role` | `viewer` / `editor` / `operator` / `driver`                                                                   | 主机授予的角色（SaaS 按 `source_grants` 合成）；缺省时 core 按邀请 / 本地设置决定   |
| `team` | `{ teamId, role: "lead" \| "member" }`                                                                        | 已变更（1.4）：现为 `{ teamId, role: "owner" \| … \| "guest" }`，见 §30；`org` 不变 |

`scp` 的顺序固定为 `ws:`、`sess:`、`ro`；会话项总是伴随它所在的工作空间项与 `ro`。协议包的 `scopeClaims(scope)` / `parseScopeClaims(scp)` 是两个方向的唯一实现。

**中继令牌**（`aud = "relay"`）新增 `org`（1.4 起换成 `team`，见 §30）与 `ro`（`true` = 只读）。**隧道令牌**（`aud = "relay-tunnel"`）新增 `org`（1.4 起同样换成 `team`，见 §30）。

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
- **saas**：`ARMADRA_CLOUD_NAME`（运营方配置的服务名）原样报，超过 128 个 UTF-16 码元按码元截断，没配不报。

## §16 挑战

协议 1.2（协议包 0.3.5 定形状，0.3.6 起个人中转实现校验）。服务端可以要求登录、接受分享链接之前先过一道人机挑战（Cloudflare Turnstile）。在服务端报出 `platform.info.challenge` 之前，客户端带不带 `challenge` 行为都不变。

- **服务端声明**：`platform.info.challenge = { provider: "turnstile", siteKey, scope }`，`scope` 是要求挑战的 procedure（`auth.login`、`links.accept`，0.3.10 起还有 `auth.changePassword`，见 §18），`siteKey` 不是秘密；同时 `capabilities` 含 `challenge.turnstile`。不报 `challenge` = 不要求。
- **请求**：`auth.login` 与 `links.accept` 的输入多一个可选的 `challenge: { provider: "turnstile", token }`（`token` 1–2048 字符），是页面或手机里挑战组件交回的一次性令牌（有效 300 秒、只能用一次）。原生 App 在中继托管的同源挑战页里取令牌。
- **答复**：`scope` 列了这条 procedure 而请求没带 → `400 challenge_required`；令牌无效、过期、用过或提供方不认 → `400 challenge_invalid`。挑战先于口令校验与按账号的锁定计数，挑战失败不计入登录失败次数；按 IP 的限流照常先计。
- **注册与找回**（SaaS 控制面，协议包下一版起，0.11.0）：`auth.register` 与 `auth.passwordResetStart` 的输入同样多一个可选的 `challenge`，`scope` 可含 `auth.register`（action `armadra-register`）与 `auth.passwordResetStart`（action `armadra-reset`），规则同上；先于口令策略、晚于按 IP 限流，挑战失败不计入任何失败次数。personal 不实现这两个过程，也不接受它们作范围。**客户端应忽略 `platform.info.challenge.scope` 里不认识的项**（以后可能再加新项）；协议包的输出侧解析会过滤未知项。
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
- **实现**：Go 中继（`go/internal/edge` 的端到端管道）与 Workers 中继（`SourceDO` 的 `e2e-bridge.ts`，休眠后照样接上）行为相同；签发方从 0.4.0 起在中继令牌里带 `dev`。

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

## §21 团队

1.4 补充：`teams.members` 每行多 `groups: { groupId, name }[]`（M2，guest 恒空）与 `reachableHosts`（只对 guest 有值：被直接授予、能到达的主机数）。

协议 1.4（协议包 0.10.0）。SaaS 的租户是团队：每个账号自带一个个人空间，也可以创建或加入多个团队；主机、授予、链接、配额、计费与审计都挂在空间上（设计 `saas-design.md` §1）。本节与 §22–§29 的 procedure 现在全是预留：personal 答 `404 not_found`，cloud 在实现落地前答 `501 not_implemented`，实现时逐条拿掉。

- **scope** 里的 `team:*` 是调用者在路径（或体里）那个团队里的最低角色：`team:member` 任一角色（含 guest）；`team:admin` owner / admin；`team:owner` 仅 owner；`team:billing` owner / billing。具体的能做 / 不能做表见设计 §2.1，处理器还要按它收窄（例如 billing 只看成员列表、guest 只看被直接授予的主机）。
- **角色** `owner` / `admin` / `member` / `billing` / `guest`（`guest` 是外部协作者，不占席位，不能进分组，团队设置可关）。每个团队至少一个 owner：最后一个 owner 不能离开、降级或被移除（`409 team_last_owner`），要先 `teams.transferOwnership`（目标必须是 admin）。
- **删除**：`teams.delete` 先要求没有在线主机（有则答 `409 team_hosts_online`，`details.onlineHosts` 是在线主机数，协议包 0.11.0 起，只加不改；其余冲突仍是通用 `conflict`），进入 30 天恢复期（`purgeAtMs`），期内 `teams.restore`，之后清除；恢复期内对该团队的其余调用答 `410 team_deleted`。已是成员再被邀请 / 加入答 `409 member_exists`。
- **设置** `teams.settings`：M1 为 `membersCanRegisterHosts`、`defaultHostRole`、`allowGuests`、`allowAnonymousLinks`、新设备背书人 `deviceEndorsers` / `memberSelfEndorse`；M2 起生效的 `requireMfa`、`sessionMaxAgeMs`、`allowedEmailDomains`。设置禁止某个操作时答 `403`：成员登记主机 `host_registration_disabled`、guest `guests_disabled`、匿名接受分享链接 `anonymous_links_disabled`（接受链接 `links.accept`），其余设置项答通用的 `team_policy_denied`，`details.setting` 写是哪一项；界面按码取文案。当前计划不含的功能（如分组、访问申请、强制两步验证）答 `403 plan_feature_unavailable`，与按数量计的 `429 limit_reached` 区分。
- **配额**：只有 Free 时硬上限答 `429 limit_reached`，`details` 是 `{ quota, used, limit }`（协议包 `./errors` 的 `limitReachedDetailsSchema`）；`quota` 取 `teams`（个人账号能建的团队数）、`hosts`、`members`、`guests`（团队内数量）、`concurrent`（同时连接数）、`traffic`（本月中继流量，字节）。`teams.usage` / `billing.usage` 的用量项 `{ metric, value, limit }` 的 `metric` 取同一张枚举。
- 团队被运营方停用后拒绝签断言与登记主机（`403 team_suspended`）。
- 未验证邮箱的账号可以登录，但不能建团队、登记主机、签链接、被授予（`403 email_unverified`）。

<!-- rpc:begin contract=§21 -->

| procedure                 | 方法与路径                                      | scope         | 错误码                                                                                 |
| ------------------------- | ----------------------------------------------- | ------------- | -------------------------------------------------------------------------------------- |
| `teams.create`            | `POST /v1/teams`                                | `session`     | `team_slug_taken`、`bad_request`、`email_unverified`、`limit_reached`                  |
| `teams.list`              | `GET /v1/teams`                                 | `session`     | —                                                                                      |
| `teams.get`               | `GET /v1/teams/{teamId}`                        | `team:member` | `not_found`                                                                            |
| `teams.update`            | `PATCH /v1/teams/{teamId}`                      | `team:admin`  | `not_found`、`team_role_insufficient`、`team_deleted`                                  |
| `teams.delete`            | `DELETE /v1/teams/{teamId}`                     | `team:owner`  | `not_found`、`team_role_insufficient`、`team_deleted`、`conflict`、`team_hosts_online` |
| `teams.restore`           | `POST /v1/teams/{teamId}/restore`               | `team:owner`  | `not_found`、`team_role_insufficient`、`team_deleted`                                  |
| `teams.transferOwnership` | `POST /v1/teams/{teamId}/transfer-ownership`    | `team:owner`  | `not_found`、`team_role_insufficient`、`team_deleted`、`bad_request`                   |
| `teams.members`           | `GET /v1/teams/{teamId}/members`                | `team:member` | `not_found`、`team_role_insufficient`、`team_deleted`                                  |
| `teams.memberRole`        | `PATCH /v1/teams/{teamId}/members/{accountId}`  | `team:admin`  | `not_found`、`team_role_insufficient`、`team_deleted`、`team_last_owner`               |
| `teams.memberRemove`      | `DELETE /v1/teams/{teamId}/members/{accountId}` | `team:admin`  | `not_found`、`team_role_insufficient`、`team_deleted`、`team_last_owner`               |
| `teams.leave`             | `POST /v1/teams/{teamId}/leave`                 | `team:member` | `not_found`、`team_role_insufficient`、`team_deleted`、`team_last_owner`               |
| `teams.settings`          | `GET /v1/teams/{teamId}/settings`               | `team:member` | `not_found`、`team_role_insufficient`、`team_deleted`                                  |
| `teams.settingsUpdate`    | `PATCH /v1/teams/{teamId}/settings`             | `team:admin`  | `not_found`、`team_role_insufficient`、`team_deleted`、`bad_request`                   |
| `teams.usage`             | `GET /v1/teams/{teamId}/usage`                  | `team:member` | `not_found`、`team_role_insufficient`、`team_deleted`                                  |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §22 邀请

协议 1.4。两类邀请共用一组 procedure：团队邀请（按邮箱、指定角色 `admin` / `member` / `billing` / `guest`、7 天有效）和主机邀请（按邮箱邀请到某台主机，可限工作空间，接受后成为该主机的 guest 授予）。

- 邮件里的链接带一次性 `token`；`invitations.get` 匿名可调，只答摘要（遮掩后的邮箱、团队 / 主机名、角色、邀请人），不泄露更多。接受要登录云账号且邮箱与被邀请的一致（`403 invitation_email_mismatch`）。
- 过期答 `410 invitation_expired`，已撤销答 `410 invitation_revoked`，其余无效情形答 `404 not_found`。接受是幂等的：已接受的再次接受答原结果。
- 主机邀请由云端实现为**云端授予加 guest 成员关系**，不落成 core 的邀请：接受后控制面登记一条对该主机的授予（`grants`，§24），之后按授予向 core 签带 `role` / `scp` 的断言。分享链接仍落成 core 的邀请，core 的接受与消费逻辑不变（设计 §11.3）。
- `invitations.resend`：换新令牌、重新计 7 天、旧链接作废，按邀请限流。
- **`invitations.preview`**（协议包 0.11.0 起，只加不改）：`POST /v1/invitations/{invitationId}/preview`，令牌放请求体，答复与错误同 `invitations.get`。`invitations.get` 的 `?token=` 会进代理与访问日志，**已弃用**，客户端应改用 `preview`；服务端暂保留 `get` 以兼容。
- `invitations.list` 的 `box = sent`（缺省）列自己能管的邀请，`received` 列发给自己邮箱的待处理邀请。
- **主机邀请的两个有效期**（SaaS M1 实现）：邀请自身恒为 7 天（`invitations.expiresAtMs`，只管多久内可接受，`resend` 重新计）；创建入参里的 `expiresAtMs` 是接受后**授予的到期**（必须在将来、至多 30 天；不给 = 授予不过期）。接受时授予的到期已过按 `410 invitation_expired` 答。团队主机上，受邀者不是团队 owner / admin / member 时接受后成为 guest 成员，所以创建时 `workspaceIds` 不能为空（`400 bad_request`）；接受答 `{ sourceId }`，团队主机另带 `teamId`。已有同主体的有效授予时合成一条（会降低他已有的角色时答 `409 conflict`，需管理者用 `grants.update` 明确处理）：范围取并集（任一整台即整台）、角色取「使范围内每一处都被角色 ≥ 它的来路覆盖」的最高角色（不放大：driver@w1 加 viewer 整台是 viewer 整台）、到期取对结果有贡献的来路里最早的非空到期（直接用 `composeSources`，与 `Compose` 同一规则）；接受前后他的有效权限收窄时（含新增一条受限授予压低了别的来路）撤掉已签的令牌并踢人（reason `invitation_merged`）。 接受时主机设置关了 guest 答 `403 guests_disabled`，团队设置关了答 `403 team_policy_denied`；`list` 带 `sourceId` 看不到主机答 `404 not_found`；个人空间主机邀请的 `revoke` / `resend` 管不了答 `403 source_access_denied`；`resend` 时授予到期已过答 `400 bad_request`。成员被移除 / 降级、授予被撤销或收窄时，发给其邮箱的待处理主机邀请随之作废。

<!-- rpc:begin contract=§22 -->

| procedure             | 方法与路径                                    | scope                  | 错误码                                                                                                                                                                                                                          |
| --------------------- | --------------------------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `invitations.create`  | `POST /v1/invitations`                        | `team:admin` / `owner` | `team_role_insufficient`、`source_access_denied`、`team_policy_denied`、`guests_disabled`、`member_exists`、`email_unverified`、`limit_reached`、`conflict`、`rate_limited`、`bad_request`                                      |
| `invitations.list`    | `GET /v1/invitations`                         | `session`              | `team_role_insufficient`、`source_access_denied`、`not_found`                                                                                                                                                                   |
| `invitations.revoke`  | `DELETE /v1/invitations/{invitationId}`       | `team:admin` / `owner` | `not_found`、`team_role_insufficient`、`source_access_denied`                                                                                                                                                                   |
| `invitations.resend`  | `POST /v1/invitations/{invitationId}/resend`  | `team:admin` / `owner` | `not_found`、`team_role_insufficient`、`invitation_revoked`、`source_access_denied`、`bad_request`、`conflict`、`rate_limited`                                                                                                  |
| `invitations.get`     | `GET /v1/invitations/{invitationId}`          | `anonymous`            | `not_found`、`invitation_expired`、`invitation_revoked`、`rate_limited`                                                                                                                                                         |
| `invitations.preview` | `POST /v1/invitations/{invitationId}/preview` | `anonymous`            | `not_found`、`invitation_expired`、`invitation_revoked`、`rate_limited`                                                                                                                                                         |
| `invitations.accept`  | `POST /v1/invitations/{invitationId}/accept`  | `session`              | `not_found`、`invitation_expired`、`invitation_revoked`、`invitation_email_mismatch`、`email_unverified`、`limit_reached`、`team_policy_denied`、`member_exists`、`guests_disabled`、`team_deleted`、`conflict`、`rate_limited` |
| `invitations.decline` | `POST /v1/invitations/{invitationId}/decline` | `session`              | `not_found`、`invitation_expired`、`invitation_revoked`、`invitation_email_mismatch`                                                                                                                                            |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §23 分组

协议 1.4（M2）。分组是团队内的一组成员（部门、项目组），是授予的主体之一，有组长。组员必须是团队成员（不含 guest，否则 `400 bad_request`）。组长可以增删本组成员；建、改、删分组与指定组长要团队 admin。删除分组即撤销以它为主体的授予。数据模型 M1 就建好，接口 M2 起实现。

<!-- rpc:begin contract=§23 -->

| procedure             | 方法与路径                                                         | scope                       | 错误码                                                                                         |
| --------------------- | ------------------------------------------------------------------ | --------------------------- | ---------------------------------------------------------------------------------------------- |
| `groups.list`         | `GET /v1/teams/{teamId}/groups`                                    | `team:member`               | `not_found`、`team_role_insufficient`                                                          |
| `groups.create`       | `POST /v1/teams/{teamId}/groups`                                   | `team:admin`                | `not_found`、`team_role_insufficient`、`limit_reached`、`plan_feature_unavailable`、`conflict` |
| `groups.update`       | `PATCH /v1/teams/{teamId}/groups/{groupId}`                        | `team:admin`                | `not_found`、`team_role_insufficient`、`conflict`                                              |
| `groups.delete`       | `DELETE /v1/teams/{teamId}/groups/{groupId}`                       | `team:admin`                | `not_found`、`team_role_insufficient`                                                          |
| `groups.members`      | `GET /v1/teams/{teamId}/groups/{groupId}/members`                  | `team:member`               | `not_found`、`team_role_insufficient`                                                          |
| `groups.memberAdd`    | `POST /v1/teams/{teamId}/groups/{groupId}/members`                 | `team:admin` / `group:lead` | `not_found`、`team_role_insufficient`、`bad_request`、`conflict`                               |
| `groups.memberRemove` | `DELETE /v1/teams/{teamId}/groups/{groupId}/members/{accountId}`   | `team:admin` / `group:lead` | `not_found`、`team_role_insufficient`                                                          |
| `groups.setLead`      | `PUT /v1/teams/{teamId}/groups/{groupId}/members/{accountId}/lead` | `team:admin`                | `not_found`、`team_role_insufficient`                                                          |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §24 授予与访问申请

协议 1.4。云端只管「谁能以什么角色到达哪台主机」，意图经断言（`team`、`role`、`scp`）带进 core，最终判定仍在 core（设计 §11.2）。

- **主体** `subject`：`{ kind: "team", teamId }`（团队全员）、`{ kind: "member", accountId }`、`{ kind: "group", groupId }`（M2）、`{ kind: "guest", accountId }`。**角色**为 core 四档 `viewer` / `editor` / `operator` / `driver`；**范围** `workspaceIds` 空 = 整台主机，否则只含列出的工作空间；可设 `expiresAtMs`。
- **有效权限**：同一个人从多条授予里取最高角色、范围取并集（`grants.effective` 答合成结果与各来源）；断言只带结果，合成不落库。
- **撤销即生效**：写撤销表（`jti`）并通知中继踢掉在线连接；下次登录时 core 清掉该主体的云端授予，core 侧授予另有 30 天租约兜底。
- **谁能管**：主机 owner，或（团队主机）团队 admin；member 只能管自己是 owner 的主机的授予。
- **访问申请**（M2）：成员对看得到但不可达的主机申请访问（角色、可选范围与理由），主机 owner 或团队 admin 批准（可改小角色 / 范围）或拒绝；`accessRequests.list` 的 `box = inbox`（缺省）是自己能处理的，`mine` 是自己提的。重复申请答 `409 conflict`。
- 主机目录里「谁能访问」仍是 `sources.accessList`（§4），1.4 起行里多了可选的 `grantId`、`role`、`workspaceIds`、`expiresAtMs`。

<!-- rpc:begin contract=§24 -->

| procedure                | 方法与路径                                     | scope                  | 错误码                                                                                                                                                                 |
| ------------------------ | ---------------------------------------------- | ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `grants.list`            | `GET /v1/grants`                               | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`bad_request`                                                                                           |
| `grants.create`          | `POST /v1/grants`                              | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`team_policy_denied`、`guests_disabled`、`email_unverified`、`limit_reached`、`conflict`、`bad_request` |
| `grants.update`          | `PATCH /v1/grants/{grantId}`                   | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`bad_request`                                                                                           |
| `grants.revoke`          | `DELETE /v1/grants/{grantId}`                  | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`                                                                                                          |
| `grants.effective`       | `GET /v1/grants/effective`                     | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`                                                                                                          |
| `accessRequests.create`  | `POST /v1/access-requests`                     | `session`              | `not_found`、`conflict`、`limit_reached`、`plan_feature_unavailable`、`email_unverified`、`rate_limited`、`bad_request`                                                |
| `accessRequests.list`    | `GET /v1/access-requests`                      | `session`              | —                                                                                                                                                                      |
| `accessRequests.approve` | `POST /v1/access-requests/{requestId}/approve` | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`conflict`、`team_policy_denied`、`limit_reached`                                                       |
| `accessRequests.deny`    | `POST /v1/access-requests/{requestId}/deny`    | `owner` / `team:admin` | `not_found`、`source_access_denied`、`team_role_insufficient`、`conflict`                                                                                              |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §25 审计

协议 1.4。团队审计记录：成员、分组、主机登记与撤销、授予、链接、设置、计划变更，以及每一次断言签发（带 `jti`，可与 core 审计对账）。只记主体、动作、对象、时间与来源 IP 的前缀，不记任何内容与令牌。账号本人的安全日志是 `me.securityLog`（§29）。

- `audit.list`：`fromMs`（含）/ `toMs`（不含）/ `actor` / `sourceId` / `action`（前缀）筛选，游标分页（`limit` 缺省 100、最多 500，`nextCursor` 原样回传）。
- `audit.export`：同样的筛选，`format = csv | json`，一次最多 10 000 行，超出答 `400 bad_request`；`content` 是序列化后的文本。
- 保留期按计划（Free 7 天、Pro 30 天、Team 90 天、Enterprise 1 年起），由作业清理；查询与导出 M2 起。

<!-- rpc:begin contract=§25 -->

| procedure      | 方法与路径                            | scope        | 错误码                                               |
| -------------- | ------------------------------------- | ------------ | ---------------------------------------------------- |
| `audit.list`   | `GET /v1/teams/{teamId}/audit`        | `team:admin` | `not_found`、`team_role_insufficient`、`bad_request` |
| `audit.export` | `GET /v1/teams/{teamId}/audit/export` | `team:admin` | `not_found`、`team_role_insufficient`、`bad_request` |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §26 计费

协议 1.4。接替 §9：按空间，路径 `/v1/billing/…`，输入带 `space`（`{ kind: "personal" }` 或 `{ kind: "team", teamId }`；GET 里写成 `space[kind]=team&space[teamId]=…`）。scope 是 `session`（个人空间）或 `team:billing`（团队空间的 owner 与 billing）；`billing.plan` / `billing.usage` 只读，团队空间另放开 `team:admin`（owner、admin、billing，协议包 0.11.0 起，只加不改），其余过程（含 invoices、checkout、portal）仍只给 owner 与 billing。

- `billing.plan` / `billing.usage`（M1）：计划代号、状态（`active` / `trialing` / `past_due` / `canceled`）、席位、周期；用量项 `{ metric, value, limit }`，`limit = null` 为不限。
- `billing.usage` 入参加 `granularity`（`month` 缺省 / `day`，1.4 补充）：`day` 时出参多 `series: { startMs, relayBytes }[]`，本计费周期内每天一条（`startMs` 为当天 UTC 零点）。数据源是中继的 `relay_traffic_buckets` 小时桶按团队汇总到天，口径是入口节点计的端到端字节（`e2e`）；个人空间同一条（`space = personal`）。
- `billing.checkout` / `billing.portal`（M2）：答支付服务的结账页 / 客户门户地址；`returnUrl` 必须在控制面的 `APP_ORIGIN` 之下。支付服务未配置答 `503 unavailable`。
- `billing.webhook`（M2）：`POST /v1/billing/webhooks/{provider}`，scope `billing-webhook`——不带账号，按提供方的签名头验证原始请求体，对不上答 `401 webhook_signature_invalid`；体按提供方原样接收，不在契约里展开；回调更新订阅与配额。
- `billing.invoices`（M2）：发票列表，游标分页，`hostedUrl` / `pdfUrl` 指向支付服务托管的页面，金额为最小货币单位。
- 个人空间的订阅（Pro）用同一组 procedure（`space = personal`），是否开放由计划决定。

<!-- rpc:begin contract=§26 -->

| procedure          | 方法与路径                             | scope                                     | 错误码                                                                                                      |
| ------------------ | -------------------------------------- | ----------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `billing.plan`     | `GET /v1/billing/plan`                 | `session` / `team:admin` / `team:billing` | `not_found`、`team_role_insufficient`                                                                       |
| `billing.usage`    | `GET /v1/billing/usage`                | `session` / `team:admin` / `team:billing` | `not_found`、`team_role_insufficient`                                                                       |
| `billing.checkout` | `POST /v1/billing/checkout`            | `session` / `team:billing`                | `not_found`、`team_role_insufficient`、`bad_request`、`conflict`、`unavailable`、`plan_feature_unavailable` |
| `billing.invoices` | `GET /v1/billing/invoices`             | `session` / `team:billing`                | `not_found`、`team_role_insufficient`、`unavailable`                                                        |
| `billing.portal`   | `POST /v1/billing/portal`              | `session` / `team:billing`                | `not_found`、`team_role_insufficient`、`conflict`、`unavailable`                                            |
| `billing.webhook`  | `POST /v1/billing/webhooks/{provider}` | `billing-webhook`                         | `webhook_signature_invalid`、`bad_request`、`not_found`                                                     |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §27 API 令牌

协议 1.4（M2）。个人访问令牌，或属于团队、不随某个人离开而失效的团队令牌（只有团队 admin 能建）。范围：`directory:read`（只读目录）、`grants:manage`（管理授予）、`hosts:manage`（管理主机），可设到期。令牌明文只在 `apiTokens.create` 的答案里出现一次，库里只存哈希；API 令牌不能再建 API 令牌。用 `Authorization: Bearer <令牌>` 调用，权限是令牌范围与创建者（或团队）权限的交集。

<!-- rpc:begin contract=§27 -->

| procedure          | 方法与路径                        | scope                    | 错误码                                                   |
| ------------------ | --------------------------------- | ------------------------ | -------------------------------------------------------- |
| `apiTokens.create` | `POST /v1/api-tokens`             | `session` / `team:admin` | `team_role_insufficient`、`limit_reached`、`bad_request` |
| `apiTokens.list`   | `GET /v1/api-tokens`              | `session` / `team:admin` | `team_role_insufficient`                                 |
| `apiTokens.revoke` | `DELETE /v1/api-tokens/{tokenId}` | `session` / `team:admin` | `not_found`、`team_role_insufficient`                    |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §28 运营后台

1.4 补充：`admin.*` 除 Bearer 运营令牌外，也接受**运营会话**——`auth.login` 带 `scope: "operator"` 签出的 Cookie `__Host-armadra-ops`（配刷新 Cookie `__Host-armadra-ops-refresh`，二者都是 `Path=/`；访问寿命 1 小时，不覆盖普通会话 `__Host-armadra-cloud` / `__Host-armadra-cloud-refresh`；同名重复 Cookie 的处理见 §1），此时照样校验 `X-Armadra-CSRF`（§1）。只对账号带 `operator` 范围且开了两步验证的运营账号签出：非运营账号答 `403 forbidden`，运营账号没开两步验证答 `403 operator_mfa_required`；走两步验证时范围随挑战带到 `auth.mfaVerify`。personal 没有运营账号：`scope: "operator"` 一律答 `403 forbidden`。`auth.refresh` / `auth.logout` 带 `scope: "operator"` 作用于这枚 Cookie（要它的 CSRF 值），不带则作用于普通会话，见 §1。该 Cookie 只对 `admin.*` 与它自己的 `auth.logout` / `auth.refresh` 有效，过期后回到全新登录。运营会话的 `auth.refresh` 同样免 CSRF（§1），`auth.logout` 要 `__Host-armadra-ops` 对应的 CSRF 值。

协议 1.4。单独的 scope `operator`（运营令牌，`Authorization: Bearer`），不对外公开。只读元数据：账号与团队的计划、用量、主机数、状态——看不到任何内容与令牌，也不提供「以用户身份登录」。

- 停用 / 恢复账号或团队（`accountStatus` / `teamStatus`，必须写 `reason`）：拒绝登录与断言、踢掉连接。
- 中继节点列表与排空（`relayNodes` / `relayNodeDrain`，GOAWAY rebalance）。`relayNodeDrain` 带可选 `reason`（1–500 字，协议包 0.11.0 起，只加不改），写进审计；缺省时审计里理由为空。
- 事故开关（`incident` / `incidentSet`）：停签断言、停注册；中继侧拒绝新连接、保留在线连接。三项开关：`pauseAssertions`（停签断言）、`pauseRegistrations`（停注册）、`rejectNewConnections`（中继节点拒绝新的隧道与端到端升级，在线连接保留；1.4 发布后的只加不改补充，协议包 0.11.0 起，缺省 = 关）；`incidentSet` 只改给出的项，必须带 `reason`。
- 租户搜索 `admin.tenants`：账号与团队混排的一张表（名称 / 邮箱 / slug 模糊查）；详情与停用 / 恢复按 `kind` 走 `account` / `accountStatus` 或 `team` / `teamStatus`，详情多席位、本月流量与最近 5 条运营动作。
- 节点行多当前连接数 `connections`。
- 对外状态（M2）：`status.get` 匿名可读（`GET /v1/status`，组件状态与公告），`admin.statusPublish` 发布。
- 滥用（M2）：`admin.abuseList` 列流量异常 / 举报信号，`admin.linkFreeze` 冻结 / 解冻链接（冻结后 `links.accept` 答 `403 link_frozen`）。
- 功能开关（M2）：`admin.flagsList` / `admin.flagsSet`，全部团队开或灰度到指定团队。
- 所有写操作都写审计（`actor.kind = "operator"`），都必须带 `reason`。

<!-- rpc:begin contract=§28 -->

| procedure              | 方法与路径                                  | scope       | 错误码                                  |
| ---------------------- | ------------------------------------------- | ----------- | --------------------------------------- |
| `admin.tenants`        | `GET /v1/admin/tenants`                     | `operator`  | `not_found`、`forbidden`                |
| `admin.accounts`       | `GET /v1/admin/accounts`                    | `operator`  | `not_found`、`forbidden`                |
| `admin.account`        | `GET /v1/admin/accounts/{accountId}`        | `operator`  | `not_found`、`forbidden`                |
| `admin.accountStatus`  | `PUT /v1/admin/accounts/{accountId}/status` | `operator`  | `not_found`、`forbidden`                |
| `admin.teams`          | `GET /v1/admin/teams`                       | `operator`  | `not_found`、`forbidden`                |
| `admin.team`           | `GET /v1/admin/teams/{teamId}`              | `operator`  | `not_found`、`forbidden`                |
| `admin.teamStatus`     | `PUT /v1/admin/teams/{teamId}/status`       | `operator`  | `not_found`、`forbidden`                |
| `admin.relayNodes`     | `GET /v1/admin/relay-nodes`                 | `operator`  | `not_found`、`forbidden`                |
| `admin.relayNodeDrain` | `PUT /v1/admin/relay-nodes/{node}/drain`    | `operator`  | `not_found`、`forbidden`                |
| `admin.incident`       | `GET /v1/admin/incident`                    | `operator`  | `not_found`、`forbidden`                |
| `admin.incidentSet`    | `PUT /v1/admin/incident`                    | `operator`  | `not_found`、`forbidden`                |
| `admin.statusPublish`  | `PUT /v1/admin/status`                      | `operator`  | `not_found`、`forbidden`、`bad_request` |
| `admin.abuseList`      | `GET /v1/admin/abuse`                       | `operator`  | `not_found`、`forbidden`                |
| `admin.linkFreeze`     | `PUT /v1/admin/links/{linkId}/frozen`       | `operator`  | `not_found`、`forbidden`                |
| `admin.flagsList`      | `GET /v1/admin/flags`                       | `operator`  | `not_found`、`forbidden`                |
| `admin.flagsSet`       | `PUT /v1/admin/flags/{flag}`                | `operator`  | `not_found`、`forbidden`、`bad_request` |
| `status.get`           | `GET /v1/status`                            | `anonymous` | `rate_limited`                          |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §29 账号补充

协议 1.4。

- **邮箱验证**：注册后发验证邮件；`auth.emailVerify`（匿名，一次性令牌，24 小时）同时用于改邮箱的确认，答 `{ email, emailVerified: true }`；令牌无效 / 用过答 `400 email_token_invalid`，过期答 `410 email_token_expired`；`auth.emailVerifyResend` 按账号限流。
- **找回口令**：`auth.passwordResetStart` 总是答成功（不泄露邮箱是否注册），邮件链接 30 分钟有效；`auth.passwordResetFinish` 过同一套口令策略，成功后撤销该账号全部设备、不答会话，要重新登录。`auth.register` 与 `auth.passwordResetStart` 可带 `challenge`（§16 的 scope 多了 `auth.register`、`auth.passwordResetStart`）。
- **第三方登录**（M2）：`oauthStart` 答授权地址与 `state`，`oauthFinish` 用回调的 `code` 换会话（可能答 MFA 挑战）；提供方 `github` / `google`；绑定到已有账号要先验证邮箱（`403 email_unverified`）。`oauthUnlink` 要求至少留一种登录方式（`409 conflict`）。单点登录（`sso*`，M3）暂不预留。
- **`me.emailChange`**：要口令；新地址收验证邮件，旧地址收通知；验证前旧邮箱照常生效。
- **设备授权页**：`auth.deviceGet` 答发起设备的平台与名字（设计 §11.5 要求页面显示），另带可选 `ipPrefix`（发起请求的 IP，IPv4 原样、IPv6 截到 /64，协议包 0.11.0 起，只加不改），页面据此提示用户核对请求来源，`auth.deviceApprove` 批准，`auth.deviceDeny` 拒绝（发起设备的 `devicePoll` 答 `denied`）。
- **通知偏好**（M2）：`me.notificationPrefs` / `me.notificationPrefsUpdate`，邀请、访问申请、主机上下线按类开关邮件；`security` 恒为 `true`，不可改。
- **`me.spaces`**：个人空间 + 所属的每个团队及角色、计划，控制台顶部的空间切换用。
- **`me.securityLog`**：登录、新设备、改口令、两步验证变化；本人可见，游标分页。
- **`me.delete` / `me.deleteCancel`**：注销账号要口令；先转移名下团队的 owner（否则 `409 team_owner_required`）；7 天冷静期（`purgeAtMs`），期内可取消。
- **`me.export`**：导出账号、设备、团队身份、授予与安全日志（JSON），按账号限流。
- `me.get` 的答案 1.4 起多可选的 `teams`（所属团队）与 `scopes`（账号持有的特殊范围，如运营方的 `operator`，控制台据此显示运营入口）；`orgs` 保留给企业组织层，恒为空。

<!-- rpc:begin contract=§29 -->

| procedure                    | 方法与路径                              | scope       | 错误码                                                                                                                                                                        |
| ---------------------------- | --------------------------------------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auth.deviceGet`             | `GET /v1/auth/device`                   | `session`   | `device_code_expired`、`not_found`、`rate_limited`                                                                                                                            |
| `auth.deviceDeny`            | `POST /v1/auth/device/deny`             | `session`   | `device_code_expired`、`not_found`                                                                                                                                            |
| `auth.emailVerify`           | `POST /v1/auth/email/verify`            | `anonymous` | `email_token_invalid`、`email_token_expired`、`rate_limited`                                                                                                                  |
| `auth.emailVerifyResend`     | `POST /v1/auth/email/resend`            | `session`   | `rate_limited`、`conflict`                                                                                                                                                    |
| `auth.passwordResetStart`    | `POST /v1/auth/password-reset/start`    | `anonymous` | `rate_limited`、`challenge_required`、`challenge_invalid`                                                                                                                     |
| `auth.passwordResetFinish`   | `POST /v1/auth/password-reset/finish`   | `anonymous` | `email_token_invalid`、`email_token_expired`、`rate_limited`、`password_too_short`、`password_too_long`、`password_too_common`、`password_contains_name`、`password_breached` |
| `auth.oauthStart`            | `POST /v1/auth/oauth/{provider}/start`  | `anonymous` | `rate_limited`、`bad_request`、`not_found`                                                                                                                                    |
| `auth.oauthFinish`           | `POST /v1/auth/oauth/{provider}/finish` | `anonymous` | `credentials_invalid`、`email_unverified`、`account_disabled`、`rate_limited`、`conflict`                                                                                     |
| `auth.oauthUnlink`           | `DELETE /v1/auth/oauth/{provider}`      | `session`   | `not_found`、`conflict`                                                                                                                                                       |
| `me.emailChange`             | `POST /v1/me/email`                     | `session`   | `email_taken`、`credentials_invalid`、`rate_limited`、`bad_request`                                                                                                           |
| `me.notificationPrefs`       | `GET /v1/me/notification-prefs`         | `session`   | —                                                                                                                                                                             |
| `me.notificationPrefsUpdate` | `PATCH /v1/me/notification-prefs`       | `session`   | `bad_request`                                                                                                                                                                 |
| `me.spaces`                  | `GET /v1/me/spaces`                     | `session`   | —                                                                                                                                                                             |
| `me.securityLog`             | `GET /v1/me/security-log`               | `session`   | `bad_request`                                                                                                                                                                 |
| `me.delete`                  | `POST /v1/me/delete`                    | `session`   | `credentials_invalid`、`team_owner_required`、`rate_limited`                                                                                                                  |
| `me.deleteCancel`            | `POST /v1/me/delete/cancel`             | `session`   | `conflict`                                                                                                                                                                    |
| `me.export`                  | `POST /v1/me/export`                    | `session`   | `rate_limited`                                                                                                                                                                |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §30 协议 1.4 的声明与令牌

协议 1.4（协议包 0.10.0）。新增的 procedure 与字段是加法；但断言 `team`、令牌 `org` → `team` 的语义是原地改的，只因为 SaaS 从未上线、这些声明从未签发过才安全。

| 位置                          | 1.3                                            | 1.4                                                                                                                                                |
| ----------------------------- | ---------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| 断言 `team`                   | `{ teamId, role: "lead" \| "member" }`（部门） | `{ teamId, role: "owner" \| "admin" \| "member" \| "billing" \| "guest" }`（租户团队）                                                             |
| 断言 `org`                    | `{ orgId, role }`                              | 不变，保留给以后团队之上的企业组织层；1.4 没有签发方                                                                                               |
| 中继令牌 `org`                | 源所属组织                                     | 删除，换成可选的 `team`（团队 id 字符串），用于 `edge.*.team` 限流与按团队计量                                                                     |
| 隧道令牌 `org`                | 源所属组织                                     | 删除，换成可选的 `team`                                                                                                                            |
| `links.create` 的 `org_join`  | 从未实现                                       | 换成 `team_join`（`teamId`、`role`、`emailDomains`），`org_join` 枚举值保留                                                                        |
| `sources.registrationToken`   | `orgId`                                        | `space: { kind: "personal" } \| { kind: "team", teamId }`，缺省个人空间                                                                            |
| `sources.list` / `links.list` | `orgId`                                        | `space`（`{ kind: "personal" }` \| `{ kind: "team", teamId }`，缺省 = 全部可达的空间）                                                             |
| `sourceVia`                   | `owner` / `link` / `org`                       | 多 `team`                                                                                                                                          |
| 契约                          | `orgs.*`、`billing`（挂组织）                  | 删 `orgs.*`；追加 `teams` / `invitations` / `groups` / `grants` / `accessRequests` / `audit` / `billing`（按团队）/ `apiTokens` / `admin`，§21–§29 |

- 断言的 `team` 只在主机属于团队时带；个人空间的主机不带。`role`、`scp` 是合成后的主机角色与范围，语义不变。
- **老中继**收到带 `team` 的令牌照常放行（令牌 schema 非 strict，更高 minor 的声明被忽略）。
- **1.3 的 core 会拒绝**：它按严格的 1.3 `assertionClaimsSchema` 校验断言，`team.role` 取 `owner` / `admin` / `billing` / `guest` 这些 1.4 的值时整张断言无效；它也只认 `org` + `role` 作 SaaS 准入。1.3 的签发方没有发过 `org` / 旧 `team`，所以没有存量令牌需要兼容。
- **签发规则**：控制面只向登记的 `coreVersion`（`sources.register` 已上报）不低于协议包常量 `MIN_CORE_VERSION_TEAM_ASSERTION` 的源签带 `team` 的断言（只带 `role` / `scp`、不带 `team` 的断言用 `MIN_CORE_VERSION_ROLE_ONLY_ASSERTION`）；两个常量都是语义化版本号，也导出在 `schema/constants.json` 的 `minCoreVersion`，现在都是 `0.10.0`——这是第一个采用协议 1.4 的 Armadra 版本的占位，发版时填定；更老的 core 一律不带 `team`（主机属于团队时也不签，答 `403 source_access_denied`）。
- core 侧（Armadra）的 `cloud/login` 改成以 `team` + `role` 作 SaaS 准入，另在 Armadra 仓提 PR。
- **节点令牌**（1.4 补充）：运营方给每个中继节点发的令牌（§17 的 `relay-node` scope）有了固定的声明：头 `typ = armadra-node`，`{ iss, sub = 节点名, aud = "relay-node", iat, exp, jti, ver }`（`nodeTokenClaimsSchema`，`schema/assertion/node-token-claims.json`，夹具 `fixtures/assertion/node-token.jwt`）。现有已部署的中继继续用原来的不透明节点令牌；这是新增的声明格式，不改变任何已部署 procedure。
- 新增的 scope（`team:member` / `team:admin` / `team:owner` / `team:billing` / `group:lead` / `operator` / `billing-webhook`）取代 `org:admin` / `org:owner`。

## §31 设备信任

协议 1.4（设计 `saas-design.md` §11.4）。团队成员第一次从新设备连团队主机，会停在 `device_pending`，要有人从**自己已受信的设备**背书；缺省的背书人是主机 owner 与团队 admin；团队设置「新设备背书人」可以收窄为只有主机 owner；成员本人从自己另一台已受信的设备背书缺省开启（`memberSelfEndorse`），也可以关。

- **云端不担保设备，也不参与批准。** 批准走端到端通道发到主机，用的是现有的「已受信设备背书」路径；云端看不到也改不了背书内容，否则控制面运营方就能冒充设备。
- 云端只做两件事：把待信任请求（成员、设备名、平台、主机、过期时间；**不含校验码**）推给背书人——`trust.list` 与 me-stream 的 `deviceTrustRequested` / `deviceTrustResolved`（§32）——以及记下背书人的拒绝（`trust.reject`，请求作废，设备继续停在 `device_pending`）。已被批准或已过期的请求答 `409 conflict`。
- `trust.list` / `trust.reject` 的 scope 含 `team:member`：成员只看得到、处理得了自己的待信任请求（`memberSelfEndorse` 开着时），主机 owner / 团队 admin 按 `deviceEndorsers` 设置看到团队里的请求，处理器据此收窄；缺省列调用者能背书的全部空间，给 `space` 只列那个空间；`trust.list` 对非背书人答过滤后的空列表；`trust.reject` 不是背书人答 `403 team_role_insufficient`；`device_untrusted` 云端不使用。**guest 的请求只给主机所有者**（成员本人背书与 admin 背书都不适用，core 只认 owner / admin / member）。`device_untrusted` 表示背书要在已受信的设备上做。
- **校验码（SAS）云端不经手**（1.4 补充：`trustRequest` 删去 `sas`）：校验码只在新设备与主机之间的端到端通道里产生，由新设备显示；背书人把新设备上显示的码输入自己已受信的设备，由端到端通道在主机侧核对，核对通过才批准。`trust.list` 与 me-stream 的 `deviceTrustRequested` 都没有这个字段。
- 走分享链接进来的访客仍按链接的 `max_devices` 与指纹钉扎，不经这里。
- **core 的上报**（1.4 补充，scope `source`，`Authorization: Source <jws>`，同 `sources.heartbeatHint`）：core 在团队成员的新设备停在 `device_pending` 时调 `sources.trustRequest`，入参 `{ pendingId, accountId, deviceId, expiresAtMs }`——`pendingId` 是 core 侧待信任条目的 id，兼作幂等键（同一 `pendingId` 重复调答同一个 `requestId`）；`expiresAtMs` 至多比现在晚 24 小时（超出答 `400 bad_request`）；设备名与平台由控制面从自己的设备表填，不由 core 报。答 `{ requestId, status }`：`pending` 表示已登记并通知背书人；`trust.list` 的每条请求带 `pendingId`（同一个 id，不是秘密，协议包 0.11.0 起，可选字段），控制台背书时用它调 core 的 `devices.approve`；刚被背书人拒绝过的设备在冷却期内答 `rejected`，不再通知。（冷却 1 小时；冷却结束后同一个 `pendingId` 重报会重新开成 `pending`，`requestId` 是新的）条目被批准或过期后 core 调 `sources.trustResolve`（`{ result: "trusted" | "expired" }`），控制面撤掉通知并发 `deviceTrustResolved`（§32）。错误码：`source_access_denied`（源已不在团队或账号不是该团队成员）、`not_found`（`pendingId` 或设备不存在）、`conflict`（已解决的请求再次登记 / 解决）、`rate_limited`。已部署的中继不实现这两条（personal 答 `404 not_found`，cloud 实现前答 `501 not_implemented`）。

<!-- rpc:begin contract=§31 -->

| procedure              | 方法与路径                                               | scope                                  | 错误码                                                                         |
| ---------------------- | -------------------------------------------------------- | -------------------------------------- | ------------------------------------------------------------------------------ |
| `sources.trustRequest` | `POST /v1/sources/me/trust-requests`                     | `source`                               | `source_access_denied`、`not_found`、`conflict`、`rate_limited`、`bad_request` |
| `sources.trustResolve` | `POST /v1/sources/me/trust-requests/{pendingId}/resolve` | `source`                               | `source_access_denied`、`not_found`、`conflict`、`rate_limited`                |
| `trust.list`           | `GET /v1/trust-requests`                                 | `owner` / `team:admin` / `team:member` | `team_role_insufficient`、`not_found`                                          |
| `trust.reject`         | `POST /v1/trust-requests/{requestId}/reject`             | `owner` / `team:admin` / `team:member` | `not_found`、`team_role_insufficient`、`device_untrusted`、`conflict`          |

输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。

<!-- rpc:end -->

## §32 me-stream 的团队事件

协议 1.4。`GET /v1/me/stream`（§6）的事件多七种，schema 在协议包 `cloud-api` 的 `meStreamEventSchema`。SaaS 控制面（M1）发送 `memberChanged`、`invitationChanged`、`grantChanged`（只发给受众里的账号，`invitationChanged` 只发给邮箱已验证的受邀账号），其余四种预留、还没有发送方；老客户端遇到不认识的 `type` 应忽略。事件只带标识，不带内容，客户端收到后重拉对应列表。

| `type`                 | 字段                                                                      | 含义                                  |
| ---------------------- | ------------------------------------------------------------------------- | ------------------------------------- |
| `memberChanged`        | `teamId`、`accountId?`、`change: added \| removed \| roleChanged`、`atMs` | 团队成员变化                          |
| `invitationChanged`    | `invitationId`、`teamId?`、`sourceId?`、`state`、`atMs`                   | 邀请被接受、拒绝、撤销、过期          |
| `grantChanged`         | `sourceId`、`grantId?`、`teamId?`、`atMs`                                 | 主机授予变化                          |
| `accessRequested`      | `requestId`、`sourceId`、`teamId?`、`atMs`                                | 有新的访问申请要处理                  |
| `deviceTrustRequested` | `requestId`、`sourceId`、`teamId?`、`atMs`                                | 有新设备待背书（详情用 `trust.list`） |
| `deviceTrustResolved`  | `requestId`、`outcome: trusted \| rejected \| expired`、`atMs`            | 待信任请求已处理                      |
| `planChanged`          | `space`、`plan`、`status`、`atMs`                                         | 空间的计划或订阅状态变化              |
