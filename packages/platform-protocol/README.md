# @armadra/platform-protocol

Armadra 平台的协议包：core、页面、手机、控制面与中继共用的 schema、类型、纯编解码与常量。真相源在 `AMA-Link/armadra-cloud`，Armadra 钉精确版本。

| 子路径               | 内容                                                                                                |
| -------------------- | --------------------------------------------------------------------------------------------------- |
| `.`                  | 版本与路径常量（`PROTOCOL_VERSION`、`TUNNEL_PATH`…）与公共类型                                      |
| `./tunnel`           | 隧道 `t/v1`：握手消息、帧编解码 `encodeFrame` / `decodeFrame`、限制、RST 与关闭码                   |
| `./assertion`        | 五种令牌的声明、范围 `scp` 与只读门、compact JWS 拆装（不验签）、时间 / 受众校验、`ReplayCache`     |
| `./cloud-api`        | `/v1/*` 契约 `cloudContract`（oRPC）、共用形状、`/v1/me/stream` 事件                                |
| `./core-api`         | core 契约 §31–§33 的请求 / 响应 schema                                                              |
| `./errors`           | 错误码注册表与线上错误形状 `{ code, message, requestId?, details? }`                                |
| `./identity-vectors` | 口令策略、scrypt 参数、令牌哈希前缀、限流常量与测试向量                                             |
| `./fixtures`         | 黄金夹具（帧、握手、令牌、`/v1` 样例、HTTP/1.1 与 WebSocket 线上字节），**Node 专用、只在测试里用** |

`schema/` 是语言无关的导出（生成物，随包发布），给非 TypeScript 的实现生成类型：`assertion/*.json` 与 `tunnel/*.json` 是令牌声明与握手消息的 JSON Schema（`z.toJSONSchema`），`errors.json` 是错误码 → HTTP 状态码，`constants.json` 是协议版本、路径、隧道限制与身份常量，`cloud-openapi.json` 是 `/v1` 契约的 OpenAPI。黄金夹具 `fixtures/` 同样语言无关。

- `zod` 4 是 peer 依赖；`@orpc/contract` 1.15.4 是运行时依赖。
- 除 `./fixtures` 外不 import `node:*`，页面与手机可以直接用。
- JWT 的 `iat` / `exp` / `nbf` 是整数秒；其余时间字段是毫秒（`…Ms`）。
- 签名与验签不在包里：各仓用 `node:crypto` 的 Ed25519。
