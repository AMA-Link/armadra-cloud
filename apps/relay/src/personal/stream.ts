/**
 * `GET /v1/me/stream`（契约 §6，协议包 §5.7）：源上下线等事件的 WS。
 *
 * - 票据：`POST /v1/me/stream/ticket`（会话）→ 30 秒、一次性，只在内存，存哈希；
 * - 升级：子协议 `armadra-ticket.<票>`，选中同一个子协议回给浏览器；
 * - 事件：owner 收全部源的；访客只收链接指向的那个源的，以及发给它那条链接的 `accessRevoked`；
 * - 心跳：客户端 `{ type: "ping" }` → `pong`；服务端每 30 秒发 `ping`，同时复核设备仍有效，
 *   失效（登出、撤销）就以 4401 关闭。
 */
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import { TICKET_SUBPROTOCOL_PREFIX } from "@armadra/platform-protocol";
import {
  ME_STREAM_PATH,
  ME_STREAM_PING_MS,
  meStreamClientMessageSchema,
  type MeStreamEvent,
} from "@armadra/platform-protocol/cloud-api";
import { SESSION } from "@armadra/platform-protocol/identity-vectors";
import { CLOSE } from "@armadra/platform-protocol/tunnel";
import { WebSocketServer, type WebSocket } from "ws";

import { rejectUpgrade } from "../http/errors.ts";
import type { PersonalContext, PersonalEvent } from "./context.ts";
import { randomToken, secretHash } from "./secrets.ts";
import { isLiveDevice, type Caller } from "./sessions.ts";

const MAX_TICKETS = 1_000;

export interface MeStream {
  ticket(caller: Caller): { ticket: string; expiresAtMs: number };
  /** `upgrade` 钩子：路径是 `/v1/me/stream` 就认领。 */
  handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): boolean;
  close(): void;
}

export function createMeStream(
  context: PersonalContext,
  options: { pingMs?: number } = {},
): MeStream {
  const tickets = new Map<string, { caller: Caller; expiresAtMs: number }>();
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

  const sweep = () => {
    const nowMs = context.now();
    for (const [key, entry] of tickets)
      if (entry.expiresAtMs <= nowMs) tickets.delete(key);
  };

  /** 这个调用方能看到这个源吗（访客只看链接指向的那个）。 */
  const sees = (caller: Caller, sourceId: string | undefined): boolean => {
    if (caller.role === "owner") return true;
    const link = context.store.current.links.find(
      (row) => row.linkId === caller.linkId,
    );
    return link !== undefined && link.sourceId === sourceId;
  };

  const stillValid = (caller: Caller): boolean => {
    const device = context.store.current.devices.find(
      (row) => row.deviceId === caller.deviceId,
    );
    return device !== undefined && isLiveDevice(device, context.now());
  };

  const attach = (ws: WebSocket, caller: Caller) => {
    clients.add(ws);
    const send = (event: MeStreamEvent) => {
      if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(event));
    };
    const deliver = (event: PersonalEvent, audience?: { linkId: string }) => {
      if (audience) {
        if (caller.linkId === audience.linkId) send(event);
        return;
      }
      if (sees(caller, event.sourceId)) send(event);
    };
    const offBus = context.subscribe(deliver);
    const offPresence = context.control.presence.subscribe((presence) => {
      const source = context.store.current.sources.find(
        (row) => row.sourceId === presence.sourceId && row.revokedAtMs === null,
      );
      if (!source) return;
      if (presence.type === "sourceOnline")
        deliver({
          type: "sourceOnline",
          sourceId: presence.sourceId,
          sinceMs: presence.atMs,
          relayOrigin: context.signer.issuer,
        });
      else
        deliver({
          type: "sourceOffline",
          sourceId: presence.sourceId,
          atMs: presence.atMs,
        });
    });
    const timer = setInterval(() => {
      if (!stillValid(caller)) {
        ws.close(CLOSE.unauthenticated, "session_revoked");
        return;
      }
      send({ type: "ping", ts: context.now() });
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
        send({ type: "pong", ts: context.now() });
    });
    ws.on("error", () => {});
    ws.once("close", () => {
      clients.delete(ws);
      clearInterval(timer);
      offBus();
      offPresence();
    });
  };

  return {
    ticket(caller) {
      sweep();
      if (tickets.size >= MAX_TICKETS) tickets.clear();
      const ticket = randomToken(32);
      const expiresAtMs = context.now() + SESSION.wsTicketTtlMs;
      tickets.set(secretHash("ticket", ticket), { caller, expiresAtMs });
      return { ticket, expiresAtMs };
    },

    handleUpgrade(req, socket, head) {
      const path = (req.url ?? "/").split("?")[0];
      if (path !== ME_STREAM_PATH) return false;
      socket.on("error", () => {});
      const offered = String(req.headers["sec-websocket-protocol"] ?? "")
        .split(",")
        .map((value) => value.trim())
        .find((value) => value.startsWith(TICKET_SUBPROTOCOL_PREFIX));
      const ticket = offered?.slice(TICKET_SUBPROTOCOL_PREFIX.length);
      const key = ticket ? secretHash("ticket", ticket) : "";
      const entry = tickets.get(key);
      tickets.delete(key);
      if (
        !entry ||
        entry.expiresAtMs <= context.now() ||
        !stillValid(entry.caller)
      ) {
        rejectUpgrade(socket, "unauthenticated");
        return true;
      }
      wss.handleUpgrade(req, socket, head, (ws) => attach(ws, entry.caller));
      return true;
    },

    close() {
      for (const ws of clients) ws.terminate();
      clients.clear();
      tickets.clear();
      wss.close();
    },
  };
}
