/**
 * `GET /v1/me/stream`（契约 §6，协议包 §5.7）的 WebSocket 外壳：票据与事件扇出在控制核
 * （`@armadra/control-core/me-stream`）。
 *
 * - 升级：子协议 `armadra-ticket.<票>`，选中同一个子协议回给浏览器；
 * - 心跳：客户端 `{ type: "ping" }` → `pong`；服务端每 30 秒发 `ping`，同时复核设备仍有效，
 *   失效（登出、撤销）就以 4401 关闭。
 */
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import type { MeStreamCore } from "@armadra/control-core/me-stream";
import type { Caller } from "@armadra/control-core/sessions";
import { TICKET_SUBPROTOCOL_PREFIX } from "@armadra/platform-protocol";
import {
  ME_STREAM_PATH,
  ME_STREAM_PING_MS,
  meStreamClientMessageSchema,
  type MeStreamEvent,
} from "@armadra/platform-protocol/cloud-api";
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { rejectUpgrade } from "@armadra/relay-kernel/http/errors";
import { WebSocketServer, type WebSocket } from "ws";

export interface MeStream {
  /** `upgrade` 钩子：路径是 `/v1/me/stream` 就认领。 */
  handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): boolean;
  close(): void;
}

export function createMeStream(
  core: MeStreamCore,
  options: { pingMs?: number; now?: () => number } = {},
): MeStream {
  const now = options.now ?? Date.now;
  const wss = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    maxPayload: 4096,
    handleProtocols: (protocols) =>
      [...protocols].find((value) =>
        value.startsWith(TICKET_SUBPROTOCOL_PREFIX),
      ) ?? false,
  });
  const clients = new Set<WebSocket>();

  const attach = (ws: WebSocket, caller: Caller) => {
    clients.add(ws);
    const send = (event: MeStreamEvent) => {
      if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(event));
    };
    const detach = core.attach(caller, send);
    const timer = setInterval(() => {
      core.stillValid(caller).then(
        (valid) => {
          if (!valid) {
            ws.close(CLOSE.unauthenticated, "session_revoked");
            return;
          }
          send({ type: "ping", ts: now() });
        },
        () => {},
      );
    }, options.pingMs ?? ME_STREAM_PING_MS);
    timer.unref();
    ws.on("message", (data, isBinary) => {
      if (isBinary) return;
      let parsed: unknown;
      try {
        parsed = JSON.parse(data.toString());
      } catch {
        return;
      }
      if (meStreamClientMessageSchema.safeParse(parsed).success)
        send({ type: "pong", ts: now() });
    });
    ws.on("error", () => {});
    ws.once("close", () => {
      clients.delete(ws);
      clearInterval(timer);
      detach();
    });
  };

  return {
    handleUpgrade(req, socket, head) {
      const path = (req.url ?? "/").split("?")[0];
      if (path !== ME_STREAM_PATH) return false;
      socket.on("error", () => {});
      const offered = String(req.headers["sec-websocket-protocol"] ?? "")
        .split(",")
        .map((value) => value.trim())
        .find((value) => value.startsWith(TICKET_SUBPROTOCOL_PREFIX));
      const ticket = offered?.slice(TICKET_SUBPROTOCOL_PREFIX.length);
      core.redeem(ticket).then(
        (caller) => {
          if (!caller) {
            rejectUpgrade(socket, "unauthenticated");
            return;
          }
          wss.handleUpgrade(req, socket, head, (ws) => attach(ws, caller));
        },
        () => rejectUpgrade(socket, "unavailable"),
      );
      return true;
    },

    close() {
      for (const ws of clients) ws.terminate();
      clients.clear();
      core.clear();
      wss.close();
    },
  };
}
