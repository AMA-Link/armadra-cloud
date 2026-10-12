# 中继只转端到端通道（0.5.0）

> 状态：已实施（cloud 0.5.0，Armadra 0.6.0）。契约：cloud-api §7、§12、§20。Refs #43、AMA-Link/Armadra#359。

## 结论

中继不再转明文的应用请求。源地址上唯一的入口是 `_relay/e2e` 的 WebSocket 升级（子协议 `armadra-e2e.1` + `armadra-relay.<jwt>`），中继开 `OPEN kind: "e2e"` 的流，之后两边只转 `armadra-e2e/1` 的密文单元。

## 删掉的

| 部分                              | Node 中继                           | Workers 中继                               |
| --------------------------------- | ----------------------------------- | ------------------------------------------ |
| HTTP 转发（请求头改写、响应解析） | `edge.ts` 的转发，内核 `forward.ts` | `http-forward.ts`                          |
| 明文 WebSocket 桥                 | `ws-downstream.ts`                  | `ws-bridge.ts`；唤醒时旧桥以 4404 关闭     |
| 媒体票（签、兑）                  | 内核 `media-tickets.ts`             | `media-tickets.ts`（DO 同步 KV）           |
| CORS 预检                         | 内核 `cors.ts` 的预检部分           | 入口闸的 `LIMIT_PREFLIGHT` / `LIMIT_MEDIA` |

源地址上这些请求一律 `404 not_found`，不验令牌、不进隧道。HTTP/1.1 与 WebSocket 帧的编解码留在协议包（`./http1`、`./ws-frames`），由客户端在通道里用。

## 中继还能看到的

源、令牌的 `sub` / `dev`、通道开关时间与关闭码、两个方向的密文字节数（统计 §19 的 `e2e` 类别）。看不到方法、路径、头、正文、会话令牌与内层 WebSocket 帧。core 统计里通道对端设备那一行 `transport` 的字节与中继的 `e2e` 字节逐字节相等（Armadra 契约 §76）。

## 上线顺序

1. Armadra 0.6.0：客户端与 core 只走通道（`security.requireE2e` 缺省开、客户端不再退回明文）。
2. cloud 0.5.0：中继删明文路径。旧 core（不报 `e2e`）经中转连不上，客户端提示升级；直连不受影响。

浏览器端的边界不变：中继托管的页面在下发那一刻被替换，端到端加密防不住；`/j/` 落地页首选「在 Armadra 中打开」。
